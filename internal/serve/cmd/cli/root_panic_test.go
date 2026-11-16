package cli

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestWebReadyBannerUsesReadableSeparator(t *testing.T) {
	for _, copied := range []bool{false, true} {
		banner := webReadyBanner("http://localhost:8080", "test-token", copied)
		require.Contains(t, banner, "rush web UI  ->  http://localhost:8080")
		require.Contains(t, banner, "Access token  ->  test-token")
		require.True(t, isASCII(banner))
		require.Equal(t, copied, strings.Contains(banner, "(copied to clipboard)"))
	}
}

func isASCII(value string) bool {
	for _, char := range value {
		if char > 127 {
			return false
		}
	}
	return true
}

func TestRecoverAndLogPanic_LogsBeforeRePanicking(t *testing.T) {
	prevLogger := slog.Default()
	t.Cleanup(func() { slog.SetDefault(prevLogger) })
	var logBuf bytes.Buffer
	slog.SetDefault(slog.New(slog.NewTextHandler(&logBuf, &slog.HandlerOptions{Level: slog.LevelDebug})))

	const panicValue = "boom: simulated top-level panic"
	var rePanicked any
	func() {
		defer func() { rePanicked = recover() }()
		func() {
			defer recoverAndLogPanic()
			panic(panicValue)
		}()
	}()

	require.Equal(t, panicValue, rePanicked, "recoverAndLogPanic must re-panic with the exact same value, unchanged")
	logged := logBuf.String()
	require.Contains(t, logged, crashLogMarker, "the panic must be logged under the fixed crashLogMarker so sessions_why's hint stays accurate")
	require.Contains(t, logged, panicValue, "the actual panic value must appear in the logged line")
	require.Contains(t, logged, "goroutine", "a real stack trace (runtime/debug.Stack output) must be logged")
}

func TestRecoverAndLogPanic_NoPanicIsANoop(t *testing.T) {
	prevLogger := slog.Default()
	t.Cleanup(func() { slog.SetDefault(prevLogger) })
	var logBuf bytes.Buffer
	slog.SetDefault(slog.New(slog.NewTextHandler(&logBuf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	func() { defer recoverAndLogPanic() }()
	require.False(t, strings.Contains(logBuf.String(), crashLogMarker), "recoverAndLogPanic must not log anything when nothing panicked")
}
