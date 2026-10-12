package shell

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/PHPCraftdream/rush/internal/session"
	"github.com/stretchr/testify/require"
)

func TestRetainedShebangHelper(t *testing.T) {
	if os.Getenv("RUSH_SHEBANG_RETAINED_HELPER") != "1" {
		return
	}
	fmt.Printf("ready:%d\n", os.Getpid())
	// Workload: hold inherited output open until physical cancellation.
	<-time.NewTimer(time.Minute).C
}

func retainedShebangReady(t *testing.T) (*BackgroundExecution, int) {
	t.Helper()
	if _, err := resolveInterpreter("bash"); err != nil {
		t.Skipf("bash unavailable: %v", err)
	}
	self, err := os.Executable()
	require.NoError(t, err)
	dir := t.TempDir()
	script := writeScript(t, dir, "retained.sh", "#!/usr/bin/env bash\nRUSH_SHEBANG_RETAINED_HELPER=1 '"+strings.ReplaceAll(self, "\\", "/")+"' -test.run=^TestRetainedShebangHelper$ &\nwait\n")
	manager := NewBackgroundShellManager()
	e, err := manager.StartRetainedOwned(context.Background(), "owner", dir, nil, "'"+script+"'", "")
	require.NoError(t, err)
	var pid int
	t.Cleanup(func() {
		if pid > 0 {
			_ = session.KillProcess(pid)
		}
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		manager.Close(ctx)
		select {
		case <-e.Done():
			_ = e.Dispose(context.Background())
		case <-ctx.Done():
			t.Error("shebang cleanup did not join Done")
		}
	})
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	var text string
	var cursor uint64
	for !strings.Contains(text, "\n") {
		c, err := e.ReadOutput(ctx, cursor)
		require.NoError(t, err)
		text += string(c.Data)
		cursor = c.Next
	}
	line := strings.Split(text, "\n")[0]
	require.True(t, strings.HasPrefix(line, "ready:"), text)
	pid, err = strconv.Atoi(strings.TrimPrefix(line, "ready:"))
	require.NoError(t, err)
	return e, pid
}

func TestRetainedShebangCancellationRegistersAndCompletes(t *testing.T) {
	e, pid := retainedShebangReady(t)
	e.physical.mu.Lock()
	registered := len(e.physical.entries)
	var kills atomic.Int32
	for p := range e.physical.entries {
		p.mu.Lock()
		original := p.kill
		p.kill = func() error { kills.Add(1); return original() }
		p.mu.Unlock()
	}
	e.physical.mu.Unlock()
	require.Positive(t, registered, "SHEBANG_BYPASSED_RETAINED_REGISTRATION")
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	require.NoError(t, e.Stop(ctx))
	require.Positive(t, kills.Load(), "SHEBANG_ACK_BEFORE_PHYSICAL_KILL")
	select {
	case <-e.Done():
	case <-ctx.Done():
		t.Fatal("SHEBANG_STOP_ACK_WITHOUT_DONE")
	}
	// SIGKILL to the process group is posted by the ack, but the grandchild's
	// exit (and its reaping by init) is asynchronous: bound the wait instead of
	// sampling a zombie that is about to disappear.
	require.Eventually(t, func() bool { return !session.IsProcessAlive(pid) },
		10*time.Second, 10*time.Millisecond, "SHEBANG_CHILD_SURVIVED_ACK")
}

func TestRetainedShebangPhysicalKillError(t *testing.T) {
	e, pid := retainedShebangReady(t)
	physicalErr := errors.New("shebang physical kill refused")
	e.physical.mu.Lock()
	for p := range e.physical.entries {
		p.mu.Lock()
		original := p.kill
		t.Cleanup(func() { p.mu.Lock(); p.kill = original; p.mu.Unlock() })
		p.kill = func() error { return physicalErr }
		p.mu.Unlock()
	}
	e.physical.mu.Unlock()
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	require.ErrorIs(t, e.Stop(ctx), physicalErr, "SHEBANG_KILL_ERROR_MASKED")
	select {
	case <-e.Done():
		t.Error("SHEBANG_ERROR_SYNTHESIZED_DONE")
	default:
	}
	require.True(t, session.IsProcessAlive(pid), "failed physical kill must leave helper alive")
}

func TestRetainedShebangNonzeroExit(t *testing.T) {
	if _, err := resolveInterpreter("bash"); err != nil {
		t.Skipf("bash unavailable: %v", err)
	}
	dir := t.TempDir()
	script := writeScript(t, dir, "exit.sh", "#!/usr/bin/env bash\nexit 7\n")
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	manager := NewBackgroundShellManager()
	e, err := manager.StartRetainedOwned(ctx, "owner", dir, nil, "'"+script+"'", "")
	require.NoError(t, err)
	select {
	case <-e.Done():
	case <-ctx.Done():
		t.Fatal("shebang exit did not complete")
	}
	_, _, _, err = e.GetOutput()
	require.Equal(t, 7, ExitCode(err))
	require.NoError(t, e.Dispose(context.Background()))
	manager.Close(context.Background())
}
