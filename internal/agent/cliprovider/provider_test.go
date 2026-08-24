package cliprovider

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"testing"
)

// fastExitHelperEnv turns this test binary into the fast-exiting child process
// used by TestStreamFastExitNoLastLineLoss. See that test for why the child is
// the test binary rather than a shell.
const fastExitHelperEnv = "RUSH_TEST_FAST_EXIT_FILE"

func TestMain(m *testing.M) {
	// Child-process mode: dump the fixture and exit before the testing
	// framework emits anything of its own (no flag parsing, no "PASS" line),
	// so the parent's scanner sees exactly the bytes of the file.
	if path := os.Getenv(fastExitHelperEnv); path != "" {
		data, err := os.ReadFile(path)
		if err != nil {
			fmt.Fprintln(os.Stderr, "fast-exit helper:", err)
			os.Exit(1)
		}
		os.Stdout.Write(data)
		os.Exit(0)
	}

	// go-pty's Windows ConPTY path has an internal data race that the -race
	// detector flags. Force pipe mode for the whole cliprovider suite on
	// Windows so streaming tests stay race-clean; Unix keeps PTY coverage.
	testDisablePTY = runtime.GOOS == "windows"
	os.Exit(m.Run())
}

func TestNewProvider(t *testing.T) {
	p := New("/tmp", "", nil, nil, nil, nil)
	if p.Name() != ProviderID {
		t.Errorf("Name() = %q, want %q", p.Name(), ProviderID)
	}
}

func TestLanguageModelUnknown(t *testing.T) {
	p := New("/tmp", "", nil, nil, nil, nil)
	_, err := p.LanguageModel(context.Background(), "nonexistent")
	if err == nil {
		t.Fatal("expected error for unknown model")
	}
	if !strings.Contains(err.Error(), "unknown CLI model") {
		t.Errorf("error = %q, want to contain 'unknown CLI model'", err)
	}
}

func TestLanguageModelKnown(t *testing.T) {
	p := New("/tmp", "", nil, nil, nil, nil)
	lm, err := p.LanguageModel(context.Background(), "cli-claude-sonnet")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if lm.Provider() != ProviderID {
		t.Errorf("Provider() = %q, want %q", lm.Provider(), ProviderID)
	}
	if lm.Model() != "cli-claude-sonnet" {
		t.Errorf("Model() = %q, want %q", lm.Model(), "cli-claude-sonnet")
	}
}

func TestAvailable(t *testing.T) {
	available := Available()
	for _, spec := range available {
		if _, err := exec.LookPath(spec.Binary); err != nil {
			t.Errorf("Available() returned spec with missing binary %q", spec.Binary)
		}
	}
}

func TestMaxPromptArgLen(t *testing.T) {
	if maxPromptArgLen != 30*1024 {
		t.Errorf("maxPromptArgLen = %d, want %d", maxPromptArgLen, 30*1024)
	}
}

// TestIsWindowsCmdShim covers the fix for a real `crush run` failure found
// via a Windows smoke test: claude.cmd (npm's shim) invoked with a routine
// ~12KB system prompt as a CLI argument hit cmd.exe's own ~8191-character
// command-line ceiling ("The command line is too long.") well before
// maxPromptArgLen's 30KB threshold ever triggered the stdin fallback.
func TestIsWindowsCmdShim(t *testing.T) {
	cases := []struct {
		name        string
		path        string
		wantWindows bool // expected result when GOOS == "windows"
	}{
		{"cmd shim", `C:\Users\test\AppData\Roaming\npm\claude.cmd`, true},
		{"bat shim", `C:\tools\gemini.bat`, true},
		{"cmd shim uppercase extension", `C:\tools\claude.CMD`, true},
		{"native exe", `C:\Program Files\claude\claude.exe`, false},
		{"no extension (typical Unix binary)", `/usr/local/bin/claude`, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			want := tc.wantWindows
			if runtime.GOOS != "windows" {
				want = false
			}
			if got := isWindowsCmdShim(tc.path); got != want {
				t.Errorf("isWindowsCmdShim(%q) = %v, want %v (GOOS=%s)", tc.path, got, want, runtime.GOOS)
			}
		})
	}
}

// TestEffectiveMaxPromptArgLen covers effectiveMaxPromptArgLen's fallback:
// when resolveBinary can't resolve the given path (as here, a path that
// doesn't exist on disk), it falls back to inspecting the raw path's own
// extension directly.
func TestEffectiveMaxPromptArgLen(t *testing.T) {
	wantCmdThreshold := maxPromptArgLen
	if runtime.GOOS == "windows" {
		wantCmdThreshold = maxPromptArgLenWindowsCmdShim
	}
	if got := effectiveMaxPromptArgLen(`C:\does\not\exist\claude.cmd`); got != wantCmdThreshold {
		t.Errorf("effectiveMaxPromptArgLen(claude.cmd) = %d, want %d (GOOS=%s)", got, wantCmdThreshold, runtime.GOOS)
	}
	if got := effectiveMaxPromptArgLen(`C:\does\not\exist\claude.exe`); got != maxPromptArgLen {
		t.Errorf("effectiveMaxPromptArgLen(claude.exe) = %d, want %d", got, maxPromptArgLen)
	}
}

func TestBuildArgsPromptFlag(t *testing.T) {
	for _, spec := range All {
		args := spec.BuildArgs(false)
		for _, arg := range args {
			if arg == spec.PromptFlag {
				t.Errorf("BuildArgs for %s should not contain prompt flag %q", spec.ModelID, spec.PromptFlag)
			}
		}
	}
}

func TestAllSpecsHavePartParser(t *testing.T) {
	for _, spec := range All {
		if spec.NewPartParser == nil {
			t.Errorf("spec %q has nil NewPartParser", spec.ModelID)
		}
	}
}

// ── Spec invariants ──────────────────────────────────────────────────────────

func TestAllSpecsHaveUniqueIDs(t *testing.T) {
	seen := make(map[string]bool)
	for _, spec := range All {
		if seen[spec.ModelID] {
			t.Errorf("duplicate ModelID: %q", spec.ModelID)
		}
		seen[spec.ModelID] = true
	}
}

func TestCodexSpecsHaveAlwaysStdin(t *testing.T) {
	for _, spec := range All {
		if spec.Binary == "codex" && !spec.AlwaysStdin {
			t.Errorf("codex spec %q must have AlwaysStdin=true", spec.ModelID)
		}
	}
}

func TestQwenSpecHasAlwaysStdin(t *testing.T) {
	for _, spec := range All {
		if spec.Binary == "qwen" && !spec.AlwaysStdin {
			t.Errorf("qwen spec %q must have AlwaysStdin=true", spec.ModelID)
		}
	}
}

func TestCodexSpecsHaveCorrectBinary(t *testing.T) {
	codexIDs := []string{
		"cli-codex",
		"cli-codex-gpt-5-4",
		"cli-codex-gpt-5-2",
		"cli-codex-max",
		"cli-codex-gpt-5-2-base",
		"cli-codex-mini",
	}
	specsByID := make(map[string]CLISpec)
	for _, s := range All {
		specsByID[s.ModelID] = s
	}
	for _, id := range codexIDs {
		spec, ok := specsByID[id]
		if !ok {
			t.Errorf("missing expected codex spec %q", id)
			continue
		}
		if spec.Binary != "codex" {
			t.Errorf("spec %q has Binary=%q, want 'codex'", id, spec.Binary)
		}
		if spec.NewPartParser == nil {
			t.Errorf("spec %q has nil NewPartParser", id)
		}
		if spec.ParseUsageLine == nil {
			t.Errorf("spec %q has nil ParseUsageLine", id)
		}
	}
}

func TestAll_HaikuModelsRegistered(t *testing.T) {
	// After the 2026-06-17 cleanup the per-thinking and npx variants
	// were removed. We only carry the canonical `cli-claude-haiku`
	// alias now; the operator picks effort via the UI selector and the
	// cliprovider forwards it through context at call time.
	want := []string{
		"cli-claude-haiku",
	}
	byID := make(map[string]CLISpec, len(All))
	for _, s := range All {
		byID[s.ModelID] = s
	}
	for _, id := range want {
		spec, ok := byID[id]
		if !ok {
			t.Errorf("missing expected spec %q in All", id)
			continue
		}
		if spec.ContextWindow != 200_000 {
			t.Errorf("spec %q ContextWindow = %d, want 200_000", id, spec.ContextWindow)
		}
		if spec.NewPartParser == nil {
			t.Errorf("spec %q has nil NewPartParser", id)
		}
		if spec.ParseUsageLine == nil {
			t.Errorf("spec %q has nil ParseUsageLine", id)
		}
	}
}
