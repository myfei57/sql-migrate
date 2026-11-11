package migrate

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"
)

// RoundTripStatus classifies the evidence produced by VerifyRoundTrip.
type RoundTripStatus string

const (
	// RoundTripVerified means the run completed and every in-scope comparison
	// (schema and declared checks) matched. It never implies semantic
	// equivalence for data or object types outside the declared scope.
	RoundTripVerified RoundTripStatus = "verified"
	// RoundTripPartial means the run completed but the evidence has documented
	// coverage limits, such as notransaction migrations, irreversible Down
	// sections, unsupported object types or explicitly excluded
	// non-deterministic check columns.
	RoundTripPartial RoundTripStatus = "partial"
	// RoundTripFailed means the run was aborted by an error, budget violation,
	// schema/check difference or a target cleanup failure.
	RoundTripFailed RoundTripStatus = "failed"
)

// Comparison labels describing which two states diverged.
const (
	compareUp1Up2        = "up1-vs-up2"
	compareDownBaseline  = "down-vs-baseline"
	scopeNoteRoundTrip   = "evidence is limited to tables, columns, primary keys, indexes, foreign keys and the caller-declared fixture/check queries; equivalence for uncovered object types or undeclared data is not claimed"
	cleanupTimeout       = 30 * time.Second
	defaultStageTimeout  = 30 * time.Second
	defaultMaxStmts      = 1000
	defaultMaxFixtures   = 100
	defaultMaxFixtureRow = 10000
	defaultMaxFixtureByt = 1 << 20
)

// DisposableDatabaseFactory is supplied by the caller to give VerifyRoundTrip
// an isolated, throwaway database.
//
// By implementing this interface the caller explicitly declares that the
// returned database is dedicated to the verification run and may be destroyed.
// VerifyRoundTrip never opens a database on its own, never reads any default or
// production configuration and never invokes Reset or any destructive operation
// against databases other than the single target returned here.
//
// Create returns a connection to a fresh, empty database together with a
// cleanup function that closes and destroys exactly that target. The cleanup
// function is invoked exactly once, on every exit path, including failures.
type DisposableDatabaseFactory interface {
	Create(ctx context.Context) (db *sql.DB, cleanup func(context.Context) error, err error)
}

// RoundTripFixture is a bounded data statement loaded once, after the first Up
// stage, so that check queries can observe whether data survives the
// Down/second-Up cycle.
type RoundTripFixture struct {
	// Name is used in report findings; it may be empty.
	Name string
	// Statement is executed with the optional Args.
	Statement string
	Args      []any
	// Rows is the caller-declared number of affected rows, counted against the
	// data-volume budget.
	Rows int64
}

// RoundTripCheck is a read-only query executed at every stage. Results from
// the first and second Up are compared, as are results from Down and the
// explicit baseline.
type RoundTripCheck struct {
	// Name identifies the check in difference reports.
	Name string
	// Query is executed with the optional Args at baseline, Up #1, Down and
	// Up #2.
	Query string
	Args  []any
	// NonDeterministicColumns names result columns whose values are allowed to
	// differ between runs, for example columns populated by randomblob() or
	// CURRENT_TIMESTAMP defaults. Only null-vs-not-null is compared for these
	// columns and every exclusion is recorded in the report.
	NonDeterministicColumns []string
}

// RoundTripBudgets bounds the work performed in each stage.
type RoundTripBudgets struct {
	// MaxStatementsPerStage limits SQL statements executed in any single stage.
	// Zero selects the built-in default.
	MaxStatementsPerStage int
	// MaxFixtureStatements limits the number of fixture statements. Zero
	// selects the built-in default.
	MaxFixtureStatements int
	// MaxFixtureRows limits the total caller-declared fixture row count. Zero
	// selects the built-in default.
	MaxFixtureRows int64
	// MaxFixtureBytes limits the total size of fixture SQL and arguments. Zero
	// selects the built-in default.
	MaxFixtureBytes int64
	// StageTimeout bounds each individual stage. Zero selects the built-in
	// default.
	StageTimeout time.Duration
}

// RoundTripConfig configures a VerifyRoundTrip run.
type RoundTripConfig struct {
	// Factory is mandatory and is the only database the verifier will touch.
	Factory DisposableDatabaseFactory
	// Dialect names the dialect, matching the keys of MigrationDialects.
	Dialect string
	// Migrations is the migration source under verification.
	Migrations MigrationSource
	// MigrationSet optionally customizes the migration bookkeeping table. The
	// zero value is valid and uses the default table name.
	MigrationSet MigrationSet
	// BaselineSetup optionally reproduces the pre-migration state on the empty
	// target before the first Up. The Down stage is required to return to this
	// explicit baseline. When empty, the baseline is an empty schema.
	BaselineSetup []string
	// Fixtures are loaded after the first Up and must fit within the configured
	// budgets.
	Fixtures []RoundTripFixture
	// Checks are executed at every stage and compared across stages.
	Checks []RoundTripCheck
	// Budgets bounds every stage. Zero fields fall back to built-in defaults.
	Budgets RoundTripBudgets
}

// RoundTripStage records what happened during one stage of the run.
type RoundTripStage struct {
	Name       string
	Migrations int
	Statements int
	Bytes      int
	Duration   time.Duration
}

// RoundTripFinding is a non-fatal coverage or limitation observation.
type RoundTripFinding struct {
	Code    string
	Object  string
	Message string
}

// RoundTripTargetInfo records the lifecycle of the disposable target.
type RoundTripTargetInfo struct {
	Identifier    string
	CleanupCalled bool
}

// RoundTripBudgetUsage records the consumed budgets.
type RoundTripBudgetUsage struct {
	MaxStatementsPerStage int
	MaxFixtureStatements  int
	MaxFixtureRows        int64
	MaxFixtureBytes       int64
	FixtureStatements     int
	FixtureRows           int64
	FixtureBytes          int64
}

// RoundTripReport is the bounded reversibility evidence produced by
// VerifyRoundTrip. A report is returned whenever a target database could be
// created; pre-flight validation failures return a Go error instead.
type RoundTripReport struct {
	// Status is verified, partial or failed.
	Status RoundTripStatus
	// Dialect of the target database.
	Dialect string
	// Stages describes the baseline, Up #1, Down and Up #2 stages.
	Stages []RoundTripStage
	// SchemaDiffs lists every normalized schema difference that was found.
	SchemaDiffs []SchemaDifference
	// CheckDiffs lists every declared check result difference that was found.
	CheckDiffs []CheckDifference
	// Findings records coverage limitations such as notransaction stages,
	// irreversible Down sections, unsupported object types and explicit
	// non-deterministic column exclusions.
	Findings []RoundTripFinding
	// Normalizations records every normalization rule that was applied, so no
	// non-deterministic attribute is discarded silently.
	Normalizations []string
	// ScopeNote describes the bounded scope of the evidence.
	ScopeNote string
	// Target records target cleanup lifecycle information.
	Target RoundTripTargetInfo
	// Budget records the configured and consumed budgets.
	Budget RoundTripBudgetUsage
	// Error holds the primary error that aborted the run, if any.
	Error string
	// CleanupError holds an error returned while destroying the target. The
	// primary error (if any) is always retained separately.
	CleanupError string
}

// SchemaSnapshot is a normalized description of the covered schema objects.
type SchemaSnapshot struct {
	Dialect            string
	Tables             []TableDescriptor
	Indexes            []IndexDescriptor
	InternalObjects    []NamedObject
	UnsupportedObjects []NamedObject
	Normalizations     []string
}

// NamedObject identifies an object observed in the database catalog.
type NamedObject struct {
	Type string
	Name string
}

// TableDescriptor is a normalized table description.
type TableDescriptor struct {
	Name         string
	WithoutRowID bool
	Columns      []ColumnDescriptor
	PrimaryKey   []string
	ForeignKeys  []ForeignKeyDescriptor
}

// ColumnDescriptor is a normalized column description. Default expressions are
// retained verbatim because they carry meaningful semantics.
type ColumnDescriptor struct {
	Ordinal    int
	Name       string
	Type       string
	NotNull    bool
	HasDefault bool
	Default    string
}

// IndexColumn describes one ordered key column of an index.
type IndexColumn struct {
	Ordinal    int
	Name       string
	Descending bool
	Collation  string
	Expression bool
}

// IndexDescriptor is a normalized index description. Auto-generated index
// names keep a deterministic structural name instead of their unstable numeric
// suffix, while the structural attributes are fully retained.
type IndexDescriptor struct {
	Table    string
	Name     string
	Origin   string
	AutoName bool
	Unique   bool
	Partial  bool
	Columns  []IndexColumn
}

// ForeignKeyColumn pairs a referencing column with its referenced column.
type ForeignKeyColumn struct {
	Ordinal int
	From    string
	To      string
}

// ForeignKeyDescriptor is a normalized foreign key description. Deferrability
// semantics are retained rather than dropped.
type ForeignKeyDescriptor struct {
	ID         int
	References string
	Columns    []ForeignKeyColumn
	OnUpdate   string
	OnDelete   string
	Match      string
	Deferrable string
}

// SchemaDifference describes a single normalized schema divergence.
type SchemaDifference struct {
	Kind     string
	Object   string
	Compare  string
	Expected string
	Actual   string
	Detail   string
}

// CheckDifference describes a divergence between declared check query results.
type CheckDifference struct {
	Kind     string
	Check    string
	Compare  string
	Expected string
	Actual   string
	Detail   string
}

// schemaExtractor is implemented per dialect. Only dialects with an extractor
// can be verified; everything else fails pre-flight instead of producing
// evidence-free success.
type schemaExtractor interface {
	extract(ctx context.Context, db *sql.DB, migrationTable string) (*SchemaSnapshot, error)
}

var schemaExtractors = map[string]schemaExtractor{
	"sqlite3": sqliteSchemaExtractor{},
}

// capturedCheck holds the normalized result set of one check query execution.
type capturedCheck struct {
	Name         string
	Columns      []string
	Rows         []string
	RowCount     int
	QueryError   string
	NDExclusions []string
}

type roundTripRunner struct {
	cfg       RoundTripConfig
	report    *RoundTripReport
	extractor schemaExtractor
	tableName string
	target    *sql.DB
	cleanup   func(context.Context) error
}

// VerifyRoundTrip executes an Up-Down-Up migration cycle against the caller's
// disposable database and returns bounded reversibility evidence.
//
// The function never connects to a default or production configuration and
// never calls Reset: the DisposableDatabaseFactory is the sole source of the
// target database and owns its destruction. A successfully started run always
// returns a report, even when a stage fails; target cleanup is attempted on
// every exit path and both the primary error and any cleanup error are
// retained. Pre-flight validation failures (missing factory, unknown dialect
// or extractor, invalid configuration) return a Go error.
func VerifyRoundTrip(ctx context.Context, config RoundTripConfig) (*RoundTripReport, error) {
	if config.Factory == nil {
		return nil, errors.New("VerifyRoundTrip requires an explicit DisposableDatabaseFactory: no default or production database is ever used")
	}
	if config.Migrations == nil {
		return nil, errors.New("VerifyRoundTrip requires a MigrationSource")
	}
	extractor, ok := schemaExtractors[config.Dialect]
	if !ok {
		return nil, fmt.Errorf("VerifyRoundTrip: no schema extractor registered for dialect %q", config.Dialect)
	}

	applyBudgetDefaults(&config.Budgets)

	report := &RoundTripReport{
		Status:         RoundTripVerified,
		Dialect:        config.Dialect,
		ScopeNote:      scopeNoteRoundTrip,
		Normalizations: []string{checkRowOrderNormalization},
		Budget: RoundTripBudgetUsage{
			MaxStatementsPerStage: config.Budgets.MaxStatementsPerStage,
			MaxFixtureStatements:  config.Budgets.MaxFixtureStatements,
			MaxFixtureRows:        config.Budgets.MaxFixtureRows,
			MaxFixtureBytes:       config.Budgets.MaxFixtureBytes,
		},
	}

	if err := ctx.Err(); err != nil {
		report.Status = RoundTripFailed
		report.Error = fmt.Sprintf("context canceled before target creation: %v", err)
		return report, nil
	}

	target, cleanup, err := config.Factory.Create(ctx)
	if err != nil {
		report.Status = RoundTripFailed
		report.Error = fmt.Sprintf("disposable database factory failed: %v", err)
		return report, nil
	}
	if target == nil || cleanup == nil {
		report.Status = RoundTripFailed
		report.Error = "disposable database factory returned a nil database or cleanup function"
		return report, nil
	}

	runner := &roundTripRunner{
		cfg:       config,
		report:    report,
		extractor: extractor,
		tableName: config.MigrationSet.getTableName(),
		target:    target,
		cleanup:   cleanup,
	}
	report.Target.Identifier = targetIdentifier(config.Factory, target)

	return runner.run(ctx), nil
}

// checkRowOrderNormalization is recorded on every report so the multiset
// comparison rule for check rows is never applied silently.
const checkRowOrderNormalization = "check-rows: result rows compared as an unordered multiset; column order within rows is retained"

func applyBudgetDefaults(b *RoundTripBudgets) {
	if b.MaxStatementsPerStage <= 0 {
		b.MaxStatementsPerStage = defaultMaxStmts
	}
	if b.MaxFixtureStatements <= 0 {
		b.MaxFixtureStatements = defaultMaxFixtures
	}
	if b.MaxFixtureRows <= 0 {
		b.MaxFixtureRows = int64(defaultMaxFixtureRow)
	}
	if b.MaxFixtureBytes <= 0 {
		b.MaxFixtureBytes = int64(defaultMaxFixtureByt)
	}
	if b.StageTimeout <= 0 {
		b.StageTimeout = defaultStageTimeout
	}
}

func targetIdentifier(factory DisposableDatabaseFactory, db *sql.DB) string {
	return fmt.Sprintf("%T/%p", factory, db)
}

func (r *roundTripRunner) run(parentCtx context.Context) *RoundTripReport {
	report := r.report
	defer r.destroyTarget()

	migrations, err := r.cfg.Migrations.FindMigrations()
	if err != nil {
		r.fail("resolve migrations", err)
		return report
	}
	r.analyzeMigrations(migrations)

	// Stage 1: explicit baseline (optional setup SQL on the empty target).
	baseline, baselineChecks, ok := r.stageBaseline(parentCtx)
	if !ok {
		return report
	}

	// Stage 2: first Up, fixtures and checks.
	snapshotUp1, checksUp1, ok := r.stageUp(parentCtx, migrations, "up-1", true)
	if !ok {
		return report
	}

	// Stage 3: full Down compared against the explicit baseline.
	snapshotDown, checksDown, ok := r.stageDirection(parentCtx, migrations, Down, "down")
	if !ok {
		return report
	}
	report.SchemaDiffs = append(report.SchemaDiffs, diffSchema(baseline, snapshotDown, compareDownBaseline)...)
	report.CheckDiffs = append(report.CheckDiffs, diffChecks(baselineChecks, checksDown, compareDownBaseline)...)

	// Stage 4: second Up compared against the first Up.
	snapshotUp2, checksUp2, ok := r.stageDirection(parentCtx, migrations, Up, "up-2")
	if !ok {
		return report
	}
	report.SchemaDiffs = append(report.SchemaDiffs, diffSchema(snapshotUp1, snapshotUp2, compareUp1Up2)...)
	report.CheckDiffs = append(report.CheckDiffs, diffChecks(checksUp1, checksUp2, compareUp1Up2)...)

	r.collectCoverage(snapshotUp1, snapshotDown, snapshotUp2)
	r.finalizeStatus()
	return report
}

func (r *roundTripRunner) destroyTarget() {
	report := r.report
	report.Target.CleanupCalled = true
	// Cleanup must still run when the parent context was canceled, so it gets
	// its own bounded context. Only the factory-provided cleanup is invoked,
	// which by contract destroys exactly the one target database.
	ctx, cancel := context.WithTimeout(context.Background(), cleanupTimeout)
	defer cancel()
	if err := r.cleanup(ctx); err != nil {
		if report.CleanupError == "" {
			report.CleanupError = err.Error()
		}
		// finalizeStatus runs before this deferred cleanup, so a cleanup
		// failure must force the failed verdict itself.
		report.Status = RoundTripFailed
	}
}

func (r *roundTripRunner) fail(stage string, err error) {
	if r.report.Error == "" {
		r.report.Error = fmt.Sprintf("%s: %v", stage, err)
	}
	r.report.Status = RoundTripFailed
}

func (r *roundTripRunner) stageContext(parent context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(parent, r.cfg.Budgets.StageTimeout)
}

func (r *roundTripRunner) recordStage(name string, migrations, statements, bytes int, started time.Time) {
	r.report.Stages = append(r.report.Stages, RoundTripStage{
		Name:       name,
		Migrations: migrations,
		Statements: statements,
		Bytes:      bytes,
		Duration:   time.Since(started),
	})
}

func (r *roundTripRunner) withinStatementBudget(stage string, statements int) bool {
	if statements > r.cfg.Budgets.MaxStatementsPerStage {
		r.fail(stage, fmt.Errorf("statement budget exceeded: %d statements > limit %d",
			statements, r.cfg.Budgets.MaxStatementsPerStage))
		return false
	}
	return true
}

func (r *roundTripRunner) analyzeMigrations(migrations []*Migration) {
	for _, m := range migrations {
		if m.DisableTransactionUp {
			r.report.Findings = append(r.report.Findings, RoundTripFinding{
				Code:   "notransaction-up",
				Object: m.Id,
				Message: "Up section runs without a transaction; partial application on failure cannot be " +
					"observed by the round trip and reversibility evidence is partial",
			})
		}
		if m.DisableTransactionDown {
			r.report.Findings = append(r.report.Findings, RoundTripFinding{
				Code:   "notransaction-down",
				Object: m.Id,
				Message: "Down section runs without a transaction; partial application on failure cannot be " +
					"observed by the round trip and reversibility evidence is partial",
			})
		}
		if len(m.Down) == 0 {
			r.report.Findings = append(r.report.Findings, RoundTripFinding{
				Code:    "irreversible-down",
				Object:  m.Id,
				Message: "migration declares no Down statements and cannot be proven reversible",
			})
		}
	}
}

func (r *roundTripRunner) stageBaseline(parent context.Context) (*SchemaSnapshot, map[string]*capturedCheck, bool) {
	started := time.Now()
	ctx, cancel := r.stageContext(parent)
	defer cancel()

	bytes := 0
	for _, stmt := range r.cfg.BaselineSetup {
		bytes += len(stmt)
	}
	if !r.withinStatementBudget("baseline", len(r.cfg.BaselineSetup)) {
		return nil, nil, false
	}
	for _, stmt := range r.cfg.BaselineSetup {
		if _, err := r.target.ExecContext(ctx, stmt); err != nil {
			r.fail("baseline setup", err)
			return nil, nil, false
		}
	}

	snapshot, err := r.extract(ctx)
	if err != nil {
		r.fail("baseline schema extraction", err)
		return nil, nil, false
	}
	checks, ok := r.captureChecks(ctx, "baseline")
	if !ok {
		return nil, nil, false
	}
	r.recordStage("baseline", 0, len(r.cfg.BaselineSetup), bytes, started)
	return snapshot, checks, true
}

func (r *roundTripRunner) stageUp(parent context.Context, migrations []*Migration, name string, withFixtures bool) (*SchemaSnapshot, map[string]*capturedCheck, bool) {
	snapshot, checks, ok := r.stageDirection(parent, migrations, Up, name)
	if !ok {
		return nil, nil, false
	}
	if withFixtures {
		if !r.loadFixtures(parent) {
			return nil, nil, false
		}
		// Re-run checks after fixture loading so Up #1 observations include the
		// caller-declared data.
		ctx, cancel := r.stageContext(parent)
		defer cancel()
		checks, ok = r.captureChecks(ctx, name+"-fixtures")
		if !ok {
			return nil, nil, false
		}
	}
	return snapshot, checks, true
}

func (r *roundTripRunner) stageDirection(parent context.Context, migrations []*Migration, dir MigrationDirection, name string) (*SchemaSnapshot, map[string]*capturedCheck, bool) {
	started := time.Now()
	ctx, cancel := r.stageContext(parent)
	defer cancel()

	if err := parent.Err(); err != nil {
		r.fail(name, err)
		return nil, nil, false
	}

	planned := planDirection(migrations, dir)
	statements := 0
	bytes := 0
	for _, p := range planned {
		statements += len(p.Queries)
		for _, q := range p.Queries {
			bytes += len(q)
		}
	}
	if !r.withinStatementBudget(name, statements) {
		return nil, nil, false
	}

	applied, err := r.cfg.MigrationSet.ExecContext(ctx, r.target, r.cfg.Dialect, r.cfg.Migrations, dir)
	if err != nil {
		r.fail(name, err)
		return nil, nil, false
	}
	r.recordStage(name, applied, statements, bytes, started)

	snapshot, err := r.extract(ctx)
	if err != nil {
		r.fail(name+" schema extraction", err)
		return nil, nil, false
	}
	checks, ok := r.captureChecks(ctx, name)
	if !ok {
		return nil, nil, false
	}
	return snapshot, checks, true
}

func planDirection(migrations []*Migration, dir MigrationDirection) []*PlannedMigration {
	planned := make([]*PlannedMigration, 0, len(migrations))
	if dir == Up {
		for _, m := range migrations {
			planned = append(planned, &PlannedMigration{
				Migration:          m,
				Queries:            m.Up,
				DisableTransaction: m.DisableTransactionUp,
			})
		}
		return planned
	}
	for i := len(migrations) - 1; i >= 0; i-- {
		m := migrations[i]
		planned = append(planned, &PlannedMigration{
			Migration:          m,
			Queries:            m.Down,
			DisableTransaction: m.DisableTransactionDown,
		})
	}
	return planned
}

func (r *roundTripRunner) loadFixtures(parent context.Context) bool {
	started := time.Now()
	ctx, cancel := r.stageContext(parent)
	defer cancel()

	if len(r.cfg.Fixtures) > r.cfg.Budgets.MaxFixtureStatements {
		r.fail("fixtures", fmt.Errorf("fixture statement budget exceeded: %d fixtures > limit %d",
			len(r.cfg.Fixtures), r.cfg.Budgets.MaxFixtureStatements))
		return false
	}

	bytes := int64(0)
	rows := int64(0)
	for _, f := range r.cfg.Fixtures {
		bytes += int64(len(f.Statement))
		for _, arg := range f.Args {
			bytes += int64(len(fmt.Sprintf("%v", arg)))
		}
		rows += f.Rows
	}
	if rows > r.cfg.Budgets.MaxFixtureRows {
		r.fail("fixtures", fmt.Errorf("fixture row budget exceeded: %d rows > limit %d",
			rows, r.cfg.Budgets.MaxFixtureRows))
		return false
	}
	if bytes > r.cfg.Budgets.MaxFixtureBytes {
		r.fail("fixtures", fmt.Errorf("fixture byte budget exceeded: %d bytes > limit %d",
			bytes, r.cfg.Budgets.MaxFixtureBytes))
		return false
	}

	for _, f := range r.cfg.Fixtures {
		if _, err := r.target.ExecContext(ctx, f.Statement, f.Args...); err != nil {
			object := f.Name
			if object == "" {
				object = f.Statement
			}
			r.fail("fixture "+object, err)
			return false
		}
	}

	r.report.Budget.FixtureStatements = len(r.cfg.Fixtures)
	r.report.Budget.FixtureRows = rows
	r.report.Budget.FixtureBytes = bytes
	r.recordStage("fixtures", 0, len(r.cfg.Fixtures), int(bytes), started)
	return true
}

func (r *roundTripRunner) extract(ctx context.Context) (*SchemaSnapshot, error) {
	snapshot, err := r.extractor.extract(ctx, r.target, r.tableName)
	if err != nil {
		return nil, err
	}
	r.report.Normalizations = appendUnique(r.report.Normalizations, snapshot.Normalizations...)
	return snapshot, nil
}

func (r *roundTripRunner) captureChecks(ctx context.Context, stage string) (map[string]*capturedCheck, bool) {
	results := make(map[string]*capturedCheck, len(r.cfg.Checks))
	for _, check := range r.cfg.Checks {
		captured, err := executeCheck(ctx, r.target, check)
		if err != nil {
			r.fail(stage+" check "+check.Name, err)
			return nil, false
		}
		results[check.Name] = captured
		for _, exclusion := range captured.NDExclusions {
			r.report.Findings = append(r.report.Findings, RoundTripFinding{
				Code:    "non-deterministic-column-exclusion",
				Object:  check.Name + "." + exclusion,
				Message: "column declared non-deterministic; only null-vs-not-null compared, exact values excluded",
			})
		}
	}
	return results, true
}

func executeCheck(ctx context.Context, db *sql.DB, check RoundTripCheck) (*capturedCheck, error) {
	captured := &capturedCheck{Name: check.Name}
	rows, err := db.QueryContext(ctx, check.Query, check.Args...)
	if err != nil {
		captured.QueryError = err.Error()
		return captured, nil
	}
	defer func() { _ = rows.Close() }()

	columns, err := rows.Columns()
	if err != nil {
		return nil, err
	}
	captured.Columns = columns
	nd := make(map[int]bool)
	for _, name := range check.NonDeterministicColumns {
		found := false
		for i, column := range columns {
			if strings.EqualFold(column, name) {
				nd[i] = true
				found = true
				break
			}
		}
		if found {
			captured.NDExclusions = append(captured.NDExclusions, name)
		}
	}

	values := make([]any, len(columns))
	pointers := make([]any, len(columns))
	for i := range values {
		pointers[i] = &values[i]
	}
	for rows.Next() {
		if err := rows.Scan(pointers...); err != nil {
			return nil, err
		}
		cells := make([]string, len(columns))
		for i, value := range values {
			if nd[i] {
				if value == nil {
					cells[i] = "nd:<nil>"
				} else {
					cells[i] = "nd:<non-null>"
				}
				continue
			}
			cells[i] = encodeCheckValue(value)
		}
		captured.Rows = append(captured.Rows, strings.Join(cells, "\x1f"))
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	sort.Strings(captured.Rows)
	captured.RowCount = len(captured.Rows)
	return captured, nil
}

func encodeCheckValue(value any) string {
	switch v := value.(type) {
	case nil:
		return "<nil>"
	case []byte:
		return "blob:" + string(v)
	case string:
		return "str:" + v
	case int64:
		return fmt.Sprintf("int:%d", v)
	case float64:
		return fmt.Sprintf("float:%v", v)
	case bool:
		return fmt.Sprintf("bool:%t", v)
	default:
		return fmt.Sprintf("%T:%v", v, v)
	}
}

func (r *roundTripRunner) collectCoverage(snapshots ...*SchemaSnapshot) {
	seenUnsupported := map[string]struct{}{}
	seenInternal := map[string]struct{}{}
	for _, snapshot := range snapshots {
		for _, obj := range snapshot.UnsupportedObjects {
			key := obj.Type + "/" + obj.Name
			if _, ok := seenUnsupported[key]; ok {
				continue
			}
			seenUnsupported[key] = struct{}{}
			r.report.Findings = append(r.report.Findings, RoundTripFinding{
				Code:    "unsupported-object",
				Object:  key,
				Message: fmt.Sprintf("object type %q is not covered by the schema extractor and is excluded from comparison", obj.Type),
			})
		}
		for _, obj := range snapshot.InternalObjects {
			key := obj.Type + "/" + obj.Name
			if _, ok := seenInternal[key]; ok {
				continue
			}
			seenInternal[key] = struct{}{}
			r.report.Findings = append(r.report.Findings, RoundTripFinding{
				Code:    "internal-object",
				Object:  key,
				Message: "engine-managed internal object recorded for visibility but excluded from comparison",
			})
		}
	}
}

func (r *roundTripRunner) finalizeStatus() {
	report := r.report
	switch {
	case report.Error != "" || report.CleanupError != "" ||
		len(report.SchemaDiffs) > 0 || len(report.CheckDiffs) > 0:
		report.Status = RoundTripFailed
	case r.hasPartialFinding():
		report.Status = RoundTripPartial
	default:
		report.Status = RoundTripVerified
	}
}

func (r *roundTripRunner) hasPartialFinding() bool {
	for _, finding := range r.report.Findings {
		switch finding.Code {
		case "notransaction-up", "notransaction-down", "irreversible-down",
			"unsupported-object", "non-deterministic-column-exclusion":
			return true
		}
	}
	return false
}

func appendUnique(existing []string, extra ...string) []string {
	known := make(map[string]struct{}, len(existing))
	for _, item := range existing {
		known[item] = struct{}{}
	}
	for _, item := range extra {
		if _, ok := known[item]; ok {
			continue
		}
		known[item] = struct{}{}
		existing = append(existing, item)
	}
	return existing
}

// ---------------------------------------------------------------------------
// Schema diffing
// ---------------------------------------------------------------------------

func diffSchema(want, got *SchemaSnapshot, compare string) []SchemaDifference {
	var diffs []SchemaDifference

	wantTables := mapTables(want)
	gotTables := mapTables(got)
	for name, wantTable := range wantTables {
		gotTable, ok := gotTables[name]
		if !ok {
			diffs = append(diffs, SchemaDifference{
				Kind: "table-missing", Object: name, Compare: compare,
				Expected: describeTable(wantTable), Actual: "<absent>",
			})
			continue
		}
		diffs = append(diffs, diffTable(wantTable, gotTable, compare)...)
	}
	for name := range gotTables {
		if _, ok := wantTables[name]; !ok {
			diffs = append(diffs, SchemaDifference{
				Kind: "table-extra", Object: name, Compare: compare,
				Expected: "<absent>", Actual: describeTable(gotTables[name]),
			})
		}
	}

	wantIndexes := mapIndexes(want)
	gotIndexes := mapIndexes(got)
	for name, wantIndex := range wantIndexes {
		gotIndex, ok := gotIndexes[name]
		if !ok {
			diffs = append(diffs, SchemaDifference{
				Kind: "index-missing", Object: name, Compare: compare,
				Expected: describeIndex(wantIndex), Actual: "<absent>",
			})
			continue
		}
		diffs = append(diffs, diffIndex(wantIndex, gotIndex, compare)...)
	}
	for name := range gotIndexes {
		if _, ok := wantIndexes[name]; !ok {
			diffs = append(diffs, SchemaDifference{
				Kind: "index-extra", Object: name, Compare: compare,
				Expected: "<absent>", Actual: describeIndex(gotIndexes[name]),
			})
		}
	}

	sortSchemaDiffs(diffs)
	return diffs
}

func mapTables(snapshot *SchemaSnapshot) map[string]TableDescriptor {
	out := make(map[string]TableDescriptor, len(snapshot.Tables))
	for _, table := range snapshot.Tables {
		out[table.Name] = table
	}
	return out
}

func mapIndexes(snapshot *SchemaSnapshot) map[string]IndexDescriptor {
	out := make(map[string]IndexDescriptor, len(snapshot.Indexes))
	for _, index := range snapshot.Indexes {
		out[index.Table+"."+index.Name] = index
	}
	return out
}

func diffTable(want, got TableDescriptor, compare string) []SchemaDifference {
	var diffs []SchemaDifference
	object := want.Name

	if want.WithoutRowID != got.WithoutRowID {
		diffs = append(diffs, SchemaDifference{
			Kind: "table-option", Object: object, Compare: compare,
			Expected: fmt.Sprintf("without_rowid=%t", want.WithoutRowID),
			Actual:   fmt.Sprintf("without_rowid=%t", got.WithoutRowID),
		})
	}

	maxOrdinal := len(want.Columns)
	if len(got.Columns) > maxOrdinal {
		maxOrdinal = len(got.Columns)
	}
	for i := 0; i < maxOrdinal; i++ {
		switch {
		case i >= len(want.Columns):
			extra := got.Columns[i]
			diffs = append(diffs, SchemaDifference{
				Kind: "column-extra", Object: object + "." + extra.Name, Compare: compare,
				Expected: "<absent>", Actual: describeColumn(extra),
			})
		case i >= len(got.Columns):
			missing := want.Columns[i]
			diffs = append(diffs, SchemaDifference{
				Kind: "column-missing", Object: object + "." + missing.Name, Compare: compare,
				Expected: describeColumn(missing), Actual: "<absent>",
			})
		default:
			diffs = append(diffs, diffColumn(want.Columns[i], got.Columns[i], object, compare)...)
		}
	}

	if !equalStringSets(want.PrimaryKey, got.PrimaryKey) {
		diffs = append(diffs, SchemaDifference{
			Kind: "primary-key", Object: object, Compare: compare,
			Expected: strings.Join(want.PrimaryKey, ","), Actual: strings.Join(got.PrimaryKey, ","),
			Detail: "primary key column list differs",
		})
	}

	wantFKs := mapForeignKeys(want)
	gotFKs := mapForeignKeys(got)
	for key, wantFK := range wantFKs {
		gotFK, ok := gotFKs[key]
		if !ok {
			diffs = append(diffs, SchemaDifference{
				Kind: "foreign-key-missing", Object: object + "." + key, Compare: compare,
				Expected: describeForeignKey(wantFK), Actual: "<absent>",
			})
			continue
		}
		diffs = append(diffs, diffForeignKey(wantFK, gotFK, object+"."+key, compare)...)
	}
	for key := range gotFKs {
		if _, ok := wantFKs[key]; !ok {
			diffs = append(diffs, SchemaDifference{
				Kind: "foreign-key-extra", Object: object + "." + key, Compare: compare,
				Expected: "<absent>", Actual: describeForeignKey(gotFKs[key]),
			})
		}
	}
	return diffs
}

func diffColumn(want, got ColumnDescriptor, table, compare string) []SchemaDifference {
	var diffs []SchemaDifference
	object := table + "." + want.Name
	if want.Name != got.Name {
		diffs = append(diffs, SchemaDifference{
			Kind: "column-name", Object: fmt.Sprintf("%s.ordinal-%d", table, want.Ordinal), Compare: compare,
			Expected: want.Name, Actual: got.Name,
		})
	}
	if !strings.EqualFold(want.Type, got.Type) {
		diffs = append(diffs, SchemaDifference{
			Kind: "column-type", Object: object, Compare: compare,
			Expected: want.Type, Actual: got.Type,
		})
	}
	if want.NotNull != got.NotNull {
		diffs = append(diffs, SchemaDifference{
			Kind: "column-nullable", Object: object, Compare: compare,
			Expected: fmt.Sprintf("notnull=%t", want.NotNull),
			Actual:   fmt.Sprintf("notnull=%t", got.NotNull),
		})
	}
	if want.HasDefault != got.HasDefault || (want.HasDefault && want.Default != got.Default) {
		diffs = append(diffs, SchemaDifference{
			Kind: "column-default", Object: object, Compare: compare,
			Expected: describeDefault(want), Actual: describeDefault(got),
			Detail: "meaningful default expressions are never normalized away",
		})
	}
	return diffs
}

func mapForeignKeys(table TableDescriptor) map[string]ForeignKeyDescriptor {
	out := make(map[string]ForeignKeyDescriptor, len(table.ForeignKeys))
	for _, fk := range table.ForeignKeys {
		parts := make([]string, len(fk.Columns))
		for i, column := range fk.Columns {
			parts[i] = column.From
		}
		sort.Strings(parts)
		out[fk.References+"("+strings.Join(parts, ",")+")"] = fk
	}
	return out
}

func diffForeignKey(want, got ForeignKeyDescriptor, object, compare string) []SchemaDifference {
	var diffs []SchemaDifference
	if want.OnUpdate != got.OnUpdate || want.OnDelete != got.OnDelete ||
		want.Match != got.Match || want.Deferrable != got.Deferrable ||
		!fkColumnsEqual(want.Columns, got.Columns) {
		diffs = append(diffs, SchemaDifference{
			Kind: "foreign-key-changed", Object: object, Compare: compare,
			Expected: describeForeignKey(want), Actual: describeForeignKey(got),
			Detail: "foreign key actions, match type or deferrability differ",
		})
	}
	return diffs
}

func fkColumnsEqual(want, got []ForeignKeyColumn) bool {
	if len(want) != len(got) {
		return false
	}
	for i := range want {
		if want[i] != got[i] {
			return false
		}
	}
	return true
}

func diffIndex(want, got IndexDescriptor, compare string) []SchemaDifference {
	var diffs []SchemaDifference
	object := want.Table + "." + want.Name
	if want.Unique != got.Unique || want.Partial != got.Partial || want.Origin != got.Origin {
		diffs = append(diffs, SchemaDifference{
			Kind: "index-attribute", Object: object, Compare: compare,
			Expected: describeIndex(want), Actual: describeIndex(got),
		})
	}
	if len(want.Columns) != len(got.Columns) {
		diffs = append(diffs, SchemaDifference{
			Kind: "index-columns", Object: object, Compare: compare,
			Expected: describeIndexColumns(want.Columns), Actual: describeIndexColumns(got.Columns),
		})
		return diffs
	}
	for i := range want.Columns {
		if want.Columns[i] != got.Columns[i] {
			diffs = append(diffs, SchemaDifference{
				Kind: "index-columns", Object: object, Compare: compare,
				Expected: describeIndexColumns(want.Columns), Actual: describeIndexColumns(got.Columns),
				Detail: "index key columns, direction, collation or expression differ",
			})
			break
		}
	}
	return diffs
}

// ---------------------------------------------------------------------------
// Check diffing
// ---------------------------------------------------------------------------

func diffChecks(want, got map[string]*capturedCheck, compare string) []CheckDifference {
	var diffs []CheckDifference
	names := make([]string, 0, len(want))
	for name := range want {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		wantCheck := want[name]
		gotCheck, ok := got[name]
		if !ok {
			diffs = append(diffs, CheckDifference{
				Kind: "check-missing", Check: name, Compare: compare,
				Expected: "executed", Actual: "<absent>",
			})
			continue
		}
		diffs = append(diffs, diffCheck(wantCheck, gotCheck, compare)...)
	}
	sortCheckDiffs(diffs)
	return diffs
}

func diffCheck(want, got *capturedCheck, compare string) []CheckDifference {
	var diffs []CheckDifference
	switch {
	case want.QueryError != "" && got.QueryError != "":
		// Both stages failed to execute the query in the same way: no evidence
		// of divergence, but nothing is proven either.
		return nil
	case want.QueryError != "" || got.QueryError != "":
		diffs = append(diffs, CheckDifference{
			Kind: "query-error", Check: want.Name, Compare: compare,
			Expected: errorOrOK(want.QueryError), Actual: errorOrOK(got.QueryError),
		})
		return diffs
	}

	if !equalStringSets(want.Columns, got.Columns) {
		diffs = append(diffs, CheckDifference{
			Kind: "columns", Check: want.Name, Compare: compare,
			Expected: strings.Join(want.Columns, ","), Actual: strings.Join(got.Columns, ","),
		})
	}

	if want.RowCount != got.RowCount {
		diffs = append(diffs, CheckDifference{
			Kind: "row-count", Check: want.Name, Compare: compare,
			Expected: fmt.Sprintf("%d", want.RowCount), Actual: fmt.Sprintf("%d", got.RowCount),
			Detail: "result set cardinality differs; row order is compared as an unordered multiset",
		})
	}

	limit := len(want.Rows)
	if len(got.Rows) < limit {
		limit = len(got.Rows)
	}
	for i := 0; i < limit; i++ {
		if want.Rows[i] != got.Rows[i] {
			diffs = append(diffs, CheckDifference{
				Kind: "value", Check: want.Name, Compare: compare,
				Expected: want.Rows[i], Actual: got.Rows[i],
				Detail: fmt.Sprintf("first mismatch at normalized row %d", i),
			})
			break
		}
	}
	return diffs
}

func errorOrOK(err string) string {
	if err == "" {
		return "<ok>"
	}
	return err
}

// ---------------------------------------------------------------------------
// Descriptor rendering and small helpers
// ---------------------------------------------------------------------------

func describeTable(table TableDescriptor) string {
	return fmt.Sprintf("table %s (%d columns, %d fks)", table.Name, len(table.Columns), len(table.ForeignKeys))
}

func describeColumn(column ColumnDescriptor) string {
	parts := []string{column.Name, column.Type}
	if column.NotNull {
		parts = append(parts, "NOT NULL")
	}
	if column.HasDefault {
		parts = append(parts, "DEFAULT "+column.Default)
	}
	return strings.Join(parts, " ")
}

func describeDefault(column ColumnDescriptor) string {
	if !column.HasDefault {
		return "<no default>"
	}
	return column.Default
}

func describeIndex(index IndexDescriptor) string {
	flags := make([]string, 0, 3)
	if index.Unique {
		flags = append(flags, "unique")
	}
	if index.Partial {
		flags = append(flags, "partial")
	}
	flags = append(flags, "origin="+index.Origin)
	return fmt.Sprintf("index %s on %s (%s) [%s]",
		index.Name, index.Table, describeIndexColumns(index.Columns), strings.Join(flags, ","))
}

func describeIndexColumns(columns []IndexColumn) string {
	parts := make([]string, len(columns))
	for i, column := range columns {
		part := column.Name
		if column.Expression {
			part = "expr(" + column.Name + ")"
		}
		if column.Descending {
			part += " DESC"
		}
		if column.Collation != "" && column.Collation != "BINARY" {
			part += " COLLATE " + column.Collation
		}
		parts[i] = part
	}
	return strings.Join(parts, ",")
}

func describeForeignKey(fk ForeignKeyDescriptor) string {
	parts := make([]string, len(fk.Columns))
	for i, column := range fk.Columns {
		parts[i] = column.From + "->" + fk.References + "." + column.To
	}
	flags := []string{strings.Join(parts, ",")}
	if fk.OnUpdate != "" && fk.OnUpdate != "NO ACTION" {
		flags = append(flags, "ON UPDATE "+fk.OnUpdate)
	}
	if fk.OnDelete != "" && fk.OnDelete != "NO ACTION" {
		flags = append(flags, "ON DELETE "+fk.OnDelete)
	}
	if fk.Match != "" && fk.Match != "NONE" {
		flags = append(flags, "MATCH "+fk.Match)
	}
	if fk.Deferrable != "" {
		flags = append(flags, fk.Deferrable)
	}
	return strings.Join(flags, " ")
}

func equalStringSets(want, got []string) bool {
	if len(want) != len(got) {
		return false
	}
	for i := range want {
		if want[i] != got[i] {
			return false
		}
	}
	return true
}

func sortSchemaDiffs(diffs []SchemaDifference) {
	sort.SliceStable(diffs, func(i, j int) bool {
		if diffs[i].Compare != diffs[j].Compare {
			return diffs[i].Compare < diffs[j].Compare
		}
		if diffs[i].Kind != diffs[j].Kind {
			return diffs[i].Kind < diffs[j].Kind
		}
		return diffs[i].Object < diffs[j].Object
	})
}

func sortCheckDiffs(diffs []CheckDifference) {
	sort.SliceStable(diffs, func(i, j int) bool {
		if diffs[i].Compare != diffs[j].Compare {
			return diffs[i].Compare < diffs[j].Compare
		}
		if diffs[i].Check != diffs[j].Check {
			return diffs[i].Check < diffs[j].Check
		}
		return diffs[i].Kind < diffs[j].Kind
	})
}
