package cliprovider

import (
	"context"
	"errors"
	"os/exec"
	"strings"
	"testing"

	"charm.land/fantasy"
)

// Revert-check: removing mapCLIExitError or exitDiagnostics.add in Stream must fail synthetic generic wording cases; these are not CLI captures.
func TestSyntheticGenericCLIQuotaExit(t *testing.T) {
	shell, err := resolveBinary("bash")
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name, text             string
		merged, success, quota bool
	}{
		{"usage separate", "Usage limit reached. Your limit will reset at 2026-06-17 14:49:28", false, false, true},
		{"quota merged", "quota exceeded", true, false, true},
		{"reset merged", "reset at tomorrow", true, false, true},
		{"will reset separate", "limit will reset tomorrow", false, false, true},
		{"bare rate", "HTTP 429 rate limit", true, false, false},
		{"ordinary", "ordinary failure", false, false, false},
		{"success", "quota exceeded", true, true, false},
		{"assistant JSON", `{"type":"message","role":"assistant","content":"quota exceeded"}`, true, false, false},
		{"tool JSON", `{"type":"tool_result","content":"usage limit"}`, true, false, false},
		{"user JSON", `{"type":"message","role":"user","content":"quota exceeded"}`, true, false, false},
		{"separate JSON", `{"type":"message","role":"assistant","content":"quota exceeded"}`, false, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			code := "1"
			if tc.success {
				code = "0"
			}
			script := "printf '%s\\n' '" + tc.text + "' >&2; exit " + code
			m := &cliModel{workingDir: t.TempDir(), spec: CLISpec{Binary: shell, NoPTY: tc.merged, AlwaysStdin: true, BuildArgs: func(bool) []string { return []string{"-c", script} }, NewPartParser: geminiPartParser}}
			stream, err := m.Stream(context.Background(), fantasy.Call{Prompt: fantasy.Prompt{fantasy.NewUserMessage("test")}})
			if err != nil {
				t.Fatal(err)
			}
			var got error
			for p := range stream {
				if p.Type == fantasy.StreamPartTypeError {
					got = p.Error
				}
			}
			if tc.success {
				if got != nil {
					t.Fatal(got)
				}
				return
			}
			if got == nil {
				t.Fatal("missing exit error")
			}
			var exit *exec.ExitError
			if !errors.As(got, &exit) {
				t.Fatalf("lost exit cause: %v", got)
			}
			var pe *fantasy.ProviderError
			mapped := errors.As(got, &pe)
			if mapped != tc.quota {
				t.Fatalf("quota mapping=%v want %v: %v", mapped, tc.quota, got)
			}
			if mapped {
				if pe.StatusCode != 429 || !strings.Contains(pe.Message, tc.text) {
					t.Fatalf("bad provider error: %+v", pe)
				}
			}
			if !strings.Contains(got.Error(), "failed:") {
				t.Fatalf("lost original error: %v", got)
			}
			if !tc.merged && !strings.Contains(got.Error(), tc.text) {
				t.Fatalf("lost stderr: %v", got)
			}
		})
	}
}

// Revert-check: removing exitDiagnostics.add's JSON exclusion or size cap must fail; synthetic diagnostics only.
func TestSyntheticExitDiagnostics(t *testing.T) {
	var d exitDiagnostics
	d.add([]byte(`{"content":"quota"}`))
	if d.text != "" {
		t.Fatal("JSON captured")
	}
	d.add([]byte("\x1b[31mquota\x1b[0m\x00"))
	if d.text != "quota\n" {
		t.Fatalf("unsanitized: %q", d.text)
	}
	d.add([]byte(strings.Repeat("x", maxExitDiagnostics*2)))
	if len(d.text) > maxExitDiagnostics {
		t.Fatal("unbounded diagnostics")
	}
	d.add([]byte("more quota"))
	if len(d.text) > maxExitDiagnostics {
		t.Fatal("unbounded append")
	}
}
