package app

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/PHPCraftdream/rush/internal/session"
)

// TestDrainOutcomeError_NilGuard verifies that drainOutcomeError correctly
// handles contract violations where DrainPartial or DrainFailed report a nil
// drainErr. It must wrap the violation in session.ErrDrainFailureUnspecified
// rather than returning nil (which would incorrectly signal success).
func TestDrainOutcomeError_NilGuard(t *testing.T) {
	t.Parallel()

	sessID := "test-session-123"
	someErr := errors.New("some drain error")
	orig := context.Canceled

	tests := []struct {
		name          string
		result        session.DrainResult
		drainErr      error
		originalErr   error
		wantErr       error
		wantIsUnspec  bool
		wantIsOrig    bool
		wantIsSomeErr bool
	}{
		{
			name:        "DrainComplete with nil drainErr -> nil",
			result:      session.DrainComplete,
			drainErr:    nil,
			originalErr: orig,
			wantErr:     nil,
		},
		{
			name:          "DrainComplete with non-nil drainErr -> drainErr",
			result:        session.DrainComplete,
			drainErr:      someErr,
			originalErr:   orig,
			wantErr:       someErr,
			wantIsSomeErr: true,
		},
		{
			name:          "DrainPartial with non-nil drainErr -> drainErr",
			result:        session.DrainPartial,
			drainErr:      someErr,
			originalErr:   orig,
			wantErr:       someErr,
			wantIsSomeErr: true,
		},
		{
			name:          "DrainFailed with non-nil drainErr -> drainErr",
			result:        session.DrainFailed,
			drainErr:      someErr,
			originalErr:   orig,
			wantErr:       someErr,
			wantIsSomeErr: true,
		},
		{
			name:         "DrainFailed with nil drainErr -> ErrDrainFailureUnspecified",
			result:       session.DrainFailed,
			drainErr:     nil,
			originalErr:  orig,
			wantErr:      fmt.Errorf("%w (session=%s)", session.ErrDrainFailureUnspecified, sessID),
			wantIsUnspec: true,
		},
		{
			name:         "DrainPartial with nil drainErr -> ErrDrainFailureUnspecified",
			result:       session.DrainPartial,
			drainErr:     nil,
			originalErr:  orig,
			wantErr:      fmt.Errorf("%w (session=%s)", session.ErrDrainFailureUnspecified, sessID),
			wantIsUnspec: true,
		},
		{
			name:        "DrainNoWork with nil drainErr -> originalErr",
			result:      session.DrainNoWork,
			drainErr:    nil,
			originalErr: orig,
			wantErr:     orig,
			wantIsOrig:  true,
		},
		{
			// Task #616/P2-2: DrainSessionNow's own doc records that several
			// of its early-exit paths pair DrainNoWork with a non-nil,
			// call-scoped drainErr (this call's own ctx already done, its own
			// lease attempt failing, etc — see session.DrainNoWork's doc).
			// drainOutcomeError must still return originalErr unchanged in
			// that case, exactly as it does when drainErr is nil: drainErr
			// on a DrainNoWork pairing describes why DrainSessionNow itself
			// did not run anything, not this run's own outcome, so it must
			// never leak into the run's returned error.
			name:        "DrainNoWork with non-nil drainErr -> originalErr, NOT drainErr",
			result:      session.DrainNoWork,
			drainErr:    someErr,
			originalErr: orig,
			wantErr:     orig,
			wantIsOrig:  true,
		},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got := drainOutcomeError(sessID, tt.result, tt.drainErr, tt.originalErr)

			if tt.wantErr == nil {
				require.NoError(t, got, "expected nil error")
				return
			}

			require.Error(t, got, "expected non-nil error")

			if tt.wantIsSomeErr {
				require.ErrorIs(t, got, someErr, "error should wrap someErr")
			}
			if tt.wantIsUnspec {
				require.ErrorIs(t, got, session.ErrDrainFailureUnspecified, "error should wrap ErrDrainFailureUnspecified")
				require.NotErrorIs(t, got, orig, "error should NOT wrap originalErr")
			}
			if tt.wantIsOrig {
				require.ErrorIs(t, got, orig, "error should wrap originalErr")
				if tt.drainErr != nil && !errors.Is(tt.drainErr, orig) {
					require.NotErrorIs(t, got, tt.drainErr, "a DrainNoWork pairing must never leak drainErr into the run's returned error -- drainErr on DrainNoWork describes why DrainSessionNow itself did not run anything, not the run's own outcome")
				}
			}
		})
	}
}

// TestDrainOutcomeError_UnrecognizedResult_FailsClosed is task #616/P2-2's
// own regression coverage: drainOutcomeError's default branch used to catch
// BOTH session.DrainNoWork and any genuinely invalid session.DrainResult
// value, silently reusing DrainNoWork's "return originalErr unchanged"
// behavior for both. DrainNoWork now has its own explicit case (see the
// table above), so this test exercises what remains in default: an
// out-of-range DrainResult (a stand-in for a future fifth enum value added
// without a matching case here) must fail CLOSED -- a non-nil sentinel
// distinct from originalErr -- rather than silently inheriting
// DrainNoWork's success-preserving semantics.
func TestDrainOutcomeError_UnrecognizedResult_FailsClosed(t *testing.T) {
	t.Parallel()

	sessID := "test-session-456"
	orig := context.Canceled
	unrecognized := session.DrainResult(99)

	got := drainOutcomeError(sessID, unrecognized, nil, orig)
	require.Error(t, got, "an unrecognized DrainResult must not silently succeed")
	require.ErrorIs(t, got, session.ErrDrainFailureUnspecified, "must fail closed via the same sentinel used for other contract violations")
	require.NotErrorIs(t, got, orig, "must NOT silently fall back to originalErr the way the old shared default/DrainNoWork case did")
}
