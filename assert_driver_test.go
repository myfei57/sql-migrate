package migrate

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"io"
	"sync"

	//revive:disable-next-line:blank-imports
	_ "modernc.org/sqlite"
)

// recordingDriverName is a database/sql driver wrapping the real, pure-Go
// SQLite engine (modernc.org/sqlite) while recording every interaction so
// tests can assert on the actual SQL order, transaction boundaries and
// resource release without replacing the database with a mock.
const recordingDriverName = "recording-sqlite"

type queryKind string

const (
	kindExec  queryKind = "exec"
	kindQuery queryKind = "query"
)

type queryRecord struct {
	Kind  queryKind
	Query string
}

type recorderStats struct {
	ConnOpened   int
	ConnClosed   int
	RowsOpened   int
	RowsClosed   int
	TxBegun      int
	TxCommitted  int
	TxRolledBack int
	NextCalls    int
}

// recorder collects the observable driver behavior of one migration run.
type recorder struct {
	mu sync.Mutex

	queries []queryRecord
	stats   recorderStats

	// queryHook, when set, runs before every QueryContext call on a
	// connection. Returning an error short-circuits the inner driver. The
	// context is the call context, which lets a test block until
	// cancellation and observe how the runner releases resources afterwards.
	queryHook func(ctx context.Context, query string) error
}

func (r *recorder) record(kind queryKind, query string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.queries = append(r.queries, queryRecord{Kind: kind, Query: query})
}

func (r *recorder) queriesSnapshot() []queryRecord {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]queryRecord, len(r.queries))
	copy(out, r.queries)
	return out
}

func (r *recorder) reset() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.queries = nil
	r.stats = recorderStats{}
}

func (r *recorder) snapshotStats() recorderStats {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.stats
}

// recordingDriver wraps a real driver.Driver while delegating all database
// work to it.
type recordingDriver struct {
	inner driver.Driver
	rec   *recorder
}

func (d *recordingDriver) Open(name string) (driver.Conn, error) {
	conn, err := d.inner.Open(name)
	if err != nil {
		return nil, err
	}
	d.rec.mu.Lock()
	d.rec.stats.ConnOpened++
	d.rec.mu.Unlock()
	return &recordingConn{conn: conn, rec: d.rec}, nil
}

type recordingConn struct {
	conn driver.Conn
	rec  *recorder
}

func (c *recordingConn) Prepare(query string) (driver.Stmt, error) {
	stmt, err := c.conn.Prepare(query)
	if err != nil {
		return nil, err
	}
	return &recordingStmt{stmt: stmt, rec: c.rec, query: query}, nil
}

func (c *recordingConn) PrepareContext(ctx context.Context, query string) (driver.Stmt, error) {
	preparer, ok := c.conn.(driver.ConnPrepareContext)
	if !ok {
		return c.Prepare(query)
	}
	stmt, err := preparer.PrepareContext(ctx, query)
	if err != nil {
		return nil, err
	}
	return &recordingStmt{stmt: stmt, rec: c.rec, query: query}, nil
}

func (c *recordingConn) Close() error {
	c.rec.mu.Lock()
	c.rec.stats.ConnClosed++
	c.rec.mu.Unlock()
	return c.conn.Close()
}

func (c *recordingConn) Begin() (driver.Tx, error) {
	tx, err := c.conn.Begin()
	if err != nil {
		return nil, err
	}
	c.rec.mu.Lock()
	c.rec.stats.TxBegun++
	c.rec.mu.Unlock()
	return &recordingTx{tx: tx, rec: c.rec}, nil
}

func (c *recordingConn) BeginTx(ctx context.Context, opts driver.TxOptions) (driver.Tx, error) {
	beginner, ok := c.conn.(driver.ConnBeginTx)
	if !ok {
		return c.Begin()
	}
	tx, err := beginner.BeginTx(ctx, opts)
	if err != nil {
		return nil, err
	}
	c.rec.mu.Lock()
	c.rec.stats.TxBegun++
	c.rec.mu.Unlock()
	return &recordingTx{tx: tx, rec: c.rec}, nil
}

func (c *recordingConn) Ping(ctx context.Context) error {
	if pinger, ok := c.conn.(driver.Pinger); ok {
		return pinger.Ping(ctx)
	}
	return nil
}

func (c *recordingConn) ExecContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Result, error) {
	c.rec.record(kindExec, query)
	execer, ok := c.conn.(driver.ExecerContext)
	if !ok {
		return nil, driver.ErrSkip
	}
	return execer.ExecContext(ctx, query, args)
}

func (c *recordingConn) QueryContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Rows, error) {
	if c.rec.queryHook != nil {
		if err := c.rec.queryHook(ctx, query); err != nil {
			return nil, err
		}
	}
	c.rec.record(kindQuery, query)
	queryer, ok := c.conn.(driver.QueryerContext)
	if !ok {
		return nil, driver.ErrSkip
	}
	rows, err := queryer.QueryContext(ctx, query, args)
	if err != nil {
		return nil, err
	}
	c.rec.mu.Lock()
	c.rec.stats.RowsOpened++
	c.rec.mu.Unlock()
	return &recordingRows{rows: rows, rec: c.rec}, nil
}

type recordingStmt struct {
	stmt  driver.Stmt
	rec   *recorder
	query string
}

func (s *recordingStmt) Close() error {
	return s.stmt.Close()
}

func (s *recordingStmt) NumInput() int {
	return s.stmt.NumInput()
}

func (s *recordingStmt) ColumnConverter(idx int) driver.ValueConverter {
	if converter, ok := s.stmt.(driver.ColumnConverter); ok {
		return converter.ColumnConverter(idx)
	}
	return driver.DefaultParameterConverter
}

func (s *recordingStmt) Exec(args []driver.Value) (driver.Result, error) {
	s.rec.record(kindExec, s.query)
	return s.stmt.Exec(args)
}

func (s *recordingStmt) Query(args []driver.Value) (driver.Rows, error) {
	s.rec.record(kindQuery, s.query)
	rows, err := s.stmt.Query(args)
	if err != nil {
		return nil, err
	}
	s.rec.mu.Lock()
	s.rec.stats.RowsOpened++
	s.rec.mu.Unlock()
	return &recordingRows{rows: rows, rec: s.rec}, nil
}

func (s *recordingStmt) ExecContext(ctx context.Context, args []driver.NamedValue) (driver.Result, error) {
	s.rec.record(kindExec, s.query)
	if execer, ok := s.stmt.(driver.StmtExecContext); ok {
		return execer.ExecContext(ctx, args)
	}
	values := make([]driver.Value, len(args))
	for i, arg := range args {
		values[i] = arg.Value
	}
	return s.stmt.Exec(values)
}

func (s *recordingStmt) QueryContext(ctx context.Context, args []driver.NamedValue) (driver.Rows, error) {
	s.rec.record(kindQuery, s.query)
	if queryer, ok := s.stmt.(driver.StmtQueryContext); ok {
		rows, err := queryer.QueryContext(ctx, args)
		if err != nil {
			return nil, err
		}
		s.rec.mu.Lock()
		s.rec.stats.RowsOpened++
		s.rec.mu.Unlock()
		return &recordingRows{rows: rows, rec: s.rec}, nil
	}
	values := make([]driver.Value, len(args))
	for i, arg := range args {
		values[i] = arg.Value
	}
	return s.Query(values)
}

type recordingRows struct {
	rows driver.Rows
	rec  *recorder
}

func (r *recordingRows) Columns() []string {
	return r.rows.Columns()
}

func (r *recordingRows) Close() error {
	r.rec.mu.Lock()
	r.rec.stats.RowsClosed++
	r.rec.mu.Unlock()
	return r.rows.Close()
}

func (r *recordingRows) Next(dest []driver.Value) error {
	err := r.rows.Next(dest)
	if err == nil {
		r.rec.mu.Lock()
		r.rec.stats.NextCalls++
		r.rec.mu.Unlock()
	}
	if err == io.EOF {
		return io.EOF
	}
	return err
}

type recordingTx struct {
	tx  driver.Tx
	rec *recorder
}

func (t *recordingTx) Commit() error {
	if err := t.tx.Commit(); err != nil {
		return err
	}
	t.rec.mu.Lock()
	t.rec.stats.TxCommitted++
	t.rec.mu.Unlock()
	return nil
}

func (t *recordingTx) Rollback() error {
	if err := t.tx.Rollback(); err != nil {
		return err
	}
	t.rec.mu.Lock()
	t.rec.stats.TxRolledBack++
	t.rec.mu.Unlock()
	return nil
}

// registeredRecordingDriver is the single wrapper registered with
// database/sql. Its recorder is swapped per test.
var registeredRecordingDriver *recordingDriver

func init() {
	// The Driver() of a handle opened against the package-registered
	// "sqlite" driver is the default modernc driver instance, including any
	// package-level scalar function registrations.
	bootstrap, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		panic(err)
	}
	registeredRecordingDriver = &recordingDriver{
		inner: bootstrap.Driver(),
		rec:   &recorder{},
	}
	if err := bootstrap.Close(); err != nil {
		panic(err)
	}
	sql.Register(recordingDriverName, registeredRecordingDriver)
}

// newRecordingDB opens an in-memory SQLite database through the recording
// driver. The pool is limited to one connection: an in-memory SQLite database
// is private to a connection, and a single connection also makes connection
// release assertions deterministic.
func newRecordingDB() (*sql.DB, *recorder) {
	rec := &recorder{}
	registeredRecordingDriver.rec = rec

	db, err := sql.Open(recordingDriverName, ":memory:")
	if err != nil {
		panic(err)
	}
	db.SetMaxOpenConns(1)
	return db, rec
}
