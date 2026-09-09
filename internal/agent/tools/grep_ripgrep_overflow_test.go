package tools

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/PHPCraftdream/rush/internal/platform"
	"github.com/stretchr/testify/require"
)

func TestRipgrepSearchProcessHelper(t *testing.T) {
	if os.Getenv("RUSH_RIPGREP_TEST_HELPER") != "1" {
		return
	}

	path := os.Getenv("RUSH_RIPGREP_TEST_PATH")
	scenario := os.Getenv("RUSH_RIPGREP_TEST_SCENARIO")
	writeLine := func(text string) {
		_, _ = os.Stdout.Write(testRipgrepJSONLine(path, text))
	}

	switch scenario {
	case "first-overflow", "overflow-exit-1", "overflow-exit-2":
		writeLine(strings.Repeat("x", 2048))
	case "valid-then-overflow":
		writeLine("valid match")
		writeLine(strings.Repeat("x", 2048))
	case "drain":
		writeLine(strings.Repeat("x", 2048))
		chunk := strings.Repeat("x", 8*1024)
		for range 64 {
			_, _ = os.Stdout.Write([]byte(chunk))
		}
	case "valid-exit-1":
		writeLine("valid match")
	case "fs-overflow-drain":
		writeOversizedFSGrepJSONLine(path)
		chunk := strings.Repeat("x", 16*1024)
		for range 8 {
			_, _ = os.Stdout.Write([]byte(chunk))
		}
	case "fs-stderr":
		chunk := strings.Repeat("d", 4096)
		remaining := maxRipgrepDiagnosticBytes + 1
		for remaining > 0 {
			n := min(remaining, len(chunk))
			_, _ = os.Stderr.Write([]byte(chunk[:n]))
			remaining -= n
		}
		os.Exit(2)
	default:
		os.Exit(3)
	}

	if scenario == "overflow-exit-1" || scenario == "valid-exit-1" {
		os.Exit(1)
	}
	if scenario == "overflow-exit-2" {
		os.Exit(2)
	}
}

func writeOversizedFSGrepJSONLine(path string) {
	pathJSON, _ := json.Marshal(path)
	prefix := fmt.Sprintf(`{"type":"match","data":{"path":{"text":%s},"lines":{"text":"`, pathJSON)
	suffix := `"},"line_number":1}}
` // The newline is part of the JSON stream, not the line text.
	_, _ = os.Stdout.Write([]byte(prefix))
	remaining := maxRipgrepJSONLineBytes + 2 - len(prefix) - len(suffix)
	chunk := strings.Repeat("x", 64*1024)
	for remaining > 0 {
		n := min(remaining, len(chunk))
		_, _ = os.Stdout.Write([]byte(chunk[:n]))
		remaining -= n
	}
	_, _ = os.Stdout.Write([]byte(suffix))
}

func testRipgrepJSONLine(path, text string) []byte {
	pathJSON, _ := json.Marshal(path)
	textJSON, _ := json.Marshal(text)
	return []byte(fmt.Sprintf(
		`{"type":"match","data":{"path":{"text":%s},"lines":{"text":%s},"line_number":1,"submatches":[{"start":0}]}}`+"\n",
		pathJSON, textJSON))
}

func testRipgrepCommand(ctx context.Context, scenario, path string) *exec.Cmd {
	cmd := platform.Command(ctx, os.Args[0], "-test.run=TestRipgrepSearchProcessHelper", "--")
	cmd.Env = append(os.Environ(),
		"RUSH_RIPGREP_TEST_HELPER=1",
		"RUSH_RIPGREP_TEST_SCENARIO="+scenario,
		"RUSH_RIPGREP_TEST_PATH="+path,
	)
	return cmd
}

func runRipgrepOverflowScenario(ctx context.Context, t *testing.T, scenario string) ([]grepMatch, bool, error) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "match.txt")
	require.NoError(t, os.WriteFile(path, []byte("needle\n"), 0o644))
	cmd := testRipgrepCommand(ctx, scenario, path)
	return searchWithRipgrepCommand(cmd, 100, 256)
}

func TestSearchWithRipgrepRejectsFirstOversizedJSONMatch(t *testing.T) {
	matches, truncated, err := runRipgrepOverflowScenario(t.Context(), t, "first-overflow")

	var outputErr *RipgrepJSONError
	require.ErrorAs(t, err, &outputErr)
	require.ErrorIs(t, err, ErrRipgrepJSONOutput)
	require.ErrorIs(t, err, ErrRipgrepJSONOutputTooLong)
	require.ErrorIs(t, err, bufio.ErrTooLong)
	require.Empty(t, matches)
	require.False(t, truncated)
}

func TestSearchWithRipgrepRejectsOverflowAfterValidMatches(t *testing.T) {
	matches, truncated, err := runRipgrepOverflowScenario(t.Context(), t, "valid-then-overflow")

	require.ErrorIs(t, err, ErrRipgrepJSONOutputTooLong)
	require.Empty(t, matches, "overflow must not return partial matches")
	require.False(t, truncated)
}

func TestSearchWithRipgrepDrainsAfterScannerOverflow(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	_, _, err := runRipgrepOverflowScenario(ctx, t, "drain")
	require.ErrorIs(t, err, ErrRipgrepJSONOutputTooLong)
	require.NoError(t, ctx.Err(), "the deadlock safety fence must not expire")
}

func TestSearchWithRipgrepPreservesExitCodePrecedence(t *testing.T) {
	t.Run("non-one-exit-wins-over-scanner-error", func(t *testing.T) {
		_, _, err := runRipgrepOverflowScenario(t.Context(), t, "overflow-exit-2")
		var exitErr *exec.ExitError
		require.ErrorAs(t, err, &exitErr)
		require.Equal(t, 2, exitErr.ExitCode())
		require.NotErrorIs(t, err, ErrRipgrepJSONOutput)
	})

	t.Run("exit-one-does-not-hide-scanner-error", func(t *testing.T) {
		_, _, err := runRipgrepOverflowScenario(t.Context(), t, "overflow-exit-1")
		require.ErrorIs(t, err, ErrRipgrepJSONOutputTooLong)
	})

	t.Run("exit-one-still-means-no-process-error", func(t *testing.T) {
		matches, truncated, err := runRipgrepOverflowScenario(t.Context(), t, "valid-exit-1")
		require.NoError(t, err)
		require.Len(t, matches, 1)
		require.False(t, truncated)
	})
}
