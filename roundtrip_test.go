package migrate

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	//revive:disable-next-line:dot-imports
	. "gopkg.in/check.v1"

	_ "github.com/mattn/go-sqlite3"
)

// RoundTripSuite exercises VerifyRoundTrip against real, file-backed SQLite
// databases created by an explicit disposable factory. Every test verifies
// that the temporary target is destroyed and that unrelated caller databases
// are left untouched.
type RoundTripSuite struct {
	dir string
}

var _ = Suite(&RoundTripSuite{})

func (s *RoundTripSuite) SetUpTest(c *C) {
	s.dir = c.MkDir()
}

// tempSQLiteFactory is a DisposableDatabaseFactory backed by real temporary
// SQLite files. Cleanup only ever closes and removes the target it created;
// the sibling file simulates another caller-owned database.
type tempSQLiteFactory struct {
	dir          string
	sibling      string
	mu           sync.Mutex
	targets      []string
	lastDB       *sql.DB
	cleanupCalls int
	cleanupErr   error
}

func newTempSQLiteFactory(c *C, dir string) *tempSQLiteFactory {
	sibling := filepath.Join(dir, "caller-other.db")
	c.Assert(os.WriteFile(sibling, []byte("caller-owned"), 0o600), IsNil)
	return &tempSQLiteFactory{dir: dir, sibling: sibling}
}

func (f *tempSQLiteFactory) Create(ctx context.Context) (*sql.DB, func(context.Context) error, error) {
	file, err := os.CreateTemp(f.dir, "rt-target-*.db")
	if err != nil {
		return nil, nil, err
	}
	path := file.Name()
	if err := file.Close(); err != nil {
		return nil, nil, err
	}

	db, err := sql.Open("sqlite3", path)
	if err != nil {
		return nil, nil, err
	}
	if err := db.PingContext(ctx); err != nil {
		_ = db.Close()
		return nil, nil, err
	}

	f.mu.Lock()
	f.targets = append(f.targets, path)
	f.lastDB = db
	f.mu.Unlock()

	cleanup := func(cleanupCtx context.Context) error {
		_ = cleanupCtx
		f.mu.Lock()
		f.cleanupCalls++
		f.mu.Unlock()
		errClose := db.Close()
		errRemove := os.Remove(path)
		if f.cleanupErr != nil {
			return f.cleanupErr
		}
		return errors.Join(errClose, errRemove)
	}
	return db, cleanup, nil
}

func (f *tempSQLiteFactory) assertAllTargetsRemoved(c *C) {
	f.mu.Lock()
	defer f.mu.Unlock()
	c.Assert(f.cleanupCalls, Equals, len(f.targets))
	for _, path := range f.targets {
		_, err := os.Stat(path)
		c.Assert(os.IsNotExist(err), Equals, true, Commentf("target %s should have been destroyed", path))
	}
}

func (f *tempSQLiteFactory) assertSiblingSurvives(c *C) {
	_, err := os.Stat(f.sibling)
	c.Assert(err, IsNil, Commentf("caller-owned database must never be touched"))
}

func (f *tempSQLiteFactory) assertLastDBClosed(c *C) {
	c.Assert(f.lastDB, NotNil)
	c.Assert(f.lastDB.Ping(), NotNil)
}

func roundTripConfig(f DisposableDatabaseFactory, migrations MigrationSource) RoundTripConfig {
	return RoundTripConfig{
		Factory:      f,
		Dialect:      "sqlite3",
		Migrations:   migrations,
		MigrationSet: MigrationSet{TableName: "rt_verify_migrations"},
		Budgets:      RoundTripBudgets{StageTimeout: 10 * time.Second},
	}
}

func schemaDiffKinds(report *RoundTripReport, compare string) []string {
	var kinds []string
	for _, diff := range report.SchemaDiffs {
		if diff.Compare == compare {
			kinds = append(kinds, diff.Kind)
		}
	}
	return kinds
}

func findTable(snapshot *SchemaSnapshot, name string) (TableDescriptor, bool) {
	for _, table := range snapshot.Tables {
		if table.Name == name {
			return table, true
		}
	}
	return TableDescriptor{}, false
}

func findIndex(snapshot *SchemaSnapshot, table, indexName string) (IndexDescriptor, bool) {
	for _, index := range snapshot.Indexes {
		if index.Table == table && index.Name == indexName {
			return index, true
		}
	}
	return IndexDescriptor{}, false
}

func extractFreshSnapshot(c *C, factory *tempSQLiteFactory, migrations MigrationSource) *SchemaSnapshot {
	db, cleanup, err := factory.Create(context.Background())
	c.Assert(err, IsNil)
	defer func() { c.Assert(cleanup(context.Background()), IsNil) }()

	ms := MigrationSet{TableName: "rt_verify_migrations"}
	_, err = ms.Exec(db, "sqlite3", migrations, Up)
	c.Assert(err, IsNil)

	snapshot, err := sqliteSchemaExtractor{}.extract(context.Background(), db, "rt_verify_migrations")
	c.Assert(err, IsNil)
	return snapshot
}

// TestRoundTripSuccess verifies a fully reversible migration set: primary
// keys, explicit and auto indexes, foreign keys with deferral and meaningful
// defaults survive an Up-Down-Up cycle and the disposable target is released.
func (s *RoundTripSuite) TestRoundTripSuccess(c *C) {
	factory := newTempSQLiteFactory(c, s.dir)
	migrations := &MemoryMigrationSource{Migrations: []*Migration{
		{
			Id:   "1_orgs",
			Up:   []string{"CREATE TABLE orgs (id INTEGER PRIMARY KEY, name TEXT NOT NULL)"},
			Down: []string{"DROP TABLE orgs"},
		},
		{
			Id: "2_customers",
			Up: []string{
				`CREATE TABLE customers (
					id INTEGER PRIMARY KEY,
					email TEXT NOT NULL UNIQUE,
					status TEXT NOT NULL DEFAULT 'active',
					org_id INTEGER REFERENCES orgs(id) ON DELETE SET NULL DEFERRABLE INITIALLY DEFERRED
				)`,
				"CREATE INDEX idx_customers_status ON customers(status)",
			},
			Down: []string{"DROP INDEX idx_customers_status", "DROP TABLE customers"},
		},
	}}

	config := roundTripConfig(factory, migrations)
	config.Fixtures = []RoundTripFixture{
		{Name: "org", Statement: "INSERT INTO orgs (id, name) VALUES (1, 'acme')", Rows: 1},
		{Name: "customer", Statement: "INSERT INTO customers (id, email, org_id) VALUES (1, 'a@example.com', 1)", Rows: 1},
	}
	// Catalog checks are independent of fixture rows, so they remain stable
	// across the Up-Down-Up cycle while the data-bearing checks are covered by
	// the dedicated data-loss scenario.
	config.Checks = []RoundTripCheck{
		{Name: "tables", Query: "SELECT name FROM sqlite_master WHERE type='table' AND name IN ('orgs','customers') ORDER BY name"},
		{Name: "explicit_index", Query: "SELECT name FROM sqlite_master WHERE type='index' AND name='idx_customers_status'"},
	}

	report, err := VerifyRoundTrip(context.Background(), config)
	c.Assert(err, IsNil)
	c.Assert(report, NotNil)
	c.Assert(report.Status, Equals, RoundTripVerified, Commentf("report: %+v", report))
	c.Assert(report.SchemaDiffs, HasLen, 0)
	c.Assert(report.CheckDiffs, HasLen, 0)
	c.Assert(report.Error, Equals, "")
	c.Assert(report.CleanupError, Equals, "")
	c.Assert(report.Target.CleanupCalled, Equals, true)
	c.Assert(report.ScopeNote, Not(Equals), "")
	c.Assert(report.Stages, HasLen, 5)

	c.Assert(report.Budget.FixtureStatements, Equals, 2)
	c.Assert(report.Budget.FixtureRows, Equals, int64(2))

	factory.assertAllTargetsRemoved(c)
	factory.assertSiblingSurvives(c)
	factory.assertLastDBClosed(c)

	// The normalization of the unstable sqlite_autoindex name is disclosed.
	autoindexRule := false
	for _, normalization := range report.Normalizations {
		if strings.Contains(normalization, "autoindex") {
			autoindexRule = true
		}
	}
	c.Assert(autoindexRule, Equals, true)

	// Direct extractor evidence: structural semantics are retained even though
	// the auto-index numeric name is normalized.
	snapshot := extractFreshSnapshot(c, factory, migrations)
	customers, ok := findTable(snapshot, "customers")
	c.Assert(ok, Equals, true)
	c.Assert(customers.PrimaryKey, DeepEquals, []string{"id"})
	c.Assert(customers.ForeignKeys, HasLen, 1)
	fk := customers.ForeignKeys[0]
	c.Assert(fk.References, Equals, "orgs")
	c.Assert(fk.OnDelete, Equals, "SET NULL")
	c.Assert(fk.Deferrable, Equals, "DEFERRABLE INITIALLY DEFERRED")

	autoIndex, ok := findIndex(snapshot, "customers", "autoindex:customers:u:email")
	c.Assert(ok, Equals, true)
	c.Assert(autoIndex.AutoName, Equals, true)
	c.Assert(autoIndex.Origin, Equals, "u")
	c.Assert(autoIndex.Unique, Equals, true)
	c.Assert(autoIndex.Columns, HasLen, 1)
	c.Assert(autoIndex.Columns[0].Name, Equals, "email")

	explicitIndex, ok := findIndex(snapshot, "customers", "idx_customers_status")
	c.Assert(ok, Equals, true)
	c.Assert(explicitIndex.AutoName, Equals, false)
	c.Assert(explicitIndex.Origin, Equals, "c")

	statusColumn := customers.Columns[2]
	c.Assert(statusColumn.Name, Equals, "status")
	c.Assert(statusColumn.HasDefault, Equals, true)
	c.Assert(statusColumn.Default, Equals, "'active'")
	c.Assert(statusColumn.NotNull, Equals, true)
}

// TestRoundTripDownMissingIndex verifies that a Down section which forgets to
// remove an index is caught against the explicit baseline, not masked by the
// second Up succeeding.
func (s *RoundTripSuite) TestRoundTripDownMissingIndex(c *C) {
	factory := newTempSQLiteFactory(c, s.dir)
	migrations := &MemoryMigrationSource{Migrations: []*Migration{
		{
			Id:   "1_index",
			Up:   []string{"CREATE INDEX IF NOT EXISTS idx_t_name ON t(name)"},
			Down: []string{"SELECT 0"}, // buggy: the index is never dropped.
		},
	}}

	config := roundTripConfig(factory, migrations)
	config.BaselineSetup = []string{
		"CREATE TABLE t (id INTEGER PRIMARY KEY, name TEXT)",
	}
	config.Checks = []RoundTripCheck{
		{Name: "rows", Query: "SELECT COUNT(*) AS n FROM t"},
	}

	report, err := VerifyRoundTrip(context.Background(), config)
	c.Assert(err, IsNil)
	c.Assert(report.Status, Equals, RoundTripFailed)
	c.Assert(report.CleanupError, Equals, "")
	c.Assert(schemaDiffKinds(report, compareUp1Up2), HasLen, 0)

	var downDiffs []SchemaDifference
	for _, diff := range report.SchemaDiffs {
		if diff.Compare == compareDownBaseline {
			downDiffs = append(downDiffs, diff)
		}
	}
	c.Assert(downDiffs, HasLen, 1)
	c.Assert(downDiffs[0].Kind, Equals, "index-extra")
	c.Assert(downDiffs[0].Object, Equals, "t.idx_t_name")
	c.Assert(downDiffs[0].Expected, Equals, "<absent>")
	c.Assert(strings.Contains(downDiffs[0].Actual, "idx_t_name"), Equals, true)
	c.Assert(report.CheckDiffs, HasLen, 0)

	factory.assertAllTargetsRemoved(c)
	factory.assertSiblingSurvives(c)
}

// TestRoundTripDataLoss verifies that fixture data destroyed between the two
// Up stages is reported as an exact check-result difference rather than a
// silent SQL success.
func (s *RoundTripSuite) TestRoundTripDataLoss(c *C) {
	factory := newTempSQLiteFactory(c, s.dir)
	migrations := &MemoryMigrationSource{Migrations: []*Migration{
		{
			Id:   "1_accounts",
			Up:   []string{"CREATE TABLE accounts (id INTEGER PRIMARY KEY, balance INTEGER NOT NULL DEFAULT 0)"},
			Down: []string{"DROP TABLE accounts"},
		},
		{
			Id:   "2_wipe",
			Up:   []string{"DELETE FROM accounts"},
			Down: []string{"SELECT 0"},
		},
	}}

	config := roundTripConfig(factory, migrations)
	config.Fixtures = []RoundTripFixture{
		{Statement: "INSERT INTO accounts (balance) VALUES (100)", Rows: 1},
		{Statement: "INSERT INTO accounts (balance) VALUES (200)", Rows: 1},
	}
	config.Checks = []RoundTripCheck{
		{Name: "totals", Query: "SELECT COUNT(*) AS n, COALESCE(SUM(balance), 0) AS total FROM accounts"},
		{Name: "ids", Query: "SELECT id FROM accounts ORDER BY id"},
	}

	report, err := VerifyRoundTrip(context.Background(), config)
	c.Assert(err, IsNil)
	c.Assert(report.Status, Equals, RoundTripFailed)
	c.Assert(report.CleanupError, Equals, "")

	var upDiffs []CheckDifference
	for _, diff := range report.CheckDiffs {
		if diff.Compare == compareUp1Up2 {
			upDiffs = append(upDiffs, diff)
		}
	}
	c.Assert(upDiffs, Not(HasLen), 0)
	rowCountDiff := false
	valueDiff := false
	for _, diff := range upDiffs {
		if diff.Check == "ids" && diff.Kind == "row-count" {
			c.Assert(diff.Expected, Equals, "2")
			c.Assert(diff.Actual, Equals, "0")
			rowCountDiff = true
		}
		if diff.Check == "totals" && diff.Kind == "value" {
			c.Assert(strings.Contains(diff.Expected, "int:2"), Equals, true)
			c.Assert(strings.Contains(diff.Expected, "int:300"), Equals, true)
			c.Assert(strings.Contains(diff.Actual, "int:0"), Equals, true)
			valueDiff = true
		}
	}
	c.Assert(rowCountDiff, Equals, true, Commentf("exact cardinality difference must be reported"))
	c.Assert(valueDiff, Equals, true, Commentf("exact aggregate value difference must be reported"))

	factory.assertAllTargetsRemoved(c)
	factory.assertSiblingSurvives(c)
}

// TestRoundTripNonDeterministicDefault verifies that meaningful default
// expressions are preserved by normalization, while check values produced by
// non-deterministic defaults are either reported as exact differences or, when
// explicitly declared, excluded only with a recorded finding.
func (s *RoundTripSuite) TestRoundTripNonDeterministicDefault(c *C) {
	factory := newTempSQLiteFactory(c, s.dir)
	migrations := &MemoryMigrationSource{Migrations: []*Migration{
		{
			Id:   "1_label",
			Up:   []string{"ALTER TABLE events ADD COLUMN label TEXT"},
			Down: []string{"ALTER TABLE events DROP COLUMN label"},
		},
	}}

	// Schema-level evidence: default expressions survive both Up stages and
	// are retained verbatim by the extractor.
	config := roundTripConfig(factory, migrations)
	config.BaselineSetup = []string{
		`CREATE TABLE events (
			id TEXT PRIMARY KEY DEFAULT (hex(randomblob(8))),
			created_at TEXT NOT NULL DEFAULT (datetime('now')),
			note TEXT DEFAULT 'fixed'
		)`,
	}
	config.Checks = []RoundTripCheck{
		{Name: "rows", Query: "SELECT COUNT(*) AS n FROM events"},
	}

	report, err := VerifyRoundTrip(context.Background(), config)
	c.Assert(err, IsNil)
	c.Assert(report.Status, Equals, RoundTripVerified, Commentf("report: %+v", report))
	c.Assert(report.SchemaDiffs, HasLen, 0)

	snapshot := extractFreshSnapshot(c, factory, &MemoryMigrationSource{Migrations: []*Migration{
		{
			Id:   "0_baseline_events",
			Up:   config.BaselineSetup,
			Down: []string{"DROP TABLE events"},
		},
	}})
	events, ok := findTable(snapshot, "events")
	c.Assert(ok, Equals, true)
	c.Assert(events.Columns[0].HasDefault, Equals, true)
	c.Assert(events.Columns[0].Default, Equals, "hex(randomblob(8))")
	c.Assert(events.Columns[1].Default, Equals, "datetime('now')")
	c.Assert(events.Columns[2].Default, Equals, "'fixed'")

	// A changed meaningful default must be detected rather than normalized away.
	want := &SchemaSnapshot{Tables: []TableDescriptor{{
		Name: "events",
		Columns: []ColumnDescriptor{
			{Ordinal: 0, Name: "note", Type: "TEXT", HasDefault: true, Default: "'fixed'"},
		},
	}}}
	got := &SchemaSnapshot{Tables: []TableDescriptor{{
		Name: "events",
		Columns: []ColumnDescriptor{
			{Ordinal: 0, Name: "note", Type: "TEXT", HasDefault: false},
		},
	}}}
	defaultDiffs := diffSchema(want, got, compareUp1Up2)
	c.Assert(defaultDiffs, HasLen, 1)
	c.Assert(defaultDiffs[0].Kind, Equals, "column-default")

	// Check-level evidence without a non-determinism declaration: the exact
	// value difference must fail the run.
	strictFactory := newTempSQLiteFactory(c, s.dir)
	strictConfig := roundTripConfig(strictFactory, migrations)
	strictConfig.BaselineSetup = config.BaselineSetup
	strictConfig.Checks = []RoundTripCheck{
		{Name: "random", Query: "SELECT hex(randomblob(8)) AS r, datetime('now') AS now"},
	}
	strictReport, err := VerifyRoundTrip(context.Background(), strictConfig)
	c.Assert(err, IsNil)
	c.Assert(strictReport.Status, Equals, RoundTripFailed)
	foundValueDiff := false
	for _, diff := range strictReport.CheckDiffs {
		if diff.Check == "random" && diff.Kind == "value" {
			foundValueDiff = true
		}
	}
	c.Assert(foundValueDiff, Equals, true)
	strictFactory.assertAllTargetsRemoved(c)

	// With an explicit declaration the values are excluded, but the exclusion
	// is recorded and the result can only be partial, never fully verified.
	declaredFactory := newTempSQLiteFactory(c, s.dir)
	declaredConfig := roundTripConfig(declaredFactory, migrations)
	declaredConfig.BaselineSetup = config.BaselineSetup
	declaredConfig.Checks = []RoundTripCheck{
		{
			Name:                    "random",
			Query:                   "SELECT hex(randomblob(8)) AS r, datetime('now') AS now",
			NonDeterministicColumns: []string{"r", "now"},
		},
	}
	declaredReport, err := VerifyRoundTrip(context.Background(), declaredConfig)
	c.Assert(err, IsNil)
	c.Assert(declaredReport.CheckDiffs, HasLen, 0)
	c.Assert(declaredReport.Status, Equals, RoundTripPartial)
	findingSeen := false
	for _, finding := range declaredReport.Findings {
		if finding.Code == "non-deterministic-column-exclusion" {
			findingSeen = true
		}
	}
	c.Assert(findingSeen, Equals, true)
	declaredFactory.assertAllTargetsRemoved(c)
	declaredFactory.assertSiblingSurvives(c)
}

// TestRoundTripMidFailure verifies that a failing stage aborts the run, keeps
// the primary error, still destroys the target and never touches sibling
// databases. A cleanup failure is retained separately.
func (s *RoundTripSuite) TestRoundTripMidFailure(c *C) {
	factory := newTempSQLiteFactory(c, s.dir)
	migrations := &MemoryMigrationSource{Migrations: []*Migration{
		{
			Id:   "1_people",
			Up:   []string{"CREATE TABLE people (id INTEGER PRIMARY KEY)"},
			Down: []string{"DROP TABLE people"},
		},
		{
			Id:   "2_column",
			Up:   []string{"ALTER TABLE people ADD COLUMN first_name TEXT"},
			Down: []string{"ALTER TABLE people DROP COLUMN first_name"},
		},
		{
			Id:   "3_boom",
			Up:   []string{"INSERT INTO people (id) VALUES (1)", "SELECT fail"},
			Down: []string{"SELECT 0"},
		},
	}}

	report, err := VerifyRoundTrip(context.Background(), roundTripConfig(factory, migrations))
	c.Assert(err, IsNil)
	c.Assert(report.Status, Equals, RoundTripFailed)
	c.Assert(report.Error, Not(Equals), "")
	c.Assert(strings.Contains(report.Error, "up-1"), Equals, true)
	c.Assert(report.Target.CleanupCalled, Equals, true)
	c.Assert(report.CleanupError, Equals, "")
	factory.assertAllTargetsRemoved(c)
	factory.assertSiblingSurvives(c)
	factory.assertLastDBClosed(c)

	// A target whose cleanup itself fails is reported via CleanupError while
	// the run otherwise produced no schema or check differences.
	failingFactory := newTempSQLiteFactory(c, s.dir)
	failingFactory.cleanupErr = errors.New("simulated cleanup failure")
	goodMigrations := &MemoryMigrationSource{Migrations: []*Migration{
		{
			Id:   "1_people",
			Up:   []string{"CREATE TABLE people (id INTEGER PRIMARY KEY)"},
			Down: []string{"DROP TABLE people"},
		},
	}}
	failingReport, err := VerifyRoundTrip(context.Background(), roundTripConfig(failingFactory, goodMigrations))
	c.Assert(err, IsNil)
	c.Assert(failingReport.CleanupError, Equals, "simulated cleanup failure")
	c.Assert(failingReport.Status, Equals, RoundTripFailed)
	c.Assert(failingReport.Error, Equals, "")
	c.Assert(failingReport.SchemaDiffs, HasLen, 0)
	failingFactory.assertSiblingSurvives(c)
}

// TestVerifyRoundTripPreflightAndCoverage verifies the safety gates (no
// implicit database, unsupported dialect), budget enforcement and the
// partial-coverage findings for notransaction, irreversible Down and
// unsupported object types.
func (s *RoundTripSuite) TestVerifyRoundTripPreflightAndCoverage(c *C) {
	// A factory is mandatory: no default or production configuration is used.
	_, err := VerifyRoundTrip(context.Background(), RoundTripConfig{Dialect: "sqlite3"})
	c.Assert(err, NotNil)
	c.Assert(strings.Contains(err.Error(), "DisposableDatabaseFactory"), Equals, true)

	factory := newTempSQLiteFactory(c, s.dir)
	// Dialects without a registered extractor cannot produce evidence.
	_, err = VerifyRoundTrip(context.Background(), RoundTripConfig{
		Factory:    factory,
		Dialect:    "mysql",
		Migrations: &MemoryMigrationSource{},
	})
	c.Assert(err, NotNil)
	c.Assert(strings.Contains(err.Error(), "schema extractor"), Equals, true)

	// A view is an uncovered object type: the run is partial, not verified.
	viewMigrations := &MemoryMigrationSource{Migrations: []*Migration{
		{
			Id:   "1_base",
			Up:   []string{"CREATE TABLE v_base (id INTEGER PRIMARY KEY)"},
			Down: []string{"DROP TABLE v_base"},
		},
		{
			Id:   "2_view",
			Up:   []string{"CREATE VIEW v AS SELECT id FROM v_base"},
			Down: []string{"DROP VIEW v"},
		},
	}}
	viewReport, err := VerifyRoundTrip(context.Background(), roundTripConfig(factory, viewMigrations))
	c.Assert(err, IsNil)
	c.Assert(viewReport.Status, Equals, RoundTripPartial)
	unsupportedSeen := false
	for _, finding := range viewReport.Findings {
		if finding.Code == "unsupported-object" && strings.Contains(finding.Object, "view/v") {
			unsupportedSeen = true
		}
	}
	c.Assert(unsupportedSeen, Equals, true)
	factory.assertAllTargetsRemoved(c)

	// notransaction sections and missing Down sections are partial findings.
	partialFactory := newTempSQLiteFactory(c, s.dir)
	partialMigrations := &MemoryMigrationSource{Migrations: []*Migration{
		{
			Id:                     "1_no_tx",
			Up:                     []string{"CREATE TABLE no_tx (id INTEGER PRIMARY KEY)"},
			Down:                   []string{"DROP TABLE no_tx"},
			DisableTransactionUp:   true,
			DisableTransactionDown: true,
		},
		{
			Id:   "2_irreversible",
			Up:   []string{"ALTER TABLE no_tx ADD COLUMN name TEXT"},
			Down: nil,
		},
	}}
	partialReport, err := VerifyRoundTrip(context.Background(), roundTripConfig(partialFactory, partialMigrations))
	c.Assert(err, IsNil)
	c.Assert(partialReport.Status, Equals, RoundTripPartial)
	codes := map[string]bool{}
	for _, finding := range partialReport.Findings {
		codes[finding.Code] = true
	}
	c.Assert(codes["notransaction-up"], Equals, true)
	c.Assert(codes["notransaction-down"], Equals, true)
	c.Assert(codes["irreversible-down"], Equals, true)
	partialFactory.assertAllTargetsRemoved(c)

	// Statement budgets are enforced before execution and the target is still
	// destroyed.
	budgetFactory := newTempSQLiteFactory(c, s.dir)
	budgetMigrations := &MemoryMigrationSource{Migrations: []*Migration{
		{
			Id:   "1_two",
			Up:   []string{"CREATE TABLE a (id INTEGER PRIMARY KEY)", "CREATE TABLE b (id INTEGER PRIMARY KEY)"},
			Down: []string{"DROP TABLE a", "DROP TABLE b"},
		},
	}}
	budgetConfig := roundTripConfig(budgetFactory, budgetMigrations)
	budgetConfig.Budgets.MaxStatementsPerStage = 1
	budgetReport, err := VerifyRoundTrip(context.Background(), budgetConfig)
	c.Assert(err, IsNil)
	c.Assert(budgetReport.Status, Equals, RoundTripFailed)
	c.Assert(strings.Contains(budgetReport.Error, "statement budget"), Equals, true)
	budgetFactory.assertAllTargetsRemoved(c)
	budgetFactory.assertSiblingSurvives(c)

	// An expired stage context aborts the run but cleanup still happens.
	timeoutFactory := newTempSQLiteFactory(c, s.dir)
	timeoutConfig := roundTripConfig(timeoutFactory, budgetMigrations)
	timeoutConfig.Budgets.StageTimeout = time.Nanosecond
	timeoutReport, err := VerifyRoundTrip(context.Background(), timeoutConfig)
	c.Assert(err, IsNil)
	c.Assert(timeoutReport.Status, Equals, RoundTripFailed)
	c.Assert(timeoutReport.Target.CleanupCalled, Equals, true)
	timeoutFactory.assertAllTargetsRemoved(c)
}
