package app

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/charmbracelet/crush/internal/session"
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
			}
		})
	}
}
