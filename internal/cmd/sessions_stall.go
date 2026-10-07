package cmd

// Phase 0 stall detection (sessions why / locks): surface a stale lock
// heartbeat ("no activity pulse") and open tool calls from data that
// already exists -- the lock file mtime and persisted message parts.

import (
	"context"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/PHPCraftdream/rush/internal/app"
	"github.com/PHPCraftdream/rush/internal/heartbeat"
	"github.com/PHPCraftdream/rush/internal/message"
	"github.com/PHPCraftdream/rush/internal/session"
)

// stallPulseThreshold is the heartbeat age past which a live-holder session
// is reported as STALLED (task context: a turn hung with tools in flight
// while `sessions why` kept saying "running").
const stallPulseThreshold = 10 * time.Minute

// readOnlyToolNames is the local allowlist of clearly read-only tools whose
// arguments may be printed verbatim in stall output; every other tool
// (bash, edit, write, ...) keeps its arguments hidden.
var readOnlyToolNames = map[string]bool{
	"view":     true,
	"read":     true,
	"grep":     true,
	"glob":     true,
	"ls":       true,
	"fs_find":  true,
	"fs_batch": true,
}

// lockPulseAge returns the age of the session lock heartbeat file's mtime,
// the same PULSE_AGE signal `sessions locks` computes.
func lockPulseAge(dataDir, sessionID string, now time.Time) (time.Duration, bool) {
	st, err := os.Stat(session.SessionLockPath(dataDir, sessionID))
	if err != nil {
		return 0, false
	}
	return now.Sub(st.ModTime()), true
}

// openToolCall is an assistant tool call with no persisted tool result.
type openToolCall struct {
	CallID string
	Name   string
	Input  string
	Age    time.Duration
}

// openToolCallsForWhy lists open tool calls for a session: ToolCall parts
// of assistant messages whose ID never got a matching ToolResult part in
// any persisted message. A read failure means no evidence, so none.
func openToolCallsForWhy(ctx context.Context, a *app.App, sessionID string, now time.Time) []openToolCall {
	msgs, err := a.Messages.List(ctx, sessionID)
	if err != nil {
		return nil
	}
	results := map[string]bool{}
	for _, m := range msgs {
		for _, r := range m.ToolResults() {
			results[r.ToolCallID] = true
		}
	}
	var out []openToolCall
	for _, m := range msgs {
		if m.Role != message.Assistant {
			continue
		}
		base := m.UpdatedAt
		if base == 0 {
			base = m.CreatedAt
		}
		age := now.Sub(time.Unix(base, 0))
		for _, c := range m.ToolCalls() {
			if c.Finished || results[c.ID] {
				continue
			}
			out = append(out, openToolCall{CallID: c.ID, Name: c.Name, Input: c.Input, Age: age})
		}
	}
	return out
}

// formatOpenToolCall renders one open call; non-read-only arguments are
// never printed.
func formatOpenToolCall(tc openToolCall) string {
	age := formatDurationShort(tc.Age)
	if !readOnlyToolNames[tc.Name] {
		return fmt.Sprintf("<%s> <%s> open %s (args hidden)", tc.Name, tc.CallID, age)
	}
	summary := tc.Input
	if len(summary) > 80 {
		summary = summary[:80]
	}
	return fmt.Sprintf("<%s> <%s> open %s %s", tc.Name, tc.CallID, age, summary)
}

// lastMessageAgeForWhy returns "<age> ago" for the most recently persisted
// message, or "" when none exist.
func lastMessageAgeForWhy(ctx context.Context, a *app.App, sessionID string, now time.Time) string {
	msgs, err := a.Messages.List(ctx, sessionID)
	if err != nil || len(msgs) == 0 {
		return ""
	}
	last := msgs[len(msgs)-1]
	base := last.UpdatedAt
	if base == 0 {
		base = last.CreatedAt
	}
	return fmt.Sprintf("%s ago", formatDurationShort(now.Sub(time.Unix(base, 0))))
}

// printStallHeader renders the running-status block of `sessions why`:
// the pulse age and last persisted message time, and -- when the heartbeat
// is stale past stallPulseThreshold while the holder PID is alive -- a
// STALLED header line replacing the plain `status: running` line. Returns
// true when the STALLED line replaced the status line.
func printStallHeader(ctx context.Context, a *app.App, dataDir, sessionID, status string, lock session.LockFact, out io.Writer) bool {
	if status != "running" {
		fmt.Fprintf(out, "status: %s\n", status)
		return false
	}
	now := time.Now()
	pulse, ok := lockPulseAge(dataDir, sessionID, now)
	if !ok {
		fmt.Fprintf(out, "status: %s\n", status)
		return false
	}
	stalled := pulse > stallPulseThreshold && lock.Kind == session.LockHeld
	if stalled {
		opens := openToolCallsForWhy(ctx, a, sessionID, now)
		summary := "no open tool calls"
		if len(opens) > 0 {
			parts := make([]string, len(opens))
			for i, tc := range opens {
				parts[i] = formatOpenToolCall(tc)
			}
			summary = strings.Join(parts, ", ")
		}
		fmt.Fprintf(out, "STALLED %s — no activity pulse; open tools: %s\n", formatDurationShort(pulse), summary)
	} else {
		fmt.Fprintf(out, "status: %s\n", status)
	}
	fmt.Fprintf(out, "pulse: %s ago\n", formatDurationShort(pulse))
	if line := rateLimitLineForWhy(sessionID, now); line != "" && !stalled {
		fmt.Fprint(out, line)
	}
	if last := lastMessageAgeForWhy(ctx, a, sessionID, now); last != "" {
		fmt.Fprintf(out, "last message: %s\n", last)
	} else {
		fmt.Fprintf(out, "last message: (none persisted)\n")
	}
	return stalled
}

// rateLimitLineForWhy scans heartbeat entries for a future rate-limit
// wait stamp on the session; "" when absent, past, or unreadable.
func rateLimitLineForWhy(sessionID string, now time.Time) string {
	entries, err := heartbeat.ReadAll()
	if err != nil {
		return ""
	}
	for _, e := range entries {
		if e.Session != sessionID || e.RateLimitedUntil == "" {
			continue
		}
		t, err := time.Parse(time.RFC3339, e.RateLimitedUntil)
		if err != nil || !t.After(now) {
			continue
		}
		return fmt.Sprintf("rate limit: waiting for provider rate limit until %s\n", t.Local().Format("15:04"))
	}
	return ""
}
