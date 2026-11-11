package migrate

import (
	"database/sql"
	"strings"
	"time"

	"github.com/go-gorp/gorp/v3"
	//revive:disable-next-line:dot-imports
	. "gopkg.in/check.v1"

	_ "modernc.org/sqlite"
)

// DependencySqliteSuite runs real SQL migrations against SQLite through the
// pure-Go modernc.org/sqlite driver, so no cgo toolchain is required.
type DependencySqliteSuite struct {
	Db    *sql.DB
	DbMap *gorp.DbMap
	ms    MigrationSet
}

var _ = Suite(&DependencySqliteSuite{})

func (s *DependencySqliteSuite) SetUpTest(c *C) {
	db, err := sql.Open("sqlite", ":memory:")
	c.Assert(err, IsNil)
	// A single connection keeps the in-memory schema stable.
	db.SetMaxOpenConns(1)

	s.Db = db
	s.DbMap = &gorp.DbMap{Db: db, Dialect: &gorp.SqliteDialect{}}
	s.ms = MigrationSet{}
}

func (s *DependencySqliteSuite) appliedIds(c *C) map[string]struct{} {
	records, err := s.ms.GetMigrationRecords(s.Db, "sqlite3")
	c.Assert(err, IsNil)
	ids := make(map[string]struct{}, len(records))
	for _, record := range records {
		ids[record.Id] = struct{}{}
	}
	return ids
}

func plannedIds(plan *TargetPlan) []string {
	ids := make([]string, len(plan.Migrations))
	for index, migration := range plan.Migrations {
		ids[index] = migration.Id
	}
	return ids
}

// branchMergeMigrations is a diamond: two branches merge in a final migration.
func branchMergeMigrations() []*Migration {
	return []*Migration{
		{
			Id:   "1_base.sql",
			Up:   []string{"CREATE TABLE widgets (id INTEGER NOT NULL PRIMARY KEY)"},
			Down: []string{"DROP TABLE widgets"},
		},
		{
			Id:      "2_alpha.sql",
			Depends: []string{"1_base.sql"},
			Up:      []string{"ALTER TABLE widgets ADD COLUMN alpha TEXT"},
			Down:    []string{"ALTER TABLE widgets DROP COLUMN alpha"},
		},
		{
			Id:      "3_bravo.sql",
			Depends: []string{"1_base.sql"},
			Up:      []string{"ALTER TABLE widgets ADD COLUMN bravo TEXT"},
			Down:    []string{"ALTER TABLE widgets DROP COLUMN bravo"},
		},
		{
			Id:      "4_merge.sql",
			Depends: []string{"2_alpha.sql", "3_bravo.sql"},
			Up:      []string{"ALTER TABLE widgets ADD COLUMN merged TEXT", "INSERT INTO widgets (id, alpha, bravo, merged) VALUES (1, 'a', 'b', 'm')"},
			Down:    []string{"DELETE FROM widgets WHERE id = 1", "ALTER TABLE widgets DROP COLUMN merged"},
		},
	}
}

func (s *DependencySqliteSuite) TestBranchMergeUpClosure(c *C) {
	source := &MemoryMigrationSource{Migrations: branchMergeMigrations()}

	plan, _, err := s.ms.PlanTargets(s.Db, "sqlite3", source, Up, []string{"4_merge.sql"}, 0)
	c.Assert(err, IsNil)
	c.Assert(plan.Complete, Equals, true)
	c.Assert(plannedIds(plan), DeepEquals, []string{
		"1_base.sql", "2_alpha.sql", "3_bravo.sql", "4_merge.sql",
	})

	applied, err := s.ms.ExecTargets(s.Db, "sqlite3", source, Up, []string{"4_merge.sql"}, 0)
	c.Assert(err, IsNil)
	c.Assert(applied, Equals, 4)

	alpha, err := s.DbMap.SelectStr("SELECT alpha FROM widgets WHERE id = 1")
	c.Assert(err, IsNil)
	c.Assert(alpha, Equals, "a")
	merged, err := s.DbMap.SelectStr("SELECT merged FROM widgets WHERE id = 1")
	c.Assert(err, IsNil)
	c.Assert(merged, Equals, "m")

	// Re-running is a no-op.
	applied, err = s.ms.ExecTargets(s.Db, "sqlite3", source, Up, []string{"4_merge.sql"}, 0)
	c.Assert(err, IsNil)
	c.Assert(applied, Equals, 0)
}

func (s *DependencySqliteSuite) TestStepwiseDependencySatisfaction(c *C) {
	migrations := branchMergeMigrations()
	source := &MemoryMigrationSource{Migrations: migrations}
	loaded, err := source.FindMigrations()
	c.Assert(err, IsNil)
	graph, err := loadMigrationGraph(loaded)
	c.Assert(err, IsNil)

	targets := []string{"4_merge.sql"}
	executedOrder := make([]string, 0)
	for {
		plan, _, err := s.ms.PlanTargets(s.Db, "sqlite3", source, Up, targets, 1)
		c.Assert(err, IsNil)
		if len(plan.Migrations) == 0 {
			c.Assert(plan.Complete, Equals, true)
			break
		}

		next := plan.Migrations[0]
		applied := s.appliedIds(c)
		for _, dependency := range graph.depends[next.Id] {
			_, ok := applied[dependency]
			c.Assert(ok, Equals, true, Commentf(
				"migration %s must not run before dependency %s is applied", next.Id, dependency))
		}

		appliedCount, err := s.ms.ExecTargets(s.Db, "sqlite3", source, Up, targets, 1)
		c.Assert(err, IsNil)
		c.Assert(appliedCount, Equals, 1)
		executedOrder = append(executedOrder, next.Id)

		if plan.Complete {
			break
		}
	}

	c.Assert(executedOrder, DeepEquals, []string{
		"1_base.sql", "2_alpha.sql", "3_bravo.sql", "4_merge.sql",
	})
}

func (s *DependencySqliteSuite) TestHoleFillsPrerequisitesBeforeLargerIds(c *C) {
	migrations := []*Migration{
		{
			Id:   "1_alpha.sql",
			Up:   []string{"CREATE TABLE alpha (id INTEGER)"},
			Down: []string{"DROP TABLE alpha"},
		},
		{
			Id:      "2_bravo.sql",
			Depends: []string{"1_alpha.sql"},
			Up:      []string{"CREATE TABLE bravo (id INTEGER)"},
			Down:    []string{"DROP TABLE bravo"},
		},
		{
			Id:      "3_charlie.sql",
			Depends: []string{"2_bravo.sql"},
			Up:      []string{"CREATE TABLE charlie (id INTEGER)"},
			Down:    []string{"DROP TABLE charlie"},
		},
		{
			Id:      "4_delta.sql",
			Depends: []string{"3_charlie.sql"},
			Up:      []string{"CREATE TABLE delta (id INTEGER)"},
			Down:    []string{"DROP TABLE delta"},
		},
	}
	source := &MemoryMigrationSource{Migrations: migrations}

	// Simulate an applied state with a hole: 3 is recorded while 1 and 2 are
	// not, even though 3 has the largest applied numeric id.
	dbMap, err := s.ms.getMigrationDbMap(s.Db, "sqlite3")
	c.Assert(err, IsNil)
	c.Assert(dbMap.Insert(&MigrationRecord{Id: "3_charlie.sql", AppliedAt: time.Now()}), IsNil)

	plan, _, err := s.ms.PlanTargets(s.Db, "sqlite3", source, Up, []string{"4_delta.sql"}, 0)
	c.Assert(err, IsNil)
	c.Assert(plan.Complete, Equals, true)
	// The missing prerequisites come first; the already recorded 3 is not rerun.
	c.Assert(plannedIds(plan), DeepEquals, []string{"1_alpha.sql", "2_bravo.sql", "4_delta.sql"})

	applied, err := s.ms.ExecTargets(s.Db, "sqlite3", source, Up, []string{"4_delta.sql"}, 0)
	c.Assert(err, IsNil)
	c.Assert(applied, Equals, 3)

	ids := s.appliedIds(c)
	c.Assert(ids, HasLen, 4)
	for _, id := range []string{"1_alpha.sql", "2_bravo.sql", "3_charlie.sql", "4_delta.sql"} {
		_, ok := ids[id]
		c.Assert(ok, Equals, true)
	}
}

func (s *DependencySqliteSuite) TestMissingDependencyRejectedAtLoad(c *C) {
	source := &MemoryMigrationSource{Migrations: []*Migration{
		{Id: "1_a.sql", Up: []string{"CREATE TABLE a (id INTEGER)"}, Down: []string{"DROP TABLE a"}},
		{Id: "2_b.sql", Depends: []string{"9_missing.sql"}, Up: []string{"SELECT 1"}},
	}}

	_, _, err := s.ms.PlanTargets(s.Db, "sqlite3", source, Up, []string{"2_b.sql"}, 0)
	dependencyError := dependencyErrorFrom(c, err)
	c.Assert(dependencyError.Reason, Equals, DependencyMissing)
	c.Assert(strings.Contains(err.Error(), "9_missing.sql"), Equals, true)

	// Nothing was executed.
	c.Assert(s.appliedIds(c), HasLen, 0)

	// Unknown targets are rejected as well.
	source = &MemoryMigrationSource{Migrations: []*Migration{
		{Id: "1_a.sql", Up: []string{"SELECT 1"}},
	}}
	_, _, err = s.ms.PlanTargets(s.Db, "sqlite3", source, Up, []string{"4_unknown.sql"}, 0)
	dependencyError = dependencyErrorFrom(c, err)
	c.Assert(dependencyError.Reason, Equals, DependencyTarget)
}

func (s *DependencySqliteSuite) TestCycleRejectedWithActualPath(c *C) {
	source := &MemoryMigrationSource{Migrations: []*Migration{
		{Id: "1_a.sql", Depends: []string{"3_c.sql"}, Up: []string{"SELECT 1"}},
		{Id: "2_b.sql", Depends: []string{"1_a.sql"}, Up: []string{"SELECT 1"}},
		{Id: "3_c.sql", Depends: []string{"2_b.sql"}, Up: []string{"SELECT 1"}},
	}}

	_, _, err := s.ms.PlanTargets(s.Db, "sqlite3", source, Up, []string{"3_c.sql"}, 0)
	dependencyError := dependencyErrorFrom(c, err)
	c.Assert(dependencyError.Reason, Equals, DependencyCycle)
	c.Assert(err.Error(), Equals,
		"cyclic migration dependency detected: 1_a.sql -> 3_c.sql -> 2_b.sql -> 1_a.sql")
	c.Assert(s.appliedIds(c), HasLen, 0)
}

func (s *DependencySqliteSuite) TestDownRejectedWhileAppliedDependentStays(c *C) {
	migrations := []*Migration{
		{
			Id:   "1_a.sql",
			Up:   []string{"CREATE TABLE a (id INTEGER)"},
			Down: []string{"DROP TABLE a"},
		},
		{
			Id:      "2_b.sql",
			Depends: []string{"1_a.sql"},
			Up:      []string{"CREATE TABLE b (id INTEGER)"},
			Down:    []string{"DROP TABLE b"},
		},
		{
			Id:      "3_c.sql",
			Depends: []string{"2_b.sql"},
			Up:      []string{"CREATE TABLE c (id INTEGER)"},
			Down:    []string{"DROP TABLE c"},
		},
	}
	source := &MemoryMigrationSource{Migrations: migrations}

	applied, err := s.ms.ExecTargets(s.Db, "sqlite3", source, Up, []string{"3_c.sql"}, 0)
	c.Assert(err, IsNil)
	c.Assert(applied, Equals, 3)

	// Rolling back 2 while 3 stays applied must be rejected; dependents are
	// never rolled back implicitly and no record changes.
	_, _, err = s.ms.PlanTargets(s.Db, "sqlite3", source, Down, []string{"2_b.sql"}, 0)
	dependencyError := dependencyErrorFrom(c, err)
	c.Assert(dependencyError.Reason, Equals, DependencyBlocked)
	c.Assert(dependencyError.MigrationId, Equals, "2_b.sql")
	c.Assert(dependencyError.DependencyId, Equals, "3_c.sql")
	c.Assert(dependencyError.Path, DeepEquals, []string{"3_c.sql", "2_b.sql"})
	c.Assert(s.appliedIds(c), HasLen, 3)

	// Same guard for the base of the chain.
	_, _, err = s.ms.PlanTargets(s.Db, "sqlite3", source, Down, []string{"1_a.sql"}, 0)
	dependencyError = dependencyErrorFrom(c, err)
	c.Assert(dependencyError.DependencyId, Equals, "2_b.sql")

	// Removing only the leaf is allowed and touches exactly that migration.
	plan, _, err := s.ms.PlanTargets(s.Db, "sqlite3", source, Down, []string{"3_c.sql"}, 0)
	c.Assert(err, IsNil)
	c.Assert(plan.Complete, Equals, true)
	c.Assert(plannedIds(plan), DeepEquals, []string{"3_c.sql"})
	down, err := s.ms.ExecTargets(s.Db, "sqlite3", source, Down, []string{"3_c.sql"}, 0)
	c.Assert(err, IsNil)
	c.Assert(down, Equals, 1)
	c.Assert(s.appliedIds(c), DeepEquals, map[string]struct{}{"1_a.sql": {}, "2_b.sql": {}})

	// Selecting the full remaining chain is legal and ordered dependents-first.
	plan, _, err = s.ms.PlanTargets(s.Db, "sqlite3", source, Down, []string{"1_a.sql", "2_b.sql"}, 0)
	c.Assert(err, IsNil)
	c.Assert(plannedIds(plan), DeepEquals, []string{"2_b.sql", "1_a.sql"})
	down, err = s.ms.ExecTargets(s.Db, "sqlite3", source, Down, []string{"1_a.sql", "2_b.sql"}, 0)
	c.Assert(err, IsNil)
	c.Assert(down, Equals, 2)
	c.Assert(s.appliedIds(c), HasLen, 0)
}

func (s *DependencySqliteSuite) TestDownRejectsUnappliedTarget(c *C) {
	source := &MemoryMigrationSource{Migrations: []*Migration{
		{Id: "1_a.sql", Up: []string{"SELECT 1"}, Down: []string{"SELECT 1"}},
	}}

	_, _, err := s.ms.PlanTargets(s.Db, "sqlite3", source, Down, []string{"1_a.sql"}, 0)
	dependencyError := dependencyErrorFrom(c, err)
	c.Assert(dependencyError.Reason, Equals, DependencyNotApplied)
}

func (s *DependencySqliteSuite) TestMaxReturnsCompleteExecutablePrefix(c *C) {
	migrations := []*Migration{
		{Id: "1_a.sql", Up: []string{"CREATE TABLE t1 (id INTEGER)"}, Down: []string{"DROP TABLE t1"}},
		{Id: "2_b.sql", Depends: []string{"1_a.sql"}, Up: []string{"CREATE TABLE t2 (id INTEGER)"}, Down: []string{"DROP TABLE t2"}},
		{Id: "3_c.sql", Depends: []string{"2_b.sql"}, Up: []string{"CREATE TABLE t3 (id INTEGER)"}, Down: []string{"DROP TABLE t3"}},
		{Id: "4_d.sql", Depends: []string{"3_c.sql"}, Up: []string{"CREATE TABLE t4 (id INTEGER)"}, Down: []string{"DROP TABLE t4"}},
	}
	source := &MemoryMigrationSource{Migrations: migrations}

	plan, _, err := s.ms.PlanTargets(s.Db, "sqlite3", source, Up, []string{"4_d.sql"}, 2)
	c.Assert(err, IsNil)
	c.Assert(plan.Complete, Equals, false)
	c.Assert(plannedIds(plan), DeepEquals, []string{"1_a.sql", "2_b.sql"})

	applied, err := s.ms.ExecTargets(s.Db, "sqlite3", source, Up, []string{"4_d.sql"}, 2)
	c.Assert(err, IsNil)
	c.Assert(applied, Equals, 2)
	c.Assert(s.appliedIds(c), DeepEquals, map[string]struct{}{"1_a.sql": {}, "2_b.sql": {}})

	// The second round reaches the target with the remaining closure.
	plan, _, err = s.ms.PlanTargets(s.Db, "sqlite3", source, Up, []string{"4_d.sql"}, 0)
	c.Assert(err, IsNil)
	c.Assert(plan.Complete, Equals, true)
	c.Assert(plannedIds(plan), DeepEquals, []string{"3_c.sql", "4_d.sql"})
	applied, err = s.ms.ExecTargets(s.Db, "sqlite3", source, Up, []string{"4_d.sql"}, 0)
	c.Assert(err, IsNil)
	c.Assert(applied, Equals, 2)
	c.Assert(s.appliedIds(c), HasLen, 4)
}

func (s *DependencySqliteSuite) TestExecMaxAndVersionOnDependencySource(c *C) {
	migrations := []*Migration{
		{Id: "1_a.sql", Up: []string{"CREATE TABLE v1 (id INTEGER)"}, Down: []string{"DROP TABLE v1"}},
		{Id: "2_b.sql", Depends: []string{"1_a.sql"}, Up: []string{"CREATE TABLE v2 (id INTEGER)"}, Down: []string{"DROP TABLE v2"}},
		{Id: "3_c.sql", Depends: []string{"2_b.sql"}, Up: []string{"CREATE TABLE v3 (id INTEGER)"}, Down: []string{"DROP TABLE v3"}},
	}
	source := &MemoryMigrationSource{Migrations: migrations}

	// Max cuts the topological sequence as a legal prefix.
	applied, err := s.ms.ExecMax(s.Db, "sqlite3", source, Up, 2)
	c.Assert(err, IsNil)
	c.Assert(applied, Equals, 2)
	c.Assert(s.appliedIds(c), DeepEquals, map[string]struct{}{"1_a.sql": {}, "2_b.sql": {}})

	// Version targeting keeps the full closure before the target.
	applied, err = s.ms.ExecVersion(s.Db, "sqlite3", source, Up, 3)
	c.Assert(err, IsNil)
	c.Assert(applied, Equals, 1)
	c.Assert(s.appliedIds(c), HasLen, 3)

	// Down one step removes the leaf only, never a node still depended on.
	planned, _, err := s.ms.PlanMigration(s.Db, "sqlite3", source, Down, 1)
	c.Assert(err, IsNil)
	c.Assert(planned, HasLen, 1)
	c.Assert(planned[0].Id, Equals, "3_c.sql")
}

func (s *DependencySqliteSuite) TestTransactionAndRecordBehaviorPreserved(c *C) {
	migrations := []*Migration{
		{
			Id:                     "1_a.sql",
			Up:                     []string{"CREATE TABLE n1 (id INTEGER)"},
			Down:                   []string{"DROP TABLE n1"},
			DisableTransactionUp:   true,
			DisableTransactionDown: true,
		},
		{
			Id:      "2_b.sql",
			Depends: []string{"1_a.sql"},
			Up:      []string{"CREATE TABLE n2 (id INTEGER)"},
			Down:    []string{"DROP TABLE n2"},
		},
	}
	source := &MemoryMigrationSource{Migrations: migrations}

	plan, _, err := s.ms.PlanTargets(s.Db, "sqlite3", source, Up, []string{"2_b.sql"}, 0)
	c.Assert(err, IsNil)
	c.Assert(plan.Migrations[0].DisableTransaction, Equals, true)
	c.Assert(plan.Migrations[1].DisableTransaction, Equals, false)

	_, err = s.ms.ExecTargets(s.Db, "sqlite3", source, Up, []string{"2_b.sql"}, 0)
	c.Assert(err, IsNil)

	down, _, err := s.ms.PlanTargets(s.Db, "sqlite3", source, Down, []string{"2_b.sql", "1_a.sql"}, 0)
	c.Assert(err, IsNil)
	c.Assert(down.Migrations[0].Id, Equals, "2_b.sql")
	c.Assert(down.Migrations[0].DisableTransaction, Equals, false)
	c.Assert(down.Migrations[1].Id, Equals, "1_a.sql")
	c.Assert(down.Migrations[1].DisableTransaction, Equals, true)
}

func (s *DependencySqliteSuite) TestLegacySourceKeepsBytesAndOrder(c *C) {
	// Exact replica of the historical hole scenario, which must remain on the
	// legacy filename based code path.
	up := "SELECT 0"
	down := "SELECT 1"
	source := &MemoryMigrationSource{Migrations: []*Migration{
		{Id: "1", Up: []string{up}, Down: []string{down}},
		{Id: "3", Up: []string{up}, Down: []string{down}},
	}}
	applied, err := s.ms.Exec(s.Db, "sqlite3", source, Up)
	c.Assert(err, IsNil)
	c.Assert(applied, Equals, 2)

	source.Migrations = append(source.Migrations,
		&Migration{Id: "2", Up: []string{up}, Down: []string{down}},
		&Migration{Id: "4", Up: []string{up}, Down: []string{down}},
		&Migration{Id: "5", Up: []string{up}, Down: []string{down}},
	)

	planned, _, err := s.ms.PlanMigration(s.Db, "sqlite3", source, Up, 0)
	c.Assert(err, IsNil)
	c.Assert(planned, HasLen, 3)
	c.Assert(planned[0].Id, Equals, "2")
	c.Assert(planned[0].Queries, DeepEquals, []string{up})
	c.Assert(planned[1].Id, Equals, "4")
	c.Assert(planned[2].Id, Equals, "5")

	planned, _, err = s.ms.PlanMigration(s.Db, "sqlite3", source, Down, 1)
	c.Assert(err, IsNil)
	c.Assert(planned, HasLen, 2)
	c.Assert(planned[0].Id, Equals, "2")
	c.Assert(planned[0].Queries, DeepEquals, []string{up})
	c.Assert(planned[1].Id, Equals, "3")
	c.Assert(planned[1].Queries, DeepEquals, []string{down})
}

func (s *DependencySqliteSuite) TestLegacyFileSourcePlannedBytesUnchanged(c *C) {
	source := &FileMigrationSource{Dir: "test-migrations"}
	loaded, err := source.FindMigrations()
	c.Assert(err, IsNil)

	planned, _, err := s.ms.PlanMigration(s.Db, "sqlite3", source, Up, 0)
	c.Assert(err, IsNil)
	c.Assert(planned, HasLen, 2)
	for index, entry := range planned {
		c.Assert(entry.Id, Equals, loaded[index].Id)
		c.Assert(entry.Queries, DeepEquals, loaded[index].Up)
		c.Assert(entry.DisableTransaction, Equals, loaded[index].DisableTransactionUp)
	}

	applied, err := s.ms.Exec(s.Db, "sqlite3", source, Up)
	c.Assert(err, IsNil)
	c.Assert(applied, Equals, 2)
	id, err := s.DbMap.SelectInt("SELECT id FROM people")
	c.Assert(err, IsNil)
	c.Assert(id, Equals, int64(1))
}

func (*DependencySqliteSuite) TestParseMigrationDependsDirective(c *C) {
	script := strings.NewReader(`-- +migrate Depends 1_a.sql 2_b.sql
-- +migrate Up
CREATE TABLE a (id INTEGER);

-- +migrate Down
DROP TABLE a;
`)
	migration, err := ParseMigration("3_c.sql", script)
	c.Assert(err, IsNil)
	c.Assert(migration.Depends, DeepEquals, []string{"1_a.sql", "2_b.sql"})
	c.Assert(migration.Up, HasLen, 1)

	// The loaded metadata is isolated from the parser result.
	migration.Depends[0] = "changed"
	parsedAgain, err := ParseMigration("3_c.sql", strings.NewReader(`-- +migrate Depends 1_a.sql
-- +migrate Up
SELECT 1;
`))
	c.Assert(err, IsNil)
	c.Assert(parsedAgain.Depends, DeepEquals, []string{"1_a.sql"})
}
