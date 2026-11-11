package migrate

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
	"time"

	"github.com/go-gorp/gorp/v3"
)

// FrozenPlanFormatVersion is the only artifact format version understood by
// this build. Any artifact carrying another version is rejected untouched.
const FrozenPlanFormatVersion = 1

const (
	frozenPlanKind      = "sql-migrate-frozen-plan"
	frozenDirectionUp   = "up"
	frozenDirectionDown = "down"
)

// Well-known failure causes. All rejection paths return before any migration
// SQL is executed, so callers can rely on errors.Is checks without having to
// reason about partial database effects.
var (
	// ErrFrozenPlanGuardRequired is returned when no guard is supplied. A
	// single SELECT comparison cannot serialize multi-process migrators, so an
	// explicit caller-owned guard covering the whole operation is mandatory.
	ErrFrozenPlanGuardRequired = errors.New("frozen plan: an exclusive guard covering the whole check-and-execute operation is required")
	// ErrFrozenPlanGuard wraps failures while acquiring or releasing the guard.
	ErrFrozenPlanGuard = errors.New("frozen plan: exclusive guard failure")
	// ErrFrozenPlanFormat covers canonical encoding, unknown fields/versions,
	// duplicate ids, enums and fingerprint inconsistencies.
	ErrFrozenPlanFormat = errors.New("frozen plan: invalid artifact format")
	// ErrFrozenPlanHash covers statement size and SHA256 mismatches.
	ErrFrozenPlanHash = errors.New("frozen plan: statement integrity check failed")
	// ErrFrozenPlanTarget means the artifact was built for another database.
	ErrFrozenPlanTarget = errors.New("frozen plan: target database identity mismatch")
	// ErrFrozenPlanSource means the reloaded source no longer matches the bytes
	// frozen in the artifact. Same-id replacements are never silently applied.
	ErrFrozenPlanSource = errors.New("frozen plan: migration source content changed")
	// ErrFrozenPlanState means the database applied-record set (or the mere
	// existence of the migration table) changed since the artifact was built.
	ErrFrozenPlanState = errors.New("frozen plan: database applied-record state changed")
)

// FrozenPlanSource identifies where the frozen migrations came from. It carries
// no SQL: the artifact itself is the only authoritative source of executable
// bytes.
type FrozenPlanSource struct {
	// Kind classifies the source, e.g. "memory", "file", "embed-fs".
	Kind string `json:"kind"`
	// Identity is a caller-chosen stable identifier, e.g. a directory path.
	Identity string `json:"identity"`
}

// FrozenPlanTarget binds an artifact to one explicit database location. It
// deliberately contains no DSN, user name or password: callers supply logical
// names only, so a plan built for one database can never be replayed against a
// same-named table in another database.
type FrozenPlanTarget struct {
	Dialect  string `json:"dialect"`
	Database string `json:"database"`
	Schema   string `json:"schema"`
	Table    string `json:"table"`
}

// FrozenPlanAppliedState is the frozen snapshot of the migration records found
// in the target database at build time.
type FrozenPlanAppliedState struct {
	// MigrationTableExists records whether the migration table existed at all.
	// A missing table is treated as an empty state and recorded explicitly,
	// never silently created during a read-only build.
	MigrationTableExists bool `json:"migration_table_exists"`
	// AppliedIDs is the sorted set of applied migration ids.
	AppliedIDs []string `json:"applied_ids"`
	// Fingerprint is the SHA256 of the canonical applied-id set.
	Fingerprint string `json:"fingerprint"`
}

// FrozenPlanStatement freezes one exact statement string together with its
// byte length and SHA256 digest.
type FrozenPlanStatement struct {
	Bytes  string `json:"bytes"`
	Size   int    `json:"size"`
	SHA256 string `json:"sha256"`
}

// FrozenPlanMigration freezes one planned migration: its id, the transaction
// flag effective for the planned direction and every statement with its
// integrity metadata.
type FrozenPlanMigration struct {
	ID                 string                `json:"id"`
	DisableTransaction bool                  `json:"disable_transaction"`
	Statements         []FrozenPlanStatement `json:"statements"`
}

// FrozenPlan is the versioned, JSON-serializable artifact.
type FrozenPlan struct {
	Kind          string                 `json:"kind"`
	FormatVersion int                    `json:"format_version"`
	Direction     string                 `json:"direction"`
	Source        FrozenPlanSource       `json:"source"`
	Target        FrozenPlanTarget       `json:"target"`
	AppliedState  FrozenPlanAppliedState `json:"applied_state"`
	Migrations    []FrozenPlanMigration  `json:"migrations"`
	// CreatedAt uses a stable RFC3339 string so JSON round trips stay canonical.
	CreatedAt string `json:"created_at"`
}

// FrozenPlanStepStatus describes the outcome of one frozen migration.
type FrozenPlanStepStatus string

const (
	// FrozenStepNotStarted means the migration was never attempted or its
	// transaction was rolled back cleanly, leaving no database effect.
	FrozenStepNotStarted FrozenPlanStepStatus = "not-started"
	// FrozenStepCompleted means every statement and the bookkeeping record
	// were committed (together in one transaction for transactional steps).
	FrozenStepCompleted FrozenPlanStepStatus = "completed"
	// FrozenStepUncertain means some autocommitted statements may have taken
	// effect (notransaction) or a transaction outcome could not be confirmed.
	// The executor never retries such steps automatically.
	FrozenStepUncertain FrozenPlanStepStatus = "uncertain"
)

// FrozenPlanStepResult is the per-migration execution outcome.
type FrozenPlanStepResult struct {
	ID                 string
	DisableTransaction bool
	Status             FrozenPlanStepStatus
	Err                error
}

// FrozenPlanResult reports what a frozen execution did.
type FrozenPlanResult struct {
	// Applied counts migrations that reached FrozenStepCompleted.
	Applied int
	// Steps contains one entry per planned migration in plan order, including
	// migrations that were never started.
	Steps []FrozenPlanStepResult
}

// FrozenPlanGuard serializes the entire pre-state comparison and execution
// against concurrent migrators. Applications implement it on top of whatever
// coordination primitive they already operate (database advisory lock, flock,
// lease, ...). This package intentionally does not ship a second distributed
// lock service.
type FrozenPlanGuard interface {
	// Lock must block until the exclusive guard is held or ctx is done.
	Lock(ctx context.Context) error
	// Unlock releases the guard.
	Unlock(ctx context.Context) error
}

// inProcessFrozenGuard is a process-local guard built on a capacity-one
// channel. It exists for convenience and tests; it cannot coordinate separate
// processes and is not a distributed lock service.
type inProcessFrozenGuard struct {
	ch chan struct{}
}

var _ FrozenPlanGuard = (*inProcessFrozenGuard)(nil)

// NewInProcessFrozenGuard returns a process-local FrozenPlanGuard.
func NewInProcessFrozenGuard() FrozenPlanGuard {
	return &inProcessFrozenGuard{ch: make(chan struct{}, 1)}
}

func (g *inProcessFrozenGuard) Lock(ctx context.Context) error {
	select {
	case g.ch <- struct{}{}:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (g *inProcessFrozenGuard) Unlock(_ context.Context) error {
	select {
	case <-g.ch:
		return nil
	default:
		return errors.New("frozen plan: guard unlocked without being held")
	}
}

// appliedRecordsFingerprint hashes the sorted applied-id set in a deterministic
// canonical form: SHA256 of the ids joined by '\n' (SHA256 of the empty string
// for an empty set).
func appliedRecordsFingerprint(ids []string) string {
	h := sha256.New()
	for i, id := range ids {
		if i > 0 {
			_, _ = h.Write([]byte("\n"))
		}
		_, _ = h.Write([]byte(id))
	}
	return hex.EncodeToString(h.Sum(nil))
}

func hashStatementBytes(raw []byte) string {
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

func directionString(dir MigrationDirection) string {
	if dir == Down {
		return frozenDirectionDown
	}
	return frozenDirectionUp
}

func parseFrozenDirection(s string) (MigrationDirection, error) {
	switch s {
	case frozenDirectionUp:
		return Up, nil
	case frozenDirectionDown:
		return Down, nil
	default:
		return Up, fmt.Errorf("%w: unknown direction %q", ErrFrozenPlanFormat, s)
	}
}

// readOnlyDbMap builds a gorp mapping without ever issuing DDL. Registering a
// table mapping does not touch the database; only CreateTablesIfNotExists does,
// and it is never called here.
func (ms MigrationSet) readOnlyDbMap(db *sql.DB, dialect string) (*gorp.DbMap, error) {
	d, ok := MigrationDialects[dialect]
	if !ok {
		return nil, fmt.Errorf("Unknown dialect: %s", dialect)
	}

	dbMap := &gorp.DbMap{Db: db, Dialect: d}
	table := dbMap.AddTableWithNameAndSchema(MigrationRecord{}, ms.SchemaName, ms.getTableName()).SetKeys(false, "Id")
	if dialect == "oci8" || dialect == "godror" {
		table.ColMap("Id").SetMaxSize(4000)
	}
	return dbMap, nil
}

// readMigrationRecordsReadOnly performs a single SELECT against the migration
// table. When the table does not exist it reports an explicit empty state
// (tableExists == false, no records) and never creates the table or any schema.
// Errors that do not unmistakably identify a missing table fail closed.
func (ms MigrationSet) readMigrationRecordsReadOnly(db *sql.DB, dialect string) ([]MigrationRecord, bool, error) {
	dbMap, err := ms.readOnlyDbMap(db, dialect)
	if err != nil {
		return nil, false, err
	}

	var records []MigrationRecord
	_, err = dbMap.Select(&records, fmt.Sprintf("SELECT * FROM %s",
		dbMap.Dialect.QuotedTableForQuery(ms.SchemaName, ms.getTableName())))
	if err != nil {
		if isMissingMigrationTableError(dialect, err) {
			return nil, false, nil
		}
		return nil, false, err
	}
	return records, true, nil
}

// isMissingMigrationTableError recognizes the "table does not exist" errors of
// the supported dialects. Anything unrecognized is treated as a real error.
func isMissingMigrationTableError(dialect string, err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	switch dialect {
	case "sqlite3":
		return strings.Contains(msg, "no such table")
	case "postgres":
		return strings.Contains(msg, "does not exist")
	case "mysql":
		return strings.Contains(msg, "doesn't exist") || strings.Contains(msg, "Error 1146")
	case "mssql":
		return strings.Contains(msg, "Invalid object name")
	case "oci8", "godror":
		return strings.Contains(msg, "ORA-00942")
	case "snowflake":
		return strings.Contains(msg, "does not exist or not authorized")
	default:
		return false
	}
}

// BuildFrozenPlan freezes the migration plan for the given direction into a
// versioned artifact. It is strictly read-only: the migration table is never
// created, and a missing table is recorded as an explicit empty state.
func BuildFrozenPlan(db *sql.DB, dialect string, source MigrationSource, sourceID FrozenPlanSource, target FrozenPlanTarget, dir MigrationDirection, max int) (*FrozenPlan, error) {
	return migSet.BuildFrozenPlan(db, dialect, source, sourceID, target, dir, max)
}

// BuildFrozenPlan is the MigrationSet-bound variant of the package-level
// function.
func (ms MigrationSet) BuildFrozenPlan(db *sql.DB, dialect string, source MigrationSource, sourceID FrozenPlanSource, target FrozenPlanTarget, dir MigrationDirection, max int) (*FrozenPlan, error) {
	if source == nil {
		return nil, errors.New("frozen plan: migration source is required")
	}
	if strings.TrimSpace(sourceID.Kind) == "" || strings.TrimSpace(sourceID.Identity) == "" {
		return nil, fmt.Errorf("%w: source kind and identity are required", ErrFrozenPlanFormat)
	}
	if strings.TrimSpace(target.Database) == "" {
		return nil, errors.New("frozen plan: target database name is required; credentials must never be included")
	}

	resolvedTarget, err := ms.resolveFrozenTarget(dialect, target)
	if err != nil {
		return nil, err
	}

	migrations, err := source.FindMigrations()
	if err != nil {
		return nil, err
	}

	// Read-only snapshot: this must not create the migration table or any
	// schema, unlike PlanMigration's getMigrationDbMap initialization.
	records, tableExists, err := ms.readMigrationRecordsReadOnly(db, dialect)
	if err != nil {
		return nil, err
	}

	planned, err := ms.planFromRecords(migrations, records, dir, max, -1)
	if err != nil {
		return nil, err
	}

	appliedIDs := make([]string, 0, len(records))
	for _, record := range records {
		appliedIDs = append(appliedIDs, record.Id)
	}
	sort.Strings(appliedIDs)

	plan := &FrozenPlan{
		Kind:          frozenPlanKind,
		FormatVersion: FrozenPlanFormatVersion,
		Direction:     directionString(dir),
		Source:        sourceID,
		Target:        resolvedTarget,
		AppliedState: FrozenPlanAppliedState{
			MigrationTableExists: tableExists,
			AppliedIDs:           appliedIDs,
			Fingerprint:          appliedRecordsFingerprint(appliedIDs),
		},
		Migrations: make([]FrozenPlanMigration, 0, len(planned)),
		CreatedAt:  time.Now().UTC().Format(time.RFC3339Nano),
	}

	for _, pm := range planned {
		frozen := FrozenPlanMigration{
			ID:                 pm.Id,
			DisableTransaction: pm.DisableTransaction,
			Statements:         make([]FrozenPlanStatement, 0, len(pm.Queries)),
		}
		for _, query := range pm.Queries {
			raw := []byte(query)
			frozen.Statements = append(frozen.Statements, FrozenPlanStatement{
				Bytes:  query,
				Size:   len(raw),
				SHA256: hashStatementBytes(raw),
			})
		}
		plan.Migrations = append(plan.Migrations, frozen)
	}

	return plan, nil
}

// resolveFrozenTarget fills schema and table from the MigrationSet and
// verifies that any caller-supplied values agree. No credential material is
// accepted or inspected.
func (ms MigrationSet) resolveFrozenTarget(dialect string, target FrozenPlanTarget) (FrozenPlanTarget, error) {
	if _, ok := MigrationDialects[dialect]; !ok {
		return FrozenPlanTarget{}, fmt.Errorf("Unknown dialect: %s", dialect)
	}
	resolved := FrozenPlanTarget{
		Dialect:  dialect,
		Database: target.Database,
		Schema:   ms.SchemaName,
		Table:    ms.getTableName(),
	}
	if target.Schema != "" && target.Schema != resolved.Schema {
		return FrozenPlanTarget{}, fmt.Errorf("%w: schema %q does not match migration set schema %q",
			ErrFrozenPlanTarget, target.Schema, resolved.Schema)
	}
	if target.Table != "" && target.Table != resolved.Table {
		return FrozenPlanTarget{}, fmt.Errorf("%w: table %q does not match migration set table %q",
			ErrFrozenPlanTarget, target.Table, resolved.Table)
	}
	return resolved, nil
}

// Marshal serializes the artifact in its canonical encoding (two-space
// indented JSON with a trailing newline). The struct contains no maps, so the
// output is deterministic and ParseFrozenPlan can reject any non-canonical
// representation.
func (p *FrozenPlan) Marshal() ([]byte, error) {
	data, err := json.MarshalIndent(p, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrFrozenPlanFormat, err)
	}
	return append(data, '\n'), nil
}

// ParseFrozenPlan parses and fully validates an artifact. Unknown fields,
// trailing tokens, non-canonical encoding, unknown format versions, duplicate
// ids, bad fingerprints and statement size/hash mismatches are all rejected
// before the artifact can be used.
func ParseFrozenPlan(data []byte) (*FrozenPlan, error) {
	if len(bytes.TrimSpace(data)) == 0 {
		return nil, fmt.Errorf("%w: empty artifact", ErrFrozenPlanFormat)
	}

	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()

	var plan FrozenPlan
	if err := decoder.Decode(&plan); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrFrozenPlanFormat, err)
	}
	var trailing json.RawMessage
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("%w: trailing data after artifact document", ErrFrozenPlanFormat)
	}

	// Re-marshal and compare byte-for-byte: this enforces the canonical
	// representation and also rejects duplicate JSON keys, whose decoded value
	// would no longer reproduce the original document.
	canonical, err := plan.Marshal()
	if err != nil {
		return nil, err
	}
	if !bytes.Equal(canonical, data) {
		return nil, fmt.Errorf("%w: artifact is not serialized in the canonical encoding", ErrFrozenPlanFormat)
	}

	if err := plan.validate(); err != nil {
		return nil, err
	}
	return &plan, nil
}

// validate checks every semantic invariant of a parsed artifact.
func (p *FrozenPlan) validate() error {
	if p.Kind != frozenPlanKind {
		return fmt.Errorf("%w: unknown artifact kind %q", ErrFrozenPlanFormat, p.Kind)
	}
	if p.FormatVersion != FrozenPlanFormatVersion {
		return fmt.Errorf("%w: unsupported format version %d, expected %d",
			ErrFrozenPlanFormat, p.FormatVersion, FrozenPlanFormatVersion)
	}
	if _, err := parseFrozenDirection(p.Direction); err != nil {
		return err
	}
	if strings.TrimSpace(p.Source.Kind) == "" || strings.TrimSpace(p.Source.Identity) == "" {
		return fmt.Errorf("%w: source kind and identity are required", ErrFrozenPlanFormat)
	}
	if _, ok := MigrationDialects[p.Target.Dialect]; !ok {
		return fmt.Errorf("%w: unknown dialect %q", ErrFrozenPlanFormat, p.Target.Dialect)
	}
	if strings.TrimSpace(p.Target.Database) == "" || strings.TrimSpace(p.Target.Table) == "" {
		return fmt.Errorf("%w: complete target identity is required", ErrFrozenPlanFormat)
	}

	ids := p.AppliedState.AppliedIDs
	for i, id := range ids {
		if id == "" {
			return fmt.Errorf("%w: empty applied migration id", ErrFrozenPlanFormat)
		}
		if i > 0 && ids[i-1] >= id {
			return fmt.Errorf("%w: applied ids must be sorted and unique", ErrFrozenPlanFormat)
		}
	}
	if appliedRecordsFingerprint(ids) != p.AppliedState.Fingerprint {
		return fmt.Errorf("%w: applied-state fingerprint does not match applied ids", ErrFrozenPlanFormat)
	}

	seen := make(map[string]struct{}, len(p.Migrations))
	for _, migration := range p.Migrations {
		if migration.ID == "" {
			return fmt.Errorf("%w: empty migration id", ErrFrozenPlanFormat)
		}
		if _, duplicate := seen[migration.ID]; duplicate {
			return fmt.Errorf("%w: duplicate migration id %q", ErrFrozenPlanFormat, migration.ID)
		}
		seen[migration.ID] = struct{}{}
		for _, statement := range migration.Statements {
			raw := []byte(statement.Bytes)
			if statement.Size != len(raw) {
				return fmt.Errorf("%w: migration %q declares %d bytes but holds %d",
					ErrFrozenPlanHash, migration.ID, statement.Size, len(raw))
			}
			if statement.SHA256 != hashStatementBytes(raw) {
				return fmt.Errorf("%w: migration %q statement sha256 mismatch", ErrFrozenPlanHash, migration.ID)
			}
		}
	}
	return nil
}

// ExecuteFrozenPlan executes a previously built artifact. All static and
// database-side checks run under the caller-supplied exclusive guard before
// any migration SQL is executed; rejection paths perform zero DDL and zero
// writes beyond read-only SELECTs.
func ExecuteFrozenPlan(ctx context.Context, db *sql.DB, dialect string, source MigrationSource, guard FrozenPlanGuard, plan *FrozenPlan) (*FrozenPlanResult, error) {
	return migSet.ExecuteFrozenPlan(ctx, db, dialect, source, guard, plan)
}

// ExecuteFrozenPlan is the MigrationSet-bound variant of the package-level
// function.
func (ms MigrationSet) ExecuteFrozenPlan(ctx context.Context, db *sql.DB, dialect string, source MigrationSource, guard FrozenPlanGuard, plan *FrozenPlan) (result *FrozenPlanResult, err error) {
	// A guard is mandatory: comparing records once does not protect against a
	// second process applying migrations between the check and the execution.
	if guard == nil {
		return nil, ErrFrozenPlanGuardRequired
	}
	if plan == nil {
		return nil, fmt.Errorf("%w: nil artifact", ErrFrozenPlanFormat)
	}

	// Pure static validation first, so a malformed or tampered artifact never
	// reaches the database at all.
	if err := plan.validate(); err != nil {
		return nil, err
	}
	dir, err := parseFrozenDirection(plan.Direction)
	if err != nil {
		return nil, err
	}

	if err := guard.Lock(ctx); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrFrozenPlanGuard, err)
	}
	defer func() {
		// Always release, even if the caller's context was canceled.
		if unlockErr := guard.Unlock(context.Background()); unlockErr != nil && err == nil {
			err = fmt.Errorf("%w: %v", ErrFrozenPlanGuard, unlockErr)
		}
	}()

	// The artifact is bound to one explicit database identity.
	resolvedTarget, err := ms.resolveFrozenTarget(dialect, plan.Target)
	if err != nil {
		return nil, err
	}
	if resolvedTarget != plan.Target {
		return nil, fmt.Errorf("%w: plan was built for %+v but execution targets %+v",
			ErrFrozenPlanTarget, plan.Target, resolvedTarget)
	}

	// Re-read the applied state under the guard and require an exact match.
	records, tableExists, err := ms.readMigrationRecordsReadOnly(db, dialect)
	if err != nil {
		return nil, err
	}
	currentIDs := make([]string, 0, len(records))
	for _, record := range records {
		currentIDs = append(currentIDs, record.Id)
	}
	sort.Strings(currentIDs)
	if tableExists != plan.AppliedState.MigrationTableExists ||
		appliedRecordsFingerprint(currentIDs) != plan.AppliedState.Fingerprint {
		return nil, fmt.Errorf("%w: expected table_exists=%t fingerprint=%s, got table_exists=%t fingerprint=%s",
			ErrFrozenPlanState,
			plan.AppliedState.MigrationTableExists, plan.AppliedState.Fingerprint,
			tableExists, appliedRecordsFingerprint(currentIDs))
	}

	// Reload the source only to prove it is unchanged. The frozen artifact
	// remains the sole source of executable SQL; a same-id replacement is an
	// error, never a silent substitution.
	if source != nil {
		if err := verifyFrozenSource(source, dir, plan); err != nil {
			return nil, err
		}
	}

	dbMap, err := ms.readOnlyDbMap(db, dialect)
	if err != nil {
		return nil, err
	}

	// Mirror the regular execution path and create the bookkeeping table only
	// after every check has passed and only when there is work to record.
	if !tableExists && len(plan.Migrations) > 0 {
		if ms.DisableCreateTable {
			return nil, fmt.Errorf("%w: migration table %q does not exist and table creation is disabled",
				ErrFrozenPlanState, ms.getTableName())
		}
		if err := dbMap.CreateTablesIfNotExists(); err != nil {
			if (dialect == "oci8" || dialect == "godror") && strings.Contains(err.Error(), "ORA-00955:") {
				// Another actor created the table in the meantime; acceptable.
			} else {
				return nil, err
			}
		}
	}

	return ms.applyFrozenMigrations(ctx, dir, plan, dbMap)
}

// verifyFrozenSource reloads the source and proves that every frozen migration
// still has identical statements and the identical transaction flag. It never
// copies reloaded SQL into the plan.
func verifyFrozenSource(source MigrationSource, dir MigrationDirection, plan *FrozenPlan) error {
	migrations, err := source.FindMigrations()
	if err != nil {
		return fmt.Errorf("%w: %v", ErrFrozenPlanSource, err)
	}

	byID := make(map[string]*Migration, len(migrations))
	for _, migration := range migrations {
		byID[migration.Id] = migration
	}

	for _, frozen := range plan.Migrations {
		migration, ok := byID[frozen.ID]
		if !ok {
			return fmt.Errorf("%w: migration %q is no longer present in the source", ErrFrozenPlanSource, frozen.ID)
		}

		var queries []string
		var disableTransaction bool
		if dir == Up {
			queries, disableTransaction = migration.Up, migration.DisableTransactionUp
		} else {
			queries, disableTransaction = migration.Down, migration.DisableTransactionDown
		}

		if disableTransaction != frozen.DisableTransaction {
			return fmt.Errorf("%w: migration %q transaction flag changed", ErrFrozenPlanSource, frozen.ID)
		}
		if len(queries) != len(frozen.Statements) {
			return fmt.Errorf("%w: migration %q statement count changed", ErrFrozenPlanSource, frozen.ID)
		}
		for i, statement := range frozen.Statements {
			if queries[i] != statement.Bytes {
				return fmt.Errorf("%w: migration %q statement %d changed; same-id replacements are never silently applied",
					ErrFrozenPlanSource, frozen.ID, i)
			}
		}
	}
	return nil
}

// applyFrozenMigrations runs the frozen statements in plan order. Artifact
// bytes are the only SQL executed; they go through the same suffix trimming as
// regular execution so both paths produce identical driver input.
func (ms MigrationSet) applyFrozenMigrations(ctx context.Context, dir MigrationDirection, plan *FrozenPlan, dbMap *gorp.DbMap) (*FrozenPlanResult, error) {
	result := &FrozenPlanResult{Steps: make([]FrozenPlanStepResult, len(plan.Migrations))}
	for i, migration := range plan.Migrations {
		result.Steps[i] = FrozenPlanStepResult{
			ID:                 migration.ID,
			DisableTransaction: migration.DisableTransaction,
			Status:             FrozenStepNotStarted,
		}
	}

	for i, migration := range plan.Migrations {
		step := &result.Steps[i]

		var executor SqlExecutor
		if migration.DisableTransaction {
			executor = dbMap.WithContext(ctx)
		} else {
			transaction, err := dbMap.Begin()
			if err != nil {
				step.Status = FrozenStepNotStarted
				return result, newFrozenStepError(step, err)
			}
			executor = transaction.WithContext(ctx)
		}

		var execErr error
		for _, statement := range migration.Statements {
			if _, err := executor.Exec(trimStatementSuffix(statement.Bytes)); err != nil {
				execErr = err
				break
			}
		}

		if execErr == nil {
			switch dir {
			case Up:
				execErr = executor.Insert(&MigrationRecord{
					Id:        migration.ID,
					AppliedAt: time.Now(),
				})
			case Down:
				_, execErr = executor.Delete(&MigrationRecord{
					Id: migration.ID,
				})
			default:
				panic("Not possible")
			}
		}

		if transaction, ok := executor.(*gorp.Transaction); ok {
			if execErr != nil {
				if rollbackErr := transaction.Rollback(); rollbackErr != nil {
					// The transactional outcome can no longer be asserted.
					step.Status = FrozenStepUncertain
					step.Err = execErr
					return result, newFrozenStepError(step, fmt.Errorf("%v; rollback also failed: %v", execErr, rollbackErr))
				}
				step.Status = FrozenStepNotStarted
				step.Err = execErr
				return result, newFrozenStepError(step, execErr)
			}
			if err := transaction.Commit(); err != nil {
				// Commit may have succeeded server-side: do not claim certainty.
				step.Status = FrozenStepUncertain
				step.Err = err
				return result, newFrozenStepError(step, err)
			}
		} else if execErr != nil {
			// notransaction: earlier statements are already autocommitted and
			// the failed one may or may not have taken effect. This is never
			// retried automatically; re-running requires an explicit decision.
			step.Status = FrozenStepUncertain
			step.Err = execErr
			return result, newFrozenStepError(step, execErr)
		}

		step.Status = FrozenStepCompleted
		result.Applied++
	}

	return result, nil
}

func newFrozenStepError(step *FrozenPlanStepResult, err error) error {
	return fmt.Errorf("frozen plan: migration %q: %w", step.ID, err)
}
