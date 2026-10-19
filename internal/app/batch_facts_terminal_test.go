// R12-H fix regression guards: a permanently unavailable batch fact source
// (ErrBatchFactsSourceUnavailable from the unknown fail-safe) is terminal --
// the rush run loop gives up at once instead of retrying for the full DB
// bound, and the reviewer-pass gate skips straight to its conservative
// "blocked" answer. Transient DB errors keep their existing retry behavior.
package app

import (
	"bytes"
	"context"
	"testing"

	"github.com/PHPCraftdream/rush/internal/agent"
	"github.com/stretchr/testify/require"
)

func TestNextStep_UnavailableFactSource_TerminalOnFirstRead(t *testing.T) {
	src := &scriptedScopeSource{script: []scopeAnswer{{err: agent.ErrBatchFactsSourceUnavailable}}}
	step, _, err := nextStepLoop(src).nextStep()
	require.Equal(t, stepExit, step)
	require.ErrorIs(t, err, agent.ErrBatchFactsSourceUnavailable)
	require.Contains(t, err.Error(), "unavailable")
	require.EqualValues(t, 1, src.calls.Load(), "terminal error must not be retried")
}

func TestReviewerPassBlocked_UnavailableFactSource_NoRetries(t *testing.T) {
	var buf bytes.Buffer
	fake := &scriptedScopeSource{script: []scopeAnswer{{err: agent.ErrBatchFactsSourceUnavailable}}}
	require.True(t, (&App{}).reviewerPassBlocked(context.Background(), fake, "sess-1", &buf))
	require.EqualValues(t, 1, fake.calls.Load())
	require.Contains(t, buf.String(), "its scope could not be read")
}
