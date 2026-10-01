// Stage-4c wake tools (wakein/wakeon/loop/wake_list/wake_cancel, contract
// §1). The tool layer is thin — these tests pin exactly that: session id
// from context, pass-through of model-safe errors, and the response shape.
// The contract's validation/ownership/limit behavior lives in the
// coordinator adapter and is covered by coordinator_wake_tools_test.go in
// the agent package.

package tools

import (
	"context"
	"errors"
	"testing"

	"charm.land/fantasy"
	"github.com/stretchr/testify/require"
)

type fakeWakeControl struct {
	caller   string
	delay    int64
	at       string
	every    int64
	msg      string
	maxRuns  *int64
	until    *string
	cancelID string
	views    []WakeScheduleView
	view     WakeScheduleView
	answer   string
	err      error
	creates  int
	cancels  int
	lists    int
}

func (f *fakeWakeControl) ScheduleWakeOnceDelay(_ context.Context, caller string, delay int64, msg string) (WakeScheduleView, error) {
	f.calls("delay", caller, msg)
	f.delay = delay
	return f.view, f.err
}

func (f *fakeWakeControl) ScheduleWakeOnceAt(_ context.Context, caller, at, msg string) (WakeScheduleView, error) {
	f.calls("at", caller, msg)
	f.at = at
	return f.view, f.err
}

func (f *fakeWakeControl) ScheduleWakeLoop(_ context.Context, caller string, every int64, msg string, maxRuns *int64, until *string) (WakeScheduleView, error) {
	f.calls("loop", caller, msg)
	f.every, f.maxRuns, f.until = every, maxRuns, until
	return f.view, f.err
}

func (f *fakeWakeControl) ListWakeSchedules(_ context.Context, caller string) ([]WakeScheduleView, error) {
	f.calls("list", caller, "")
	f.lists++
	return f.views, f.err
}

func (f *fakeWakeControl) CancelWakeSchedule(_ context.Context, caller, id string) (string, error) {
	f.calls("cancel", caller, "")
	f.cancelID = id
	return f.answer, f.err
}

func (f *fakeWakeControl) calls(kind, caller, msg string) {
	f.creates++
	f.caller = caller
	f.msg = msg
}

func TestWakeTools_ThinDelegation(t *testing.T) {
	view := WakeScheduleView{ScheduleID: "wake_1", Kind: "once", FiresAt: "2026-09-27T19:45:00Z", Message: "hi"}
	ctrl := &fakeWakeControl{view: view, answer: "Schedule wake_1 cancelled."}

	resp := runToolTool(t, NewWakeinTool(ctrl), agentControlContext("sess-1"), WakeinParams{DelaySeconds: 3600, Message: "hi"})
	require.False(t, resp.IsError)
	require.Equal(t, "sess-1", ctrl.caller)
	require.EqualValues(t, 3600, ctrl.delay)
	require.Contains(t, resp.Content, "Wake wake_1 scheduled for 2026-09-27T19:45:00Z (in 1h0m0s)")
	require.Contains(t, resp.Content, "wake_cancel")

	resp = runToolTool(t, NewWakeonTool(ctrl), agentControlContext("sess-1"),
		WakeonParams{At: "2026-09-28T09:00:00+03:00", Message: "hi"})
	require.False(t, resp.IsError)
	require.Equal(t, "2026-09-28T09:00:00+03:00", ctrl.at)

	mr := int64(10)
	resp = runToolTool(t, NewWakeLoopTool(ctrl), agentControlContext("sess-1"),
		WakeLoopParams{EverySeconds: 1800, Message: "hi", MaxRuns: &mr})
	require.False(t, resp.IsError)
	require.EqualValues(t, 1800, ctrl.every)
	require.NotNil(t, ctrl.maxRuns)
	require.EqualValues(t, 10, *ctrl.maxRuns)

	resp = runToolTool(t, NewWakeListTool(ctrl), agentControlContext("sess-1"), WakeListParams{})
	require.False(t, resp.IsError)

	resp = runToolTool(t, NewWakeCancelTool(ctrl), agentControlContext("sess-1"), WakeCancelParams{ScheduleID: "wake_1"})
	require.False(t, resp.IsError)
	require.Equal(t, "wake_1", ctrl.cancelID)
	require.Equal(t, "Schedule wake_1 cancelled.", resp.Content)
}

func TestWakeTools_MissingSessionIDRefused(t *testing.T) {
	ctrl := &fakeWakeControl{}
	cases := []struct {
		name string
		run  func() fantasy.ToolResponse
	}{
		{"wakein", func() fantasy.ToolResponse {
			return runToolTool(t, NewWakeinTool(ctrl), context.Background(), WakeinParams{DelaySeconds: 10, Message: "m"})
		}},
		{"wakeon", func() fantasy.ToolResponse {
			return runToolTool(t, NewWakeonTool(ctrl), context.Background(), WakeonParams{At: "2026-09-28T09:00:00Z", Message: "m"})
		}},
		{"loop", func() fantasy.ToolResponse {
			return runToolTool(t, NewWakeLoopTool(ctrl), context.Background(), WakeLoopParams{EverySeconds: 300, Message: "m"})
		}},
		{"wake_list", func() fantasy.ToolResponse {
			return runToolTool(t, NewWakeListTool(ctrl), context.Background(), WakeListParams{})
		}},
		{"wake_cancel", func() fantasy.ToolResponse {
			return runToolTool(t, NewWakeCancelTool(ctrl), context.Background(), WakeCancelParams{ScheduleID: "wake_1"})
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			before := ctrl.creates + ctrl.lists + ctrl.cancels
			resp := tc.run()
			require.True(t, resp.IsError)
			require.Contains(t, resp.Content, "session ID is required")
			require.Equal(t, before, ctrl.creates+ctrl.lists+ctrl.cancels, "no control call without a session id")
		})
	}
}

func TestWakeTools_ModelSafeErrorsAndGuards(t *testing.T) {
	ctrl := &fakeWakeControl{err: errors.New("wake scheduler is unavailable: this session has no durable wake-schedule worker, so nothing would ever fire")}
	resp := runToolTool(t, NewWakeinTool(ctrl), agentControlContext("sess-1"), WakeinParams{DelaySeconds: 10, Message: "m"})
	require.True(t, resp.IsError)
	require.Contains(t, resp.Content, "wake scheduler is unavailable")

	ctrl = &fakeWakeControl{}
	resp = runToolTool(t, NewWakeCancelTool(ctrl), agentControlContext("sess-1"), WakeCancelParams{})
	require.True(t, resp.IsError)
	require.Contains(t, resp.Content, "missing schedule_id")
	require.Zero(t, ctrl.cancels)

	// Empty wake_list is not an error (§1.4).
	ctrl = &fakeWakeControl{}
	resp = runToolTool(t, NewWakeListTool(ctrl), agentControlContext("sess-1"), WakeListParams{})
	require.False(t, resp.IsError)
	require.Equal(t, "No active schedules for this session.", resp.Content)

	ctrl = &fakeWakeControl{views: []WakeScheduleView{{ScheduleID: "wake_1", Kind: "once", FiresAt: "2026-09-27T19:45:00Z", Message: "hi"}}}
	resp = runToolTool(t, NewWakeListTool(ctrl), agentControlContext("sess-1"), WakeListParams{})
	require.False(t, resp.IsError)
	require.Contains(t, resp.Content, `"schedule_id": "wake_1"`)
}

func TestWakeTools_DescriptionsPresent(t *testing.T) {
	descs := map[string]string{
		WakeinToolName:   wakeinDescription,
		WakeonToolName:   wakeonDescription,
		WakeLoopToolName: wakeLoopDescription,
		WakeListToolName: wakeListDescription,
	}
	for name, desc := range descs {
		require.NotEmpty(t, desc, "%s must carry its contract §8 description", name)
	}
	require.NotEmpty(t, wakeCancelDescription, "%s must carry its contract §8 description", WakeCancelToolName)
	require.Contains(t, wakeCancelDescription, "schedule_id")
}
