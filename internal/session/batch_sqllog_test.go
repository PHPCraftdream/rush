// Driver-level SQL capture for the plan/statement-count tests: a
// driver.Connector that wraps every modernc sqlite connection and records each
// ExecContext/QueryContext statement (with its bound args) before delegating.
package session

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
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
	txEvents    []string
	Intercept   func(query string, invocation int) bool
}

func (l *batchSQLLog) recordTxEvent(e string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.txEvents = append(l.txEvents, e)
}

func (l *batchSQLLog) txEventsSnapshot() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	out := make([]string, len(l.txEvents))
	copy(out, l.txEvents)
	return out
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
	bt, ok := c.Conn.(driver.ConnBeginTx)
	if !ok {
		return nil, errors.New("sqllog: the wrapped driver does not implement driver.ConnBeginTx")
	}
	inner, err := bt.BeginTx(ctx, opts)
	if err != nil {
		return nil, err
	}
	c.log.recordTxEvent("begin")
	return &batchSQLLogTx{Tx: inner, log: c.log}, nil
}

// batchSQLLogTx records Commit/Rollback through the log so tests can assert
// the dispatch's begin/rollback ordering.
type batchSQLLogTx struct {
	driver.Tx
	log *batchSQLLog
}

func (t *batchSQLLogTx) Commit() error {
	t.log.recordTxEvent("commit")
	return t.Tx.Commit()
}

func (t *batchSQLLogTx) Rollback() error {
	t.log.recordTxEvent("rollback")
	return t.Tx.Rollback()
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
	queryer, ok := c.Conn.(driver.QueryerContext)
	if !ok {
		// database/sql falls back to Prepare on the wrapped conn.
		return nil, driver.ErrSkip
	}
	return queryer.QueryContext(ctx, query, args)
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
