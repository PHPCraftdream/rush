package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"charm.land/fantasy"
	"github.com/PHPCraftdream/rush/internal/platform"
	"github.com/stretchr/testify/require"
)

// runCommandHelperLevel1Arg/Level2Arg select this test binary's "leaky
// process tree" helper modes (see TestMain) instead of the normal test
// suite -- the standard Go idiom for reproducing real OS-level process/
// handle-inheritance behavior deterministically. Passed as a plain argv
// element (not an env var, unlike internal/shell/exec_windows_test.go's
// equivalent) because run_command has no shell and no env-var parameter --
// only Program/Args reach the child.
const (
	runCommandHelperLevel1Arg = "__run_command_test_helper_level1__"
	runCommandHelperLevel2Arg = "__run_command_test_helper_level2__"
)

func TestMain(m *testing.M) {
	if len(os.Args) == 2 {
		switch os.Args[1] {
		case runCommandHelperLevel1Arg:
			runRunCommandLevel1Helper()
			return
		case runCommandHelperLevel2Arg:
			runRunCommandLevel2Helper()
			return
		}
	}
	os.Exit(m.Run())
}

// runRunCommandLevel1Helper models the direct child run_command spawns. It
// forks a grandchild (level 2) that inherits its stdout without waiting for
// it, then stays alive until killed -- exactly like a build tool or
// installer spawning a detached helper.
func runRunCommandLevel1Helper() {
	self, err := os.Executable()
	if err != nil {
		os.Exit(1)
	}
	cmd := platform.Command(context.Background(), self, runCommandHelperLevel2Arg)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		os.Exit(1)
	}
	// Fire-and-forget: do NOT Wait() on the grandchild -- it must outlive
	// this process's own death exactly like an orphaned worker would.
	fmt.Println("level1-alive")
	time.Sleep(30 * time.Second)
}

// runRunCommandLevel2Helper models the orphaned grandchild: it inherited
// level 1's stdout handle (run_command's own live output buffer, once
// piped through) and keeps it open long after level 1 is gone.
func runRunCommandLevel2Helper() {
	fmt.Println("grandchild-alive")
	time.Sleep(60 * time.Second)
}

// TestRunCommand_CtxCancelTreeKillsGrandchild proves job_kill/timeout
// cancellation (task #1023 §3, configureRunCommandProcess) tree-kills a
// grandchild the direct child spawned, instead of hanging on its inherited
// stdout pipe. Before this fix os/exec's default ctx-cancellation Cancel
// hook only signals the DIRECT child; the still-alive grandchild keeps its
// inherited copy of run_command's stdout pipe open, so cmd.Wait() would
// never unblock on its own -- this test would hang for the grandchild's
// full 60s sleep against the old (unfixed) behavior instead of returning
// promptly.
func TestRunCommand_CtxCancelTreeKillsGrandchild(t *testing.T) {
	t.Parallel()

	self, err := os.Executable()
	require.NoError(t, err)

	dir := t.TempDir()
	tool := newRunCommandToolForTest(t, dir)

	callerCtx, cancel := context.WithCancel(context.Background())
	ctx := context.WithValue(callerCtx, SessionIDContextKey, "test-session")

	var mu sync.Mutex
	var buf LiveOutputBuffer
	ctx = WithLiveOutputSink(ctx, func(b LiveOutputBuffer) {
		mu.Lock()
		buf = b
		mu.Unlock()
	})

	input, err := json.Marshal(RunCommandParams{Program: self, Args: []string{runCommandHelperLevel1Arg}})
	require.NoError(t, err)

	type result struct {
		resp fantasy.ToolResponse
		err  error
	}
	done := make(chan result, 1)
	go func() {
		resp, runErr := tool.Run(ctx, fantasy.ToolCall{ID: "c1", Name: RunCommandToolName, Input: string(input)})
		done <- result{resp, runErr}
	}()

	// Wait until BOTH helper levels have actually reported alive before
	// cancelling -- otherwise the tree might not exist yet, which wouldn't
	// reproduce the bug. Reads the live output buffer directly (task #1023
	// §3's sink), the same mechanism job_output uses against a real ledger.
	require.Eventually(t, func() bool {
		mu.Lock()
		b := buf
		mu.Unlock()
		if b == nil {
			return false
		}
		s := b.String()
		return strings.Contains(s, "level1-alive") && strings.Contains(s, "grandchild-alive")
	}, 5*time.Second, 50*time.Millisecond, "helper processes did not report alive")

	start := time.Now()
	cancel()

	select {
	case <-done:
	case <-time.After(30 * time.Second):
		t.Fatal("run_command did not return after ctx cancel -- grandchild likely still holding the stdout pipe open (tree-kill not wired)")
	}
	elapsed := time.Since(start)
	// Prompt tree-kill normally returns in well under 2s; 15s is a loose
	// bound distinguishing "prompt" from the broken direct-child-only kill,
	// which would hang for the grandchild's full 60s sleep instead.
	require.Less(t, elapsed, 15*time.Second, "ctx cancellation must tree-kill, not hang on the grandchild's inherited stdout")
}
