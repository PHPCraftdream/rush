// Narrow test-only bridges for the read-history tests that still live in the
// internal/session package (and other cross-package tests): each accessor
// exposes exactly one unexported reader identifier and nothing else. No read
// logic here.
package sessionbatchread

import (
	"context"
	"testing"

	"github.com/PHPCraftdream/rush/internal/store/db"
)

// BatchReadDepthForTest returns the reader's traversal depth limit.
func BatchReadDepthForTest(tb testing.TB) int {
	tb.Helper()
	return batchReadDepth
}

// BatchLiveNodesForOwnersServedForTest drives the index-served read helper
// directly (the whole-result row/order equivalence oracle needs the global
// ordering the per-owner map API cannot express).
func BatchLiveNodesForOwnersServedForTest(tb testing.TB, ctx context.Context, q *db.Queries, owners []string) ([]db.BatchNode, error) {
	tb.Helper()
	return batchLiveNodesForOwnersServed(ctx, q, owners)
}

// ErrBatchLiveRootVanishedForTest returns the vanish sentinel for
// errors.Is assertions.
func ErrBatchLiveRootVanishedForTest(tb testing.TB) error {
	tb.Helper()
	return errBatchLiveRootVanished
}

// BatchTimeoutCheckinSelectForTest returns the candidate query SQL (EXPLAIN
// QUERY PLAN pins).
func BatchTimeoutCheckinSelectForTest(tb testing.TB) string {
	tb.Helper()
	return batchTimeoutCheckinSelect
}

// SetBatchTimeoutCheckinBudgetForTest overrides the scanner's page size and
// per-scan candidate budget, registering a cleanup that restores the prior
// values.
func SetBatchTimeoutCheckinBudgetForTest(tb testing.TB, s *BatchTimeoutCheckin, pageSize, scanBudget int) {
	tb.Helper()
	s.mu.Lock()
	prevPage, prevBudget := s.pageSize, s.scanBudget
	s.pageSize, s.scanBudget = pageSize, scanBudget
	s.mu.Unlock()
	tb.Cleanup(func() {
		s.mu.Lock()
		s.pageSize, s.scanBudget = prevPage, prevBudget
		s.mu.Unlock()
	})
}
