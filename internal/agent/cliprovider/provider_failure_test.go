package cliprovider

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"charm.land/fantasy"
)

// External Codex sample, not reproduced locally: docs/research/2026-10-08-provider-limit-errors.md, agent-grounds/rhei #471, zhupanov/larch #3380.
const codexLimitLines = `{"type":"error","message":"You've hit your usage limit. Visit https://chatgpt.com/codex/settings/usage to purchase more credits or try again at Oct 9th, 2026 11:19 PM."}
{"type":"turn.failed","error":{"message":"You've hit your usage limit. Visit https://chatgpt.com/codex/settings/usage to purchase more credits or try again at Oct 9th, 2026 11:19 PM."}}`

func failureEvent(kind, text string) string {
	fields := map[string]any{"type": "result", "subtype": "success", "is_error": true, "result": text}
	if kind == "codex" {
		fields = map[string]any{"type": "turn.failed", "error": map[string]any{"message": text}}
	}
	b, _ := json.Marshal(fields)
	return string(b)
}

func runFailureScript(t *testing.T, name, lines, stderr string, code int, merged bool) (error, bool) {
	t.Helper()
	shell, err := resolveBinary("bash")
	if err != nil {
		t.Fatal(err)
	}
	quote := func(s string) string { return "'" + strings.ReplaceAll(s, "'", "'\\''") + "'" }
	script := "printf '%s\\n' " + quote(lines) + "; printf '%s\\n' " + quote(stderr) + " >&2; exit " + fmt.Sprint(code)
	parser := claudePartParser
	if name == "codex" {
		parser = codexPartParser
	}
	dir := t.TempDir()
	scriptPath := filepath.Join(dir, "fake-cli.sh")
	if err := os.WriteFile(scriptPath, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	m := &cliModel{workingDir: dir, spec: CLISpec{ModelID: "cli-" + name, Binary: shell, AlwaysStdin: true, NoPTY: merged, NewPartParser: parser, BuildArgs: func(bool) []string { return []string{filepath.ToSlash(scriptPath)} }}}
	stream, err := m.Stream(context.Background(), fantasy.Call{Prompt: fantasy.Prompt{fantasy.NewUserMessage("test")}})
	if err != nil {
		t.Fatal(err)
	}
	var got error
	var finish bool
	for p := range stream {
		if p.Type == fantasy.StreamPartTypeError {
			got = p.Error
		}
		if p.Type == fantasy.StreamPartTypeFinish {
			finish = true
		}
	}
	return got, finish
}

// Revert-check: provider_failure.go:cliFailureText must extract both Codex channels.
func TestCLIFailureCodex(t *testing.T) {
	for _, merged := range []bool{false, true} {
		for _, lines := range []string{codexLimitLines, strings.Split(codexLimitLines, "\n")[0], strings.Split(codexLimitLines, "\n")[1]} {
			got, finish := runFailureScript(t, "codex", lines, "Reading additional input from stdin...", 1, merged)
			var pe *fantasy.ProviderError
			var exit *exec.ExitError
			if finish || !errors.As(got, &pe) || !errors.As(got, &exit) {
				t.Fatalf("finish=%v error=%v", finish, got)
			}
			if pe.StatusCode != 429 || pe.Title != "Quota limit" || !strings.Contains(pe.Message, "try again at Oct 9th, 2026 11:19 PM") || !strings.Contains(got.Error(), "failed:") {
				t.Fatalf("bad mapping: %+v %v", pe, got)
			}
			if !errors.Is(got, exit) || pe.Cause == nil {
				t.Fatal("lost cause")
			}
		}
	}
}

// Revert-check: provider_stream.go:Stream waitErr != nil || failure.seen rejects exit 0.
func TestCLIFailureExitZero(t *testing.T) {
	// Claude wording, not reproduced locally: docs/research/2026-10-08-provider-limit-errors.md, anthropics/claude-code #2087; is_error: codingworkflow/claude-code-api PR #48.
	for _, name := range []string{"claude", "qwen"} {
		// Qwen is a compatibility fixture, not an observed Qwen failure capture.
		for _, code := range []int{0, 1} {
			for _, text := range []string{"Claude AI usage limit reached|1749924000", "You've hit your limit · resets 4pm (Asia/Kuala_Lumpur)", "ordinary provider failure"} {
				preceding := `{"type":"stream_event","event":{"type":"content_block_delta","delta":{"type":"text_delta","text":"preceding text"}}}` + "\n"
				got, finish := runFailureScript(t, name, preceding+failureEvent(name, text), "", code, false)
				if got == nil || finish || !strings.Contains(got.Error(), text) {
					t.Fatalf("%s/%d: finish=%v error=%v", name, code, finish, got)
				}
				var pe *fantasy.ProviderError
				if errors.As(got, &pe) != (text != "ordinary provider failure") {
					t.Fatalf("unexpected quota: %v", got)
				}
				var exit *exec.ExitError
				if errors.As(got, &exit) != (code == 1) {
					t.Fatalf("lost/invented exit cause: %v", got)
				}
			}
		}
	}
}

// Revert-check: provider_quota.go:cliLimitText's Transient branch vetoes promotion.
func TestCLIFailureTransientVeto(t *testing.T) {
	for _, text := range []string{"quota exceeded per minute", "usage limit overloaded", "quota exceeded requests"} {
		for _, name := range []string{"codex", "claude", "qwen"} {
			got, finish := runFailureScript(t, name, failureEvent(name, text), "", 0, false)
			var pe *fantasy.ProviderError
			if got == nil || finish || errors.As(got, &pe) {
				t.Fatalf("%s: finish=%v error=%v", text, finish, got)
			}
		}
	}
}

// Revert-check: provider_failure.go:cliFailureText rejects non-failure event types.
func TestCLIFailureContentExcluded(t *testing.T) {
	lines := `{"type":"assistant","message":{"content":[{"type":"text","text":"quota exceeded"}]}}
{"type":"message","role":"user","content":"usage limit"}
{"type":"tool_result","content":"quota exceeded"}
{"type":"system","message":"quota exceeded"}
{"type":"stream_event","event":{"type":"content_block_delta","delta":{"type":"text_delta","text":"quota exceeded"}}}
{"type":"result","is_error":false,"result":"quota exceeded"}
{"type":"item.completed","item":{"type":"agent_message","text":"usage limit"}}`
	for _, name := range []string{"codex", "claude", "qwen", "gemini"} {
		for _, code := range []int{0, 1} {
			got, finish := runFailureScript(t, name, lines, "ordinary failure", code, false)
			var pe *fantasy.ProviderError
			if errors.As(got, &pe) || (got != nil) != (code == 1) || finish != (code == 0) {
				t.Fatalf("%s/%d: finish=%v error=%v", name, code, finish, got)
			}
			if code == 1 && (!strings.Contains(got.Error(), "stderr: ordinary failure") || strings.Contains(got.Error(), "failure:")) {
				t.Fatalf("unrelated exit changed: %v", got)
			}
		}
	}
}

// Revert-check: provider_quota.go:cliLimitText retains the local keyword superset.
func TestCLIFailureVocabulary(t *testing.T) {
	for _, text := range []string{
		// Exact Plus message in a synthetic envelope, not reproduced locally: docs/research/2026-10-08-provider-limit-errors.md, openai/codex #29948, #16909; user-supplied variant.
		"You've hit your usage limit. Upgrade to Plus to continue using Codex (https://chatgpt.com/explore/plus) or try again at Jul 19th, 2026 10:27 AM.",
		// Synthetic composition from the admin evidence fragment, not reproduced locally: docs/research/2026-10-08-provider-limit-errors.md, openai/codex #29948, #16909; not a verbatim full message.
		"You've hit your usage limit. send a request to your admin or try again at Oct 9th, 2026 11:19 PM.",
	} {
		got, finish := runFailureScript(t, "codex", failureEvent("codex", text), "", 1, false)
		var pe *fantasy.ProviderError
		if finish || !errors.As(got, &pe) || pe.StatusCode != 429 || pe.Message != text {
			t.Fatalf("variant lost: %v", got)
		}
	}
	for _, text := range []string{"usage limit", "limit reached", "hit your limit", "hit your usage limit", "limit will reset", "reset at tomorrow", "resets 4pm", "try again at tomorrow", "quota", "usage_limit_reached"} {
		got, finish := runFailureScript(t, "codex", failureEvent("codex", text), "", 1, false)
		var pe *fantasy.ProviderError
		if finish || !errors.As(got, &pe) {
			t.Fatalf("%s: %v", text, got)
		}
	}
}

// Revert-check: provider_failure.go:boundedFailureText preserves reset contexts.
func TestCLIFailureBounds(t *testing.T) {
	text := "usage limit reached|1791586800"
	got, finish := runFailureScript(t, "claude", failureEvent("claude", text), strings.Repeat("noise", maxExitDiagnostics), 1, true)
	var pe *fantasy.ProviderError
	if finish || !errors.As(got, &pe) || !strings.Contains(pe.Message, text) {
		t.Fatalf("lost evidence: %v", got)
	}
	var evidence cliFailureEvidence
	evidence.add("claude", []byte(failureEvent("claude", strings.Repeat("x", maxExitDiagnostics*2)+" quota resets 4pm")))
	if len(evidence.text) > maxExitDiagnostics || !evidence.hard || !strings.Contains(evidence.text, "resets 4pm") {
		t.Fatal("unbounded/lost evidence")
	}
	evidence = cliFailureEvidence{}
	evidence.add("claude", []byte(failureEvent("claude", strings.Repeat("x", maxExitDiagnostics)+" usage limit reached|1791586800 "+strings.Repeat("y", maxExitDiagnostics))))
	if len(evidence.text) > maxExitDiagnostics || !strings.Contains(evidence.text, "usage limit reached|1791586800") {
		t.Fatal("middle reset lost")
	}
	evidence.add("claude", []byte(failureEvent("claude", "quota exceeded per minute")))
	evidence.add("claude", []byte(failureEvent("claude", "usage limit reached")))
	got = mapCLIExitError(errors.New("failure"), "", exitDiagnostics{}, evidence)
	if errors.As(got, &pe) {
		t.Fatal("later hard event erased transient veto")
	}
}

// Revert-check: provider_failure.go:cliFailureText extracts error string/object.message only.
func TestCLIFailureFields(t *testing.T) {
	for _, name := range []string{"claude", "qwen"} {
		for _, raw := range []string{`{"type":"result","is_error":true,"error":"quota exceeded"}`, `{"type":"result","is_error":true,"error":{"message":"quota exceeded","content":"ignored"}}`, `{"type":"result","is_error":true,"message":"quota exceeded"}`} {
			text, ok := cliFailureText(name, []byte(raw))
			if !ok || text != "quota exceeded" {
				t.Fatalf("%s: %q %v", raw, text, ok)
			}
		}
	}
	for _, raw := range []string{`{"type":"result","is_error":true,"error":["quota"]}`, `{"type":"result","is_error":true,"error":{"content":"quota"}}`} {
		text, ok := cliFailureText("qwen", []byte(raw))
		if !ok || text != "" {
			t.Fatalf("unsafe extraction: %q", text)
		}
	}
	if cliFailureName(CLISpec{Binary: "qwen.cmd"}) != "qwen" || cliFailureName(CLISpec{Binary: "codex.exe"}) != "codex" {
		t.Fatal("name selection")
	}
}
