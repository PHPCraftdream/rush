package session

// Regression coverage for nil-failure handling: rowLedger must never return
// (DrainFailed, nil) — every failure entry must carry a non-nil error, even
// if the input to recordFailure/recordUnattributed is nil. Two-layer defense:
// 1) Sentinel substitution at input (recordFailure/recordUnattributed replace
//    nil with ErrDrainFailureUnspecified).
// 2) Safety net in verdict() — if failures exist but mostRecentFailure()
//    returns nil (invariant violation), return a wrapped sentinel instead.

import (
	"errors"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestP593NilFailureViaRecordFailure(t *testing.T) {
	t.Parallel()
	l := newRowLedger()
	l.recordFailure("row-A", nil)
	result, err := l.verdict(false)
	require.Equal(t, DrainFailed, result)
	require.NotNil(t, err)
	require.True(t, errors.Is(err, ErrDrainFailureUnspecified))
	// The error must come from the input substitution (it names the row),
	// not from verdict()'s safety net -- this pins THIS guard specifically,
	// so reverting only recordFailure's nil-substitution fails this test.
	require.Contains(t, err.Error(), "(row=row-A)")
}

func TestP593NilFailureViaRecordUnattributed(t *testing.T) {
	t.Parallel()
	l := newRowLedger()
	l.recordUnattributed(nil)
	result, err := l.verdict(false)
	require.Equal(t, DrainFailed, result)
	require.NotNil(t, err)
	require.True(t, errors.Is(err, ErrDrainFailureUnspecified))
	// Pins recordUnattributed's own nil-substitution, not verdict()'s
	// safety net: only the former embeds "(unattributed)" in the message.
	require.Contains(t, err.Error(), "(unattributed)")
}

func TestP593NilFailureVerdictSafetyNet(t *testing.T) {
	t.Parallel()

	// Subcase: failed map has a nil entry with matching order key
	l := newRowLedger()
	l.anyExecuted = true
	l.failed["row-X"] = nil
	l.order = []string{"row-X"}
	result, err := l.verdict(false)
	require.Equal(t, DrainFailed, result)
	require.NotNil(t, err)
	require.True(t, errors.Is(err, ErrDrainFailureUnspecified))

	// Subcase: failed map has a live error but order is empty (order walk fails)
	l2 := newRowLedger()
	l2.anyExecuted = true
	l2.failed["row-Y"] = errors.New("x")
	l2.order = nil
	result, err = l2.verdict(false)
	require.Equal(t, DrainFailed, result)
	require.NotNil(t, err)
	require.True(t, errors.Is(err, ErrDrainFailureUnspecified))
}

func TestP593RecordSuccessClearsNilFailure(t *testing.T) {
	t.Parallel()
	l := newRowLedger()

	// Record a nil failure, which becomes a sentinel error
	l.recordFailure("row-A", nil)

	// Then record success for the same row, which should clear it
	l.recordSuccess("row-A")

	// Verdict should now be DrainComplete with no error
	result, err := l.verdict(false)
	require.Equal(t, DrainComplete, result)
	require.Nil(t, err)
}
