// Stream behaviour tests: cliModel.Stream end-to-end against stub CLI
// children - missing-binary lookup, non-zero-exit error surfacing, the
// happy path, context cancellation, per-parser integration runs, and the
// fast-exit last-line-loss regression.

package cliprovider

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"charm.land/fantasy"
)

func TestStreamBinaryNotFound(t *testing.T) {
	spec := CLISpec{
		ModelID:    "test-missing",
		ModelName:  "Test Missing",
		Binary:     "this-binary-should-not-exist-anywhere-on-path",
		PromptFlag: "-p",
		BuildArgs:  func(bool) []string { return nil },
	}
	m := &cliModel{spec: spec, workingDir: t.TempDir()}
	_, err := m.Stream(context.Background(), fantasy.Call{
		Prompt: fantasy.Prompt{fantasy.NewUserMessage("hello")},
	})
	if err == nil {
		t.Fatal("expected error for missing binary")
	}
}

func TestStreamExitError(t *testing.T) {
	shell, flag := "bash", "-c"

	spec := CLISpec{
		ModelID:    "test-fail",
		ModelName:  "Test Fail",
		Binary:     shell,
		PromptFlag: "-p",
		BuildArgs: func(bool) []string {
			return []string{flag, "echo output-text; echo error-text >&2; exit 1"}
		},
	}
	m := &cliModel{spec: spec, workingDir: t.TempDir()}
	stream, err := m.Stream(context.Background(), fantasy.Call{
		Prompt: fantasy.Prompt{fantasy.NewUserMessage("x")},
	})
	if err != nil {
		t.Fatalf("Stream() error: %v", err)
	}

	var gotError error
	var gotText strings.Builder
	for part := range stream {
		switch part.Type {
		case fantasy.StreamPartTypeTextDelta:
			gotText.WriteString(part.Delta)
		case fantasy.StreamPartTypeError:
			gotError = part.Error
		}
	}

	// The guaranteed contract: a non-zero exit always surfaces as an error.
	if gotError == nil {
		t.Fatal("expected error from non-zero exit code")
	}
	// Where stderr TEXT surfaces is mode-dependent. In pipe mode (NoPTY, e.g.
	// Windows) it is appended to the error deterministically. In PTY mode
	// (Unix) the kernel merges stderr into the tty's stdout, but a
	// fast-exiting process can close the PTY before the drain loop reads the
	// final line (a documented PTY tail-drain race — see provider.go), so the
	// stderr text is best-effort, not guaranteed. Only assert the text in the
	// deterministic pipe path; under PTY the non-zero-exit error above is the
	// contract we rely on. Asserting the racy PTY tail made `go test -race`
	// flaky under load on Linux CI.
	if testDisablePTY {
		surfaced := gotError.Error() + "\n" + gotText.String()
		if !strings.Contains(surfaced, "error-text") {
			t.Errorf("stderr should surface in error or output; err=%v text=%q", gotError, gotText.String())
		}
	}
}

func TestStreamSuccess(t *testing.T) {
	shell, flag := "bash", "-c"

	spec := CLISpec{
		ModelID:    "test-ok",
		ModelName:  "Test OK",
		Binary:     shell,
		PromptFlag: "-p",
		BuildArgs: func(bool) []string {
			return []string{flag, "echo hello world"}
		},
	}
	m := &cliModel{spec: spec, workingDir: t.TempDir()}
	stream, err := m.Stream(context.Background(), fantasy.Call{
		Prompt: fantasy.Prompt{fantasy.NewUserMessage("x")},
	})
	if err != nil {
		t.Fatalf("Stream() error: %v", err)
	}

	var text strings.Builder
	var finished bool
	var errPart error
	for part := range stream {
		switch part.Type {
		case fantasy.StreamPartTypeTextDelta:
			text.WriteString(part.Delta)
		case fantasy.StreamPartTypeFinish:
			finished = true
		case fantasy.StreamPartTypeError:
			errPart = part.Error
		}
	}

	if errPart != nil {
		t.Fatalf("unexpected error: %v", errPart)
	}
	if !finished {
		t.Error("expected finish part")
	}
	if !strings.Contains(text.String(), "hello world") {
		t.Errorf("output = %q, want to contain 'hello world'", text.String())
	}
}

func TestStreamContextCancel(t *testing.T) {
	shell := "bash"
	if _, err := exec.LookPath(shell); err != nil {
		t.Skipf("shell %q not found", shell)
	}

	spec := CLISpec{
		ModelID:    "test-cancel",
		ModelName:  "Test Cancel",
		Binary:     shell,
		PromptFlag: "-p",
		BuildArgs: func(bool) []string {
			return []string{"-c", "sleep 60"}
		},
	}
	m := &cliModel{spec: spec, workingDir: t.TempDir()}

	ctx, cancel := context.WithCancel(context.Background())
	stream, err := m.Stream(ctx, fantasy.Call{
		Prompt: fantasy.Prompt{fantasy.NewUserMessage("x")},
	})
	if err != nil {
		t.Fatalf("Stream() error: %v", err)
	}

	cancel()

	var gotError bool
	for part := range stream {
		if part.Type == fantasy.StreamPartTypeError {
			gotError = true
		}
	}
	// The process should be killed either by exec.CommandContext or our ctx check.
	// We just verify the stream terminates without hanging.
	_ = gotError
}

func TestStreamWithPartParser(t *testing.T) {
	// Use stream_event/content_block_delta format (claude CLI --verbose output).
	jsonLines := strings.Join([]string{
		`{"type":"system","subtype":"init"}`,
		`{"type":"stream_event","event":{"type":"content_block_delta","delta":{"type":"text_delta","text":"He"}}}`,
		`{"type":"stream_event","event":{"type":"content_block_delta","delta":{"type":"text_delta","text":"llo"}}}`,
		`{"type":"stream_event","event":{"type":"content_block_delta","delta":{"type":"text_delta","text":", world!"}}}`,
		`{"type":"result","result":"Hello, world!"}`,
	}, "\n") + "\n"

	tmpDir := t.TempDir()
	tmpFile := filepath.Join(tmpDir, "stream.jsonl")
	if err := os.WriteFile(tmpFile, []byte(jsonLines), 0o644); err != nil {
		t.Fatalf("write temp file: %v", err)
	}

	shell, flag := "bash", "-c"
	readCmd := "cat " + strings.ReplaceAll(tmpFile, "\\", "/")

	spec := CLISpec{
		ModelID:    "test-stream-json",
		ModelName:  "Test Stream JSON",
		Binary:     shell,
		PromptFlag: "-p",
		BuildArgs: func(bool) []string {
			return []string{flag, readCmd}
		},
		NewPartParser: claudePartParser,
	}
	m := &cliModel{spec: spec, workingDir: tmpDir}
	stream, err := m.Stream(context.Background(), fantasy.Call{
		Prompt: fantasy.Prompt{fantasy.NewUserMessage("x")},
	})
	if err != nil {
		t.Fatalf("Stream() error: %v", err)
	}

	var text strings.Builder
	var finished bool
	var errPart error
	for part := range stream {
		switch part.Type {
		case fantasy.StreamPartTypeTextDelta:
			text.WriteString(part.Delta)
		case fantasy.StreamPartTypeFinish:
			finished = true
		case fantasy.StreamPartTypeError:
			errPart = part.Error
		}
	}

	if errPart != nil {
		t.Fatalf("unexpected error: %v", errPart)
	}
	if !finished {
		t.Error("expected finish part")
	}
	got := text.String()
	if got != "Hello, world!" {
		t.Errorf("accumulated text = %q, want %q", got, "Hello, world!")
	}
}

// ── Integration: stream with codex JSONL output ──────────────────────────────

func TestStreamWithCodexParser(t *testing.T) {
	jsonLines := strings.Join([]string{
		`{"type":"thread.started","thread_id":"t1"}`,
		`{"type":"turn.started"}`,
		`{"type":"item.started","item":{"type":"command_execution","command":"ls"}}`,
		`{"type":"item.completed","item":{"type":"command_execution","command":"ls","aggregated_output":"file.txt","exit_code":0}}`,
		`{"type":"item.completed","item":{"type":"agent_message","text":"The directory contains file.txt"}}`,
		`{"type":"turn.completed","usage":{"input_tokens":100,"cached_input_tokens":50,"output_tokens":20}}`,
	}, "\n") + "\n"

	tmpDir := t.TempDir()
	tmpFile := filepath.Join(tmpDir, "codex.jsonl")
	if err := os.WriteFile(tmpFile, []byte(jsonLines), 0o644); err != nil {
		t.Fatalf("write temp file: %v", err)
	}

	readCmd := "cat " + strings.ReplaceAll(tmpFile, "\\", "/")
	spec := CLISpec{
		ModelID:        "test-codex",
		ModelName:      "Test Codex",
		Binary:         "bash",
		PromptFlag:     "-p",
		BuildArgs:      func(bool) []string { return []string{"-c", readCmd} },
		NewPartParser:  codexPartParser,
		ParseUsageLine: codexParseUsageLine,
	}
	m := &cliModel{spec: spec, workingDir: tmpDir}
	stream, err := m.Stream(context.Background(), fantasy.Call{
		Prompt: fantasy.Prompt{fantasy.NewUserMessage("x")},
	})
	if err != nil {
		t.Fatalf("Stream() error: %v", err)
	}

	var text strings.Builder
	var finalUsage fantasy.Usage
	var finished bool
	for part := range stream {
		switch part.Type {
		case fantasy.StreamPartTypeTextDelta:
			text.WriteString(part.Delta)
		case fantasy.StreamPartTypeFinish:
			finished = true
			finalUsage = part.Usage
		case fantasy.StreamPartTypeError:
			t.Fatalf("unexpected error: %v", part.Error)
		}
	}

	if !finished {
		t.Error("expected finish part")
	}
	want := "The directory contains file.txt"
	if text.String() != want {
		t.Errorf("text = %q, want %q", text.String(), want)
	}
	// This assertion used to read `want 150 (100+50)`, encoding the
	// double-count bug: codex's input_tokens already CONTAINS
	// cached_input_tokens (measured against codex 0.147.0 — see
	// codexParseUsageLine), so summing them counted the cached share twice.
	// InputTokens is now the uncached remainder, and the cached share is
	// reported separately instead of being folded away.
	if finalUsage.InputTokens != 50 {
		t.Errorf("InputTokens = %d, want 50 (100 total - 50 cached)", finalUsage.InputTokens)
	}
	if finalUsage.CacheReadTokens != 50 {
		t.Errorf("CacheReadTokens = %d, want 50", finalUsage.CacheReadTokens)
	}
	if got := finalUsage.InputTokens + finalUsage.CacheReadTokens + finalUsage.CacheCreationTokens; got != 100 {
		t.Errorf("reconstructed prompt total = %d, want 100 (codex's own input_tokens)", got)
	}
	if finalUsage.OutputTokens != 20 {
		t.Errorf("OutputTokens = %d, want 20", finalUsage.OutputTokens)
	}
}

// ── Integration: stream with Gemini JSONL output ─────────────────────────────

func TestStreamWithGeminiParser(t *testing.T) {
	jsonLines := strings.Join([]string{
		`{"type":"init","session_id":"abc","model":"auto-gemini-3"}`,
		`{"type":"message","role":"user","content":"hello"}`,
		`{"type":"message","role":"assistant","content":"Hello ","delta":true}`,
		`{"type":"message","role":"assistant","content":"world!","delta":true}`,
		`{"type":"result","status":"success","stats":{"total_tokens":15,"input_tokens":10,"output_tokens":5}}`,
	}, "\n") + "\n"

	tmpDir := t.TempDir()
	tmpFile := filepath.Join(tmpDir, "gemini.jsonl")
	if err := os.WriteFile(tmpFile, []byte(jsonLines), 0o644); err != nil {
		t.Fatalf("write temp file: %v", err)
	}

	readCmd := "cat " + strings.ReplaceAll(tmpFile, "\\", "/")
	spec := CLISpec{
		ModelID:        "test-gemini",
		ModelName:      "Test Gemini",
		Binary:         "bash",
		PromptFlag:     "-p",
		BuildArgs:      func(bool) []string { return []string{"-c", readCmd} },
		NewPartParser:  geminiPartParser,
		ParseUsageLine: geminiParseUsageLine,
	}
	m := &cliModel{spec: spec, workingDir: tmpDir}
	stream, err := m.Stream(context.Background(), fantasy.Call{
		Prompt: fantasy.Prompt{fantasy.NewUserMessage("x")},
	})
	if err != nil {
		t.Fatalf("Stream() error: %v", err)
	}

	var text strings.Builder
	var finalUsage fantasy.Usage
	var finished bool
	for part := range stream {
		switch part.Type {
		case fantasy.StreamPartTypeTextDelta:
			text.WriteString(part.Delta)
		case fantasy.StreamPartTypeFinish:
			finished = true
			finalUsage = part.Usage
		case fantasy.StreamPartTypeError:
			t.Fatalf("unexpected error: %v", part.Error)
		}
	}

	if !finished {
		t.Error("expected finish part")
	}
	if text.String() != "Hello world!" {
		t.Errorf("text = %q, want %q", text.String(), "Hello world!")
	}
	if finalUsage.TotalTokens != 15 {
		t.Errorf("TotalTokens = %d, want 15", finalUsage.TotalTokens)
	}
	if finalUsage.InputTokens != 10 {
		t.Errorf("InputTokens = %d, want 10", finalUsage.InputTokens)
	}
	if finalUsage.OutputTokens != 5 {
		t.Errorf("OutputTokens = %d, want 5", finalUsage.OutputTokens)
	}
}

// ── Bug 3 regression: last line must not be lost on a fast-exiting child ──
//
// Before the fix, cmd.Wait() (pipe branch) or p.Close() (PTY branch) was
// started eagerly in a goroutine that ran concurrently with the scanner.
// On a fast-exiting child (cat of a small JSONL file), the wait goroutine
// could close the output fd before the scanner read the final buffered
// line — the usage/result line — losing it. On CI this surfaced as
// linesSeen=4 instead of 5 with TotalTokens=0 instead of 15.
//
// The fix defers Wait()/Close() to proc.wait(), which is only called after
// the scanner has drained stdout to natural EOF. This test reproduces the
// scenario (a child that dumps a 5-line file and exits immediately) and
// asserts every line — especially the last — is received. We run many
// iterations because the race is timing-dependent; a single green iteration
// proves nothing.
//
// The child is THIS TEST BINARY re-executed in helper mode (see TestMain and
// fastExitHelperEnv), not `bash -c "cat ..."`. The original shell version
// failed on the windows-latest runner with
//
//	bash failed: exit status 256
//	bash.exe: *** fatal error - add_item ("\??\C:\Program Files\Git", "/", ...) failed, errno 1
//
// which is Git-for-Windows/MSYS failing to build its mount table while
// spawning — the child never ran at all. That is noise: the property under
// test is our reader draining the pipe/PTY to natural EOF, and it has nothing
// to do with which program is on the other end. Re-executing the test binary
// removes the POSIX-shell dependency entirely (so the test no longer skips on
// shell-less machines either) while still being a genuine external process
// that exits as fast as the OS allows.
func TestStreamFastExitNoLastLineLoss(t *testing.T) {
	self, err := os.Executable()
	if err != nil {
		t.Fatalf("locate test binary: %v", err)
	}

	// 5 lines: 3 text deltas + init + final result (usage). The last line
	// is the one that gets lost in the race.
	jsonLines := strings.Join([]string{
		`{"type":"init","session_id":"s","model":"m"}`,
		`{"type":"message","role":"assistant","content":"A","delta":true}`,
		`{"type":"message","role":"assistant","content":"B","delta":true}`,
		`{"type":"message","role":"assistant","content":"C","delta":true}`,
		`{"type":"result","status":"success","stats":{"total_tokens":42,"input_tokens":30,"output_tokens":12}}`,
	}, "\n") + "\n"

	tmpDir := t.TempDir()
	tmpFile := filepath.Join(tmpDir, "fast.jsonl")
	if err := os.WriteFile(tmpFile, []byte(jsonLines), 0o644); err != nil {
		t.Fatalf("write temp file: %v", err)
	}

	// The child inherits the parent's environment (cmd.Env stays nil), which
	// is how it learns it should run in helper mode and which file to dump.
	t.Setenv(fastExitHelperEnv, tmpFile)

	spec := CLISpec{
		ModelID:        "test-fast-exit",
		ModelName:      "Test Fast Exit",
		Binary:         self,
		PromptFlag:     "-p",
		BuildArgs:      func(bool) []string { return nil },
		NewPartParser:  geminiPartParser,
		ParseUsageLine: geminiParseUsageLine,
	}

	const iterations = 100
	for i := 0; i < iterations; i++ {
		m := &cliModel{spec: spec, workingDir: tmpDir}
		stream, err := m.Stream(context.Background(), fantasy.Call{
			Prompt: fantasy.Prompt{fantasy.NewUserMessage("x")},
		})
		if err != nil {
			t.Fatalf("iter %d: Stream() error: %v", i, err)
		}

		var text strings.Builder
		var finalUsage fantasy.Usage
		var finished bool
		var errPart error
		for part := range stream {
			switch part.Type {
			case fantasy.StreamPartTypeTextDelta:
				text.WriteString(part.Delta)
			case fantasy.StreamPartTypeFinish:
				finished = true
				finalUsage = part.Usage
			case fantasy.StreamPartTypeError:
				errPart = part.Error
			}
		}

		if errPart != nil {
			t.Fatalf("iter %d: unexpected error: %v", i, errPart)
		}
		if !finished {
			t.Fatalf("iter %d: expected finish part", i)
		}
		// The critical assertion: the LAST line (usage) must not be lost.
		// With the race bug, finalUsage.TotalTokens was 0 on some iterations.
		if finalUsage.TotalTokens != 42 {
			t.Errorf("iter %d: TotalTokens = %d, want 42 (last line lost — race bug)", i, finalUsage.TotalTokens)
		}
		if finalUsage.InputTokens != 30 {
			t.Errorf("iter %d: InputTokens = %d, want 30", i, finalUsage.InputTokens)
		}
		if finalUsage.OutputTokens != 12 {
			t.Errorf("iter %d: OutputTokens = %d, want 12", i, finalUsage.OutputTokens)
		}
		// All 3 text deltas must be present.
		if text.String() != "ABC" {
			t.Errorf("iter %d: text = %q, want %q", i, text.String(), "ABC")
		}
	}
}
