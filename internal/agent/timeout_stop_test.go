package agent

import (
	"strings"
	"testing"
)

func TestWatchdogResumeGuidance(t *testing.T) {
	t.Run("tool timeout", func(t *testing.T) {
		guidance := WatchdogResumeGuidance("sess-123", "--timeout")

		if !strings.Contains(guidance, "rush run --session sess-123 --timeout <larger-value>") {
			t.Errorf("guidance %q must contain a ready-to-run resume command with the session id and flag substituted in", guidance)
		}
		if !strings.Contains(strings.ToLower(guidance), "not a crash") {
			t.Errorf("guidance %q must clarify this is an intentional stop, not a crash", guidance)
		}
	})

	t.Run("hard cap uses its own flag", func(t *testing.T) {
		guidance := WatchdogResumeGuidance("sess-456", "--timeout-hard-cap")

		if !strings.Contains(guidance, "rush run --session sess-456 --timeout-hard-cap <larger-value>") {
			t.Errorf("guidance %q must suggest raising the flag that actually fired, not a different one", guidance)
		}
		if strings.Contains(guidance, " --timeout ") {
			t.Errorf("guidance %q must not also mention the plain --timeout flag", guidance)
		}
	})
}
