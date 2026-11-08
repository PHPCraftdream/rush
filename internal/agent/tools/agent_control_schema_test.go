package tools

import (
	"encoding/json"
	"testing"

	"charm.land/fantasy"
	"github.com/stretchr/testify/require"
)

func TestAgentControlSchemasExposeFlatChildSessionID(t *testing.T) {
	cases := []struct {
		tool     fantasy.AgentTool
		required []string
	}{
		{NewInjectAgentTool(nil), []string{"child_session_id", "message"}},
		{NewStopAgentTool(nil), []string{"child_session_id"}},
		{NewInspectAgentTool(nil), nil},
	}
	for _, tc := range cases {
		info := tc.tool.Info()
		t.Run(info.Name, func(t *testing.T) {
			t.Cleanup(func() {
				if t.Failed() {
					raw, err := json.Marshal(info)
					t.Logf("generated control schema=%s marshal_error=%v", raw, err)
				}
			})
			require.Contains(t, info.Parameters, "child_session_id", "the model must see the target required by runtime")
			require.NotContains(t, info.Parameters, "child_session_id_param", "the target is flat, not an embedded object")
			raw, err := json.Marshal(info.Parameters["child_session_id"])
			require.NoError(t, err)
			var property struct {
				Type string `json:"type"`
			}
			require.NoError(t, json.Unmarshal(raw, &property))
			require.Equal(t, "string", property.Type)
			require.ElementsMatch(t, tc.required, info.Required)
		})
	}
}

func TestAgentControlChildSessionIDWireRemainsFlat(t *testing.T) {
	const child = "message-uuid$$call_worker"
	for _, name := range []string{InjectAgentToolName, InspectAgentToolName, StopAgentToolName} {
		t.Run(name, func(t *testing.T) {
			control := &fakeAgentControl{answer: "accepted", inspection: AgentInspection{ChildSessionID: child, Status: "running"}}
			var tool fantasy.AgentTool
			input := map[string]any{"child_session_id": child}
			switch name {
			case InjectAgentToolName:
				tool = NewInjectAgentTool(control)
				input["message"] = "reply"
			case InspectAgentToolName:
				tool = NewInspectAgentTool(control)
			case StopAgentToolName:
				tool = NewStopAgentTool(control)
			}
			response := runToolTool(t, tool, agentControlContext("parent"), input)
			t.Cleanup(func() {
				if t.Failed() {
					t.Logf("tool=%s actual_child=%q response_error=%v content=%s", name, control.child, response.IsError, response.Content)
				}
			})
			require.False(t, response.IsError)
			require.Equal(t, child, control.child, "the full id, including its call suffix, must reach the same control path")
			require.Equal(t, "parent", control.caller)
		})
	}
}
