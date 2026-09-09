package tools

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/PHPCraftdream/rush/internal/platform"
	"github.com/stretchr/testify/require"
)

func TestGlobRipgrepProcessHelper(t *testing.T) {
	if os.Getenv("RUSH_GLOB_TEST_HELPER") != "1" {
		return
	}
	switch os.Getenv("RUSH_GLOB_TEST_SCENARIO") {
	case "many":
		for i := range 160 {
			name := strings.Repeat("x", (i*17)%97) + fmt.Sprintf("-%03d.txt", i)
			_, _ = fmt.Fprintf(os.Stdout, "dir/%s\x00", name)
		}
	case "oversized":
		_, _ = os.Stdout.Write([]byte(strings.Repeat("x", maxRipgrepGlobRecordBytes+1)))
		_, _ = os.Stdout.Write([]byte("\x00after.txt\x00"))
	case "incomplete":
		_, _ = os.Stdout.Write([]byte("missing-nul.txt"))
	case "diagnostic":
		_, _ = os.Stderr.Write([]byte(strings.Repeat("d", maxRipgrepDiagnosticBytes+4096)))
		os.Exit(2)
	case "cancel":
		_, _ = os.Stdout.Write([]byte("started.txt\x00"))
		select {}
	default:
		os.Exit(3)
	}
	os.Exit(0)
}

func globTestCommand(ctx context.Context, scenario string) *exec.Cmd {
	cmd := platform.Command(ctx, os.Args[0], "-test.run=TestGlobRipgrepProcessHelper", "--")
	cmd.Env = append(os.Environ(),
		"RUSH_GLOB_TEST_HELPER=1",
		"RUSH_GLOB_TEST_SCENARIO="+scenario,
	)
	return cmd
}

func TestRunRipgrepBoundedRetainsShortestStableSet(t *testing.T) {
	root := t.TempDir()
	const limit = 7
	cmd := globTestCommand(t.Context(), "many")

	got, truncated, err := runRipgrepBounded(t.Context(), cmd, root, limit)
	require.NoError(t, err)
	require.True(t, truncated)
	require.Len(t, got, limit)

	want := make([]string, 160)
	for i := range want {
		name := strings.Repeat("x", (i*17)%97) + fmt.Sprintf("-%03d.txt", i)
		want[i] = filepath.Join(root, "dir", name)
	}
	sort.SliceStable(want, func(i, j int) bool { return len(want[i]) < len(want[j]) })
	require.Equal(t, want[:limit], got)
}

func TestRunRipgrepBoundedZeroLimitRemainsUnlimited(t *testing.T) {
	got, truncated, err := runRipgrepBounded(
		t.Context(), globTestCommand(t.Context(), "many"), t.TempDir(), 0,
	)
	require.NoError(t, err)
	require.Len(t, got, 160)
	require.False(t, truncated)
}

func TestRunRipgrepBoundedRejectsIncompleteAndOversizedRecords(t *testing.T) {
	for _, scenario := range []string{"incomplete", "oversized"} {
		t.Run(scenario, func(t *testing.T) {
			cmd := globTestCommand(t.Context(), scenario)
			got, truncated, err := runRipgrepBounded(t.Context(), cmd, t.TempDir(), 10)
			require.Error(t, err)
			require.Empty(t, got)
			require.False(t, truncated)
			require.ErrorIs(t, err, ErrGlobRipgrepOutput)
			if scenario == "incomplete" {
				require.ErrorIs(t, err, ErrGlobRipgrepIncompleteRecord)
			} else {
				require.ErrorIs(t, err, ErrGlobRipgrepRecordTooLong)
			}
		})
	}
}

func TestRunRipgrepBoundedCapsDiagnostics(t *testing.T) {
	cmd := globTestCommand(t.Context(), "diagnostic")
	_, _, err := runRipgrepBounded(t.Context(), cmd, t.TempDir(), 10)
	require.Error(t, err)
	require.LessOrEqual(t, strings.Count(err.Error(), "d"), maxRipgrepDiagnosticBytes)
}

func TestRunRipgrepBoundedCancellationReapsProcess(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	started := make(chan struct{})
	ctx = context.WithValue(ctx, globRipgrepRecordHookKey{}, func(string) { close(started) })
	cmd := globTestCommand(ctx, "cancel")
	type result struct {
		err error
	}
	done := make(chan result, 1)
	go func() {
		_, _, err := runRipgrepBounded(ctx, cmd, t.TempDir(), 10)
		done <- result{err: err}
	}()
	select {
	case <-started:
		cancel()
	case <-time.After(10 * time.Second):
		t.Fatal("child did not produce its deterministic started event")
	}
	select {
	case got := <-done:
		require.ErrorIs(t, got.err, context.Canceled)
	case <-time.After(10 * time.Second):
		t.Fatal("cancellation did not reap the child")
	}
}
