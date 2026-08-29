package log

import (
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestNewLogger_DoesNotTouchDefault is the phase-3 contract test: NewLogger
// must be a pure constructor. Unlike Setup, it must never call
// slog.SetDefault — a library consumer invoking NewLogger must not hijack
// the host program's default logger (and tests in this process must not
// mutate global state either, which is why Setup is never called here).
func TestNewLogger_DoesNotTouchDefault(t *testing.T) {
	t.Parallel()

	captured := slog.Default()
	logger := NewLogger(filepath.Join(t.TempDir(), "test.log"), false)
	require.NotNil(t, logger)

	require.Same(t, captured, slog.Default(),
		"NewLogger must not replace slog.Default() — that is Setup's job")
}

// TestNewLogger_WritesToFileWithPID proves the returned logger actually
// works: it writes JSON records with the message and the pid attribute to
// the rotating log file, mirroring what Setup used to build inline.
func TestNewLogger_WritesToFileWithPID(t *testing.T) {
	t.Parallel()

	// Not t.TempDir: lumberjack keeps the log file open for the life of the
	// logger, so Windows cannot unlink it during cleanup. Best-effort removal
	// instead, ignoring the inevitable sharing violation.
	dir, err := os.MkdirTemp("", "rush-log-test")
	require.NoError(t, err)
	defer func() { _ = os.RemoveAll(dir) }()

	logPath := filepath.Join(dir, "test.log")
	logger := NewLogger(logPath, false)
	logger.Info("phase three hello")

	bts, err := os.ReadFile(logPath)
	require.NoError(t, err, "lumberjack must have created the log file on first write")

	line := strings.TrimSpace(string(bts))
	require.NotEmpty(t, line)

	var record map[string]any
	require.NoError(t, json.Unmarshal([]byte(line), &record), "record must be valid JSON, got: %s", line)
	assert.Equal(t, "phase three hello", record["msg"])
	assert.Equal(t, float64(os.Getpid()), record["pid"],
		"every entry must be tagged with the process pid, got: %s", line)
	assert.Contains(t, line, strconv.Quote("level")+":",
		"the JSON handler shape from Setup must be preserved")
}
