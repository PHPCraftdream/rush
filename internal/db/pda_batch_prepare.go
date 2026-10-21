package db

// pda_batch_prepare.go is hand-written (package PD11A): a prepared-statement
// cache for the batch write path. sqlc generates the Queries struct with
// unexported *sql.Stmt fields (emit_prepared_queries); generated methods use
// the bound stmt when non-nil and fall back to the raw SQL via QueryContext/
// ExecContext when nil. The helpers here populate those fields once per
// writer pool so per-transaction SQLite statement preparation disappears from
// the per-call batch hot paths.
//
// Lifecycle: the cache is keyed by the writer *sql.DB and dropped from
// closeEntry (connect.go) via pdaDropBatchStmts BEFORE the pool itself is
// closed. Tx-bound statements created by PdaBatchTxQueries are never closed
// here: database/sql closes every tx.StmtContext binding automatically when
// the transaction is committed or rolled back.

import (
	"context"
	"database/sql"
	"fmt"
	"sync"
)

// pdaBatchStmts holds the DB-level prepared statements for one writer pool,
// plus the owner handle used as the registry key.
type pdaBatchStmts struct {
	owner                                    *sql.DB
	closed                                   bool
	byQuery                                  map[string]*sql.Stmt
	getLiveBatchRoot                         *sql.Stmt
	reserveBatchNode                         *sql.Stmt
	projectBatchNode                         *sql.Stmt
	advanceBatchRootSequence                 *sql.Stmt
	getBatchNode                             *sql.Stmt
	selectBatchNodeNotice                    *sql.Stmt
	updateBatchRunnerCheckpoint              *sql.Stmt
	getBatchRoot                             *sql.Stmt
	listBatchNodes                           *sql.Stmt
	batchOutboxAnnounce                      *sql.Stmt
	batchOutboxSetAnnouncementMessage        *sql.Stmt
	batchOutboxInlineElect                   *sql.Stmt
	batchOutboxNameMessage                   *sql.Stmt
	batchOutboxConsumeWithToolResult         *sql.Stmt
	getBatchRootIdentity                     *sql.Stmt
	deleteBatchSubtree                       *sql.Stmt
	getMessage                               *sql.Stmt
	markSessionNoticesReactedForOwner        *sql.Stmt
	clearSessionNoticesReactedFailedForOwner *sql.Stmt
	all                                      []*sql.Stmt
}

var (
	pdaBatchStmtsMu   sync.Mutex
	pdaBatchStmtsByDB = make(map[*sql.DB]*pdaBatchStmts)
)

// pdaBatchPrepareLocked prepares the whole batch statement set once for the
// given writer. The caller must hold pdaBatchStmtsMu. On any error, every
// statement opened so far is closed and the partial registry entry removed.
func pdaBatchPrepareLocked(ctx context.Context, writer *sql.DB) (*pdaBatchStmts, error) {
	c := &pdaBatchStmts{owner: writer, byQuery: make(map[string]*sql.Stmt, 19)}
	type pending struct {
		field **sql.Stmt
		name  string
		query string
	}
	order := []pending{
		{&c.getLiveBatchRoot, "getLiveBatchRoot", getLiveBatchRoot},
		{&c.reserveBatchNode, "reserveBatchNode", reserveBatchNode},
		{&c.projectBatchNode, "projectBatchNode", projectBatchNode},
		{&c.advanceBatchRootSequence, "advanceBatchRootSequence", advanceBatchRootSequence},
		{&c.getBatchNode, "getBatchNode", getBatchNode},
		{&c.selectBatchNodeNotice, "selectBatchNodeNotice", selectBatchNodeNotice},
		{&c.updateBatchRunnerCheckpoint, "updateBatchRunnerCheckpoint", updateBatchRunnerCheckpoint},
		{&c.getBatchRoot, "getBatchRoot", getBatchRoot},
		{&c.listBatchNodes, "listBatchNodes", listBatchNodes},
		{&c.batchOutboxAnnounce, "batchOutboxAnnounce", batchOutboxAnnounce},
		{&c.batchOutboxSetAnnouncementMessage, "batchOutboxSetAnnouncementMessage", batchOutboxSetAnnouncementMessage},
		{&c.batchOutboxInlineElect, "batchOutboxInlineElect", batchOutboxInlineElect},
		{&c.batchOutboxNameMessage, "batchOutboxNameMessage", batchOutboxNameMessage},
		{&c.batchOutboxConsumeWithToolResult, "batchOutboxConsumeWithToolResult", batchOutboxConsumeWithToolResult},
		{&c.getBatchRootIdentity, "getBatchRootIdentity", getBatchRootIdentity},
		{&c.deleteBatchSubtree, "deleteBatchSubtree", deleteBatchSubtree},
		{&c.getMessage, "getMessage", getMessage},
		{&c.markSessionNoticesReactedForOwner, "markSessionNoticesReactedForOwner", markSessionNoticesReactedForOwner},
		{&c.clearSessionNoticesReactedFailedForOwner, "clearSessionNoticesReactedFailedForOwner", clearSessionNoticesReactedFailedForOwner},
	}
	for _, p := range order {
		stmt, err := writer.PrepareContext(ctx, p.query) //nolint:sqlclosecheck // owned by the cache; closed in pdaDropBatchStmts
		if err != nil {
			// Close what was opened, drop the half-built entry, wrap the
			// error the same way the generated code does. The caller holds
			// pdaBatchStmtsMu, so close inline instead of via
			// pdaDropBatchStmts (which takes the mutex).
			c.closed = true
			for _, stmt := range c.all {
				_ = stmt.Close() //nolint:errcheck
			}
			return nil, fmt.Errorf("error preparing query %s: %w", p.name, err)
		}
		*p.field = stmt
		c.byQuery[p.query] = stmt
		c.all = append(c.all, stmt)
	}
	return c, nil
}

// pdaBatchStmtsLookup returns the prepared set for writer if one exists and
// is open. Mutex-guarded map lookup only: it NEVER prepares anything, so it
// is safe to call while the caller holds an open transaction (the writer pool
// has SetMaxOpenConns(1), so preparing there would deadlock).
func pdaBatchStmtsLookup(writer *sql.DB) (*pdaBatchStmts, bool) {
	pdaBatchStmtsMu.Lock()
	defer pdaBatchStmtsMu.Unlock()
	c, ok := pdaBatchStmtsByDB[writer]
	if !ok || c.closed {
		return nil, false
	}
	return c, true
}

// pdaBatchStmtsEnsure returns the prepared set for writer, creating it
// lazily. Preparation happens under pdaBatchStmtsMu and only once per
// *sql.DB. Only call this from paths that hold NO open transaction on the
// writer pool (autocommit sites, or before BeginTx) — see PdaBatchEnsure.
func pdaBatchStmtsEnsure(ctx context.Context, writer *sql.DB) (*pdaBatchStmts, error) {
	pdaBatchStmtsMu.Lock()
	defer pdaBatchStmtsMu.Unlock()
	if c, ok := pdaBatchStmtsByDB[writer]; ok && !c.closed {
		return c, nil
	}
	c, err := pdaBatchPrepareLocked(ctx, writer)
	if err != nil {
		return nil, err
	}
	pdaBatchStmtsByDB[writer] = c
	return c, nil
}

// PdaBatchEnsure makes sure the batch statement cache for writer exists,
// preparing it if necessary. It exists so preparation happens BEFORE any
// transaction is opened on the writer pool: that pool has exactly one
// connection (SetMaxOpenConns(1)), and preparing while a transaction holds
// it would block until that transaction commits — a permanent deadlock on
// the first batch call after every pool open.
func PdaBatchEnsure(ctx context.Context, writer *sql.DB) error {
	_, err := pdaBatchStmtsEnsure(ctx, writer)
	return err
}

// pdbBatchTxBinder implements db.DBTX over one transaction: each distinct
// query is bound to the transaction via tx.StmtContext at most once, on
// first use, from the DB-level prepared statements cached for the writer
// pool. Queries missing from the cache fall back to raw SQL through the tx
// (identical to the pre-PD11A path); the binder NEVER prepares on its own.
// Used only by the single goroutine that owns the transaction.
type pdbBatchTxBinder struct {
	tx    *sql.Tx
	stmts map[string]*sql.Stmt
	bound map[string]*sql.Stmt
}

// bind returns the tx-bound statement for query, binding the cached DB-level
// statement on first use. A failed StmtContext still memoises the returned
// (non-nil) statement: database/sql defers the error to the first use of
// that statement, which matches the eager PD11A path exactly.
func (b *pdbBatchTxBinder) bind(ctx context.Context, query string) (*sql.Stmt, bool) {
	if stmt, ok := b.bound[query]; ok {
		return stmt, true
	}
	dbStmt, ok := b.stmts[query]
	if !ok {
		return nil, false
	}
	stmt := b.tx.StmtContext(ctx, dbStmt)
	b.bound[query] = stmt
	return stmt, true
}

func (b *pdbBatchTxBinder) ExecContext(ctx context.Context, query string, args ...interface{}) (sql.Result, error) {
	if stmt, ok := b.bind(ctx, query); ok {
		return stmt.ExecContext(ctx, args...)
	}
	return b.tx.ExecContext(ctx, query, args...)
}

func (b *pdbBatchTxBinder) QueryContext(ctx context.Context, query string, args ...interface{}) (*sql.Rows, error) {
	if stmt, ok := b.bind(ctx, query); ok {
		return stmt.QueryContext(ctx, args...)
	}
	return b.tx.QueryContext(ctx, query, args...)
}

func (b *pdbBatchTxBinder) QueryRowContext(ctx context.Context, query string, args ...interface{}) *sql.Row {
	if stmt, ok := b.bind(ctx, query); ok {
		return stmt.QueryRowContext(ctx, args...)
	}
	return b.tx.QueryRowContext(ctx, query, args...)
}

func (b *pdbBatchTxBinder) PrepareContext(ctx context.Context, query string) (*sql.Stmt, error) {
	return b.tx.PrepareContext(ctx, query)
}

// PdbBatchTxBoundCount reports how many distinct statements the lazy tx
// binder behind q has bound so far. It returns -1 when q was not built by
// PdaBatchTxQueries (nil binder, plain tx, or pool), so tests can assert
// the Queries they hold really is the lazy-bound one.
func PdbBatchTxBoundCount(q *Queries) int {
	b, ok := q.db.(*pdbBatchTxBinder)
	if !ok {
		return -1
	}
	return len(b.bound)
}

// PdaBatchTxQueries returns a Queries bound to tx whose statements are
// re-bindings of the cached DB-level statements, bound LAZILY: each distinct
// query is bound via tx.StmtContext at most once, on first use, by the
// pdbBatchTxBinder behind the returned Queries. tx.StmtContext reuses an
// already-prepared statement on the tx's underlying connection without
// re-preparing, so the bound statement executes byte-identical SQL to the
// eager PD11A path; only the number of bindings per tx changes, from 19 to
// the number of distinct queries actually executed. The tx-bound statements
// are closed automatically by tx.Commit()/tx.Rollback(); do NOT close them
// here.
//
// This function NEVER prepares: if the cache is missing (e.g. a pool release
// raced between PdaBatchEnsure and BeginTx, or Ensure was skipped), it returns
// a Queries bound to tx with nil statement fields — the raw-SQL fallback,
// byte-identical behavior to db.New(tx). Preparing here would deadlock on the
// single-connection writer pool: the tx already holds the only connection.
func PdaBatchTxQueries(ctx context.Context, writer *sql.DB, tx *sql.Tx) *Queries {
	c, ok := pdaBatchStmtsLookup(writer)
	if !ok {
		return &Queries{db: tx}
	}
	return &Queries{db: &pdbBatchTxBinder{tx: tx, stmts: c.byQuery, bound: make(map[string]*sql.Stmt)}}
}

// PdaBatchWriterQueries returns a Queries bound to the writer pool whose
// statements are the cached DB-level statements. *sql.Stmt values returned
// by DB.PrepareContext are safe for concurrent use by multiple goroutines.
// Use for autocommit (no-tx) call sites only. It creates the cache lazily via
// pdaBatchStmtsEnsure: autocommit sites hold no transaction, so a busy-wait
// stall there is the pre-existing behavior of any query on this pool, not a
// deadlock.
func PdaBatchWriterQueries(ctx context.Context, writer *sql.DB) (*Queries, error) {
	c, err := pdaBatchStmtsEnsure(ctx, writer)
	if err != nil {
		return nil, err
	}
	return &Queries{
		db:                                           writer,
		getLiveBatchRootStmt:                         c.getLiveBatchRoot,
		reserveBatchNodeStmt:                         c.reserveBatchNode,
		projectBatchNodeStmt:                         c.projectBatchNode,
		advanceBatchRootSequenceStmt:                 c.advanceBatchRootSequence,
		getBatchNodeStmt:                             c.getBatchNode,
		selectBatchNodeNoticeStmt:                    c.selectBatchNodeNotice,
		updateBatchRunnerCheckpointStmt:              c.updateBatchRunnerCheckpoint,
		getBatchRootStmt:                             c.getBatchRoot,
		listBatchNodesStmt:                           c.listBatchNodes,
		batchOutboxAnnounceStmt:                      c.batchOutboxAnnounce,
		batchOutboxSetAnnouncementMessageStmt:        c.batchOutboxSetAnnouncementMessage,
		batchOutboxInlineElectStmt:                   c.batchOutboxInlineElect,
		batchOutboxNameMessageStmt:                   c.batchOutboxNameMessage,
		batchOutboxConsumeWithToolResultStmt:         c.batchOutboxConsumeWithToolResult,
		getBatchRootIdentityStmt:                     c.getBatchRootIdentity,
		deleteBatchSubtreeStmt:                       c.deleteBatchSubtree,
		getMessageStmt:                               c.getMessage,
		markSessionNoticesReactedForOwnerStmt:        c.markSessionNoticesReactedForOwner,
		clearSessionNoticesReactedFailedForOwnerStmt: c.clearSessionNoticesReactedFailedForOwner,
	}, nil
}

// pdaDropBatchStmts removes and closes the cached statement set for writer.
// Idempotent: a second call is a no-op. After the drop, any Queries built by
// the PdaBatch helpers keeps returning closed statements whose use fails
// cleanly with "sql: statement is closed" — never a panic; double Close on a
// *sql.Stmt is a no-op returning nil, and nil stmts fall back to raw SQL.
func pdaDropBatchStmts(writer *sql.DB) error {
	pdaBatchStmtsMu.Lock()
	c, ok := pdaBatchStmtsByDB[writer]
	if !ok || c.closed {
		// Mark a missing entry too so a concurrently half-built entry cannot
		// be published after the drop; a nil entry is nothing to clean.
		if c != nil {
			c.closed = true
		}
		pdaBatchStmtsMu.Unlock()
		return nil
	}
	c.closed = true
	delete(pdaBatchStmtsByDB, writer)
	pdaBatchStmtsMu.Unlock()
	var errs []error
	for _, stmt := range c.all {
		if stmt == nil {
			continue
		}
		if err := stmt.Close(); err != nil {
			errs = append(errs, err)
		}
	}
	if len(errs) == 0 {
		return nil
	}
	if len(errs) == 1 {
		return errs[0]
	}
	return fmt.Errorf("closing batch prepared statements: %v errors, first: %w", len(errs), errs[0])
}
