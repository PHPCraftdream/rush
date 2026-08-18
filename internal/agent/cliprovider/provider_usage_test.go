// Usage-extraction tests: token accounting in codexParseUsageLine,
// claudeParseUsageLine and geminiParseUsageLine from each CLI's final
// JSONL line.

package cliprovider

import (
	"encoding/json"
	"testing"
)

// ── CodexParseUsageLine ──────────────────────────────────────────────────────

func TestCodexParseUsageLine(t *testing.T) {
	// turn.completed with all token fields
	ev, _ := json.Marshal(map[string]any{
		"type": "turn.completed",
		"usage": map[string]any{
			"input_tokens":        8520,
			"cached_input_tokens": 6528,
			"output_tokens":       9,
		},
	})
	usage, ok := codexParseUsageLine(ev)
	if !ok {
		t.Fatal("expected usage from turn.completed")
	}
	// codex's input_tokens (8520) ALREADY includes cached_input_tokens (6528).
	// These assertions previously read 8520+6528, which counted the cached
	// share twice; see codexParseUsageLine for the measurement that settles
	// the convention.
	if usage.InputTokens != 8520-6528 {
		t.Errorf("InputTokens = %d, want %d (total minus cached)", usage.InputTokens, 8520-6528)
	}
	if usage.CacheReadTokens != 6528 {
		t.Errorf("CacheReadTokens = %d, want %d", usage.CacheReadTokens, 6528)
	}
	if usage.OutputTokens != 9 {
		t.Errorf("OutputTokens = %d, want %d", usage.OutputTokens, 9)
	}
	if usage.TotalTokens != 8520+9 {
		t.Errorf("TotalTokens = %d, want %d", usage.TotalTokens, 8520+9)
	}

	// non-turn.completed events are skipped
	ev, _ = json.Marshal(map[string]any{"type": "item.completed"})
	if _, ok := codexParseUsageLine(ev); ok {
		t.Error("item.completed should not produce usage")
	}

	// turn.completed with zero usage is skipped
	ev, _ = json.Marshal(map[string]any{"type": "turn.completed", "usage": map[string]any{}})
	if _, ok := codexParseUsageLine(ev); ok {
		t.Error("turn.completed with zero usage should be skipped")
	}

	// invalid JSON is skipped
	if _, ok := codexParseUsageLine([]byte("not json")); ok {
		t.Error("invalid JSON should be skipped")
	}
}

// ── ClaudeParseUsageLine ─────────────────────────────────────────────────────

func TestClaudeParseUsageLine(t *testing.T) {
	// result event with full usage
	ev, _ := json.Marshal(map[string]any{
		"type": "result",
		"usage": map[string]any{
			"input_tokens":                100,
			"output_tokens":               50,
			"cache_creation_input_tokens": 200,
			"cache_read_input_tokens":     300,
		},
	})
	usage, ok := claudeParseUsageLine(ev)
	if !ok {
		t.Fatal("expected usage from result event")
	}
	// Claude's input_tokens is exclusive of both cache counters, so it is
	// passed through as-is and the cache split is preserved. This used to
	// assert the folded sum (100+200+300), which produced the right prompt
	// total but made a cache-hit statistic impossible to recover.
	if usage.InputTokens != 100 {
		t.Errorf("InputTokens = %d, want 100 (claude reports it already exclusive)", usage.InputTokens)
	}
	if usage.CacheCreationTokens != 200 {
		t.Errorf("CacheCreationTokens = %d, want 200", usage.CacheCreationTokens)
	}
	if usage.CacheReadTokens != 300 {
		t.Errorf("CacheReadTokens = %d, want 300", usage.CacheReadTokens)
	}
	if usage.OutputTokens != 50 {
		t.Errorf("OutputTokens = %d, want %d", usage.OutputTokens, 50)
	}
	if usage.TotalTokens != 100+200+300+50 {
		t.Errorf("TotalTokens = %d, want %d", usage.TotalTokens, 100+200+300+50)
	}

	// non-result events are skipped
	ev, _ = json.Marshal(map[string]any{"type": "stream_event"})
	if _, ok := claudeParseUsageLine(ev); ok {
		t.Error("stream_event should not produce usage")
	}

	// result with zero usage is skipped
	ev, _ = json.Marshal(map[string]any{"type": "result", "usage": map[string]any{}})
	if _, ok := claudeParseUsageLine(ev); ok {
		t.Error("result with zero usage should be skipped")
	}
}

// ── GeminiParseUsageLine ─────────────────────────────────────────────────────

func TestGeminiParseUsageLine(t *testing.T) {
	// result event with stats
	ev, _ := json.Marshal(map[string]any{
		"type":   "result",
		"status": "success",
		"stats": map[string]any{
			"total_tokens":  10267,
			"input_tokens":  10100,
			"output_tokens": 42,
		},
	})
	usage, ok := geminiParseUsageLine(ev)
	if !ok {
		t.Fatal("expected usage from gemini result event")
	}
	if usage.InputTokens != 10100 {
		t.Errorf("InputTokens = %d, want 10100", usage.InputTokens)
	}
	if usage.OutputTokens != 42 {
		t.Errorf("OutputTokens = %d, want 42", usage.OutputTokens)
	}
	if usage.TotalTokens != 10267 {
		t.Errorf("TotalTokens = %d, want 10267", usage.TotalTokens)
	}

	// non-result events are skipped
	ev, _ = json.Marshal(map[string]any{"type": "message", "role": "assistant", "content": "hi"})
	if _, ok := geminiParseUsageLine(ev); ok {
		t.Error("message event should not produce usage")
	}

	// result with zero tokens is skipped
	ev, _ = json.Marshal(map[string]any{"type": "result", "status": "success", "stats": map[string]any{}})
	if _, ok := geminiParseUsageLine(ev); ok {
		t.Error("result with zero tokens should be skipped")
	}
}
