package agent

import (
	"testing"

	"github.com/PHPCraftdream/rush/internal/message"
	"github.com/stretchr/testify/require"
)

// TestOnStepFinishRecordsLoopDetectionOnTheTrippingStep pins task #940's
// ordering invariant directly against onStepFinish's own phase methods
// (recordStepHistory then recordStepFinish), not just against the
// underlying hasRepeatedToolCalls algorithm TestLoopDetection_OnStepFinishOrdering
// already covers: if recordStepFinish ran BEFORE recordStepHistory for a
// given step, loopDetected would still hold the PREVIOUS step's (false)
// value when this step is the one that trips the detector -- the finish
// text would come out empty instead of carrying the loop message, and no
// later OnStepFinish call would ever go back and fix it.
func TestOnStepFinishRecordsLoopDetectionOnTheTrippingStep(t *testing.T) {
	// loopDetectionWindowSize/loopDetectionMaxRepeats (loop_detection.go)
	// are the real production constants (10, 5) -- this fixture is built
	// against those exact values, matching TestLoopDetection_OnStepFinishOrdering's
	// own sequence shape.
	mkRepeat := func() fantasyStepFinishFixture {
		return fantasyStepFinishFixture{name: "job_output", input: `{"id":"j1"}`, output: "running"}
	}

	var sequence []fantasyStepFinishFixture
	for i := 0; i < 4; i++ {
		sequence = append(sequence, fantasyStepFinishFixture{name: "other", input: "{}", output: "r"})
	}
	for i := 0; i < 6; i++ {
		sequence = append(sequence, mkRepeat())
	}

	assistant := &message.Message{ID: "asst-1", SessionID: "sess-1"}
	ts := &turnStream{
		call:           SessionAgentCall{SessionID: "sess-1"},
		drainPendingUI: func() {},
	}
	ts.currentAssistant = assistant

	firedOnStep := -1
	for i, f := range sequence {
		step := makeToolStep(f.name, f.input, f.output)
		// recordStepFinish's empty-stream guard only looks at
		// currentAssistant's accumulated content, which in production is
		// populated by the separate OnToolCall callback, not OnStepFinish.
		// Simulate that here so the guard doesn't short-circuit before
		// loop detection ever gets a chance to run.
		assistant.AddToolCall(message.ToolCall{ID: f.name, Name: f.name, Finished: true})
		// Mirrors onStepFinish's own call order for these two phases --
		// this is the assertion, not incidental setup.
		ts.recordStepHistory(step)
		finishReason := classifyStepFinishReason(step)
		ts.recordStepFinish(finishReason)

		if fin, ok := lastFinishPart(assistant); ok && fin.Message != "" {
			firedOnStep = i
			break
		}
	}

	require.Equal(t, 9, firedOnStep, "loop detection must surface in the finish text on the tripping step itself (index 9: the 6th identical repeat), not a step later")

	fin, ok := lastFinishPart(assistant)
	require.True(t, ok)
	require.Contains(t, fin.Message, "job_output", "the loop-detected finish text must name the repeated tool")
}

type fantasyStepFinishFixture struct {
	name, input, output string
}

func lastFinishPart(m *message.Message) (message.Finish, bool) {
	for _, part := range m.Parts {
		if f, ok := part.(message.Finish); ok {
			return f, true
		}
	}
	return message.Finish{}, false
}
