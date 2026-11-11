package migrate

import (
	"bytes"
	"database/sql"
	"errors"
	"io/fs"
	"reflect"
	"sort"
	"strings"
	"sync"
	"testing/fstest"

	"github.com/go-gorp/gorp/v3"
	//revive:disable-next-line:dot-imports
	. "gopkg.in/check.v1"
	_ "modernc.org/sqlite"

	"github.com/rubenv/sql-migrate/sqlparse"
)

type FragmentMigrateSuite struct {
	Db    *sql.DB
	DbMap *gorp.DbMap
}

var _ = Suite(&FragmentMigrateSuite{})

func (s *FragmentMigrateSuite) SetUpTest(c *C) {
	// The pure-Go modernc driver registers as "sqlite"; gorp's sqlite dialect
	// is driver-agnostic for the SQL emitted by the migrator.
	db, err := sql.Open("sqlite", ":memory:")
	c.Assert(err, IsNil)
	// A single connection keeps the in-memory database shared for the test.
	db.SetMaxOpenConns(1)
	s.Db = db
	s.DbMap = &gorp.DbMap{Db: db, Dialect: &gorp.SqliteDialect{}}
}

func fragmentTestFS() fstest.MapFS {
	return fstest.MapFS{
		"migrations/1_people.sql": {Data: []byte(`-- +migrate Up
-- +migrate IncludeFragment shared/create_people.sqlinc
INSERT INTO people VALUES (2, 'root');

-- +migrate Down
-- +migrate IncludeFragment shared/drop_people.sqlinc
`)},
		"migrations/2_more.sql": {Data: []byte(`-- +migrate Up
INSERT INTO people VALUES (3, 'second');

-- +migrate Down
DELETE FROM people WHERE id = 3;
`)},
		"fragments/shared/create_people.sqlinc": {Data: []byte(`CREATE TABLE people (id int, name text);
INSERT INTO people VALUES (1, 'fragment');
`)},
		"fragments/shared/drop_people.sqlinc": {Data: []byte(`DROP TABLE people;
`)},
		"fragments/log/d.sqlinc": {Data: []byte(`INSERT INTO log VALUES ('d');
`)},
		"fragments/log/a.sqlinc": {Data: []byte(`-- +migrate IncludeFragment log/d.sqlinc
INSERT INTO log VALUES ('a');
`)},
		"fragments/log/b.sqlinc": {Data: []byte(`-- +migrate IncludeFragment log/d.sqlinc
INSERT INTO log VALUES ('b');
`)},
		"fragments/log/marker.sqlinc": {Data: []byte(`INSERT INTO log VALUES ('marker');
`)},
	}
}

func newFragmentSource(fsys fs.FS) *FragmentFileSystemMigrationSource {
	return &FragmentFileSystemMigrationSource{
		FileSystem:   fsys,
		Root:         "migrations",
		FragmentRoot: "fragments",
	}
}

func (s *FragmentMigrateSuite) userTables(c *C) []string {
	var tables []string
	rows, err := s.Db.Query("SELECT name FROM sqlite_master WHERE type='table' ORDER BY name")
	c.Assert(err, IsNil)
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var name string
		c.Assert(rows.Scan(&name), IsNil)
		if name != "gorp_migrations" && !strings.HasPrefix(name, "sqlite_") {
			tables = append(tables, name)
		}
	}
	c.Assert(rows.Err(), IsNil)
	return tables
}

func (s *FragmentMigrateSuite) assertZeroMigrationDDL(c *C) {
	c.Assert(s.userTables(c), HasLen, 0)
	count, err := s.DbMap.SelectInt("SELECT COUNT(*) FROM gorp_migrations")
	c.Assert(err, IsNil)
	c.Assert(count, Equals, int64(0))
}

func (s *FragmentMigrateSuite) TestIncludeEndToEndAndRecords(c *C) {
	source := newFragmentSource(fragmentTestFS())

	n, err := Exec(s.Db, "sqlite3", source, Up)
	c.Assert(err, IsNil)
	c.Assert(n, Equals, 2)

	// Fragment statement runs first, then the root statement.
	var ids []int64
	_, err = s.DbMap.Select(&ids, "SELECT id FROM people ORDER BY id")
	c.Assert(err, IsNil)
	c.Assert(ids, DeepEquals, []int64{1, 2, 3})

	// Only root migrations are recorded: no .sqlinc fragment ever becomes a
	// MigrationRecord.
	records, err := GetMigrationRecords(s.Db, "sqlite3")
	c.Assert(err, IsNil)
	c.Assert(records, HasLen, 2)
	recordIds := []string{records[0].Id, records[1].Id}
	sort.Strings(recordIds)
	c.Assert(recordIds, DeepEquals, []string{"1_people.sql", "2_more.sql"})

	// Down applies the fragment DROP through the same root migration record.
	n, err = Exec(s.Db, "sqlite3", source, Down)
	c.Assert(err, IsNil)
	c.Assert(n, Equals, 2)
	_, err = s.DbMap.Exec("SELECT * FROM people")
	c.Assert(err, NotNil)
}

func (s *FragmentMigrateSuite) TestDiamondIncludeOrder(c *C) {
	fsys := fstest.MapFS{
		"migrations/1_diamond.sql": {Data: []byte(`-- +migrate Up
CREATE TABLE log (msg text);
-- +migrate IncludeFragment log/a.sqlinc
-- +migrate IncludeFragment log/b.sqlinc

-- +migrate Down
DROP TABLE log;
`)},
		"fragments/log/d.sqlinc": fragmentTestFS()["fragments/log/d.sqlinc"],
		"fragments/log/a.sqlinc": fragmentTestFS()["fragments/log/a.sqlinc"],
		"fragments/log/b.sqlinc": fragmentTestFS()["fragments/log/b.sqlinc"],
	}
	source := newFragmentSource(fsys)

	n, err := Exec(s.Db, "sqlite3", source, Up)
	c.Assert(err, IsNil)
	c.Assert(n, Equals, 1)

	var messages []string
	rows, err := s.Db.Query("SELECT msg FROM log ORDER BY rowid")
	c.Assert(err, IsNil)
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var msg string
		c.Assert(rows.Scan(&msg), IsNil)
		messages = append(messages, msg)
	}
	c.Assert(messages, DeepEquals, []string{"d", "a", "d", "b"})

	// The shared fragment d was expanded twice with independent occurrences.
	migrations, err := source.FindMigrations()
	c.Assert(err, IsNil)
	var diamond *Migration
	for _, migration := range migrations {
		if migration.Id == "1_diamond.sql" {
			diamond = migration
		}
	}
	c.Assert(diamond, NotNil)
	c.Assert(diamond.Up, HasLen, 5)
	// Statement 0 is the root CREATE TABLE; d is reached at indices 1 and 3.
	c.Assert(diamond.UpSources[0].Chain, HasLen, 0)
	c.Assert(diamond.UpSources[1].Chain, HasLen, 2)
	c.Assert(diamond.UpSources[3].Chain, HasLen, 2)
	c.Assert(diamond.UpSources[1].Chain[1].Path, Equals, "fragments/log/d.sqlinc")
	c.Assert(diamond.UpSources[3].Chain[1].Path, Equals, "fragments/log/d.sqlinc")
}

func (s *FragmentMigrateSuite) TestRepeatedInclude(c *C) {
	fsys := fstest.MapFS{
		"migrations/1_repeat.sql": {Data: []byte(`-- +migrate Up
CREATE TABLE log (msg text);
-- +migrate IncludeFragment log/marker.sqlinc
-- +migrate IncludeFragment log/marker.sqlinc
-- +migrate IncludeFragment log/marker.sqlinc

-- +migrate Down
DROP TABLE log;
`)},
		"fragments/log/marker.sqlinc": fragmentTestFS()["fragments/log/marker.sqlinc"],
	}
	source := newFragmentSource(fsys)

	n, err := Exec(s.Db, "sqlite3", source, Up)
	c.Assert(err, IsNil)
	c.Assert(n, Equals, 1)
	count, err := s.DbMap.SelectInt("SELECT COUNT(*) FROM log")
	c.Assert(err, IsNil)
	c.Assert(count, Equals, int64(3))
}

func (s *FragmentMigrateSuite) TestCycleRejectedBeforeExecution(c *C) {
	fsys := fstest.MapFS{
		"migrations/1_cycle.sql": {Data: []byte("-- +migrate Up\n-- +migrate IncludeFragment a.sqlinc\n")},
		"fragments/a.sqlinc":     {Data: []byte("-- +migrate IncludeFragment b.sqlinc\nSELECT 1;\n")},
		"fragments/b.sqlinc":     {Data: []byte("-- +migrate IncludeFragment a.sqlinc\nSELECT 2;\n")},
	}
	_, err := Exec(s.Db, "sqlite3", newFragmentSource(fsys), Up)
	c.Assert(err, NotNil)
	c.Assert(strings.Contains(err.Error(), "include cycle"), Equals, true)
	c.Assert(strings.Contains(err.Error(), "fragments/a.sqlinc"), Equals, true)
	s.assertZeroMigrationDDL(c)
}

func (s *FragmentMigrateSuite) TestMissingFragmentRejected(c *C) {
	fsys := fstest.MapFS{
		"migrations/1_missing.sql": {Data: []byte("-- +migrate Up\n-- +migrate IncludeFragment gone.sqlinc\n")},
	}
	_, err := Exec(s.Db, "sqlite3", newFragmentSource(fsys), Up)
	c.Assert(err, NotNil)
	c.Assert(strings.Contains(err.Error(), "gone.sqlinc"), Equals, true)
	s.assertZeroMigrationDDL(c)
}

func (s *FragmentMigrateSuite) TestUnsafeFragmentPathsRejected(c *C) {
	badRefs := []string{
		"../outside.sqlinc",
		"/absolute.sqlinc",
		`windows\escape.sqlinc`,
		"plain.txt",
		"sub/../../outside.sqlinc",
	}
	for _, ref := range badRefs {
		fsys := fstest.MapFS{
			"migrations/1_bad.sql": {Data: []byte("-- +migrate Up\n-- +migrate IncludeFragment " + ref + "\n")},
			"outside.sqlinc":       {Data: []byte("SELECT 1;\n")},
		}
		_, err := Exec(s.Db, "sqlite3", newFragmentSource(fsys), Up)
		c.Assert(err, NotNil, Commentf("ref %q should be rejected", ref))
		s.assertZeroMigrationDDL(c)
	}
}

func (s *FragmentMigrateSuite) TestDepthLimitRejected(c *C) {
	fsys := fstest.MapFS{
		"migrations/1_depth.sql": {Data: []byte("-- +migrate Up\n-- +migrate IncludeFragment a.sqlinc\n")},
		"fragments/a.sqlinc":     {Data: []byte("-- +migrate IncludeFragment b.sqlinc\nSELECT 1;\n")},
		"fragments/b.sqlinc":     {Data: []byte("-- +migrate IncludeFragment c.sqlinc\nSELECT 2;\n")},
		"fragments/c.sqlinc":     {Data: []byte("SELECT 3;\n")},
	}
	source := newFragmentSource(fsys)
	source.MaxIncludeDepth = 2
	_, err := Exec(s.Db, "sqlite3", source, Up)
	c.Assert(err, NotNil)
	c.Assert(strings.Contains(err.Error(), "exceeds the limit"), Equals, true)
	s.assertZeroMigrationDDL(c)
}

func (s *FragmentMigrateSuite) TestByteLimitRejected(c *C) {
	fsys := fstest.MapFS{
		"migrations/1_bytes.sql": {Data: []byte("-- +migrate Up\n-- +migrate IncludeFragment big.sqlinc\n")},
		"fragments/big.sqlinc":   {Data: []byte("SELECT '01234567890123456789';\n")},
	}
	source := newFragmentSource(fsys)
	source.MaxIncludeBytes = 8
	_, err := Exec(s.Db, "sqlite3", source, Up)
	c.Assert(err, NotNil)
	c.Assert(strings.Contains(err.Error(), "cumulative fragment size"), Equals, true)
	s.assertZeroMigrationDDL(c)
}

func (s *FragmentMigrateSuite) TestFragmentWithDirectionRejected(c *C) {
	fsys := fstest.MapFS{
		"migrations/1_badfrag.sql": {Data: []byte("-- +migrate Up\n-- +migrate IncludeFragment bad.sqlinc\n")},
		"fragments/bad.sqlinc":     {Data: []byte("-- +migrate Down\nDROP TABLE x;\n")},
	}
	_, err := Exec(s.Db, "sqlite3", newFragmentSource(fsys), Up)
	c.Assert(err, NotNil)
	c.Assert(strings.Contains(err.Error(), "fragments must not contain Up or Down"), Equals, true)
	s.assertZeroMigrationDDL(c)
}

func (s *FragmentMigrateSuite) TestLiteralPseudoDirectivesAreIgnored(c *C) {
	fsys := fstest.MapFS{
		"migrations/1_literals.sql": {Data: []byte(`-- +migrate Up
CREATE TABLE notes (body text);
INSERT INTO notes VALUES ('line1
-- +migrate IncludeFragment does_not_exist.sqlinc
line2');
/* plain block comment
-- +migrate IncludeFragment does_not_exist.sqlinc
*/
INSERT INTO notes VALUES ('after comment');

-- +migrate Down
DROP TABLE notes;
`)},
	}
	n, err := Exec(s.Db, "sqlite3", newFragmentSource(fsys), Up)
	c.Assert(err, IsNil)
	c.Assert(n, Equals, 1)
	count, err := s.DbMap.SelectInt("SELECT COUNT(*) FROM notes")
	c.Assert(err, IsNil)
	c.Assert(count, Equals, int64(2))
}

func (s *FragmentMigrateSuite) TestProcedureBodyWithPseudoDirective(c *C) {
	fsys := fstest.MapFS{
		"migrations/1_trigger.sql": {Data: []byte(`-- +migrate Up
CREATE TABLE log (msg text);
CREATE TABLE audit (msg text);
-- +migrate StatementBegin
CREATE TRIGGER log_trig AFTER INSERT ON log
BEGIN
-- +migrate IncludeFragment does_not_exist.sqlinc
  INSERT INTO audit VALUES ('triggered');
END;
-- +migrate StatementEnd
INSERT INTO log VALUES ('hi');

-- +migrate Down
DROP TABLE log;
DROP TABLE audit;
`)},
	}
	n, err := Exec(s.Db, "sqlite3", newFragmentSource(fsys), Up)
	c.Assert(err, IsNil)
	c.Assert(n, Equals, 1)
	count, err := s.DbMap.SelectInt("SELECT COUNT(*) FROM audit")
	c.Assert(err, IsNil)
	c.Assert(count, Equals, int64(1))
}

// driftingFS serves original content on the first content read of a file and
// changed content on every subsequent read, emulating a source that changes
// mid-load. Directory access and metadata calls are delegated unchanged.
type driftingFS struct {
	inner   fs.FS
	mu      sync.Mutex
	reads   map[string]int
	changed map[string][]byte
}

func (d *driftingFS) Open(name string) (fs.File, error) {
	return d.inner.Open(name)
}

func (d *driftingFS) ReadFile(name string) ([]byte, error) {
	d.mu.Lock()
	d.reads[name]++
	serveChanged := d.reads[name] > 1
	d.mu.Unlock()

	if serveChanged {
		if data, ok := d.changed[name]; ok {
			return data, nil
		}
	}
	return fs.ReadFile(d.inner, name)
}

func (s *FragmentMigrateSuite) TestSnapshotDriftOnRepeatedRead(c *C) {
	inner := fstest.MapFS{
		"migrations/1_drift.sql":  {Data: []byte("-- +migrate Up\nCREATE TABLE log (msg text);\n-- +migrate IncludeFragment marker.sqlinc\n-- +migrate IncludeFragment marker.sqlinc\n")},
		"fragments/marker.sqlinc": {Data: []byte("INSERT INTO log VALUES ('marker');\n")},
	}
	fsys := &driftingFS{
		inner:   inner,
		reads:   map[string]int{},
		changed: map[string][]byte{"fragments/marker.sqlinc": []byte("INSERT INTO log VALUES ('changed');\n")},
	}
	_, err := Exec(s.Db, "sqlite3", newFragmentSource(fsys), Up)
	c.Assert(err, NotNil)
	c.Assert(AsSnapshotChangedError(err), Equals, true)
	s.assertZeroMigrationDDL(c)
}

func (s *FragmentMigrateSuite) TestSnapshotDriftOnFinalVerification(c *C) {
	inner := fstest.MapFS{
		"migrations/1_drift.sql":  {Data: []byte("-- +migrate Up\n-- +migrate IncludeFragment marker.sqlinc\n")},
		"fragments/marker.sqlinc": {Data: []byte("INSERT INTO log VALUES ('marker');\n")},
	}
	fsys := &driftingFS{
		inner:   inner,
		reads:   map[string]int{},
		changed: map[string][]byte{"fragments/marker.sqlinc": []byte("INSERT INTO log VALUES ('changed');\n")},
	}
	_, err := Exec(s.Db, "sqlite3", newFragmentSource(fsys), Up)
	c.Assert(err, NotNil)
	c.Assert(AsSnapshotChangedError(err), Equals, true)
	s.assertZeroMigrationDDL(c)
}

func (*FragmentMigrateSuite) TestLineSeparatorNeverModified(c *C) {
	original := sqlparse.LineSeparator
	sqlparse.LineSeparator = "GO"
	defer func() { sqlparse.LineSeparator = original }()

	fsys := fragmentTestFS()
	source := newFragmentSource(fsys)
	_, err := source.FindMigrations()
	c.Assert(err, IsNil)
	c.Assert(sqlparse.LineSeparator, Equals, "GO")

	// A failing precheck load must not touch the global either.
	bad := fstest.MapFS{
		"migrations/1_cycle.sql": {Data: []byte("-- +migrate Up\n-- +migrate IncludeFragment a.sqlinc\n")},
		"fragments/a.sqlinc":     {Data: []byte("-- +migrate IncludeFragment b.sqlinc\n")},
		"fragments/b.sqlinc":     {Data: []byte("-- +migrate IncludeFragment a.sqlinc\n")},
	}
	_, err = newFragmentSource(bad).FindMigrations()
	c.Assert(err, NotNil)
	c.Assert(sqlparse.LineSeparator, Equals, "GO")
}

func (*FragmentMigrateSuite) TestLoadsAreDeterministic(c *C) {
	source := newFragmentSource(fragmentTestFS())
	first, err := source.FindMigrations()
	c.Assert(err, IsNil)
	second, err := source.FindMigrations()
	c.Assert(err, IsNil)
	c.Assert(reflect.DeepEqual(first, second), Equals, true)
}

func (s *FragmentMigrateSuite) TestExecutionErrorCarriesFragmentLocation(c *C) {
	fsys := fstest.MapFS{
		"migrations/1_broken.sql": {Data: []byte("-- +migrate Up\n-- +migrate IncludeFragment broken.sqlinc\n")},
		"fragments/broken.sqlinc": {Data: []byte("CREATE TABLE oops (;\n")},
	}
	_, err := Exec(s.Db, "sqlite3", newFragmentSource(fsys), Up)
	c.Assert(err, NotNil)

	var statementError *StatementError
	c.Assert(errors.As(err, &statementError), Equals, true)
	c.Assert(statementError.Source.Ref.File, Equals, "fragments/broken.sqlinc")
	c.Assert(statementError.Source.Ref.Line, Equals, 1)
	c.Assert(statementError.Source.Chain, HasLen, 1)
	c.Assert(statementError.Source.Chain[0].File, Equals, "1_broken.sql")
	// The transaction error still identifies the root migration.
	c.Assert(strings.Contains(err.Error(), "1_broken.sql"), Equals, true)
	s.assertZeroMigrationDDL(c)
}

func (*FragmentMigrateSuite) TestLegacyRouteWithoutIncludes(c *C) {
	fsys := fstest.MapFS{
		"migrations/1_plain.sql": {Data: []byte("-- +migrate Up\nCREATE TABLE plain (id int);\n\n-- +migrate Down\nDROP TABLE plain;\n")},
	}
	migrations, err := newFragmentSource(fsys).FindMigrations()
	c.Assert(err, IsNil)
	c.Assert(migrations, HasLen, 1)
	c.Assert(migrations[0].UpSources, IsNil)
	c.Assert(migrations[0].DownSources, IsNil)
	c.Assert(strings.TrimSpace(migrations[0].Up[0]), Equals, "CREATE TABLE plain (id int);")

	// The output matches the legacy parser byte for byte.
	legacy, err := ParseMigration("1_plain.sql", bytes.NewReader(fsys["migrations/1_plain.sql"].Data))
	c.Assert(err, IsNil)
	c.Assert(migrations[0].Up, DeepEquals, legacy.Up)
	c.Assert(migrations[0].Down, DeepEquals, legacy.Down)
}

func (*FragmentMigrateSuite) TestOnlySqlFilesAtRootAreMigrations(c *C) {
	fsys := fstest.MapFS{
		"migrations/1_real.sql":    {Data: []byte("-- +migrate Up\nSELECT 1;\n")},
		"migrations/helper.sqlinc": {Data: []byte("SELECT 2;\n")},
		"fragments/stray.sql":      {Data: []byte("SELECT 3;\n")},
	}
	migrations, err := newFragmentSource(fsys).FindMigrations()
	c.Assert(err, IsNil)
	c.Assert(migrations, HasLen, 1)
	c.Assert(migrations[0].Id, Equals, "1_real.sql")
}

func (s *FragmentMigrateSuite) TestNoTransactionFlagFromRoot(c *C) {
	fsys := fstest.MapFS{
		"migrations/1_notx.sql": {Data: []byte("-- +migrate Up notransaction\n-- +migrate IncludeFragment stmt.sqlinc\n")},
		"fragments/stmt.sqlinc": {Data: []byte("CREATE TABLE notx (id int);\n")},
	}
	migrations, err := newFragmentSource(fsys).FindMigrations()
	c.Assert(err, IsNil)
	c.Assert(migrations[0].DisableTransactionUp, Equals, true)

	n, err := Exec(s.Db, "sqlite3", newFragmentSource(fsys), Up)
	c.Assert(err, IsNil)
	c.Assert(n, Equals, 1)
	c.Assert(s.userTables(c), DeepEquals, []string{"notx"})
}
