package session

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// Revert-check: changing any shared framing literal must fail this pin.
func TestChildQuestionFormatLiterals(t *testing.T) {
	t.Parallel()
	require.Equal(t, "Stopped: agent asked a question and is awaiting an answer", AwaitingAnswerStoppedTitle)
	require.Equal(t, "QUESTION: ", AwaitingQuestionPrefix)
	require.Equal(t, "\n\nThis is not a crash", AwaitingGuidanceMarker)
}
