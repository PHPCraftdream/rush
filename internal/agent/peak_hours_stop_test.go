package agent

import (
	"errors"
	"strings"
	"testing"
	"time"
)

func TestPeakHoursStoppedFinishText(t *testing.T) {
	t.Run("generic error falls back gracefully", func(t *testing.T) {
		underlying := errors.New("provider zai is in peak hours (08:00–12:00), refusing until 12:00")
		msg, details := peakHoursStoppedFinishText(underlying)

		if msg == "" {
			t.Fatal("message must be non-empty — empty message looks identical to a voluntary finish")
		}
		if details == "" {
			t.Fatal("details must be non-empty")
		}
		if !strings.Contains(details, underlying.Error()) {
			t.Errorf("details %q must include the underlying checkPeakHours error verbatim (provider/window/reopen time)", details)
		}
		if !strings.Contains(strings.ToLower(details), "resume") {
			t.Errorf("details %q should instruct the orchestrator to schedule a resume", details)
		}
		if !strings.Contains(strings.ToLower(details), "not a crash") {
			t.Errorf("details %q should clarify this is an intentional stop, not a crash", details)
		}
	})

	t.Run("PeakHoursError yields an exact, cron-ready resume timestamp", func(t *testing.T) {
		reopensAt := time.Date(2026, 7, 8, 12, 0, 0, 0, time.Local)
		err := &PeakHoursError{ProviderID: "zai", Start: "08:00", End: "12:00", ReopensAt: reopensAt}
		_, details := peakHoursStoppedFinishText(err)

		if !strings.Contains(details, "RESUME AT") {
			t.Fatalf("details %q must contain an explicit RESUME AT line an orchestrator can act on", details)
		}
		if !strings.Contains(details, reopensAt.Format(time.RFC3339)) {
			t.Errorf("details %q must contain the exact RFC3339 reopen timestamp for unambiguous cron scheduling, want %s", details, reopensAt.Format(time.RFC3339))
		}
	})
}

func TestPeakHoursGuidance_CustomMessage(t *testing.T) {
	reopensAt := time.Date(2026, 7, 8, 12, 0, 0, 0, time.Local)

	t.Run("custom message is appended after a blank-line separator", func(t *testing.T) {
		err := &PeakHoursError{ProviderID: "zai", Start: "08:00", End: "12:00", ReopensAt: reopensAt, Message: "Ping #ops-oncall before overriding this window."}
		guidance := PeakHoursGuidance(err)

		if !strings.HasSuffix(guidance, "\n\nPing #ops-oncall before overriding this window.") {
			t.Fatalf("guidance must end with a blank-line separator followed by the exact custom message verbatim, got %q", guidance)
		}
	})

	t.Run("empty message leaves guidance unchanged from the no-message case", func(t *testing.T) {
		withoutMessage := &PeakHoursError{ProviderID: "zai", Start: "08:00", End: "12:00", ReopensAt: reopensAt}
		withEmptyMessage := &PeakHoursError{ProviderID: "zai", Start: "08:00", End: "12:00", ReopensAt: reopensAt, Message: ""}

		if PeakHoursGuidance(withoutMessage) != PeakHoursGuidance(withEmptyMessage) {
			t.Fatal("an empty Message must be a no-op, not append a stray blank-line separator")
		}
	})

	t.Run("peakHoursStoppedFinishText carries the custom message through details", func(t *testing.T) {
		err := &PeakHoursError{ProviderID: "zai", Start: "08:00", End: "12:00", ReopensAt: reopensAt, Message: "See runbook RB-42."}
		_, details := peakHoursStoppedFinishText(err)

		if !strings.Contains(details, "See runbook RB-42.") {
			t.Fatalf("details %q must include the custom message (peakHoursStoppedFinishText delegates to PeakHoursGuidance)", details)
		}
	})
}

func TestPeakHoursStoppedFinishText_IncludesOperatorMessage(t *testing.T) {
	pe := &PeakHoursError{ProviderID: "zai", Start: "20:00", End: "23:59", ReopensAt: time.Now().Add(time.Hour), Message: "OPERATOR-NOTE: resume after midnight"}
	_, details := peakHoursStoppedFinishText(pe)
	if !strings.Contains(details, pe.Message) {
		t.Fatalf("mid-turn stop details %q must carry the operator message", details)
	}
}
