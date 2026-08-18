// Release gate tests whose criteria are covered by dedicated tests in other
// files/packages; these document that coverage and skip. Holds gates 5, 6, 8.

package agent

import (
	"testing"
)

// TestReleaseGate_5_ConcurrentModelChangeSummarizeIsolation proves that
// manual/queued /compact (summarize) uses a single immutable snapshot of
// model/provider-options/prompt-prefix from the TARGET session, even when
// shared state is concurrently mutated by another session.
//
// CRITERION: Concurrently change models of TWO sessions, run manual summary on one
//
//	→ summary uses TARGET session's provider/model/options/prefix.
//
// This criterion is already covered by TestP341_ConcurrentSetModelsSummarizeUsesTargetSessionSnapshot
// which follows the "no external poke" rule using mailbox.testPreSnapshotConsumeSeam.
func TestReleaseGate_5_ConcurrentModelChangeSummarizeIsolation(t *testing.T) {
	t.Parallel()
	t.Log("This criterion is covered by TestP341_ConcurrentSetModelsSummarizeUsesTargetSessionSnapshot")
	t.Log("That test already follows the 'no external poke' rule using mailbox.testPreSnapshotConsumeSeam")
	t.Log("Run that test separately with: go test -run TestP341_ConcurrentSetModelsSummarizeUsesTargetSessionSnapshot -v")

	// For release gate automation, we document the coverage rather than re-implement.
	t.Skip("Covered by TestP341_ConcurrentSetModelsSummarizeUsesTargetSessionSnapshot - run separately")
}

// TestReleaseGate_6_ProviderCancellationHardAbort proves that all provider
// adapter categories respect context cancellation as a hard execution boundary.
//
// CRITERION: For EACH provider adapter category → hung stream stops within 5s on cancellation.
//
// This criterion is already covered by TestProviderCancellationConformance which
// comprehensively tests all HTTP-based providers (openaicompat, openai, anthropic,
// azure, bedrock, vercel, openrouter) and documents CLI provider coverage.
func TestReleaseGate_6_ProviderCancellationHardAbort(t *testing.T) {
	t.Parallel()
	t.Log("This criterion is covered by TestProviderCancellationConformance")
	t.Log("That test comprehensively verifies all HTTP provider categories respect 5s cancellation bound")
	t.Log("It also documents CLI provider (cliprovider) coverage via existing tests in internal/agent/cliprovider/")
	t.Log("Run that test separately with: go test -run TestProviderCancellationConformance -v")

	// For release gate automation, we document the coverage rather than re-implement.
	t.Skip("Covered by TestProviderCancellationConformance - run separately")
}

// TestReleaseGate_8_RaceDetector proves that the entire test suite passes
// with Go's race detector enabled.
//
// CRITERION: Run entire suite with -race → no data races detected.
//
// This is not a test but a requirement. To verify:
//
//	go test ./internal/agent/... ./internal/session/... ./internal/app/... -race
//
// Expected: PASS with no race reports.
func TestReleaseGate_8_RaceDetector(t *testing.T) {
	t.Skip("This is a requirement, not a test. Run: go test ./internal/agent/... ./internal/session/... ./internal/app/... -race")
}
