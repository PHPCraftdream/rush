package agent

import (
	"log"
	"log/slog"
	"strings"
	"sync"
	"testing"
)

type ownerStopLogCapture struct {
	mu  sync.Mutex
	buf strings.Builder
}

func (c *ownerStopLogCapture) Write(p []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.buf.Write(p)
}

func (c *ownerStopLogCapture) String() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.buf.String()
}

func captureOwnerStopErrors(t *testing.T) *ownerStopLogCapture {
	t.Helper()
	capture := &ownerStopLogCapture{}
	previous := slog.Default()
	prevLogOut, prevLogFlags := log.Writer(), log.Flags()
	slog.SetDefault(slog.New(slog.NewTextHandler(capture, &slog.HandlerOptions{Level: slog.LevelError})))
	t.Cleanup(func() {
		// slog.SetDefault(previous) alone does not undo log's own output
		// when previous is defaultHandler-backed (log/slog/logger.go).
		slog.SetDefault(previous)
		log.SetOutput(prevLogOut)
		log.SetFlags(prevLogFlags)
	})
	return capture
}
