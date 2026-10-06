package app

// The exit-mapping test for the turn-stall abort: a turn that died through
// the stall policy's causeStall fire path carries agent.ErrTurnStalled in
// its error chain, and exit() must classify that as exit_reason "stalled"
// (non-zero exit, since err stays non-nil).

import (
	"bytes"
	"context"
	"errors"
	"io"
	"testing"

	"github.com/PHPCraftdream/rush/internal/agent"
	"github.com/PHPCraftdream/rush/internal/session"
	"github.com/stretchr/testify/require"
)

// errSessions fails every read, keeping the loop's exit path off the DB.
type errSessions struct {
	session.Service
}

func (errSessions) Get(ctx context.Context, sessionID string) (session.Session, error) {
	return session.Session{}, errors.New("off")
}

// TestExitMapsStallAbort guards exit()'s ErrTurnStalled classification:
// the run envelope must say exit_reason "stalled" and carry a non-nil
// error (non-zero exit) instead of the generic "error".
func TestExitMapsStallAbort(t *testing.T) {
	l := &cliLoop{
		app:          &App{Sessions: errSessions{}},
		ctx:          t.Context(),
		output:       io.Discard,
		final:        &RunResult{SessionID: "s", ToolCalls: []ToolCallStat{}},
		lastBuffered: &bytes.Buffer{},
	}
	res, err := l.exit(errors.Join(context.Canceled, agent.ErrTurnStalled), "")
	require.Error(t, err)
	require.Equal(t, "stalled", res.ExitReason)
}
