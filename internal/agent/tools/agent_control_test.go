package tools

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"charm.land/fantasy"
	"github.com/stretchr/testify/require"
)

// fakeAgentControl records the routing decision each tool made, so the tests
// below pin the tool layer's thinness: session id from context, parameter
// validation, and pass-through of model-safe errors.
type fakeAgentControl struct {
	caller, child string
	msg           string
	interrupt     bool
	inspection    AgentInspection
	answer        string
	err           error
	calls         int
	delegations   []AgentDelegationSummary
}

func (f *fakeAgentControl) InspectAgent(_ context.Context, caller, child string) (AgentInspection, error) {
	f.calls++
	f.caller, f.child = caller, child
	return f.inspection, f.err
}

func (f *fakeAgentControl) ListDelegations(_ context.Context, caller string) ([]AgentDelegationSummary, error) {
	f.calls++
	f.caller = caller
	return f.delegations, f.err
}

func (f *fakeAgentControl) InjectAgent(_ context.Context, caller, child, msg string, interrupt bool) (string, error) {
	f.calls++
	f.caller, f.child, f.msg, f.interrupt = caller, child, msg, interrupt
	return f.answer, f.err
}

func (f *fakeAgentControl) StopAgent(_ context.Context, caller, child string) (string, error) {
	f.calls++
	f.caller, f.child = caller, child
	return f.answer, f.err
}

func agentControlContext(sessionID string) context.Context {
	return context.WithValue(context.Background(), SessionIDContextKey, sessionID)
}

func runToolTool(t *testing.T, tool fantasy.AgentTool, ctx context.Context, params any) fantasy.ToolResponse {
	t.Helper()
	input, err := json.Marshal(params)
	require.NoError(t, err)
	resp, err := tool.Run(ctx, fantasy.ToolCall{ID: "call-1", Input: string(input)})
	require.NoError(t, err, "control tools surface every refusal as a model-safe error response")
	return resp
}

func TestInspectAgentTool_RoutesThroughControl(t *testing.T) {
	ctrl := &fakeAgentControl{inspection: AgentInspection{
		ChildSessionID: "child-1", Status: "running", QueuedMessages: 2,
	}}
	resp := runToolTool(t, NewInspectAgentTool(ctrl), agentControlContext("parent-1"),
		InspectAgentParams{childSessionIDParam{"child-1"}})
	require.False(t, resp.IsError)
	require.Equal(t, "parent-1", ctrl.caller)
	require.Equal(t, "child-1", ctrl.child)
	require.Contains(t, resp.Content, `"status":"running"`)
	require.Contains(t, resp.Content, `"queued_messages":2`)
}

func TestInspectAgentTool_MissingSessionIDRefused(t *testing.T) {
	ctrl := &fakeAgentControl{}
	resp := runToolTool(t, NewInspectAgentTool(ctrl), context.Background(),
		InspectAgentParams{childSessionIDParam{"child-1"}})
	require.True(t, resp.IsError)
	require.Contains(t, resp.Content, "session ID is required")
	require.Zero(t, ctrl.calls)
}

func TestInspectAgentTool_NoChildListsDelegations(t *testing.T) {
	// Zero delegations: an answer, not an error.
	ctrl := &fakeAgentControl{}
	resp := runToolTool(t, NewInspectAgentTool(ctrl), agentControlContext("parent-1"), InspectAgentParams{})
	require.False(t, resp.IsError)
	require.Contains(t, resp.Content, "No live sub-agent delegations")

	// Exactly one: the single child's full summary, no round-trip needed.
	ctrl = &fakeAgentControl{
		delegations: []AgentDelegationSummary{{ChildSessionID: "child-1", Status: "running"}},
		inspection:  AgentInspection{ChildSessionID: "child-1", Status: "running", QueuedMessages: 1},
	}
	resp = runToolTool(t, NewInspectAgentTool(ctrl), agentControlContext("parent-1"), InspectAgentParams{})
	require.False(t, resp.IsError)
	require.Contains(t, resp.Content, `"status":"running"`)
	require.Equal(t, "child-1", ctrl.child, "the single delegation must be inspected directly")

	// Several: the listing with a pointer back at child_session_id.
	ctrl = &fakeAgentControl{delegations: []AgentDelegationSummary{
		{ChildSessionID: "child-a", Status: "running"},
		{ChildSessionID: "child-b", Status: "idle"},
	}}
	resp = runToolTool(t, NewInspectAgentTool(ctrl), agentControlContext("parent-1"), InspectAgentParams{})
	require.False(t, resp.IsError)
	require.Contains(t, resp.Content, `"child_session_id":"child-a"`)
	require.Contains(t, resp.Content, `"child_session_id":"child-b"`)
	require.Contains(t, resp.Content, "child_session_id from this list")
}

func TestInjectAgentTool_PassesInterruptAndMessage(t *testing.T) {
	ctrl := &fakeAgentControl{answer: "queued"}
	resp := runToolTool(t, NewInjectAgentTool(ctrl), agentControlContext("parent-1"),
		InjectAgentParams{childSessionIDParam: childSessionIDParam{"child-1"}, Message: "stop at X", Interrupt: true})
	require.False(t, resp.IsError)
	require.Equal(t, "stop at X", ctrl.msg)
	require.True(t, ctrl.interrupt)
	require.Equal(t, "queued", resp.Content)
}

func TestInjectAgentTool_EmptyMessageRefused(t *testing.T) {
	ctrl := &fakeAgentControl{}
	resp := runToolTool(t, NewInjectAgentTool(ctrl), agentControlContext("parent-1"),
		InjectAgentParams{childSessionIDParam: childSessionIDParam{"child-1"}})
	require.True(t, resp.IsError)
	require.Contains(t, resp.Content, "message is required")
	require.Zero(t, ctrl.calls)
}

func TestStopAgentTool_ModelSafeErrorNotGoError(t *testing.T) {
	ctrl := &fakeAgentControl{err: errors.New("child_session_id \"child-1\" has no active turn or pending delegation to stop")}
	resp := runToolTool(t, NewStopAgentTool(ctrl), agentControlContext("parent-1"),
		StopAgentParams{childSessionIDParam{"child-1"}})
	require.True(t, resp.IsError)
	require.Contains(t, resp.Content, "no active turn or pending delegation")
	require.Equal(t, "parent-1", ctrl.caller)
}

func TestDelegationJobErrorPointsAtControlTools(t *testing.T) {
	// Stage 3 (task #1024): job_kill/job_output's refusal for a delegation
	// job_id must name the tools that CAN stop/inspect it.
	// Revert-check: restore the pre-stage-3 DelegationJobError text (without
	// stop_agent/inspect_agent) and this test fails.
	err := (&DelegationJobError{JobID: "call-1", ChildSessionID: "child-1"}).Error()
	require.Contains(t, err, "stop_agent")
	require.Contains(t, err, "inspect_agent")
}
