package cliprovider

import (
	"context"
	"os/exec"
	"runtime"
	"strings"
	"testing"
)

// Facade-level tests split out of provider_test.go when the stream cluster
// (cliModel, TestMain/testDisablePTY) moved to clipstream (design §3.3 S3,
// §4.4). Bodies verbatim.

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

// TestIsWindowsCmdShim covers the fix for a real `rush run` failure found
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
