// Chain-guard e2e gates (app_run_chain_guard_e2e_test.go's withW scenario):
// explicit, event-driven staging helpers. Every background command the
// fixture launches -- W, timer 1, timer 2 -- is sentinel-held from the
// launch, and a release file is written only once the provider has served
// the request the staging depends on. The first version staggered the timers
// with `sleep 4` / `sleep 8` and released W after a fixed count of provider
// requests; batch bash has a REAL 3s inline window (batch_delivery.go,
// wired in batch_install_tools.go), calls of one assistant message run
// sequentially, and production pulls completion notices before every
// provider step -- so a time-based timer could complete inside the launch
// turn, whose unconditional closing answer then reacted it away, and the
// counted request never appeared (shadow-pp-118511110443's 90s deadline).
package app

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"sync"
	"testing"
)

// The completion markers the gates key on. Both delivery formats put a job's
// output into the notice text, so a timer command that echoes its marker
// makes its completion notice identifiable in the drain request's user
// content; W-DONE is the held job's marker. cgRefused is a stable substring
// of the in-turn guard's wait refusal (refuseWaitText).
const (
	cgTick1   = "CG-TICK-1"
	cgTick2   = "CG-TICK-2"
	cgWDone   = "W-DONE"
	cgRefused = "does not wait"
)

// The phase machine's states, in the order the requests must arrive.
const (
	cgPhaseClose = iota // launch turn's closing step: NO completion consumed yet
	cgPhaseLeg1Open
	cgPhaseLeg1Close
	cgPhaseLeg2Open
	cgPhaseLeg2Close
	cgPhaseFinal
)

// cgHeldCommand returns a sentinel-held command whose completion carries
// marker, plus the release file path that unblocks it: the command loops on
// the file's absence (recoveryJobCommand's Windows-safe poll) and echoes the
// marker once released, so its completion is deterministic in both
// directions and cannot happen before the fixture writes the file.
func cgHeldCommand(t *testing.T, marker string) (command, release string) {
	t.Helper()
	command, release = recoveryJobCommand(t)
	return command + "; echo " + marker, release
}

// cgReleaseFile writes release once so the held job finishes. It runs on the
// provider handler's goroutine, so it reports with t.Errorf (safe off the
// test goroutine) instead of require, and the caller's sync.Once makes a
// retry a no-op instead of a double write.
func cgReleaseFile(t *testing.T, release string, once *sync.Once) {
	t.Helper()
	once.Do(func() {
		if err := os.WriteFile(release, []byte("go"), 0o644); err != nil {
			t.Errorf("release %s: %v", release, err)
		}
	})
}

// cgRequestTexts reads the provider request's wire body and returns its LAST
// user message, its FULL user history (the launch turn's closing step uses
// it to prove NO completion was consumed anywhere in the turn -- the one
// phase that must look backward), and the LAST tool result. Every other
// phase matches on the last user message / last tool result only, so it
// proves the intended completion was consumed in THIS request.
func cgRequestTexts(r *http.Request) (lastUser, allUsers, lastTool string, err error) {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		return "", "", "", fmt.Errorf("read provider request: %w", err)
	}
	var req wireRequest
	if json.Unmarshal(body, &req) != nil {
		return "", "", "", fmt.Errorf("unparsable provider request body")
	}
	var u strings.Builder
	for _, m := range req.Messages {
		s, _ := m.Content.(string)
		switch m.Role {
		case "user":
			u.WriteString(s)
			u.WriteString("\n")
			lastUser = s
		case "tool":
			lastTool = s
		}
	}
	return lastUser, u.String(), lastTool, nil
}
