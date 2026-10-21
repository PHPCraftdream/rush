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
	c := &pdaBatchStmts{owner: writer}
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
		stmt, err := writer.PrepareContext(ctx, p.query)
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

// PdaBatchTxQueries returns a Queries bound to tx whose statements are
// re-bindings of the cached DB-level statements. tx.StmtContext reuses an
// already-prepared statement on the tx's underlying connection without
// re-preparing (the writer pool has SetMaxOpenConns(1), so the tx is always
// on the same connection; even if it were not, database/sql re-prepares
// transparently and correctly). The tx-bound statements are closed
// automatically by tx.Commit()/tx.Rollback(); do NOT close them here.
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
	bind := func(stmt *sql.Stmt) *sql.Stmt { return tx.StmtContext(ctx, stmt) }
	getLiveBatchRoot := bind(c.getLiveBatchRoot)
	reserveBatchNode := bind(c.reserveBatchNode)
	projectBatchNode := bind(c.projectBatchNode)
	advanceBatchRootSequence := bind(c.advanceBatchRootSequence)
	getBatchNode := bind(c.getBatchNode)
	selectBatchNodeNotice := bind(c.selectBatchNodeNotice)
	updateBatchRunnerCheckpoint := bind(c.updateBatchRunnerCheckpoint)
	getBatchRoot := bind(c.getBatchRoot)
	listBatchNodes := bind(c.listBatchNodes)
	batchOutboxAnnounce := bind(c.batchOutboxAnnounce)
	batchOutboxSetAnnouncementMessage := bind(c.batchOutboxSetAnnouncementMessage)
	batchOutboxInlineElect := bind(c.batchOutboxInlineElect)
	batchOutboxNameMessage := bind(c.batchOutboxNameMessage)
	batchOutboxConsumeWithToolResult := bind(c.batchOutboxConsumeWithToolResult)
	getBatchRootIdentity := bind(c.getBatchRootIdentity)
	deleteBatchSubtree := bind(c.deleteBatchSubtree)
	getMessage := bind(c.getMessage)
	markSessionNoticesReactedForOwner := bind(c.markSessionNoticesReactedForOwner)
	clearSessionNoticesReactedFailedForOwner := bind(c.clearSessionNoticesReactedFailedForOwner)
	return &Queries{
		db:                                           tx,
		getLiveBatchRootStmt:                         getLiveBatchRoot,
		reserveBatchNodeStmt:                         reserveBatchNode,
		projectBatchNodeStmt:                         projectBatchNode,
		advanceBatchRootSequenceStmt:                 advanceBatchRootSequence,
		getBatchNodeStmt:                             getBatchNode,
		selectBatchNodeNoticeStmt:                    selectBatchNodeNotice,
		updateBatchRunnerCheckpointStmt:              updateBatchRunnerCheckpoint,
		getBatchRootStmt:                             getBatchRoot,
		listBatchNodesStmt:                           listBatchNodes,
		batchOutboxAnnounceStmt:                      batchOutboxAnnounce,
		batchOutboxSetAnnouncementMessageStmt:        batchOutboxSetAnnouncementMessage,
		batchOutboxInlineElectStmt:                   batchOutboxInlineElect,
		batchOutboxNameMessageStmt:                   batchOutboxNameMessage,
		batchOutboxConsumeWithToolResultStmt:         batchOutboxConsumeWithToolResult,
		getBatchRootIdentityStmt:                     getBatchRootIdentity,
		deleteBatchSubtreeStmt:                       deleteBatchSubtree,
		getMessageStmt:                               getMessage,
		markSessionNoticesReactedForOwnerStmt:        markSessionNoticesReactedForOwner,
		clearSessionNoticesReactedFailedForOwnerStmt: clearSessionNoticesReactedFailedForOwner,
	}
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
