// Per-CLI output parsing: the JSONL envelopes emitted by the claude,
// gemini and codex CLIs, the stateful part parsers that map lines to
// stream parts, and the per-line usage extractors.

package cliprovider

import (
	"encoding/json"
	"log/slog"

	"charm.land/fantasy"
)

// streamEvent is the JSON envelope for Claude CLI stream-json output.
// Only the fields relevant to text extraction are parsed.
type streamEvent struct {
	Type      string `json:"type"`
	Subtype   string `json:"subtype,omitempty"`    // "init" for system init events
	SessionID string `json:"session_id,omitempty"` // CLI session ID from init/stream events
	// stream_event: raw Anthropic API SSE event forwarded by claude CLI (--verbose).
	// content_block_delta events carry text tokens (text_delta) or thinking (thinking_delta).
	Event struct {
		Type  string `json:"type"`
		Index int    `json:"index"`
		Delta struct {
			Type     string `json:"type"`     // "text_delta" or "thinking_delta"
			Text     string `json:"text"`     // text_delta content
			Thinking string `json:"thinking"` // thinking_delta content
		} `json:"delta"`
		ContentBlock struct {
			Type string `json:"type"` // "text" or "thinking"
		} `json:"content_block"`
	} `json:"event"`
	// assistant: accumulated text snapshot (--include-partial-messages)
	Message struct {
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
	} `json:"message"`
	// Claude CLI result event usage (snake_case).
	Usage struct {
		InputTokens              int64 `json:"input_tokens"`
		OutputTokens             int64 `json:"output_tokens"`
		CacheCreationInputTokens int64 `json:"cache_creation_input_tokens"`
		CacheReadInputTokens     int64 `json:"cache_read_input_tokens"`
	} `json:"usage"`
}

// geminiCLIEvent is the JSONL envelope emitted by `gemini --output-format stream-json`.
//
// Actual format (verified against @google/gemini-cli v0.32+):
//
//	{"type":"init","session_id":"...","model":"..."}
//	{"type":"message","role":"user","content":"..."}
//	{"type":"message","role":"assistant","content":"<delta text>","delta":true}
//	{"type":"result","status":"success","stats":{"total_tokens":N,"input_tokens":N,"output_tokens":N}}
type geminiCLIEvent struct {
	Type    string `json:"type"`
	Role    string `json:"role"`
	Content string `json:"content"`
	Delta   bool   `json:"delta"`
	Status  string `json:"status"`
	Stats   struct {
		TotalTokens  int64 `json:"total_tokens"`
		InputTokens  int64 `json:"input_tokens"`
		OutputTokens int64 `json:"output_tokens"`
		// Cached is cache-read tokens; Input is the uncached remainder.
		// InputTokens == Input + Cached (see geminiParseUsageLine). Both
		// were missing here and silently discarded at unmarshal.
		Cached int64 `json:"cached"`
		Input  int64 `json:"input"`
	} `json:"stats"`
}

// claudePartParser returns a stateful parser for Claude CLI stream-json output.
// With --verbose, claude CLI emits "stream_event" events wrapping raw Anthropic
// API SSE events. Handles both text tokens (text_delta) and thinking tokens
// (thinking_delta) so the reasoning box is visible during extended thinking.
func claudePartParser() func([]byte) (fantasy.StreamPart, bool) {
	const id = "0"
	var inThinking bool
	return func(line []byte) (fantasy.StreamPart, bool) {
		var ev streamEvent
		if json.Unmarshal(line, &ev) != nil {
			return fantasy.StreamPart{}, false
		}
		if ev.Type != "stream_event" {
			return fantasy.StreamPart{}, false
		}
		switch ev.Event.Type {
		case "content_block_start":
			slog.Debug("cliprovider: content_block_start", "block_type", ev.Event.ContentBlock.Type)
			if ev.Event.ContentBlock.Type == "thinking" {
				inThinking = true
				return fantasy.StreamPart{Type: fantasy.StreamPartTypeReasoningStart, ID: id}, true
			}
		case "content_block_stop":
			if inThinking {
				inThinking = false
				return fantasy.StreamPart{Type: fantasy.StreamPartTypeReasoningEnd, ID: id}, true
			}
		case "content_block_delta":
			switch ev.Event.Delta.Type {
			case "thinking_delta":
				if ev.Event.Delta.Thinking != "" {
					return fantasy.StreamPart{Type: fantasy.StreamPartTypeReasoningDelta, ID: id, Delta: ev.Event.Delta.Thinking}, true
				}
			case "text_delta":
				if ev.Event.Delta.Text != "" {
					return fantasy.StreamPart{Type: fantasy.StreamPartTypeTextDelta, ID: id, Delta: ev.Event.Delta.Text}, true
				}
			}
		}
		return fantasy.StreamPart{}, false
	}
}

// geminiPartParser returns a parser for Gemini CLI stream-json output.
//
// Gemini CLI (--output-format stream-json) emits JSONL events where assistant
// text arrives as:
//
//	{"type":"message","role":"assistant","content":"<delta>","delta":true}
//
// Each event carries an incremental text delta.  Non-assistant events
// (init, user message echo, result) are silently skipped.
func geminiPartParser() func([]byte) (fantasy.StreamPart, bool) {
	const id = "0"
	return func(line []byte) (fantasy.StreamPart, bool) {
		var ev geminiCLIEvent
		if json.Unmarshal(line, &ev) != nil {
			return fantasy.StreamPart{}, false
		}
		if ev.Type == "message" && ev.Role == "assistant" && ev.Content != "" {
			return fantasy.StreamPart{Type: fantasy.StreamPartTypeTextDelta, ID: id, Delta: ev.Content}, true
		}
		return fantasy.StreamPart{}, false
	}
}

// claudeParseUsageLine extracts token usage from a Claude CLI "result" event.
//
// Claude's input_tokens is EXCLUSIVE of both cache counters — verified by
// running the CLI three times against a fixed prompt: input_tokens held at 10
// while cache_creation/cache_read shifted between 6203+17298 and 0+23501. The
// previous implementation summed all three into InputTokens, which produced
// the right prompt total but destroyed the cache breakdown, making a cache-hit
// statistic impossible downstream. See usage.go for the shared convention.
func claudeParseUsageLine(line []byte) (fantasy.Usage, bool) {
	var ev streamEvent
	if json.Unmarshal(line, &ev) != nil {
		return fantasy.Usage{}, false
	}
	if ev.Type != "result" {
		return fantasy.Usage{}, false
	}
	raw := rawUsage{
		input:              ev.Usage.InputTokens,
		output:             ev.Usage.OutputTokens,
		cacheCreation:      ev.Usage.CacheCreationInputTokens,
		cacheRead:          ev.Usage.CacheReadInputTokens,
		inputIncludesCache: false,
		reportsCache:       true,
	}
	if raw.isEmpty() {
		return fantasy.Usage{}, false
	}
	u := raw.normalize()
	logUsage("claude", raw, u)
	return u, true
}

// geminiParseUsageLine extracts token usage from the Gemini CLI result event.
//
// The Gemini CLI emits a final event (verified against gemini 0.55.1):
//
//	{"type":"result","status":"success","stats":{
//	  "total_tokens":12898,"input_tokens":12601,"output_tokens":1,
//	  "cached":8148,"input":4453,"duration_ms":25076,"tool_calls":0}}
//
// input_tokens INCLUDES cached — the event proves it itself by also emitting
// the exclusive `input`: 4453 + 8148 == 12601. Both `cached` and `input` were
// previously absent from geminiCLIEvent and therefore dropped at unmarshal,
// which is why gemini looked like it reported no cache data at all.
func geminiParseUsageLine(line []byte) (fantasy.Usage, bool) {
	var ev geminiCLIEvent
	if json.Unmarshal(line, &ev) != nil {
		return fantasy.Usage{}, false
	}
	if ev.Type != "result" {
		return fantasy.Usage{}, false
	}
	raw := rawUsage{
		input:              ev.Stats.InputTokens,
		output:             ev.Stats.OutputTokens,
		cacheRead:          ev.Stats.Cached,
		inputIncludesCache: true,
		reportsCache:       true,
		// gemini's total_tokens exceeds input_tokens+output_tokens (12898 vs
		// 12602 on a real run) because it also counts thinking tokens that
		// the stats block does not itemize. Keep its number.
		totalOverride: ev.Stats.TotalTokens,
	}
	if raw.isEmpty() {
		return fantasy.Usage{}, false
	}
	u := raw.normalize()
	logUsage("gemini", raw, u)
	return u, true
}

// codexEvent is the top-level JSONL envelope emitted by `codex exec --json`.
type codexEvent struct {
	Type string `json:"type"`
	// item.started / item.completed
	Item struct {
		Type             string `json:"type"`              // "agent_message" | "command_execution" | "reasoning" | ...
		Text             string `json:"text"`              // agent_message: full response text
		Command          string `json:"command"`           // command_execution: command string
		AggregatedOutput string `json:"aggregated_output"` // command_execution: combined stdout+stderr
	} `json:"item"`
	// turn.completed usage. input_tokens is the prompt TOTAL and already
	// contains cached_input_tokens — see codexParseUsageLine for the
	// measurement that establishes this.
	Usage struct {
		InputTokens           int64 `json:"input_tokens"`
		CachedInputTokens     int64 `json:"cached_input_tokens"`
		CacheWriteInputTokens int64 `json:"cache_write_input_tokens"`
		OutputTokens          int64 `json:"output_tokens"`
		ReasoningOutputTokens int64 `json:"reasoning_output_tokens"`
	} `json:"usage"`
}

// codexPartParser returns a stateful parser for `codex exec --json` JSONL output.
// Text is NOT streamed token-by-token; the full response arrives in a single
// item.completed event with type "agent_message". We emit it as one TextDelta.
func codexPartParser() func([]byte) (fantasy.StreamPart, bool) {
	const id = "0"
	return func(line []byte) (fantasy.StreamPart, bool) {
		var ev codexEvent
		if json.Unmarshal(line, &ev) != nil {
			return fantasy.StreamPart{}, false
		}
		if ev.Type == "item.completed" && ev.Item.Type == "agent_message" && ev.Item.Text != "" {
			return fantasy.StreamPart{Type: fantasy.StreamPartTypeTextDelta, ID: id, Delta: ev.Item.Text}, true
		}
		return fantasy.StreamPart{}, false
	}
}

// codexParseUsageLine extracts token usage from a Codex `turn.completed` event.
//
// Real event (codex 0.147.0):
//
//	{"type":"turn.completed","usage":{"input_tokens":16856,
//	  "cached_input_tokens":6912,"cache_write_input_tokens":0,
//	  "output_tokens":5,"reasoning_output_tokens":0}}
//
// codex's input_tokens INCLUDES cached_input_tokens, exactly like OpenAI's
// prompt_tokens. Verified by three consecutive runs of a fixed prompt:
// input_tokens stayed at 16856 while cached_input_tokens moved 6912 -> 5888.
// Were it exclusive, it would have risen as the cache share fell.
//
// The previous implementation computed `input_tokens + cached_input_tokens`,
// double-counting every cached token — 23768 instead of 16856 on the sample
// above, a 41% overstatement of prompt size that inflated session.PromptTokens
// and pulled auto-summarization forward. cache_write_input_tokens and
// reasoning_output_tokens were not in codexEvent at all and were dropped at
// unmarshal.
func codexParseUsageLine(line []byte) (fantasy.Usage, bool) {
	var ev codexEvent
	if json.Unmarshal(line, &ev) != nil {
		return fantasy.Usage{}, false
	}
	if ev.Type != "turn.completed" {
		return fantasy.Usage{}, false
	}
	raw := rawUsage{
		input:              ev.Usage.InputTokens,
		output:             ev.Usage.OutputTokens,
		cacheRead:          ev.Usage.CachedInputTokens,
		cacheCreation:      ev.Usage.CacheWriteInputTokens,
		reasoning:          ev.Usage.ReasoningOutputTokens,
		inputIncludesCache: true,
		reportsCache:       true,
	}
	if raw.isEmpty() {
		return fantasy.Usage{}, false
	}
	u := raw.normalize()
	logUsage("codex", raw, u)
	return u, true
}
