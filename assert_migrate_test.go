package migrate

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"strings"
	"sync/atomic"
	"time"

	"github.com/rubenv/sql-migrate/sqlparse"
	//revive:disable-next-line:dot-imports
	. "gopkg.in/check.v1"

	sqlitelib "modernc.org/sqlite"
)

// sideEffectCalls counts invocations of the assert_side_effect SQLite
// function, proving that assertion queries may invoke functions with side
// effects (the runner must not assume checks are read-only).
var sideEffectCalls int64

func init() {
	sqlitelib.MustRegisterScalarFunction("assert_side_effect", 0,
		func(_ *sqlitelib.FunctionContext, _ []driver.Value) (driver.Value, error) {
			atomic.AddInt64(&sideEffectCalls, 1)
			return int64(1), nil
		})
}

type AssertMigrateSuite struct {
	Db  *sql.DB
	Rec *recorder
}

var _ = Suite(&AssertMigrateSuite{})

func (s *AssertMigrateSuite) SetUpTest(c *C) {
	s.Db, s.Rec = newRecordingDB()
	atomic.StoreInt64(&sideEffectCalls, 0)
}

func (s *AssertMigrateSuite) TearDownTest(c *C) {
	c.Assert(s.Db.Close(), IsNil)
}

// normalizeWS collapses runs of whitespace so SQL recorded by the driver can
// be compared robustly.
func normalizeWS(query string) string {
	return strings.Join(strings.Fields(query), " ")
}

func (s *AssertMigrateSuite) migrationRecordCount(c *C) int {
	var count int
	err := s.Db.QueryRow("SELECT COUNT(*) FROM gorp_migrations").Scan(&count)
	c.Assert(err, IsNil)
	return count
}

func (s *AssertMigrateSuite) tableExists(c *C, table string) bool {
	var found int
	err := s.Db.QueryRow(
		"SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = ?", table).Scan(&found)
	c.Assert(err, IsNil)
	return found == 1
}

func (s *AssertMigrateSuite) assertIdle(c *C) {
	c.Assert(s.Db.Stats().InUse, Equals, 0, Commentf("connection was not returned to the pool"))
	stats := s.Rec.snapshotStats()
	c.Assert(stats.RowsOpened, Equals, stats.RowsClosed,
		Commentf("every opened rows must be closed"))
}

func (s *AssertMigrateSuite) queryIndex(substr string) int {
	needle := strings.ToLower(substr)
	for i, record := range s.Rec.queriesSnapshot() {
		if strings.Contains(strings.ToLower(normalizeWS(record.Query)), needle) {
			return i
		}
	}
	return -1
}

// migrationQueries drops the bookkeeping SQL issued by the planner (creating
// and reading the gorp_migrations table) so assertions only see the queries
// produced by the migration itself.
func (s *AssertMigrateSuite) migrationQueries() []queryRecord {
	var out []queryRecord
	for _, record := range s.Rec.queriesSnapshot() {
		if strings.Contains(record.Query, "gorp_migrations") {
			continue
		}
		out = append(out, record)
	}
	return out
}

func (s *AssertMigrateSuite) migrationQueryIndex(substr string) int {
	needle := strings.ToLower(substr)
	for i, record := range s.migrationQueries() {
		if strings.Contains(strings.ToLower(normalizeWS(record.Query)), needle) {
			return i
		}
	}
	return -1
}

// recordStatementIndex locates the migration-record write (insert on Up,
// delete on Down). Dialects quote the table name differently, hence matching
// on the verb and table name instead of a fixed spelling.
func (s *AssertMigrateSuite) recordStatementIndex(verb string) int {
	needle := strings.ToLower(verb)
	for i, record := range s.Rec.queriesSnapshot() {
		query := strings.ToLower(normalizeWS(record.Query))
		if record.Kind == kindExec &&
			strings.Contains(query, "gorp_migrations") &&
			strings.Contains(query, needle) {
			return i
		}
	}
	return -1
}

func assertAssertionError(c *C, err error, id, name string, phase AssertionPhase) *AssertionError {
	c.Assert(err, NotNil)
	var assertionErr *AssertionError
	c.Assert(errors.As(err, &assertionErr), Equals, true, Commentf("got %v", err))
	c.Assert(assertionErr.Migration.Id, Equals, id)
	c.Assert(assertionErr.Name, Equals, name)
	c.Assert(assertionErr.Phase, Equals, phase)
	c.Assert(strings.Contains(err.Error(), id), Equals, true)
	c.Assert(strings.Contains(err.Error(), name), Equals, true)
	return assertionErr
}

// TestAssertionHappyPathOrdering covers the successful order: before check,
// migration statements, after check, success record, all in one transaction.
func (s *AssertMigrateSuite) TestAssertionHappyPathOrdering(c *C) {
	migrations := &MemoryMigrationSource{
		Migrations: []*Migration{{
			Id:   "1_assert.sql",
			Up:   []string{"CREATE TABLE people (id int)"},
			Down: []string{"DROP TABLE people"},
			AssertBeforeUp: []sqlparse.Assertion{{
				Name:      "before_marker",
				Statement: "SELECT 1 AS before_marker",
			}},
			AssertAfterUp: []sqlparse.Assertion{{
				Name:      "after_marker",
				Statement: "SELECT 1 AS after_marker",
			}},
		}},
	}

	n, err := Exec(s.Db, "sqlite3", migrations, Up)
	c.Assert(err, IsNil)
	c.Assert(n, Equals, 1)

	// Assert on the recorded SQL before helper queries of the test itself.
	queries := s.migrationQueries()
	idxBefore := s.migrationQueryIndex("before_marker")
	idxCreate := s.migrationQueryIndex("CREATE TABLE people")
	idxAfter := s.migrationQueryIndex("after_marker")
	c.Assert(idxBefore, Equals, 0)
	c.Assert(idxBefore < idxCreate, Equals, true)
	c.Assert(idxCreate < idxAfter, Equals, true)
	c.Assert(queries[idxBefore].Kind, Equals, kindQuery)
	c.Assert(queries[idxAfter].Kind, Equals, kindQuery)

	// The success record is written last, after the after-assertion.
	idxRecord := s.recordStatementIndex("insert")
	c.Assert(idxRecord >= 0, Equals, true)
	c.Assert(idxRecord > s.queryIndex("after_marker"), Equals, true)

	c.Assert(s.tableExists(c, "people"), Equals, true)
	c.Assert(s.migrationRecordCount(c), Equals, 1)

	stats := s.Rec.snapshotStats()
	c.Assert(stats.TxBegun, Equals, 1)
	c.Assert(stats.TxCommitted, Equals, 1)
	c.Assert(stats.TxRolledBack, Equals, 0)
	s.assertIdle(c)
}

// TestAssertBeforeBlocksMigration verifies a failing before-assertion stops
// everything: no DDL, no success record, the transaction rolls back.
func (s *AssertMigrateSuite) TestAssertBeforeBlocksMigration(c *C) {
	migrations := &MemoryMigrationSource{
		Migrations: []*Migration{{
			Id:   "2_blocked.sql",
			Up:   []string{"CREATE TABLE people (id int)"},
			Down: []string{"DROP TABLE people"},
			AssertBeforeUp: []sqlparse.Assertion{{
				Name:      "gate",
				Statement: "SELECT 0 AS gate",
			}},
		}},
	}

	n, err := Exec(s.Db, "sqlite3", migrations, Up)
	c.Assert(n, Equals, 0)

	// Inspect the recorded SQL before any test helper queries pollute the log.
	c.Assert(s.migrationQueryIndex("CREATE TABLE people"), Equals, -1)
	c.Assert(s.recordStatementIndex("insert"), Equals, -1)
	c.Assert(s.migrationQueries(), HasLen, 1)
	c.Assert(s.migrationQueries()[0].Kind, Equals, kindQuery)

	assertAssertionError(c, err, "2_blocked.sql", "gate", AssertBefore)
	c.Assert(s.tableExists(c, "people"), Equals, false)
	c.Assert(s.migrationRecordCount(c), Equals, 0)

	stats := s.Rec.snapshotStats()
	c.Assert(stats.TxRolledBack, Equals, 1)
	c.Assert(stats.TxCommitted, Equals, 0)
	s.assertIdle(c)
}

// TestMultipleAssertionsRunInOrder verifies source-order evaluation and
// fail-fast behavior across several assertions of both phases.
func (s *AssertMigrateSuite) TestMultipleAssertionsRunInOrder(c *C) {
	migrations := &MemoryMigrationSource{
		Migrations: []*Migration{{
			Id: "2b_multi.sql",
			Up: []string{"CREATE TABLE people (id int)"},
			AssertBeforeUp: []sqlparse.Assertion{
				{Name: "first", Statement: "SELECT 1 AS first"},
				{Name: "second", Statement: "SELECT 0 AS second"},
			},
		}},
	}

	n, err := Exec(s.Db, "sqlite3", migrations, Up)
	c.Assert(n, Equals, 0)
	assertAssertionError(c, err, "2b_multi.sql", "second", AssertBefore)

	// Both checks ran in order, but no migration statement did.
	c.Assert(s.migrationQueryIndex("first") >= 0, Equals, true)
	c.Assert(s.migrationQueryIndex("second") > s.migrationQueryIndex("first"), Equals, true)
	c.Assert(s.migrationQueryIndex("CREATE TABLE people"), Equals, -1)
	c.Assert(s.recordStatementIndex("insert"), Equals, -1)
	s.assertIdle(c)

	// On the success path, both after-checks run in order before the record.
	s.Rec.reset()
	migrations = &MemoryMigrationSource{
		Migrations: []*Migration{{
			Id: "2c_multi_ok.sql",
			Up: []string{"CREATE TABLE places (id int)"},
			AssertAfterUp: []sqlparse.Assertion{
				{Name: "a_one", Statement: "SELECT 1 AS a_one"},
				{Name: "a_two", Statement: "SELECT 1 AS a_two"},
			},
		}},
	}
	n, err = Exec(s.Db, "sqlite3", migrations, Up)
	c.Assert(err, IsNil)
	c.Assert(n, Equals, 1)
	idxOne := s.migrationQueryIndex("a_one")
	idxTwo := s.migrationQueryIndex("a_two")
	c.Assert(idxOne >= 0 && idxTwo > idxOne, Equals, true)
	c.Assert(s.recordStatementIndex("insert") > s.queryIndex("a_two"), Equals, true)
	s.assertIdle(c)
}

// TestAssertionResultShapes enumerates every invalid result shape.
func (s *AssertMigrateSuite) TestAssertionResultShapes(c *C) {
	cases := []struct {
		label     string
		statement string
	}{
		{"boolean false", "SELECT 0"},
		{"null value", "SELECT NULL"},
		{"no rows", "SELECT 1 WHERE 0"},
		{"multiple rows", "SELECT 1 UNION ALL SELECT 1 UNION ALL SELECT 1 UNION ALL SELECT 1 UNION ALL SELECT 1"},
		{"multiple columns", "SELECT 1, 2"},
		{"query error", "SELECT no_such_column FROM no_such_table"},
	}

	for _, tc := range cases {
		s.Rec.reset()
		migrations := &MemoryMigrationSource{
			Migrations: []*Migration{{
				Id:   "3_shapes.sql",
				Up:   []string{"CREATE TABLE people (id int)"},
				Down: []string{"DROP TABLE people"},
				AssertBeforeUp: []sqlparse.Assertion{{
					Name:      "shape",
					Statement: tc.statement,
				}},
			}},
		}

		n, err := Exec(s.Db, "sqlite3", migrations, Up)
		c.Assert(n, Equals, 0, Commentf(tc.label))
		assertAssertionError(c, err, "3_shapes.sql", "shape", AssertBefore)
		c.Assert(s.tableExists(c, "people"), Equals, false, Commentf(tc.label))
		c.Assert(s.migrationRecordCount(c), Equals, 0, Commentf(tc.label))
		s.assertIdle(c)
	}

	// The multi-row check must stop reading after observing the second row:
	// result consumption is capped.
	s.Rec.reset()
	migrations := &MemoryMigrationSource{
		Migrations: []*Migration{{
			Id: "3b_rows.sql",
			Up: []string{"CREATE TABLE people (id int)"},
			AssertBeforeUp: []sqlparse.Assertion{{
				Name:      "many",
				Statement: "SELECT 1 UNION ALL SELECT 2 UNION ALL SELECT 3",
			}},
		}},
	}
	_, err := Exec(s.Db, "sqlite3", migrations, Up)
	c.Assert(err, NotNil)
	c.Assert(s.Rec.snapshotStats().NextCalls, Equals, 2)
	s.assertIdle(c)
}

// TestAssertAfterFailureRollsBack verifies transactional after-failure
// semantics: statements and record write are all rolled back together.
func (s *AssertMigrateSuite) TestAssertAfterFailureRollsBack(c *C) {
	migrations := &MemoryMigrationSource{
		Migrations: []*Migration{{
			Id: "4_rollback.sql",
			Up: []string{
				"CREATE TABLE people (id int)",
				"INSERT INTO people (id) VALUES (1)",
			},
			Down: []string{"DROP TABLE people"},
			AssertAfterUp: []sqlparse.Assertion{{
				Name:      "post",
				Statement: "SELECT 0 AS post",
			}},
		}},
	}

	n, err := Exec(s.Db, "sqlite3", migrations, Up)
	c.Assert(n, Equals, 0)

	idxCreate := s.migrationQueryIndex("CREATE TABLE people")
	idxInsert := s.migrationQueryIndex("INSERT INTO people")
	idxAfter := s.migrationQueryIndex("post")
	c.Assert(idxCreate >= 0 && idxInsert > idxCreate && idxAfter > idxInsert, Equals, true)
	c.Assert(s.recordStatementIndex("insert"), Equals, -1)

	assertionErr := assertAssertionError(c, err, "4_rollback.sql", "post", AssertAfter)
	c.Assert(assertionErr.Partial, Equals, false)

	c.Assert(s.tableExists(c, "people"), Equals, false)
	c.Assert(s.migrationRecordCount(c), Equals, 0)

	stats := s.Rec.snapshotStats()
	c.Assert(stats.TxRolledBack, Equals, 1)
	c.Assert(stats.TxCommitted, Equals, 0)
	s.assertIdle(c)
}

// TestNotransactionAfterFailureIsPartial verifies that a notransaction
// migration reports committed changes honestly and writes no success record.
func (s *AssertMigrateSuite) TestNotransactionAfterFailureIsPartial(c *C) {
	script := `-- +migrate Up notransaction
CREATE TABLE people (id int);
INSERT INTO people (id) VALUES (1);
-- +migrate AssertAfterBegin post
SELECT 0 AS post;
-- +migrate AssertAfterEnd

-- +migrate Down
DROP TABLE people;
`
	migration, err := ParseMigration("5_partial.sql", strings.NewReader(script))
	c.Assert(err, IsNil)
	c.Assert(migration.DisableTransactionUp, Equals, true)

	n, err := Exec(s.Db, "sqlite3", &MemoryMigrationSource{Migrations: []*Migration{migration}}, Up)
	c.Assert(n, Equals, 0)
	assertionErr := assertAssertionError(c, err, "5_partial.sql", "post", AssertAfter)
	c.Assert(assertionErr.Partial, Equals, true)
	msg := err.Error()
	c.Assert(strings.Contains(strings.ToLower(msg), "already applied"), Equals, true)
	c.Assert(strings.Contains(strings.ToLower(msg), "not rolled back"), Equals, true)
	c.Assert(strings.Contains(strings.ToLower(msg), "no success record"), Equals, true)
	c.Assert(strings.Contains(strings.ToLower(msg), "manually"), Equals, true)

	// The DDL/DML really happened and no migration record was written.
	c.Assert(s.tableExists(c, "people"), Equals, true)
	var rows int
	c.Assert(s.Db.QueryRow("SELECT COUNT(*) FROM people").Scan(&rows), IsNil)
	c.Assert(rows, Equals, 1)
	c.Assert(s.migrationRecordCount(c), Equals, 0)

	stats := s.Rec.snapshotStats()
	c.Assert(stats.TxBegun, Equals, 0)
	c.Assert(stats.TxCommitted, Equals, 0)
	c.Assert(stats.TxRolledBack, Equals, 0)
	s.assertIdle(c)
}

// TestNotransactionBeforeFailureTouchesNothing verifies a before-failure in
// notransaction mode runs no statements at all.
func (s *AssertMigrateSuite) TestNotransactionBeforeFailureTouchesNothing(c *C) {
	script := `-- +migrate Up notransaction
-- +migrate AssertBeforeBegin gate
SELECT 0 AS gate;
-- +migrate AssertBeforeEnd
CREATE TABLE people (id int);
`
	migration, err := ParseMigration("6_notx_before.sql", strings.NewReader(script))
	c.Assert(err, IsNil)

	n, err := Exec(s.Db, "sqlite3", &MemoryMigrationSource{Migrations: []*Migration{migration}}, Up)
	c.Assert(n, Equals, 0)
	assertionErr := assertAssertionError(c, err, "6_notx_before.sql", "gate", AssertBefore)
	c.Assert(assertionErr.Partial, Equals, false)
	c.Assert(s.tableExists(c, "people"), Equals, false)
	c.Assert(s.migrationRecordCount(c), Equals, 0)
	c.Assert(s.migrationQueryIndex("CREATE TABLE"), Equals, -1)
	s.assertIdle(c)
}

// TestOnlyExecutedDirectionRunsAssertions ensures skipped directions and
// already-applied migrations never trigger assertion queries.
func (s *AssertMigrateSuite) TestOnlyExecutedDirectionRunsAssertions(c *C) {
	migrations := &MemoryMigrationSource{
		Migrations: []*Migration{{
			Id:   "7_dir.sql",
			Up:   []string{"CREATE TABLE people (id int)"},
			Down: []string{"DROP TABLE people"},
			AssertBeforeUp: []sqlparse.Assertion{{
				Name: "up_only", Statement: "SELECT 1 AS up_only",
			}},
			AssertBeforeDown: []sqlparse.Assertion{{
				Name: "down_only", Statement: "SELECT 1 AS down_only",
			}},
		}},
	}

	n, err := Exec(s.Db, "sqlite3", migrations, Up)
	c.Assert(err, IsNil)
	c.Assert(n, Equals, 1)
	c.Assert(s.migrationQueryIndex("up_only") >= 0, Equals, true)
	c.Assert(s.migrationQueryIndex("down_only"), Equals, -1)

	// Re-running applies nothing and must not evaluate any assertion.
	s.Rec.reset()
	n, err = Exec(s.Db, "sqlite3", migrations, Up)
	c.Assert(err, IsNil)
	c.Assert(n, Equals, 0)
	c.Assert(s.migrationQueries(), HasLen, 0)

	// Going down runs only the down assertions and removes the record last.
	s.Rec.reset()
	n, err = ExecMax(s.Db, "sqlite3", migrations, Down, 1)
	c.Assert(err, IsNil)
	c.Assert(n, Equals, 1)
	c.Assert(s.migrationQueryIndex("down_only") >= 0, Equals, true)
	c.Assert(s.migrationQueryIndex("up_only"), Equals, -1)
	idxDownCheck := s.migrationQueryIndex("down_only")
	idxDrop := s.migrationQueryIndex("DROP TABLE people")
	c.Assert(idxDownCheck < idxDrop, Equals, true)
	idxRecord := s.recordStatementIndex("delete")
	c.Assert(idxRecord >= 0, Equals, true)
	c.Assert(idxRecord > s.queryIndex("DROP TABLE people"), Equals, true)
	c.Assert(s.tableExists(c, "people"), Equals, false)
	s.assertIdle(c)
}

// TestSkipDoesNotRunAssertions verifies SkipMax only writes records.
func (s *AssertMigrateSuite) TestSkipDoesNotRunAssertions(c *C) {
	migrations := &MemoryMigrationSource{
		Migrations: []*Migration{{
			Id:   "8_skip.sql",
			Up:   []string{"CREATE TABLE people (id int)"},
			Down: []string{"DROP TABLE people"},
			AssertBeforeUp: []sqlparse.Assertion{{
				Name: "never_run", Statement: "SELECT 1 AS never_run",
			}},
			AssertAfterUp: []sqlparse.Assertion{{
				Name: "never_run_either", Statement: "SELECT 1 AS never_run_either",
			}},
		}},
	}

	n, err := SkipMax(s.Db, "sqlite3", migrations, Up, 0)
	c.Assert(err, IsNil)
	c.Assert(n, Equals, 1)
	c.Assert(s.migrationRecordCount(c), Equals, 1)
	c.Assert(s.tableExists(c, "people"), Equals, false)
	c.Assert(s.migrationQueryIndex("never_run"), Equals, -1)
	c.Assert(s.migrationQueryIndex("never_run_either"), Equals, -1)
	c.Assert(s.recordStatementIndex("insert") >= 0, Equals, true)
	s.assertIdle(c)
}

// TestAssertionContextCanceled verifies cancellation surfaces as an
// AssertionError and all resources are released.
func (s *AssertMigrateSuite) TestAssertionContextCanceled(c *C) {
	// Deterministic gate: the wrapper blocks inside the assertion query until
	// the context is canceled, then returns ctx.Err().
	s.Rec.queryHook = func(ctx context.Context, query string) error {
		if strings.Contains(query, "cancel_gate") {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(5 * time.Second):
				c.Fatal("gate was not canceled in time")
			}
		}
		return nil
	}

	migrations := &MemoryMigrationSource{
		Migrations: []*Migration{{
			Id: "9_cancel.sql",
			Up: []string{"CREATE TABLE people (id int)"},
			AssertBeforeUp: []sqlparse.Assertion{{
				Name: "gate", Statement: "SELECT 'cancel_gate'",
			}},
		}},
	}

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(100 * time.Millisecond)
		cancel()
	}()

	start := time.Now()
	n, err := ExecContext(ctx, s.Db, "sqlite3", migrations, Up)
	c.Assert(time.Since(start) < 2*time.Second, Equals, true)
	c.Assert(n, Equals, 0)
	assertionErr := assertAssertionError(c, err, "9_cancel.sql", "gate", AssertBefore)
	c.Assert(errors.Is(assertionErr, context.Canceled), Equals, true)
	c.Assert(s.tableExists(c, "people"), Equals, false)
	c.Assert(s.migrationRecordCount(c), Equals, 0)
	s.assertIdle(c)

	// An already-canceled context fails immediately as well.
	s.Rec.reset()
	preCanceled, cancelNow := context.WithCancel(context.Background())
	cancelNow()
	_, err = ExecContext(preCanceled, s.Db, "sqlite3", migrations, Up)
	c.Assert(err, NotNil)
	c.Assert(errors.Is(err, context.Canceled), Equals, true)
	s.assertIdle(c)
}

// TestAssertionTimeout verifies the per-assertion time limit.
func (s *AssertMigrateSuite) TestAssertionTimeout(c *C) {
	originalTimeout := AssertionTimeout
	AssertionTimeout = 50 * time.Millisecond
	defer func() { AssertionTimeout = originalTimeout }()

	s.Rec.queryHook = func(ctx context.Context, query string) error {
		if strings.Contains(query, "timeout_gate") {
			<-ctx.Done()
			return ctx.Err()
		}
		return nil
	}

	migrations := &MemoryMigrationSource{
		Migrations: []*Migration{{
			Id: "10_timeout.sql",
			Up: []string{"CREATE TABLE people (id int)"},
			AssertBeforeUp: []sqlparse.Assertion{{
				Name: "slow", Statement: "SELECT 'timeout_gate'",
			}},
		}},
	}

	start := time.Now()
	n, err := ExecContext(context.Background(), s.Db, "sqlite3", migrations, Up)
	elapsed := time.Since(start)
	c.Assert(err, NotNil)
	c.Assert(n, Equals, 0)
	c.Assert(errors.Is(err, context.DeadlineExceeded), Equals, true)
	c.Assert(elapsed < 2*time.Second, Equals, true)
	s.assertIdle(c)
}

// TestSideEffectingChecksAreAllowed verifies checks are trusted SQL that may
// invoke side-effecting functions and even run DML (RETURNING), rather than
// being rejected by any keyword-based read-only heuristic.
func (s *AssertMigrateSuite) TestSideEffectingChecksAreAllowed(c *C) {
	migrations := &MemoryMigrationSource{
		Migrations: []*Migration{{
			Id: "11_sideeffect.sql",
			Up: []string{
				"CREATE TABLE side_log (id int)",
				"CREATE TABLE people (id int)",
			},
			AssertBeforeUp: []sqlparse.Assertion{{
				Name: "udf", Statement: "SELECT assert_side_effect()",
			}},
			AssertAfterUp: []sqlparse.Assertion{{
				// DML inside a check: real side effect, returns one true row.
				Name: "dml", Statement: "INSERT INTO side_log (id) VALUES (42) RETURNING 1",
			}},
		}},
	}

	n, err := Exec(s.Db, "sqlite3", migrations, Up)
	c.Assert(err, IsNil)
	c.Assert(n, Equals, 1)
	c.Assert(atomic.LoadInt64(&sideEffectCalls), Equals, int64(1))
	var logged int
	c.Assert(s.Db.QueryRow("SELECT COUNT(*) FROM side_log WHERE id = 42").Scan(&logged), IsNil)
	c.Assert(logged, Equals, 1)
	c.Assert(s.migrationRecordCount(c), Equals, 1)
	s.assertIdle(c)
}

// TestPseudoAnnotationsInExecutedSQL runs a full parse-and-execute cycle with
// fake directive lines hidden in strings and block comments.
func (s *AssertMigrateSuite) TestPseudoAnnotationsInExecutedSQL(c *C) {
	script := `-- +migrate Up
CREATE TABLE notes (id int, body text);
INSERT INTO notes (id, body) VALUES (1, '
-- +migrate AssertBeforeBegin fake
-- +migrate AssertBeforeEnd
') /*
-- +migrate AssertAfterBegin fake
*/;
-- +migrate AssertAfterBegin real_check
SELECT 1 AS real_check;
-- +migrate AssertAfterEnd

-- +migrate Down
DROP TABLE notes;
`
	migration, err := ParseMigration("12_pseudo.sql", strings.NewReader(script))
	c.Assert(err, IsNil)
	c.Assert(migration.AssertBeforeUp, HasLen, 0)
	c.Assert(migration.AssertAfterUp, HasLen, 1)
	c.Assert(migration.AssertAfterUp[0].Name, Equals, "real_check")

	n, err := Exec(s.Db, "sqlite3", &MemoryMigrationSource{Migrations: []*Migration{migration}}, Up)
	c.Assert(err, IsNil)
	c.Assert(n, Equals, 1)
	c.Assert(s.migrationRecordCount(c), Equals, 1)
	var body string
	c.Assert(s.Db.QueryRow("SELECT body FROM notes WHERE id = 1").Scan(&body), IsNil)
	c.Assert(strings.Contains(body, "AssertBeforeBegin fake"), Equals, true)
	s.assertIdle(c)
}

// TestParsedMigrationEndToEnd exercises assertions coming from the file
// parser through a full Up run, covering multi-statement transactional flow.
func (s *AssertMigrateSuite) TestParsedMigrationEndToEnd(c *C) {
	script := `-- +migrate Up
-- +migrate AssertBeforeBegin table_absent
SELECT NOT EXISTS (SELECT 1 FROM sqlite_master WHERE name = 'people');
-- +migrate AssertBeforeEnd
CREATE TABLE people (id int);
INSERT INTO people (id) VALUES (1);
-- +migrate AssertAfterBegin row_present
SELECT EXISTS (SELECT 1 FROM people WHERE id = 1);
-- +migrate AssertAfterEnd

-- +migrate Down
DELETE FROM people;
DROP TABLE people;
`
	migration, err := ParseMigration("13_file.sql", strings.NewReader(script))
	c.Assert(err, IsNil)

	n, err := Exec(s.Db, "sqlite3", &MemoryMigrationSource{Migrations: []*Migration{migration}}, Up)
	c.Assert(err, IsNil)
	c.Assert(n, Equals, 1)

	idxBefore := s.migrationQueryIndex("NOT EXISTS")
	idxInsert := s.migrationQueryIndex("INSERT INTO people")
	idxAfter := s.migrationQueryIndex("EXISTS (SELECT 1 FROM people")
	c.Assert(idxBefore >= 0 && idxBefore < idxInsert && idxInsert < idxAfter, Equals, true)
	idxRecord := s.recordStatementIndex("insert")
	c.Assert(idxRecord >= 0 && idxRecord > s.queryIndex("EXISTS (SELECT 1 FROM people"), Equals, true)

	c.Assert(s.migrationRecordCount(c), Equals, 1)
	var rows int
	c.Assert(s.Db.QueryRow("SELECT COUNT(*) FROM people").Scan(&rows), IsNil)
	c.Assert(rows, Equals, 1)
	s.assertIdle(c)
}
