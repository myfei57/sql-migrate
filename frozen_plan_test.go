package migrate

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strings"
	"time"

	//revive:disable-next-line:dot-imports
	. "gopkg.in/check.v1"

	_ "github.com/mattn/go-sqlite3"
)

type FrozenPlanSuite struct {
	Db *sql.DB
	ms MigrationSet
}

var _ = Suite(&FrozenPlanSuite{})

func (s *FrozenPlanSuite) SetUpTest(c *C) {
	db, err := sql.Open("sqlite3", ":memory:")
	c.Assert(err, IsNil)
	s.Db = db
	// Use an explicit MigrationSet so global migSet state from other suites
	// cannot influence these tests.
	s.ms = MigrationSet{}
}

var frozenTestMigrations = []*Migration{
	{
		Id:   "1_create",
		Up:   []string{"CREATE TABLE people (id int)"},
		Down: []string{"DROP TABLE people"},
	},
	{
		Id:   "2_insert",
		Up:   []string{"INSERT INTO people (id) VALUES (1)"},
		Down: []string{"DELETE FROM people WHERE id=1"},
	},
}

func frozenTestSource() *MemoryMigrationSource {
	migrations := make([]*Migration, len(frozenTestMigrations))
	copy(migrations, frozenTestMigrations)
	return &MemoryMigrationSource{Migrations: migrations}
}

func frozenTestTarget() FrozenPlanTarget {
	return FrozenPlanTarget{Database: "frozen-test-database"}
}

func frozenTestSourceID() FrozenPlanSource {
	return FrozenPlanSource{Kind: "memory", Identity: "frozen-test-source"}
}

// dbSnapshot captures every user-visible schema and data surface needed to
// prove that a rejected execution performed zero DDL and zero writes.
type dbSnapshot struct {
	tables         []string
	recordTable    bool
	recordCount    int64
	peoplePresent  bool
	peopleRowCount int64
}

func (s *FrozenPlanSuite) snapshot(c *C) dbSnapshot {
	snap := dbSnapshot{}

	rows, err := s.Db.Query("SELECT name FROM sqlite_master WHERE type='table' ORDER BY name")
	c.Assert(err, IsNil)
	for rows.Next() {
		var name string
		c.Assert(rows.Scan(&name), IsNil)
		snap.tables = append(snap.tables, name)
	}
	c.Assert(rows.Close(), IsNil)

	for _, name := range snap.tables {
		switch name {
		case "gorp_migrations":
			snap.recordTable = true
			c.Assert(s.Db.QueryRow("SELECT COUNT(*) FROM gorp_migrations").Scan(&snap.recordCount), IsNil)
		case "people":
			snap.peoplePresent = true
			c.Assert(s.Db.QueryRow("SELECT COUNT(*) FROM people").Scan(&snap.peopleRowCount), IsNil)
		}
	}
	return snap
}

func assertSnapshotUnchanged(c *C, before, after dbSnapshot) {
	c.Assert(after.tables, DeepEquals, before.tables,
		Commentf("rejected path must perform zero DDL"))
	c.Assert(after.recordTable, Equals, before.recordTable)
	c.Assert(after.recordCount, Equals, before.recordCount,
		Commentf("rejected path must perform zero writes to the migration table"))
	c.Assert(after.peoplePresent, Equals, before.peoplePresent)
	c.Assert(after.peopleRowCount, Equals, before.peopleRowCount,
		Commentf("rejected path must perform zero writes to migrated tables"))
}

func (s *FrozenPlanSuite) buildAndParse(c *C, dir MigrationDirection, max int) *FrozenPlan {
	plan, err := s.ms.BuildFrozenPlan(s.Db, "sqlite3", frozenTestSource(), frozenTestSourceID(), frozenTestTarget(), dir, max)
	c.Assert(err, IsNil)
	data, err := plan.Marshal()
	c.Assert(err, IsNil)
	parsed, err := ParseFrozenPlan(data)
	c.Assert(err, IsNil)
	return parsed
}

func (s *FrozenPlanSuite) TestBuildFrozenPlanIsReadOnly(c *C) {
	before := s.snapshot(c)

	plan, err := s.ms.BuildFrozenPlan(s.Db, "sqlite3", frozenTestSource(), frozenTestSourceID(), frozenTestTarget(), Up, 0)
	c.Assert(err, IsNil)

	// No schema, no migration table may appear during a read-only build.
	after := s.snapshot(c)
	assertSnapshotUnchanged(c, before, after)
	c.Assert(after.tables, HasLen, 0)

	c.Assert(plan.Kind, Equals, frozenPlanKind)
	c.Assert(plan.FormatVersion, Equals, FrozenPlanFormatVersion)
	c.Assert(plan.Direction, Equals, frozenDirectionUp)
	c.Assert(plan.Target, DeepEquals, FrozenPlanTarget{
		Dialect:  "sqlite3",
		Database: "frozen-test-database",
		Schema:   "",
		Table:    "gorp_migrations",
	})
	c.Assert(plan.Source, DeepEquals, frozenTestSourceID())

	// Missing table is an explicitly recorded empty state.
	c.Assert(plan.AppliedState.MigrationTableExists, Equals, false)
	c.Assert(plan.AppliedState.AppliedIDs, HasLen, 0)
	c.Assert(plan.AppliedState.Fingerprint, Equals, appliedRecordsFingerprint(nil))

	c.Assert(plan.Migrations, HasLen, 2)
	c.Assert(plan.Migrations[0].ID, Equals, "1_create")
	c.Assert(plan.Migrations[0].DisableTransaction, Equals, false)
	c.Assert(plan.Migrations[0].Statements, HasLen, 1)
	stmt := plan.Migrations[0].Statements[0]
	c.Assert(stmt.Bytes, Equals, "CREATE TABLE people (id int)")
	c.Assert(stmt.Size, Equals, len([]byte(stmt.Bytes)))
	c.Assert(stmt.SHA256, Equals, hashStatementBytes([]byte(stmt.Bytes)))
	c.Assert(plan.Migrations[1].ID, Equals, "2_insert")

	// Canonical JSON round trip.
	data, err := plan.Marshal()
	c.Assert(err, IsNil)
	c.Assert(strings.HasSuffix(string(data), "\n"), Equals, true)
	parsed, err := ParseFrozenPlan(data)
	c.Assert(err, IsNil)
	c.Assert(parsed, DeepEquals, plan)
}

func (s *FrozenPlanSuite) TestBuildFrozenPlanRecordsExistingState(c *C) {
	n, err := s.ms.Exec(s.Db, "sqlite3", frozenTestSource(), Up)
	c.Assert(err, IsNil)
	c.Assert(n, Equals, 2)

	plan, err := s.ms.BuildFrozenPlan(s.Db, "sqlite3", frozenTestSource(), frozenTestSourceID(), frozenTestTarget(), Up, 0)
	c.Assert(err, IsNil)
	c.Assert(plan.AppliedState.MigrationTableExists, Equals, true)
	c.Assert(plan.AppliedState.AppliedIDs, DeepEquals, []string{"1_create", "2_insert"})
	c.Assert(plan.AppliedState.Fingerprint, Equals, appliedRecordsFingerprint([]string{"1_create", "2_insert"}))
	c.Assert(plan.Migrations, HasLen, 0)
}

func (s *FrozenPlanSuite) TestExecuteFrozenPlanParityWithExec(c *C) {
	// Reference database migrated through the regular path.
	refDB, err := sql.Open("sqlite3", ":memory:")
	c.Assert(err, IsNil)
	defer func() { _ = refDB.Close() }()
	n, err := s.ms.Exec(refDB, "sqlite3", frozenTestSource(), Up)
	c.Assert(err, IsNil)
	c.Assert(n, Equals, 2)

	// Frozen path on a second database.
	plan := s.buildAndParse(c, Up, 0)
	result, err := s.ms.ExecuteFrozenPlan(context.Background(), s.Db, "sqlite3", frozenTestSource(), NewInProcessFrozenGuard(), plan)
	c.Assert(err, IsNil)
	c.Assert(result.Applied, Equals, 2)
	c.Assert(result.Steps, HasLen, 2)
	for _, step := range result.Steps {
		c.Assert(step.Status, Equals, FrozenStepCompleted)
		c.Assert(step.Err, IsNil)
	}

	// Both databases must end up in exactly the same state.
	refTables := s.tablesOf(c, refDB)
	gotTables := s.tablesOf(c, s.Db)
	c.Assert(gotTables, DeepEquals, refTables)

	var refCount, gotCount int64
	c.Assert(refDB.QueryRow("SELECT COUNT(*) FROM people").Scan(&refCount), IsNil)
	c.Assert(s.Db.QueryRow("SELECT COUNT(*) FROM people").Scan(&gotCount), IsNil)
	c.Assert(gotCount, Equals, refCount)

	var refPeople, gotPeople int64
	c.Assert(refDB.QueryRow("SELECT id FROM people").Scan(&refPeople), IsNil)
	c.Assert(s.Db.QueryRow("SELECT id FROM people").Scan(&gotPeople), IsNil)
	c.Assert(gotPeople, Equals, refPeople)

	var refRecords, gotRecords int64
	c.Assert(refDB.QueryRow("SELECT COUNT(*) FROM gorp_migrations").Scan(&refRecords), IsNil)
	c.Assert(s.Db.QueryRow("SELECT COUNT(*) FROM gorp_migrations").Scan(&gotRecords), IsNil)
	c.Assert(gotRecords, Equals, refRecords)

	refIDs := s.recordIDs(c, refDB)
	gotIDs := s.recordIDs(c, s.Db)
	c.Assert(gotIDs, DeepEquals, refIDs)

	// A second frozen execution has nothing left to do and changes nothing.
	plan2 := s.buildAndParse(c, Up, 0)
	c.Assert(plan2.Migrations, HasLen, 0)
	beforeSecond := s.snapshot(c)
	result2, err := s.ms.ExecuteFrozenPlan(context.Background(), s.Db, "sqlite3", frozenTestSource(), NewInProcessFrozenGuard(), plan2)
	c.Assert(err, IsNil)
	c.Assert(result2.Applied, Equals, 0)
	assertSnapshotUnchanged(c, beforeSecond, s.snapshot(c))
}

func (s *FrozenPlanSuite) TestExecuteFrozenPlanDown(c *C) {
	n, err := s.ms.Exec(s.Db, "sqlite3", frozenTestSource(), Up)
	c.Assert(err, IsNil)
	c.Assert(n, Equals, 2)

	// Undo exactly the last migration.
	plan := s.buildAndParse(c, Down, 1)
	c.Assert(plan.Migrations, HasLen, 1)
	c.Assert(plan.Migrations[0].ID, Equals, "2_insert")
	c.Assert(plan.Direction, Equals, frozenDirectionDown)

	result, err := s.ms.ExecuteFrozenPlan(context.Background(), s.Db, "sqlite3", frozenTestSource(), NewInProcessFrozenGuard(), plan)
	c.Assert(err, IsNil)
	c.Assert(result.Applied, Equals, 1)
	c.Assert(result.Steps[0].Status, Equals, FrozenStepCompleted)

	var peopleCount int64
	c.Assert(s.Db.QueryRow("SELECT COUNT(*) FROM people").Scan(&peopleCount), IsNil)
	c.Assert(peopleCount, Equals, int64(0))
	c.Assert(s.recordIDs(c, s.Db), DeepEquals, []string{"1_create"})

	// Undo the remaining migration: the table itself must disappear.
	plan = s.buildAndParse(c, Down, 1)
	result, err = s.ms.ExecuteFrozenPlan(context.Background(), s.Db, "sqlite3", frozenTestSource(), NewInProcessFrozenGuard(), plan)
	c.Assert(err, IsNil)
	c.Assert(result.Applied, Equals, 1)
	c.Assert(s.tablesOf(c, s.Db), DeepEquals, []string{"gorp_migrations"})
}

func (s *FrozenPlanSuite) TestExecuteRejectsChangedSource(c *C) {
	plan := s.buildAndParse(c, Up, 0)
	before := s.snapshot(c)

	changed := &MemoryMigrationSource{Migrations: []*Migration{
		{
			Id:   "1_create", // same id, different content
			Up:   []string{"CREATE TABLE people (id int, name text)"},
			Down: []string{"DROP TABLE people"},
		},
		frozenTestMigrations[1],
	}}
	result, err := s.ms.ExecuteFrozenPlan(context.Background(), s.Db, "sqlite3", changed, NewInProcessFrozenGuard(), plan)
	c.Assert(err, ErrorMatches, ".*same-id replacements are never silently applied.*")
	c.Assert(errors.Is(err, ErrFrozenPlanSource), Equals, true)
	c.Assert(result, IsNil)
	assertSnapshotUnchanged(c, before, s.snapshot(c))

	// A source that dropped a frozen migration id is rejected as well.
	shrunk := &MemoryMigrationSource{Migrations: []*Migration{frozenTestMigrations[1]}}
	result, err = s.ms.ExecuteFrozenPlan(context.Background(), s.Db, "sqlite3", shrunk, NewInProcessFrozenGuard(), plan)
	c.Assert(errors.Is(err, ErrFrozenPlanSource), Equals, true)
	c.Assert(result, IsNil)
	assertSnapshotUnchanged(c, before, s.snapshot(c))
}

func (s *FrozenPlanSuite) TestExecuteRejectsChangedDatabaseState(c *C) {
	plan := s.buildAndParse(c, Up, 0)

	// Simulate another process applying a migration after the plan was built.
	_, err := s.Db.Exec("CREATE TABLE gorp_migrations (id text, applied_at datetime)")
	c.Assert(err, IsNil)
	_, err = s.Db.Exec("INSERT INTO gorp_migrations (id, applied_at) VALUES ('1_create', '2020-01-01 00:00:00')")
	c.Assert(err, IsNil)

	before := s.snapshot(c)
	result, err := s.ms.ExecuteFrozenPlan(context.Background(), s.Db, "sqlite3", frozenTestSource(), NewInProcessFrozenGuard(), plan)
	c.Assert(errors.Is(err, ErrFrozenPlanState), Equals, true)
	c.Assert(result, IsNil)
	assertSnapshotUnchanged(c, before, s.snapshot(c))

	// Table existence itself is part of the frozen pre-state as well: drop it
	// again and a plan that recorded an existing table must now be rejected.
	otherDB, err := sql.Open("sqlite3", ":memory:")
	c.Assert(err, IsNil)
	defer func() { _ = otherDB.Close() }()
	otherMS := MigrationSet{}
	_, err = otherMS.Exec(otherDB, "sqlite3", frozenTestSource(), Up)
	c.Assert(err, IsNil)
	existingPlan, err := otherMS.BuildFrozenPlan(otherDB, "sqlite3", frozenTestSource(), frozenTestSourceID(), frozenTestTarget(), Up, 0)
	c.Assert(err, IsNil)
	data, err := existingPlan.Marshal()
	c.Assert(err, IsNil)
	parsed, err := ParseFrozenPlan(data)
	c.Assert(err, IsNil)
	_, err = otherDB.Exec("DROP TABLE gorp_migrations")
	c.Assert(err, IsNil)
	_, err = otherMS.ExecuteFrozenPlan(context.Background(), otherDB, "sqlite3", frozenTestSource(), NewInProcessFrozenGuard(), parsed)
	c.Assert(errors.Is(err, ErrFrozenPlanState), Equals, true)
}

func (s *FrozenPlanSuite) TestExecuteRejectsTargetMismatch(c *C) {
	plan := s.buildAndParse(c, Up, 0)
	before := s.snapshot(c)

	// Different dialect implies a different database identity.
	result, err := s.ms.ExecuteFrozenPlan(context.Background(), s.Db, "postgres", frozenTestSource(), NewInProcessFrozenGuard(), plan)
	c.Assert(errors.Is(err, ErrFrozenPlanTarget), Equals, true)
	c.Assert(result, IsNil)
	assertSnapshotUnchanged(c, before, s.snapshot(c))

	// Same dialect but another migration table: same-named-table plans must
	// not cross between databases or schemas.
	otherMS := MigrationSet{TableName: "other_migrations"}
	result, err = otherMS.ExecuteFrozenPlan(context.Background(), s.Db, "sqlite3", frozenTestSource(), NewInProcessFrozenGuard(), plan)
	c.Assert(errors.Is(err, ErrFrozenPlanTarget), Equals, true)
	c.Assert(result, IsNil)
	assertSnapshotUnchanged(c, before, s.snapshot(c))

	// Building without an explicit database name is refused up front.
	_, err = s.ms.BuildFrozenPlan(s.Db, "sqlite3", frozenTestSource(), frozenTestSourceID(), FrozenPlanTarget{}, Up, 0)
	c.Assert(err, NotNil)
}

func (s *FrozenPlanSuite) TestParseRejectsMalformedArtifacts(c *C) {
	plan := s.buildAndParse(c, Up, 0)
	valid, err := plan.Marshal()
	c.Assert(err, IsNil)

	// Unknown JSON field. Build on a fresh copy so the valid backing array is
	// not mutated by append.
	unknownField := append(append([]byte{}, valid[:len(valid)-2]...), []byte("  ,\"bogus\": 1\n}\n")...)
	_, err = ParseFrozenPlan(unknownField)
	c.Assert(err, NotNil)

	// Trailing document.
	_, err = ParseFrozenPlan(append(append([]byte{}, valid...), []byte(" {}")...))
	c.Assert(err, NotNil)

	// Non-canonical (compact instead of indented) encoding.
	compact, err := json.Marshal(plan)
	c.Assert(err, IsNil)
	_, err = ParseFrozenPlan(compact)
	c.Assert(errors.Is(err, ErrFrozenPlanFormat), Equals, true)

	// Empty artifact.
	_, err = ParseFrozenPlan(nil)
	c.Assert(errors.Is(err, ErrFrozenPlanFormat), Equals, true)

	// Unknown format version.
	old := *plan
	old.FormatVersion = 99
	oldData, err := old.Marshal()
	c.Assert(err, IsNil)
	_, err = ParseFrozenPlan(oldData)
	c.Assert(errors.Is(err, ErrFrozenPlanFormat), Equals, true)

	// Duplicate migration ids.
	dup := *plan
	dup.Migrations = append(append([]FrozenPlanMigration{}, plan.Migrations...), plan.Migrations[0])
	dupData, err := dup.Marshal()
	c.Assert(err, IsNil)
	_, err = ParseFrozenPlan(dupData)
	c.Assert(errors.Is(err, ErrFrozenPlanFormat), Equals, true)

	// Tampered applied-state fingerprint.
	tamperedFP := *plan
	tamperedFP.AppliedState.Fingerprint = "00"
	tamperedData, err := tamperedFP.Marshal()
	c.Assert(err, IsNil)
	_, err = ParseFrozenPlan(tamperedData)
	c.Assert(errors.Is(err, ErrFrozenPlanFormat), Equals, true)

	// Tampered statement bytes, declared size and digest each fail integrity.
	tamperedBytes := *plan
	tamperedBytes.Migrations[0].Statements[0].Bytes = "SELECT 1"
	tamperedData, err = tamperedBytes.Marshal()
	c.Assert(err, IsNil)
	_, err = ParseFrozenPlan(tamperedData)
	c.Assert(errors.Is(err, ErrFrozenPlanHash), Equals, true)

	tamperedSize := *plan
	tamperedSize.Migrations[0].Statements[0].Size++
	tamperedData, err = tamperedSize.Marshal()
	c.Assert(err, IsNil)
	_, err = ParseFrozenPlan(tamperedData)
	c.Assert(errors.Is(err, ErrFrozenPlanHash), Equals, true)

	tamperedHash := *plan
	tamperedHash.Migrations[0].Statements[0].SHA256 = strings.Repeat("0", 64)
	tamperedData, err = tamperedHash.Marshal()
	c.Assert(err, IsNil)
	_, err = ParseFrozenPlan(tamperedData)
	c.Assert(errors.Is(err, ErrFrozenPlanHash), Equals, true)

	// A structurally valid document still parses and round-trips byte-stable.
	// Compare documents rather than the locally mutated plan struct.
	parsed, err := ParseFrozenPlan(valid)
	c.Assert(err, IsNil)
	reEncoded, err := parsed.Marshal()
	c.Assert(err, IsNil)
	c.Assert(string(reEncoded), Equals, string(valid))
}

func (s *FrozenPlanSuite) TestExecuteRejectsTamperedArtifactWithoutWrites(c *C) {
	plan := s.buildAndParse(c, Up, 0)
	plan.Migrations[0].Statements[0].SHA256 = strings.Repeat("0", 64)
	before := s.snapshot(c)

	result, err := s.ms.ExecuteFrozenPlan(context.Background(), s.Db, "sqlite3", frozenTestSource(), NewInProcessFrozenGuard(), plan)
	c.Assert(errors.Is(err, ErrFrozenPlanHash), Equals, true)
	c.Assert(result, IsNil)
	assertSnapshotUnchanged(c, before, s.snapshot(c))
}

type stubFrozenGuard struct {
	lockErr   error
	unlockErr error
	lockCalls int
}

func (g *stubFrozenGuard) Lock(_ context.Context) error {
	g.lockCalls++
	return g.lockErr
}

func (g *stubFrozenGuard) Unlock(_ context.Context) error {
	return g.unlockErr
}

func (s *FrozenPlanSuite) TestExecuteGuardRequirements(c *C) {
	plan := s.buildAndParse(c, Up, 0)
	before := s.snapshot(c)

	// Missing guard is refused before any database access.
	result, err := s.ms.ExecuteFrozenPlan(context.Background(), s.Db, "sqlite3", frozenTestSource(), nil, plan)
	c.Assert(errors.Is(err, ErrFrozenPlanGuardRequired), Equals, true)
	c.Assert(result, IsNil)
	assertSnapshotUnchanged(c, before, s.snapshot(c))

	// A guard that cannot be acquired rejects the operation.
	failingGuard := &stubFrozenGuard{lockErr: errors.New("boom")}
	result, err = s.ms.ExecuteFrozenPlan(context.Background(), s.Db, "sqlite3", frozenTestSource(), failingGuard, plan)
	c.Assert(errors.Is(err, ErrFrozenPlanGuard), Equals, true)
	c.Assert(failingGuard.lockCalls, Equals, 1)
	c.Assert(result, IsNil)
	assertSnapshotUnchanged(c, before, s.snapshot(c))

	// A guard held by another participant blocks execution until the context
	// expires; the database must remain untouched.
	held := NewInProcessFrozenGuard()
	c.Assert(held.Lock(context.Background()), IsNil)
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	result, err = s.ms.ExecuteFrozenPlan(ctx, s.Db, "sqlite3", frozenTestSource(), held, plan)
	cancel()
	c.Assert(errors.Is(err, ErrFrozenPlanGuard), Equals, true)
	c.Assert(result, IsNil)
	assertSnapshotUnchanged(c, before, s.snapshot(c))

	// Once released, the same artifact executes normally.
	c.Assert(held.Unlock(context.Background()), IsNil)
	result, err = s.ms.ExecuteFrozenPlan(context.Background(), s.Db, "sqlite3", frozenTestSource(), held, plan)
	c.Assert(err, IsNil)
	c.Assert(result.Applied, Equals, 2)
}

func (s *FrozenPlanSuite) TestExecuteReportsGuardUnlockFailure(c *C) {
	plan := s.buildAndParse(c, Up, 0)
	guard := &stubFrozenGuard{unlockErr: errors.New("unlock boom")}
	result, err := s.ms.ExecuteFrozenPlan(context.Background(), s.Db, "sqlite3", frozenTestSource(), guard, plan)
	// The work completed and committed, but the guard failure is surfaced.
	c.Assert(errors.Is(err, ErrFrozenPlanGuard), Equals, true)
	c.Assert(result, NotNil)
	c.Assert(result.Applied, Equals, 2)
	c.Assert(result.Steps[0].Status, Equals, FrozenStepCompleted)
}

func (s *FrozenPlanSuite) TestNoTransactionPartialExecutionIsUncertainAndNotRetried(c *C) {
	migrations := &MemoryMigrationSource{Migrations: []*Migration{
		{
			Id:                     "1_notx",
			DisableTransactionUp:   true,
			Up:                     []string{"CREATE TABLE no_tx (id int)", "INSERT INTO no_tx (id) VALUES (1)", "SELECT definitely_missing_column FROM no_tx"},
			Down:                   []string{"DROP TABLE no_tx"},
			DisableTransactionDown: true,
		},
		{
			Id:   "2_never",
			Up:   []string{"CREATE TABLE never_created (id int)"},
			Down: []string{"DROP TABLE never_created"},
		},
	}}

	plan, err := s.ms.BuildFrozenPlan(s.Db, "sqlite3", migrations, frozenTestSourceID(), frozenTestTarget(), Up, 0)
	c.Assert(err, IsNil)
	data, err := plan.Marshal()
	c.Assert(err, IsNil)
	parsed, err := ParseFrozenPlan(data)
	c.Assert(err, IsNil)

	result, err := s.ms.ExecuteFrozenPlan(context.Background(), s.Db, "sqlite3", migrations, NewInProcessFrozenGuard(), parsed)
	c.Assert(err, NotNil)
	c.Assert(result.Applied, Equals, 0)
	c.Assert(result.Steps, HasLen, 2)
	c.Assert(result.Steps[0].Status, Equals, FrozenStepUncertain)
	c.Assert(result.Steps[0].Err, NotNil)
	c.Assert(result.Steps[1].Status, Equals, FrozenStepNotStarted)

	// Partial autocommitted effects are preserved and precisely observable.
	var count int64
	c.Assert(s.Db.QueryRow("SELECT COUNT(*) FROM no_tx").Scan(&count), IsNil)
	c.Assert(count, Equals, int64(1))
	c.Assert(s.tablesOf(c, s.Db), DeepEquals, []string{"gorp_migrations", "no_tx"})
	c.Assert(s.recordIDs(c, s.Db), HasLen, 0)

	// Nothing is retried automatically. Reusing the stale artifact is rejected
	// because the bookkeeping table now exists; an explicit retry requires a
	// freshly rebuilt plan and fails again without duplicating the row.
	_, err = s.ms.ExecuteFrozenPlan(context.Background(), s.Db, "sqlite3", migrations, NewInProcessFrozenGuard(), parsed)
	c.Assert(errors.Is(err, ErrFrozenPlanState), Equals, true)

	rebuilt, err := s.ms.BuildFrozenPlan(s.Db, "sqlite3", migrations, frozenTestSourceID(), frozenTestTarget(), Up, 0)
	c.Assert(err, IsNil)
	rebuiltData, err := rebuilt.Marshal()
	c.Assert(err, IsNil)
	rebuiltParsed, err := ParseFrozenPlan(rebuiltData)
	c.Assert(err, IsNil)
	result2, err := s.ms.ExecuteFrozenPlan(context.Background(), s.Db, "sqlite3", migrations, NewInProcessFrozenGuard(), rebuiltParsed)
	c.Assert(err, NotNil)
	c.Assert(result2.Steps[0].Status, Equals, FrozenStepUncertain)
	c.Assert(s.Db.QueryRow("SELECT COUNT(*) FROM no_tx").Scan(&count), IsNil)
	c.Assert(count, Equals, int64(1))
}

func (s *FrozenPlanSuite) TestTransactionalFailureRollsBackAndIsNotStarted(c *C) {
	migrations := &MemoryMigrationSource{Migrations: []*Migration{
		{
			Id:   "1_ok",
			Up:   []string{"CREATE TABLE tx_people (id int)"},
			Down: []string{"DROP TABLE tx_people"},
		},
		{
			Id:   "2_fail",
			Up:   []string{"INSERT INTO tx_people (id) VALUES (1)", "SELECT definitely_missing_column FROM tx_people"},
			Down: []string{"DELETE FROM tx_people"},
		},
	}}

	plan, err := s.ms.BuildFrozenPlan(s.Db, "sqlite3", migrations, frozenTestSourceID(), frozenTestTarget(), Up, 0)
	c.Assert(err, IsNil)
	data, err := plan.Marshal()
	c.Assert(err, IsNil)
	parsed, err := ParseFrozenPlan(data)
	c.Assert(err, IsNil)

	result, err := s.ms.ExecuteFrozenPlan(context.Background(), s.Db, "sqlite3", migrations, NewInProcessFrozenGuard(), parsed)
	c.Assert(err, NotNil)
	c.Assert(result.Applied, Equals, 1)
	c.Assert(result.Steps[0].Status, Equals, FrozenStepCompleted)
	c.Assert(result.Steps[1].Status, Equals, FrozenStepNotStarted)

	// The failed transaction rolled its insert back.
	var count int64
	c.Assert(s.Db.QueryRow("SELECT COUNT(*) FROM tx_people").Scan(&count), IsNil)
	c.Assert(count, Equals, int64(0))
	// Only the completed migration got a bookkeeping record.
	c.Assert(s.recordIDs(c, s.Db), DeepEquals, []string{"1_ok"})
}

func (s *FrozenPlanSuite) TestEmptyPlanCreatesNoTable(c *C) {
	// A source with no pending migrations on a fresh database yields an empty
	// plan; executing it must not even create the migration bookkeeping table.
	emptySource := &MemoryMigrationSource{}

	freshDB, err := sql.Open("sqlite3", ":memory:")
	c.Assert(err, IsNil)
	defer func() { _ = freshDB.Close() }()

	emptyPlan, err := s.ms.BuildFrozenPlan(freshDB, "sqlite3", emptySource, frozenTestSourceID(), frozenTestTarget(), Up, 0)
	c.Assert(err, IsNil)
	c.Assert(emptyPlan.AppliedState.MigrationTableExists, Equals, false)
	c.Assert(emptyPlan.Migrations, HasLen, 0)
	data, err := emptyPlan.Marshal()
	c.Assert(err, IsNil)
	parsed, err := ParseFrozenPlan(data)
	c.Assert(err, IsNil)

	beforeTables := s.tablesOf(c, freshDB)
	result, err := s.ms.ExecuteFrozenPlan(context.Background(), freshDB, "sqlite3", emptySource, NewInProcessFrozenGuard(), parsed)
	c.Assert(err, IsNil)
	c.Assert(result.Applied, Equals, 0)
	c.Assert(s.tablesOf(c, freshDB), DeepEquals, beforeTables)
	c.Assert(beforeTables, HasLen, 0)
}

func (s *FrozenPlanSuite) tablesOf(c *C, db *sql.DB) []string {
	rows, err := db.Query("SELECT name FROM sqlite_master WHERE type='table' ORDER BY name")
	c.Assert(err, IsNil)
	defer func() { _ = rows.Close() }()
	var names []string
	for rows.Next() {
		var name string
		c.Assert(rows.Scan(&name), IsNil)
		names = append(names, name)
	}
	return names
}

func (s *FrozenPlanSuite) recordIDs(c *C, db *sql.DB) []string {
	rows, err := db.Query("SELECT id FROM gorp_migrations ORDER BY id")
	c.Assert(err, IsNil)
	defer func() { _ = rows.Close() }()
	var ids []string
	for rows.Next() {
		var id string
		c.Assert(rows.Scan(&id), IsNil)
		ids = append(ids, id)
	}
	return ids
}
