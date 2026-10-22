package db

// PD11A Phase B: deterministic lifecycle/registry tests for the batch
// prepared-statement cache (pda_batch_prepare.go). In-package so the tests
// can reach pdaDropBatchStmts and the pdaBatchStmtsByDB registry directly.
//
// Mutations caught (one per test, stated in the comments):
//   - lifecycle regressions that make use-after-release panic instead of
//     returning a clean error,
//   - a regressed closeEntry hook that stops dropping the cache on release,
//   - an Ensure that re-prepares per call (the single-connection deadlock
//     regression would manifest as a pool deadlock instead).

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
)

// A1: use-after-release must fail cleanly (require.Error) and never panic,
// and pdaDropBatchStmts must be idempotent.
// Mutation caught: a lifecycle change that closes statements without marking
// the entry closed, or that panics/double-closes on drop.
func TestPdaBatchStmtsUseAfterDropIsCleanError(t *testing.T) {
	ctx := context.Background()
	writer, err := Connect(ctx, t.TempDir())
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, ReleaseConn(writer)) })

	// First production-facing assertion: the cache prepares cleanly.
	require.NoError(t, PdaBatchEnsure(ctx, writer))
	// Grab Queries bound to the cached DB-level statements BEFORE the drop;
	// PdaBatchWriterQueries would re-create the cache afterwards.
	q, err := PdaBatchWriterQueries(ctx, writer)
	require.NoError(t, err)
	require.NoError(t, pdaDropBatchStmts(writer))
	// A dropped statement must fail with "statement is closed" — not panic and
	// not sql.ErrNoRows, which an unclosed statement would return for "x".
	_, err = q.GetBatchRootIdentity(ctx, "x")
	require.ErrorContains(t, err, "statement is closed")
	// Idempotent drop: no error, no panic.
	require.NoError(t, pdaDropBatchStmts(writer))
}

// A2: releasing the pool must drop the registry entry (closeEntry hook) and
// a subsequent Ensure on the closed pool must fail cleanly.
// Mutation caught: a regressed closeEntry that no longer calls
// pdaDropBatchStmts leaves the entry behind; a panic on closed-pool Ensure.
func TestPdaBatchReleaseDropsRegistryEntry(t *testing.T) {
	ctx := context.Background()
	writer, err := Connect(ctx, t.TempDir())
	require.NoError(t, err)
	// Keep the pairing required by .githooks/check_db_release_pairing.sh;
	// an explicit release below plus this no-op cleanup stays paired.
	t.Cleanup(func() { require.NoError(t, ReleaseConn(writer)) })

	require.NoError(t, PdaBatchEnsure(ctx, writer))
	pdaBatchStmtsMu.Lock()
	_, ok := pdaBatchStmtsByDB[writer]
	pdaBatchStmtsMu.Unlock()
	require.True(t, ok, "Ensure must register the cache for the writer")

	// Explicit release; the t.Cleanup release is a no-op by design.
	require.NoError(t, ReleaseConn(writer))
	pdaBatchStmtsMu.Lock()
	_, ok = pdaBatchStmtsByDB[writer]
	pdaBatchStmtsMu.Unlock()
	require.False(t, ok, "ReleaseConn must drop the batch statement cache")

	// Ensure on a closed pool fails cleanly (no panic).
	require.Error(t, PdaBatchEnsure(ctx, writer))
}

// A3: Ensure twice prepares once — the registry entry pointer is identical.
// Mutation caught: an Ensure that re-prepares per call (its first symptom on
// the MaxOpenConns(1) writer pool is a deadlock, so catch it statically here
// via pointer identity).
func TestPdaBatchEnsurePreparesOnce(t *testing.T) {
	ctx := context.Background()
	writer, err := Connect(ctx, t.TempDir())
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, ReleaseConn(writer)) })

	require.NoError(t, PdaBatchEnsure(ctx, writer))
	pdaBatchStmtsMu.Lock()
	first := pdaBatchStmtsByDB[writer]
	pdaBatchStmtsMu.Unlock()
	require.NotNil(t, first)

	require.NoError(t, PdaBatchEnsure(ctx, writer))
	pdaBatchStmtsMu.Lock()
	second := pdaBatchStmtsByDB[writer]
	pdaBatchStmtsMu.Unlock()
	require.Same(t, first, second, "Ensure must not re-prepare an open cache")
	// 19 generated statements plus the three PD11C message statements. The
	// registered (non-generated) statements are NOT counted here: the db
	// package registers none, but other packages' init() registrations are a
	// process-wide global, so the expectation adds the live count.
	pdaBatchStmtsMu.Lock()
	registered := len(pdcRegisteredBatchSQL)
	pdaBatchStmtsMu.Unlock()
	require.Len(t, first.all, 22+registered, "the full batch statement set is prepared once")
}

// PD11C (e): registration is idempotent, affects only caches built after it,
// and unregistered queries still execute through the raw fallback.
func TestPdbBatchRegisterSQLOrderingAndIdempotence(t *testing.T) {
	ctx := context.Background()
	probe := "SELECT 1 AS pdc_registration_probe"
	// Before Ensure: registered statements are part of the built cache.
	writer, err := Connect(ctx, t.TempDir())
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, ReleaseConn(writer)) })
	PdbBatchRegisterSQL(probe)
	PdbBatchRegisterSQL(probe) // idempotent
	require.NoError(t, PdaBatchEnsure(ctx, writer))
	require.True(t, PdbBatchCacheHasSQL(writer, probe), "a statement registered before Ensure is cached")
	// After Ensure: the existing cache is untouched; the statement stays raw.
	late := "SELECT 2 AS pdc_registration_probe"
	PdbBatchRegisterSQL(late)
	require.False(t, PdbBatchCacheHasSQL(writer, late), "a cache built before registration must not gain the statement")
	// The late registration still applies to a NEW cache.
	writer2, err := Connect(ctx, t.TempDir())
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, ReleaseConn(writer2)) })
	require.NoError(t, PdaBatchEnsure(ctx, writer2))
	require.True(t, PdbBatchCacheHasSQL(writer2, late))
	// An unregistered query falls through the binder to raw SQL cleanly.
	tx, err := writer2.BeginTx(ctx, nil)
	require.NoError(t, err)
	defer tx.Rollback() //nolint:errcheck
	q := PdaBatchTxQueries(ctx, writer2, tx)
	var answer int
	require.NoError(t, q.PdbBatchRawQueryRow(ctx, "SELECT 6*7").Scan(&answer))
	require.Equal(t, 42, answer)
	require.NoError(t, tx.Rollback())
}
