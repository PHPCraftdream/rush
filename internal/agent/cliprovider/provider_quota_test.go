package cliprovider

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
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

func TestSyntheticLateQuotaEvidence(t *testing.T) {
	shell, err := resolveBinary("bash")
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name, lines string
		quota       bool
		want        string
	}{
		{"late quota", strings.Repeat("x", 8191) + "\nquota exceeded", true, "quota exceeded"},
		{"late transient veto", "quota exceeded\n" + strings.Repeat("x", 8191) + "\nHTTP 429 per minute", false, ""},
		{"late reset", "quota exceeded\n" + strings.Repeat("x", 8191) + "\nYour limit will reset at 2026-06-17T14:49:28+08:00", true, "2026-06-17T14:49:28+08:00"},
		{"json after noise", strings.Repeat("x", 8191) + "\n{\"type\":\"message\",\"role\":\"assistant\",\"content\":\"quota exceeded\"}", false, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// The multi-KB payload goes through a file, not the bash -c argv.
			// The former argv form failed once on windows CI (late quota
			// case: bash exit status 2 after one merged output line; the
			// line was not retained, so the argv hypothesis is unproven).
			// A failed cat keeps its own exit status so a missing payload
			// cannot pass a negative case.
			payload := filepath.Join(t.TempDir(), "output.txt")
			if err := os.WriteFile(payload, []byte(tc.lines+"\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			quoted := "'" + strings.ReplaceAll(filepath.ToSlash(payload), "'", `'\''`) + "'"
			m := &cliModel{workingDir: t.TempDir(), spec: CLISpec{Binary: shell, NoPTY: true, AlwaysStdin: true, BuildArgs: func(bool) []string { return []string{"-c", "cat " + quoted + ` || exit "$?"; exit 1`} }, NewPartParser: geminiPartParser}}
			stream, err := m.Stream(context.Background(), fantasy.Call{Prompt: fantasy.Prompt{fantasy.NewUserMessage("synthetic test")}})
			if err != nil {
				t.Fatal(err)
			}
			var got error
			for part := range stream {
				if part.Type == fantasy.StreamPartTypeError {
					got = part.Error
				}
			}
			var exit *exec.ExitError
			if got == nil || !errors.As(got, &exit) {
				t.Fatalf("lost original exit cause: %v", got)
			}
			if exit.ExitCode() != 1 {
				t.Fatalf("exit code=%d want 1 (payload not delivered?): %v", exit.ExitCode(), got)
			}
			var pe *fantasy.ProviderError
			mapped := errors.As(got, &pe)
			if mapped != tc.quota {
				t.Fatalf("quota mapping=%v want %v: %v", mapped, tc.quota, got)
			}
			if mapped && pe.StatusCode != 429 {
				t.Fatalf("provider status=%d want 429", pe.StatusCode)
			}
			if tc.want != "" && !strings.Contains(pe.Message, tc.want) {
				t.Fatalf("provider message lacks %q: %q", tc.want, pe.Message)
			}
		})
	}
}

// Revert-check: dropping the merged-output branch in Stream's exit error makes this fail; a NoPTY failure has no separate stderr, so the bounded output line is the only explanation of the exit.
func TestSyntheticMergedOutputReachesExitError(t *testing.T) {
	shell, err := resolveBinary("bash")
	if err != nil {
		t.Fatal(err)
	}
	m := &cliModel{workingDir: t.TempDir(), spec: CLISpec{Binary: shell, NoPTY: true, AlwaysStdin: true, BuildArgs: func(bool) []string { return []string{"-c", "echo synthetic-shell-failure; exit 2"} }, NewPartParser: geminiPartParser}}
	stream, err := m.Stream(context.Background(), fantasy.Call{Prompt: fantasy.Prompt{fantasy.NewUserMessage("synthetic test")}})
	if err != nil {
		t.Fatal(err)
	}
	var got error
	for part := range stream {
		if part.Type == fantasy.StreamPartTypeError {
			got = part.Error
		}
	}
	var exit *exec.ExitError
	if got == nil || !errors.As(got, &exit) || exit.ExitCode() != 2 {
		t.Fatalf("lost original exit cause (want exit code 2): %v", got)
	}
	if !strings.Contains(got.Error(), "synthetic-shell-failure") {
		t.Fatalf("merged output lost from the exit error: %v", got)
	}
	var pe *fantasy.ProviderError
	if errors.As(got, &pe) {
		t.Fatalf("ordinary failure mapped to a provider error: %v", got)
	}
}

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
	d.add([]byte("ordinary tail"))
	if len(d.text) > maxExitDiagnostics {
		t.Fatal("ordinary diagnostics exceeded bound")
	}
	for i := 0; i < 4; i++ {
		d.add([]byte("Usage limit reached. Your limit will reset at 2026-06-17T14:49:28+08:00 " + strings.Repeat("x", maxExitDiagnostics)))
	}
	if len(d.text) > maxExitDiagnostics || len(d.evidence) > maxExitDiagnostics || len(d.resetText) > maxExitDiagnostics || !d.hard {
		t.Fatal("diagnostic evidence exceeded bound or lost hard classification")
	}
}
