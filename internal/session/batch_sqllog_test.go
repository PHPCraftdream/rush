// Driver-level SQL capture for the plan/statement-count tests: a
// driver.Connector that wraps every modernc sqlite connection and records each
// ExecContext/QueryContext statement (with its bound args) before delegating.
package session

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"io"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"
	"modernc.org/sqlite"
)

type batchSQLLogEntry struct {
	Query string
	Args  []driver.NamedValue
}

type batchSQLLog struct {
	mu          sync.Mutex
	entries     []batchSQLLogEntry
	invocations map[string]int
	Intercept   func(query string, invocation int) bool
}

func (l *batchSQLLog) add(query string, args []driver.NamedValue) {
	copied := make([]driver.NamedValue, len(args))
	copy(copied, args)
	l.mu.Lock()
	defer l.mu.Unlock()
	l.entries = append(l.entries, batchSQLLogEntry{Query: query, Args: copied})
}

// shouldIntercept counts the query's nth execution (first = 1) and consults
// Intercept; nil Intercept never intercepts.
func (l *batchSQLLog) shouldIntercept(query string) bool {
	l.mu.Lock()
	if l.invocations == nil {
		l.invocations = make(map[string]int)
	}
	l.invocations[query]++
	n := l.invocations[query]
	l.mu.Unlock()
	return l.Intercept != nil && l.Intercept(query, n)
}

// batchSQLEmptyRows satisfies driver.Rows with zero columns and no rows.
type batchSQLEmptyRows struct{}

func (batchSQLEmptyRows) Columns() []string              { return nil }
func (batchSQLEmptyRows) Close() error                   { return nil }
func (batchSQLEmptyRows) Next(dest []driver.Value) error { return io.EOF }

func (l *batchSQLLog) snapshot() []batchSQLLogEntry {
	l.mu.Lock()
	defer l.mu.Unlock()
	out := make([]batchSQLLogEntry, len(l.entries))
	copy(out, l.entries)
	return out
}

// batchSQLLogBatchNodes returns the captured statements touching batch_nodes.
func batchSQLLogBatchNodes(l *batchSQLLog) []batchSQLLogEntry {
	var out []batchSQLLogEntry
	for _, entry := range l.snapshot() {
		if strings.Contains(entry.Query, "batch_nodes") {
			out = append(out, entry)
		}
	}
	return out
}

// batchSQLLogCountContaining counts captured statements containing ALL needles.
func batchSQLLogCountContaining(l *batchSQLLog, needles ...string) int {
	count := 0
	for _, entry := range l.snapshot() {
		all := true
		for _, needle := range needles {
			if !strings.Contains(entry.Query, needle) {
				all = false
				break
			}
		}
		if all {
			count++
		}
	}
	return count
}

type batchSQLLogConn struct {
	driver.Conn
	log *batchSQLLog
}

// BeginTx forwards driver.ConnBeginTx: database/sql refuses read-only
// transactions on a conn that does not implement it.
func (c *batchSQLLogConn) BeginTx(ctx context.Context, opts driver.TxOptions) (driver.Tx, error) {
	if bt, ok := c.Conn.(driver.ConnBeginTx); ok {
		return bt.BeginTx(ctx, opts)
	}
	return c.Conn.Begin()
}

func (c *batchSQLLogConn) ExecContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Result, error) {
	c.log.add(query, args)
	execer, ok := c.Conn.(driver.ExecerContext)
	if !ok {
		return nil, driver.ErrSkip
	}
	return execer.ExecContext(ctx, query, args)
}

func (c *batchSQLLogConn) QueryContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Rows, error) {
	c.log.add(query, args)
	if c.log.shouldIntercept(query) {
		return &batchSQLEmptyRows{}, nil
	}
	if queryer, ok := c.Conn.(driver.QueryerContext); ok {
		return queryer.QueryContext(ctx, query, args)
	}
	stmt, err := c.Conn.Prepare(query)
	if err != nil {
		return nil, err
	}
	rows, err := stmt.Query(namedValuesToValues(args))
	if err != nil {
		_ = stmt.Close()
		return nil, err
	}
	return &batchSQLLogRows{Rows: rows, stmt: stmt}, nil
}

func namedValuesToValues(args []driver.NamedValue) []driver.Value {
	out := make([]driver.Value, len(args))
	for i, arg := range args {
		out[i] = arg.Value
	}
	return out
}

// batchSQLLogRows keeps the prepared statement alive until the caller closes
// the rows (Prepare-fallback path only).
type batchSQLLogRows struct {
	driver.Rows
	stmt driver.Stmt
}

func (r *batchSQLLogRows) Close() error {
	err := r.Rows.Close()
	_ = r.stmt.Close()
	return err
}

type batchSQLLogConnector struct {
	dsn string
	log *batchSQLLog
}

func (c *batchSQLLogConnector) Connect(ctx context.Context) (driver.Conn, error) {
	conn, err := (&sqlite.Driver{}).Open(c.dsn)
	if err != nil {
		return nil, err
	}
	return &batchSQLLogConn{Conn: conn, log: c.log}, nil
}

func (c *batchSQLLogConnector) Driver() driver.Driver {
	return batchSQLLogDriver{log: c.log}
}

type batchSQLLogDriver struct {
	log *batchSQLLog
}

func (d batchSQLLogDriver) Open(name string) (driver.Conn, error) {
	conn, err := (&sqlite.Driver{}).Open(name)
	if err != nil {
		return nil, err
	}
	return &batchSQLLogConn{Conn: conn, log: d.log}, nil
}

func newBatchSQLLogDB(t *testing.T, dsn string) (*sql.DB, *batchSQLLog) {
	t.Helper()
	log := &batchSQLLog{}
	conn := sql.OpenDB(&batchSQLLogConnector{dsn: dsn, log: log})
	t.Cleanup(func() { require.NoError(t, conn.Close()) })
	return conn, log
}
