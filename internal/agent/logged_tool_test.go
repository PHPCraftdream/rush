package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"strings"
	"testing"
	"unicode/utf8"

	"charm.land/fantasy"
	"github.com/PHPCraftdream/rush/internal/agent/tools"
	"github.com/stretchr/testify/require"
)

// A tool failure that reaches nobody is the failure mode this wrapper
// exists for: a run once died 42 seconds into a 75k-character prompt and
// the whole window in crush.log held no ERROR record at all, so the cause
// had to be recovered by reading source. These tests pin that both kinds of
// failure are written, and at levels that keep the log usable.

// stubTool returns whatever it is told to.
type stubTool struct {
	name string
	resp fantasy.ToolResponse
	err  error
}

func (s stubTool) Info() fantasy.ToolInfo { return fantasy.ToolInfo{Name: s.name} }
func (s stubTool) ProviderOptions() fantasy.ProviderOptions {
	return fantasy.ProviderOptions{}
}

func (s stubTool) SetProviderOptions(fantasy.ProviderOptions) {}

func (s stubTool) Run(context.Context, fantasy.ToolCall) (fantasy.ToolResponse, error) {
	return s.resp, s.err
}

// captureLogs swaps the default slog handler for one writing JSON into a
// buffer, and returns a reader over the records.
func captureLogs(t *testing.T) func() []map[string]any {
	t.Helper()
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(prev) })

	return func() []map[string]any {
		var out []map[string]any
		for _, line := range strings.Split(strings.TrimSpace(buf.String()), "\n") {
			if line == "" {
				continue
			}
			var rec map[string]any
			if err := json.Unmarshal([]byte(line), &rec); err == nil {
				out = append(out, rec)
			}
		}
		return out
	}
}

func runWrapped(t *testing.T, inner fantasy.AgentTool) []map[string]any {
	t.Helper()
	records := captureLogs(t)
	wrapped := wrapToolsWithErrorLogging([]fantasy.AgentTool{inner})
	require.Len(t, wrapped, 1)

	ctx := context.WithValue(context.Background(), tools.SessionIDContextKey, "sess-42")
	_, _ = wrapped[0].Run(ctx, fantasy.ToolCall{ID: "call-7", Name: inner.Info().Name})
	return records()
}

func TestLoggedTool_FatalErrorIsLoggedAtError(t *testing.T) {
	recs := runWrapped(t, stubTool{
		name: "write",
		err:  errors.New("disk is on fire"),
	})

	require.Len(t, recs, 1, "a returned error must produce exactly one record")
	rec := recs[0]
	require.Equal(t, "ERROR", rec["level"],
		"a returned error ends the whole run; anything quieter than ERROR buries it")
	require.Equal(t, "write", rec["tool"])
	require.Equal(t, "sess-42", rec["session_id"],
		"without the session id the record cannot be tied to the run that died — "+
			"exactly what was missing when this had to be diagnosed from source")
	require.Equal(t, "call-7", rec["tool_call_id"])
	require.Equal(t, "fatal", rec["level_kind"])
	require.Contains(t, rec["err"], "disk is on fire")
}

func TestLoggedTool_RecoverableErrorIsLoggedAtWarn(t *testing.T) {
	recs := runWrapped(t, stubTool{
		name: "view",
		resp: fantasy.NewTextErrorResponse("File not found: nope.txt"),
	})

	require.Len(t, recs, 1)
	rec := recs[0]
	require.Equal(t, "WARN", rec["level"],
		"the model mistyping a path is expected; at ERROR it would drown the failures "+
			"that actually end a session")
	require.Equal(t, "view", rec["tool"])
	require.Equal(t, "sess-42", rec["session_id"])
	require.Equal(t, "recoverable", rec["level_kind"])
	require.Contains(t, rec["content"], "nope.txt")
}

// The control: success must stay silent, or the log becomes a transcript of
// every tool call and stops being readable.
func TestLoggedTool_SuccessLogsNothing(t *testing.T) {
	recs := runWrapped(t, stubTool{
		name: "view",
		resp: fantasy.NewTextResponse("file contents"),
	})
	require.Empty(t, recs, "a successful tool call must not be logged")
}

// An unbounded error body must not reach the log. The model can retry a
// malformed call in a loop, and these bodies carry whatever the far side
// sent back, so whole ones would grow crush.log without limit.
func TestLoggedTool_LongErrorContentIsTruncated(t *testing.T) {
	long := strings.Repeat("x", maxLoggedContentRunes*3)
	recs := runWrapped(t, stubTool{name: "fetch", resp: fantasy.NewTextErrorResponse(long)})

	require.Len(t, recs, 1)
	logged, ok := recs[0]["content"].(string)
	require.True(t, ok)
	require.Less(t, utf8.RuneCountInString(logged), utf8.RuneCountInString(long),
		"the record must be shorter than the body it came from")
	require.Contains(t, logged, "truncated",
		"a clipped message that does not say so reads as the whole message")
}

// Control: a body inside the limit must survive byte-for-byte, or the
// assertion above would also pass with a wrapper that mangles everything.
func TestLoggedTool_ShortErrorContentIsUntouched(t *testing.T) {
	recs := runWrapped(t, stubTool{
		name: "view",
		resp: fantasy.NewTextErrorResponse("File not found: nope.txt"),
	})
	require.Len(t, recs, 1)
	require.Equal(t, "File not found: nope.txt", recs[0]["content"])
}

// Truncation counts runes, not bytes: cutting mid-rune would put invalid
// UTF-8 into the log, and every non-ASCII message would be at risk.
func TestTruncateForLog_NeverSplitsARune(t *testing.T) {
	// Cyrillic is two bytes per rune, so a byte-based cut at an odd
	// boundary would produce a replacement character.
	s := strings.Repeat("щ", 100)
	got := truncateForLog(s, 41)
	require.True(t, utf8.ValidString(got), "the result must stay valid UTF-8")
	require.Equal(t, 41, utf8.RuneCountInString(strings.Split(got, "…")[0]))
}

// Wrapping happens at the doors into a sessionAgent, not at the places a
// tool slice is assembled. That is the whole point: the wrapper was first
// applied at the buildTools call site, and agentic_fetch — which builds its
// own six tools and never goes through buildTools — was silently left out.
// These two tests fail if either door stops wrapping, which is what makes
// "every tool, every path" checkable instead of a promise in a comment.
func TestSessionAgent_ConstructorWrapsToolsForLogging(t *testing.T) {
	agent := NewSessionAgent(SessionAgentOptions{
		Tools: []fantasy.AgentTool{stubTool{name: "view"}, stubTool{name: "bash"}},
	}).(*sessionAgent)

	for _, tool := range agent.tools.Copy() {
		require.IsType(t, &loggedTool{}, tool,
			"a tool handed to NewSessionAgent must be wrapped; agentic_fetch reaches "+
				"the agent this way and no other")
	}
}

func TestSessionAgent_SetToolsWrapsToolsForLogging(t *testing.T) {
	agent := NewSessionAgent(SessionAgentOptions{}).(*sessionAgent)
	agent.SetTools([]fantasy.AgentTool{stubTool{name: "view"}})

	tools := agent.tools.Copy()
	require.Len(t, tools, 1)
	require.IsType(t, &loggedTool{}, tools[0],
		"the coordinator installs its tools through SetTools, not the constructor")
}

// Wrapping twice must not log twice. Both doors call the wrapper, so a
// slice that passes through the constructor and then SetTools — or a caller
// that wraps defensively — would otherwise double every record.
func TestWrapToolsWithErrorLogging_IsIdempotent(t *testing.T) {
	once := wrapToolsWithErrorLogging([]fantasy.AgentTool{stubTool{
		name: "write",
		err:  errors.New("disk is on fire"),
	}})
	twice := wrapToolsWithErrorLogging(once)
	require.Same(t, once[0], twice[0], "re-wrapping must return the same tool, not nest")

	records := captureLogs(t)
	_, _ = twice[0].Run(context.Background(), fantasy.ToolCall{ID: "c", Name: "write"})
	require.Len(t, records(), 1, "one failure must produce exactly one record")
}

// The wrapper must be transparent: whatever the inner tool returned has to
// come back unchanged, or logging would have altered behaviour.
func TestLoggedTool_PassesThroughUnchanged(t *testing.T) {
	inner := stubTool{name: "view", resp: fantasy.NewTextResponse("payload"), err: errors.New("boom")}
	wrapped := newLoggedTool(inner)

	resp, err := wrapped.Run(context.Background(), fantasy.ToolCall{ID: "c", Name: "view"})
	require.EqualError(t, err, "boom")
	require.Equal(t, "payload", resp.Content)
	require.Equal(t, "view", wrapped.Info().Name)
}
