package app

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/PHPCraftdream/rush/internal/agent"
	"github.com/stretchr/testify/assert"
)

// The --json envelope's error must carry the operator's peak-hours message,
// both for a pre-flight refusal and for a mid-turn stop (whose run error is
// the watcher's *PeakHoursError, wrapped by the run loop).
func TestBuildRunResult_PeakHoursErrorCarriesOperatorMessage(t *testing.T) {
	pe := &agent.PeakHoursError{
		ProviderID: "zai",
		Start:      "20:00",
		End:        "23:59",
		ReopensAt:  time.Date(2026, 9, 23, 23, 59, 0, 0, time.Local),
		Message:    "OPERATOR-NOTE: quota exhausted, resume after midnight",
	}
	cases := map[string]error{
		"pre-flight refusal": fmt.Errorf("failed to start agent processing stream: %w", pe),
		"mid-turn stop":      pe,
	}
	for name, runErr := range cases {
		t.Run(name, func(t *testing.T) {
			res := buildRunResult("s1", "", "", "", runErr, false, nil, 0, 0, time.Second, "", "", 0, "", "", nil, "")
			assert.Contains(t, res.Error, "is in peak hours (", "the matchable refusal text must stay")
			assert.Contains(t, res.Error, "RESUME AT:")
			assert.Contains(t, res.Error, pe.Message)
		})
	}

	t.Run("finish details already holding the guidance are not duplicated", func(t *testing.T) {
		guidance := agent.PeakHoursGuidance(pe)
		res := buildRunResult("s1", "", "", "error", pe, false, nil, 0, 0, time.Second, "Stopped", pe.Error()+"\n\n"+guidance, 0, "", "", nil, "")
		assert.Equal(t, 1, strings.Count(res.Error, pe.Message))
	})

	t.Run("non-peak errors are untouched", func(t *testing.T) {
		res := buildRunResult("s1", "", "", "", context.DeadlineExceeded, false, nil, 0, 0, time.Second, "", "", 0, "", "", nil, "")
		assert.NotContains(t, res.Error, "RESUME AT:")
	})
}
