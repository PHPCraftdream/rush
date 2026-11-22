package subagents

import (
	"fmt"
	"strings"
	"time"
)

// filterNonEmpty returns the subset of inputs that are non-empty after
// trimming surrounding whitespace. Used to join stdout/stderr cleanly.
func filterNonEmpty(parts ...string) []string {
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if strings.TrimSpace(p) != "" {
			out = append(out, p)
		}
	}
	return out
}

// BackgroundJobSummary formats a finished background command for injection
// into the owning session. Pure and deterministic so it can be unit-tested
// without a running shell.
func BackgroundJobSummary(id, command string, stdout, stderr string, exitCode int, elapsed time.Duration, truncate func(string) string) string {
	out := strings.TrimSpace(strings.Join(filterNonEmpty(stdout, stderr), "\n"))
	out = truncate(out)
	if out == "" {
		out = "(no output)"
	}
	return fmt.Sprintf("Background job %s (`%s`) finished: exit %d, ran %s.\n\n%s",
		id, command, exitCode, elapsed.Round(time.Second), out)
}
