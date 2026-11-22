package subagents

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

func TestBackgroundJobSummary(t *testing.T) {
	t.Parallel()

	identity := func(s string) string { return s }
	t.Run("with stdout", func(t *testing.T) {
		t.Parallel()
		got := BackgroundJobSummary("00A", "echo hi && make build", "hello world", "", 0, 42*time.Second, identity)
		assert.Contains(t, got, "00A")
		assert.Contains(t, got, "`echo hi && make build`")
		assert.Contains(t, got, "exit 0")
		assert.Contains(t, got, "42s")
		assert.Contains(t, got, "hello world")
	})

	t.Run("exit code and stderr surfaced", func(t *testing.T) {
		t.Parallel()
		got := BackgroundJobSummary("00B", "make test", "", "boom: tests failed", 2, 90*time.Second, identity)
		assert.Contains(t, got, "exit 2")
		assert.Contains(t, got, "1m30s")
		assert.Contains(t, got, "boom: tests failed")
	})

	t.Run("no output falls back to placeholder", func(t *testing.T) {
		t.Parallel()
		got := BackgroundJobSummary("00C", "true", "  \n ", "", 0, 3*time.Second, identity)
		assert.Contains(t, got, "(no output)")
	})

	t.Run("both stdout and stderr are joined", func(t *testing.T) {
		t.Parallel()
		got := BackgroundJobSummary("00D", "go test ./...", "ok pkg 0.1s", "warn: deprecated", 0, 5*time.Second, identity)
		assert.Contains(t, got, "ok pkg 0.1s")
		assert.Contains(t, got, "warn: deprecated")
	})

	t.Run("truncation callback receives joined output", func(t *testing.T) {
		t.Parallel()
		got := BackgroundJobSummary("00E", "run", "out", "err", 0, 0, func(s string) string { return "truncated:" + s })
		assert.Contains(t, got, "truncated:out\nerr")
	})
}
