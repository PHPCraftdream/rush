package mcp

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/PHPCraftdream/rush/internal/platform"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/require"
)

const stdioDiagnosticHelperEnv = "RUSH_MCP_STDIO_DIAGNOSTIC_HELPER"

func TestMCPStdioDiagnosticHelper(t *testing.T) {
	switch os.Getenv(stdioDiagnosticHelperEnv) {
	case "argv":
		_, _ = fmt.Fprintf(os.Stdout, "MCPDIAG argc=%d argv=%q\n", len(os.Args), os.Args)
		os.Exit(1)
	case "env-dir":
		_, _ = fmt.Fprintf(os.Stdout, "MCPDIAG env=%q dir=%q\n", os.Getenv("MCP_STDIO_DIAGNOSTIC_VALUE"), mustGetwd())
		os.Exit(1)
	case "exact":
		_, _ = os.Stdout.Write(append([]byte("MCPDIAG exact "), bytesOf('x', stdioDiagnosticMaxOutput-len("MCPDIAG exact "))...))
		os.Exit(1)
	case "overflow":
		_, _ = os.Stdout.Write(append([]byte("MCPDIAG overflow "), bytesOf('x', stdioDiagnosticMaxOutput-len("MCPDIAG overflow ")+1)...))
		os.Exit(1)
	case "streams":
		for range 128 {
			_, _ = os.Stdout.Write([]byte("MCPDIAG stdout\n"))
			_, _ = os.Stderr.Write([]byte("MCPDIAG stderr\n"))
		}
		os.Exit(1)
	case "success":
		_, _ = os.Stdout.Write([]byte("MCPDIAG success\n"))
		os.Exit(0)
	case "failure":
		_, _ = os.Stderr.Write([]byte("MCPDIAG failure\n"))
		os.Exit(1)
	case "wait":
		_, _ = fmt.Fprintln(os.Stdout, "MCPDIAG ready")
		select {}
	}
}

func bytesOf(value byte, count int) []byte {
	return []byte(strings.Repeat(string(value), count))
}

func mustGetwd() string {
	dir, err := os.Getwd()
	if err != nil {
		return "<getwd error>"
	}
	return dir
}

func diagnosticTestCommand(t *testing.T, mode string) *exec.Cmd {
	t.Helper()
	cmd := platform.Command(context.Background(), os.Args[0], "-test.run=^TestMCPStdioDiagnosticHelper$")
	cmd.Env = append(os.Environ(), stdioDiagnosticHelperEnv+"="+mode)
	return cmd
}

func TestStdioDiagnosticCommandPreservesStartupAttributes(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	old := diagnosticTestCommand(t, "env-dir")
	old.Env = append(old.Env, "MCP_STDIO_DIAGNOSTIC_VALUE=preserved")
	old.Dir = dir

	cmd := diagnosticCommand(context.Background(), old)
	require.Equal(t, old.Path, cmd.Path)
	require.Equal(t, old.Args, cmd.Args)
	require.Equal(t, old.Env, cmd.Env)
	require.Equal(t, old.Dir, cmd.Dir)
	require.NotNil(t, cmd.Cancel)
	if cmd.SysProcAttr != nil {
		require.NotSame(t, old.SysProcAttr, cmd.SysProcAttr)
	}

	checkErr := stdioCheck(old)
	require.Error(t, checkErr)
	require.Contains(t, checkErr.Error(), `MCPDIAG env="preserved"`)
	canonicalDir, err := filepath.EvalSymlinks(dir)
	require.NoError(t, err)
	require.Contains(t, checkErr.Error(), fmt.Sprintf(`dir=%q`, filepath.Clean(canonicalDir)))
}

func TestStdioCheckDoesNotDuplicateArgv0(t *testing.T) {
	cmd := diagnosticTestCommand(t, "argv")
	cmd.Args = append(cmd.Args, "-test.v")

	err := stdioCheck(cmd)
	require.Error(t, err)
	require.Contains(t, err.Error(), "MCPDIAG argc=3")
	require.NotContains(t, err.Error(), "MCPDIAG argc=4")
}

func TestStdioDiagnosticCancellationSettlesProcess(t *testing.T) {
	commandCtx, commandCancel := context.WithCancel(context.Background())
	defer commandCancel()
	deadlineCtx, deadlineCancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer deadlineCancel()

	old := diagnosticTestCommand(t, "wait")
	cmd := diagnosticCommand(commandCtx, old)
	requireStdioDiagnosticProcessConfig(t, cmd)
	stdout, err := cmd.StdoutPipe()
	require.NoError(t, err)
	ready := make(chan struct{})
	readerDone := make(chan struct{})
	go func() {
		defer close(readerDone)
		scanner := bufio.NewScanner(stdout)
		for scanner.Scan() {
			if scanner.Text() == "MCPDIAG ready" {
				close(ready)
				return
			}
		}
	}()
	require.NoError(t, cmd.Start())

	select {
	case <-ready:
	case <-deadlineCtx.Done():
		t.Fatal("diagnostic helper did not signal readiness")
	}

	waitDone := make(chan error, 1)
	go func() { waitDone <- cmd.Wait() }()
	commandCancel()
	select {
	case <-waitDone:
	case <-deadlineCtx.Done():
		t.Fatal("configured diagnostic process did not settle after cancellation")
	}
	select {
	case <-readerDone:
	case <-deadlineCtx.Done():
		t.Fatal("diagnostic stdout reader did not settle after cancellation")
	}
}

func TestStdioDiagnosticWriterLimitAndConcurrency(t *testing.T) {
	t.Run("exact limit", func(t *testing.T) {
		writer := newStdioDiagnosticWriter(4, nil)
		n, err := writer.Write([]byte("test"))
		require.NoError(t, err)
		require.Equal(t, 4, n)
		require.Equal(t, []byte("test"), writer.Bytes())
		require.False(t, writer.Exceeded())
	})

	t.Run("limit plus one", func(t *testing.T) {
		writer := newStdioDiagnosticWriter(4, nil)
		n, err := writer.Write([]byte("tests"))
		var limitErr *StdioDiagnosticOutputLimitError
		require.ErrorAs(t, err, &limitErr)
		require.ErrorIs(t, err, ErrStdioDiagnosticTooLarge)
		require.Equal(t, 4, limitErr.Limit)
		require.Equal(t, 4, n)
		require.Equal(t, []byte("test"), writer.Bytes())
		require.True(t, writer.Exceeded())
	})

	t.Run("stdout and stderr writes are safe concurrently", func(t *testing.T) {
		const (
			workers = 8
			writes  = 64
		)
		writer := newStdioDiagnosticWriter(workers*writes*2, nil)
		var wg sync.WaitGroup
		for range workers {
			wg.Add(2)
			go func() {
				defer wg.Done()
				for range writes {
					_, _ = writer.Write([]byte("o"))
				}
			}()
			go func() {
				defer wg.Done()
				for range writes {
					_, _ = writer.Write([]byte("e"))
				}
			}()
		}
		wg.Wait()
		require.Len(t, writer.Bytes(), workers*writes*2)
		require.False(t, writer.Exceeded())
	})
}

func TestStdioCheckBoundsDiagnosticOutput(t *testing.T) {
	t.Run("exact limit is a normal command error", func(t *testing.T) {
		err := stdioCheck(diagnosticTestCommand(t, "exact"))
		require.Error(t, err)
		require.False(t, errors.Is(err, ErrStdioDiagnosticTooLarge))
		require.Contains(t, err.Error(), "MCPDIAG exact")
		require.Less(t, len(err.Error()), stdioDiagnosticMaxOutput+256)
	})

	t.Run("limit plus one is a bounded size error", func(t *testing.T) {
		err := stdioCheck(diagnosticTestCommand(t, "overflow"))
		require.ErrorIs(t, err, ErrStdioDiagnosticTooLarge)
		require.Contains(t, err.Error(), "MCPDIAG overflow")
		require.Less(t, len(err.Error()), stdioDiagnosticMaxOutput+256)
	})

	t.Run("concurrent process streams are retained", func(t *testing.T) {
		err := stdioCheck(diagnosticTestCommand(t, "streams"))
		require.Error(t, err)
		require.Contains(t, err.Error(), "MCPDIAG stdout")
		require.Contains(t, err.Error(), "MCPDIAG stderr")
	})
}

func TestMaybeStdioErrKeepsEOFAndJoinsDiagnostic(t *testing.T) {
	t.Run("successful rerun keeps EOF unchanged", func(t *testing.T) {
		original := io.EOF
		got := maybeStdioErr(original, &mcp.CommandTransport{Command: diagnosticTestCommand(t, "success")})
		require.Same(t, original, got)
	})

	t.Run("failed rerun joins bounded details", func(t *testing.T) {
		original := io.EOF
		got := maybeStdioErr(original, &mcp.CommandTransport{Command: diagnosticTestCommand(t, "failure")})
		require.ErrorIs(t, got, io.EOF)
		require.Contains(t, got.Error(), "MCPDIAG failure")
	})
}
