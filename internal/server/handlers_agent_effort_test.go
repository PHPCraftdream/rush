package server

import (
	"context"
	"encoding/json"
	"testing"

	"charm.land/fantasy"
	"github.com/PHPCraftdream/rush/internal/agent"
	"github.com/PHPCraftdream/rush/internal/app"
	"github.com/PHPCraftdream/rush/internal/message"
	"github.com/stretchr/testify/require"
)

type effortCaptureCoordinator struct {
	fakeAlwaysBusyCoordinator
	smart, fast *agent.ModelOverride
}

func (c *effortCaptureCoordinator) RunWithOverrides(_ context.Context, _, _ string, smart, fast *agent.ModelOverride, _ ...message.Attachment) (*fantasy.AgentResult, error) {
	c.smart, c.fast = smart, fast
	return nil, nil
}

func (c *effortCaptureCoordinator) InterruptAndSend(_ context.Context, _, _ string, smart, fast *agent.ModelOverride, _ ...message.Attachment) error {
	c.smart, c.fast = smart, fast
	return nil
}
func (c *effortCaptureCoordinator) IsSessionBusy(string) bool { return false }

func TestWebAgentHandlersPassReasoningEffort(t *testing.T) {
	t.Parallel()
	payload := SendMessagePayload{
		SessionID: "effort", Content: "prompt",
		SmartModel: &ModelOverrideWire{Provider: "smart", Model: "smart-model", ReasoningEffort: "high"},
		FastModel:  &ModelOverrideWire{Provider: "fast", Model: "fast-model", ReasoningEffort: "low"},
	}
	for _, tc := range []struct {
		name string
		run  func(context.Context, *app.App, *Client, WSMessage)
	}{
		{"send", handleSendMessage},
		{"interrupt", func(ctx context.Context, a *app.App, c *Client, m WSMessage) { handleInterruptAndSend(ctx, a, c, m) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			coordinator := &effortCaptureCoordinator{}
			a := &app.App{AgentCoordinator: coordinator}
			raw, err := json.Marshal(payload)
			require.NoError(t, err)
			tc.run(t.Context(), a, newTestClient(), WSMessage{ID: "effort", Payload: raw})
			require.NotNil(t, coordinator.smart)
			require.NotNil(t, coordinator.fast)
			require.Equal(t, "high", coordinator.smart.ReasoningEffort)
			require.Equal(t, "low", coordinator.fast.ReasoningEffort)
			require.Equal(t, "smart-model", coordinator.smart.Model)
			require.Equal(t, "fast-model", coordinator.fast.Model)
		})
	}
}
