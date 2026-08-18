// Per-CLI argv and output tests: the argument builders (claude/gemini/qwen/
// codex args and the codex MCP-bridge config args), the JSONL part parsers
// for each CLI, and the shared contains helper.

package cliprovider

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"charm.land/fantasy"
)

func TestClaudeArgs(t *testing.T) {
	fn := claudeArgs("sonnet", "--effort", "high")
	args := fn(false)
	if !contains(args, "--model") || !contains(args, "sonnet") || !contains(args, "--effort") {
		t.Errorf("claudeArgs(false) = %v, missing expected flags", args)
	}
	if contains(args, "--dangerously-skip-permissions") {
		t.Error("claudeArgs(false) should not include --dangerously-skip-permissions")
	}
	if !contains(args, "--output-format") || !contains(args, "stream-json") {
		t.Error("claudeArgs should include --output-format stream-json")
	}
	if !contains(args, "--include-partial-messages") {
		t.Error("claudeArgs should include --include-partial-messages")
	}

	argsYolo := fn(true)
	if !contains(argsYolo, "--dangerously-skip-permissions") {
		t.Error("claudeArgs(true) should include --dangerously-skip-permissions")
	}
}

func TestGeminiArgs(t *testing.T) {
	fn := geminiArgs("gemini-3-flash")
	args := fn(false)
	if !contains(args, "-m") || !contains(args, "gemini-3-flash") {
		t.Errorf("geminiArgs(false) = %v, missing expected flags", args)
	}
	if contains(args, "-y") {
		t.Error("geminiArgs(false) should not include -y")
	}
	if !contains(args, "--output-format") || !contains(args, "stream-json") {
		t.Error("geminiArgs should include --output-format stream-json")
	}

	argsYolo := fn(true)
	if !contains(argsYolo, "-y") {
		t.Error("geminiArgs(true) should include -y")
	}
}

func TestClaudePartParser(t *testing.T) {
	parse := claudePartParser()

	// Non-stream_event events are skipped
	initEvent, _ := json.Marshal(map[string]any{
		"type":       "system",
		"subtype":    "init",
		"session_id": "abc",
	})
	if _, ok := parse(initEvent); ok {
		t.Error("system event should be skipped")
	}

	// text_delta yields a TextDelta part
	ev1, _ := json.Marshal(map[string]any{
		"type": "stream_event",
		"event": map[string]any{
			"type": "content_block_delta",
			"delta": map[string]any{
				"type": "text_delta",
				"text": "Hello",
			},
		},
	})
	part, ok := parse(ev1)
	if !ok {
		t.Fatal("expected part from text_delta event")
	}
	if part.Type != fantasy.StreamPartTypeTextDelta {
		t.Errorf("part.Type = %v, want TextDelta", part.Type)
	}
	if part.Delta != "Hello" {
		t.Errorf("part.Delta = %q, want %q", part.Delta, "Hello")
	}

	// thinking_delta yields a ReasoningDelta part
	ev2, _ := json.Marshal(map[string]any{
		"type": "stream_event",
		"event": map[string]any{
			"type": "content_block_delta",
			"delta": map[string]any{
				"type":     "thinking_delta",
				"thinking": "I think...",
			},
		},
	})
	part, ok = parse(ev2)
	if !ok {
		t.Fatal("expected part from thinking_delta event")
	}
	if part.Type != fantasy.StreamPartTypeReasoningDelta {
		t.Errorf("part.Type = %v, want ReasoningDelta", part.Type)
	}
	if part.Delta != "I think..." {
		t.Errorf("part.Delta = %q, want %q", part.Delta, "I think...")
	}

	// content_block_start with thinking type yields ReasoningStart
	ev3, _ := json.Marshal(map[string]any{
		"type": "stream_event",
		"event": map[string]any{
			"type":          "content_block_start",
			"content_block": map[string]any{"type": "thinking"},
		},
	})
	part, ok = parse(ev3)
	if !ok {
		t.Fatal("expected ReasoningStart from content_block_start thinking")
	}
	if part.Type != fantasy.StreamPartTypeReasoningStart {
		t.Errorf("part.Type = %v, want ReasoningStart", part.Type)
	}

	// content_block_stop after thinking yields ReasoningEnd
	ev4, _ := json.Marshal(map[string]any{
		"type": "stream_event",
		"event": map[string]any{
			"type": "content_block_stop",
		},
	})
	part, ok = parse(ev4)
	if !ok {
		t.Fatal("expected ReasoningEnd from content_block_stop")
	}
	if part.Type != fantasy.StreamPartTypeReasoningEnd {
		t.Errorf("part.Type = %v, want ReasoningEnd", part.Type)
	}

	// result event is skipped
	resultEvent, _ := json.Marshal(map[string]any{
		"type":    "result",
		"subtype": "success",
	})
	if _, ok := parse(resultEvent); ok {
		t.Error("result event should be skipped")
	}

	// invalid JSON is skipped
	if _, ok := parse([]byte("not json")); ok {
		t.Error("invalid JSON should be skipped")
	}
}

func TestGeminiPartParser(t *testing.T) {
	parse := geminiPartParser()

	// init event is skipped
	ev, _ := json.Marshal(map[string]any{
		"type": "init", "session_id": "x", "model": "auto-gemini-3",
	})
	if _, ok := parse(ev); ok {
		t.Error("init event should be skipped")
	}

	// user message echo is skipped
	ev, _ = json.Marshal(map[string]any{
		"type": "message", "role": "user", "content": "hello",
	})
	if _, ok := parse(ev); ok {
		t.Error("user message should be skipped")
	}

	// assistant delta yields TextDelta
	ev, _ = json.Marshal(map[string]any{
		"type": "message", "role": "assistant", "content": "Hello!", "delta": true,
	})
	part, ok := parse(ev)
	if !ok {
		t.Fatal("expected part from assistant delta event")
	}
	if part.Type != fantasy.StreamPartTypeTextDelta {
		t.Errorf("part.Type = %v, want TextDelta", part.Type)
	}
	if part.Delta != "Hello!" {
		t.Errorf("part.Delta = %q, want %q", part.Delta, "Hello!")
	}

	// result event is skipped (handled by ParseUsageLine)
	ev, _ = json.Marshal(map[string]any{
		"type": "result", "status": "success",
		"stats": map[string]any{"total_tokens": 100},
	})
	if _, ok := parse(ev); ok {
		t.Error("result event should be skipped by part parser")
	}

	// assistant message with empty content is skipped
	ev, _ = json.Marshal(map[string]any{
		"type": "message", "role": "assistant", "content": "", "delta": true,
	})
	if _, ok := parse(ev); ok {
		t.Error("assistant message with empty content should be skipped")
	}

	// Invalid JSON
	if _, ok := parse([]byte("{bad")); ok {
		t.Error("invalid JSON should be skipped")
	}
}

// ── QwenArgs ────────────────────────────────────────────────────────────────

func TestQwenArgs(t *testing.T) {
	fn := qwenArgs()

	args := fn(false)
	if !contains(args, "--output-format") || !contains(args, "stream-json") {
		t.Errorf("qwenArgs(false) = %v, missing --output-format stream-json", args)
	}
	if !contains(args, "--include-partial-messages") {
		t.Errorf("qwenArgs(false) = %v, missing --include-partial-messages", args)
	}
	if contains(args, "--approval-mode") {
		t.Errorf("qwenArgs(false) must not include --approval-mode: %v", args)
	}

	argsYolo := fn(true)
	if !contains(argsYolo, "--approval-mode") || !contains(argsYolo, "yolo") {
		t.Errorf("qwenArgs(true) = %v, missing --approval-mode yolo", argsYolo)
	}
}

// ── CodexArgs ────────────────────────────────────────────────────────────────

func TestCodexArgs(t *testing.T) {
	fn := codexArgs("gpt-5.3-codex")

	args := fn(false)
	if !contains(args, "exec") {
		t.Errorf("codexArgs(false) = %v, missing 'exec'", args)
	}
	if !contains(args, "--json") {
		t.Errorf("codexArgs(false) = %v, missing '--json'", args)
	}
	if !contains(args, "-m") || !contains(args, "gpt-5.3-codex") {
		t.Errorf("codexArgs(false) = %v, missing -m gpt-5.3-codex", args)
	}
	if contains(args, "--dangerously-bypass-approvals-and-sandbox") {
		t.Errorf("codexArgs(false) must not include --dangerously-bypass-approvals-and-sandbox: %v", args)
	}

	argsYolo := fn(true)
	if !contains(argsYolo, "--dangerously-bypass-approvals-and-sandbox") {
		t.Errorf("codexArgs(true) = %v, missing --dangerously-bypass-approvals-and-sandbox", argsYolo)
	}
}

func TestCodexArgsAllModels(t *testing.T) {
	models := []string{
		"gpt-5.3-codex",
		"gpt-5.4",
		"gpt-5.2-codex",
		"gpt-5.1-codex-max",
		"gpt-5.2",
		"gpt-5.1-codex-mini",
	}
	for _, model := range models {
		fn := codexArgs(model)
		args := fn(false)
		if !contains(args, model) {
			t.Errorf("codexArgs(%q)(false) = %v, missing model name", model, args)
		}
		argsYolo := fn(true)
		if !contains(argsYolo, "--dangerously-bypass-approvals-and-sandbox") {
			t.Errorf("codexArgs(%q)(true) = %v, missing yolo flag", model, argsYolo)
		}
	}
}

// ── CodexPartParser ──────────────────────────────────────────────────────────

func TestCodexPartParser(t *testing.T) {
	parse := codexPartParser()

	// thread.started is skipped
	ev, _ := json.Marshal(map[string]any{"type": "thread.started", "thread_id": "x"})
	if _, ok := parse(ev); ok {
		t.Error("thread.started should be skipped")
	}

	// turn.started is skipped
	ev, _ = json.Marshal(map[string]any{"type": "turn.started"})
	if _, ok := parse(ev); ok {
		t.Error("turn.started should be skipped")
	}

	// item.started is skipped
	ev, _ = json.Marshal(map[string]any{
		"type": "item.started",
		"item": map[string]any{"type": "command_execution", "command": "ls"},
	})
	if _, ok := parse(ev); ok {
		t.Error("item.started should be skipped")
	}

	// item.completed command_execution is skipped
	ev, _ = json.Marshal(map[string]any{
		"type": "item.completed",
		"item": map[string]any{
			"type":              "command_execution",
			"command":           "ls",
			"aggregated_output": "file.txt",
			"exit_code":         0,
		},
	})
	if _, ok := parse(ev); ok {
		t.Error("item.completed command_execution should be skipped")
	}

	// item.completed agent_message yields TextDelta
	ev, _ = json.Marshal(map[string]any{
		"type": "item.completed",
		"item": map[string]any{
			"type": "agent_message",
			"text": "Here is the answer.",
		},
	})
	part, ok := parse(ev)
	if !ok {
		t.Fatal("item.completed agent_message should yield a part")
	}
	if part.Type != fantasy.StreamPartTypeTextDelta {
		t.Errorf("part.Type = %v, want TextDelta", part.Type)
	}
	if part.Delta != "Here is the answer." {
		t.Errorf("part.Delta = %q, want %q", part.Delta, "Here is the answer.")
	}

	// item.completed agent_message with empty text is skipped
	ev, _ = json.Marshal(map[string]any{
		"type": "item.completed",
		"item": map[string]any{"type": "agent_message", "text": ""},
	})
	if _, ok := parse(ev); ok {
		t.Error("agent_message with empty text should be skipped")
	}

	// turn.completed is skipped (usage handled by ParseUsageLine)
	ev, _ = json.Marshal(map[string]any{
		"type":  "turn.completed",
		"usage": map[string]any{"input_tokens": 100, "output_tokens": 50},
	})
	if _, ok := parse(ev); ok {
		t.Error("turn.completed should be skipped by part parser")
	}

	// invalid JSON is skipped
	if _, ok := parse([]byte("{bad json")); ok {
		t.Error("invalid JSON should be skipped")
	}
}

func contains(ss []string, s string) bool {
	for _, v := range ss {
		if v == s {
			return true
		}
	}
	return false
}

// TestCodexMCPConfigArgs_NoTokenInArgs is a regression test for P2-3:
// codexMCPConfigArgs (the actual production function that builds the -c
// inline overrides passed to the codex CLI's argv) must never embed the raw
// MCP token in the URL or anywhere else in its returned args — argv is
// visible to any local user via `ps` / a process viewer / /proc/<pid>/cmdline,
// unlike an environment variable set only on the child process.
//
// SCOPE NOTE (added on independent review): the delegated /crush fix's own
// version of this test never called codexMCPConfigArgs (or any other
// production function) at all — every assertion compared hand-written
// string literals to themselves (e.g. `expectedEnvVar != "CRUSH_CODEX_MCP_TOKEN"`,
// comparing the constant to its own value) or checked CLISpec.BuildArgs,
// which builds the codex CLI's BASE args ("exec --json -m ...") and never
// includes the MCP-related -c flags at all — those are appended separately,
// later, inside Stream(). Reverting the actual fix left this test green
// unchanged — the eighth vacuous-test occurrence this round. Fixed by
// extracting the MCP arg-building logic out of Stream() into the pure,
// directly-testable codexMCPConfigArgs function (see provider.go), and
// rewriting this test to call it for real.
//
// REVERT CHECK PROCEDURE:
//  1. In provider.go's codexMCPConfigArgs, revert to embedding the token in
//     the URL: `"http://" + addr + "/mcp?token=" + token` (passing a token
//     param in) instead of the bare URL + bearer_token_env_var override.
//  2. Run: go test ./internal/agent/cliprovider -run TestCodexMCPConfigArgs -v
//  3. FAIL: the returned args contain "?token=".
//  4. Restore the fix and PASS.
func TestCodexMCPConfigArgs_NoTokenInArgs(t *testing.T) {
	args := codexMCPConfigArgs("127.0.0.1:54321")

	for _, arg := range args {
		if strings.Contains(arg, "?token=") || strings.Contains(arg, "token=") && !strings.Contains(arg, "bearer_token_env_var") {
			t.Errorf("codexMCPConfigArgs must never embed the raw token in argv, found in: %q (all args: %v)", arg, args)
		}
	}

	joined := strings.Join(args, " ")
	if !strings.Contains(joined, "mcp_servers.crush.url=\"http://127.0.0.1:54321/mcp\"") {
		t.Errorf("codexMCPConfigArgs must set a plain MCP URL with no query string, got: %v", args)
	}
	if !strings.Contains(joined, "mcp_servers.crush.bearer_token_env_var="+codexMCPTokenEnvVar) {
		t.Errorf("codexMCPConfigArgs must reference %s as the bearer_token_env_var, got: %v", codexMCPTokenEnvVar, args)
	}
}

// TestCodexMCPConfigArgs_UsesRealServerToken exercises codexMCPConfigArgs
// against a genuinely running crushMCPServer (not a hand-written literal
// like the test above), confirming the fix holds against the actual random
// token newCrushMCPServer generates, not just a placeholder addr string.
func TestCodexMCPConfigArgs_UsesRealServerToken(t *testing.T) {
	srv, err := newCrushMCPServer(context.Background(), nil, nil, "", t.TempDir(), "", nil)
	if err != nil {
		t.Fatalf("newCrushMCPServer: %v", err)
	}
	t.Cleanup(srv.stop)

	args := codexMCPConfigArgs(srv.addr)
	joined := strings.Join(args, " ")

	if strings.Contains(joined, srv.token) {
		t.Errorf("codexMCPConfigArgs must never embed the real MCP server's token in its returned args, got: %v", args)
	}
	if strings.Contains(joined, "?token=") {
		t.Errorf("codexMCPConfigArgs must not produce a query-param token, got: %v", args)
	}
	if !strings.Contains(joined, "mcp_servers.crush.bearer_token_env_var="+codexMCPTokenEnvVar) {
		t.Errorf("codexMCPConfigArgs must reference %s as the bearer_token_env_var, got: %v", codexMCPTokenEnvVar, args)
	}
}
