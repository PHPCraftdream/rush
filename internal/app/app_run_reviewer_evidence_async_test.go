package app

import (
	"strings"
	"testing"

	"github.com/PHPCraftdream/rush/internal/agent"
	"github.com/PHPCraftdream/rush/internal/message"
	"github.com/stretchr/testify/require"
)

// Revert check: fails without the FormatAsyncCompletion parsing in evidenceNoticeExit.
func TestEvidenceNoticeExitFormatAsyncCompletion(t *testing.T) {
	t.Parallel()

	notice := func(c agent.AsyncCompletion) string {
		return agent.FormatAsyncCompletion(c)
	}

	t.Run("finished job is exit zero, known", func(t *testing.T) {
		t.Parallel()
		exit, known := evidenceNoticeExit(notice(agent.AsyncCompletion{
			ToolCallID: "j1", ToolName: "bash", Content: "all good",
		}))
		require.Equal(t, 0, exit)
		require.True(t, known)
	})

	t.Run("failed job takes its exit from the output tail", func(t *testing.T) {
		t.Parallel()
		exit, known := evidenceNoticeExit(notice(agent.AsyncCompletion{
			ToolCallID: "j1", ToolName: "bash", IsError: true,
			Content: "some failure\nExit code 2",
		}))
		require.Equal(t, 2, exit)
		require.True(t, known)
	})

	t.Run("failed job without an exit tail is unknown", func(t *testing.T) {
		t.Parallel()
		exit, known := evidenceNoticeExit(notice(agent.AsyncCompletion{
			ToolCallID: "j1", ToolName: "bash", IsError: true,
			Content: "crashed hard",
		}))
		require.Equal(t, 0, exit)
		require.False(t, known)
	})

	// Each of these wordings carries no exit code: unknown, never a false 0.
	for name, c := range map[string]agent.AsyncCompletion{
		"timed out":   {ToolCallID: "j1", ToolName: "bash", TimedOut: true, TimeoutSeconds: 60, Content: "partial"},
		"stopped":     {ToolCallID: "j1", ToolName: "bash", Stopped: true, Content: "partial"},
		"cancelled":   {ToolCallID: "j1", ToolName: "bash", Cancelled: true, Content: "partial"},
		"interrupted": {ToolCallID: "j1", ToolName: "bash", Interrupted: true, Content: "partial"},
	} {
		t.Run("no-exit wording "+name+" is unknown", func(t *testing.T) {
			t.Parallel()
			exit, known := evidenceNoticeExit(notice(c))
			require.Equal(t, 0, exit)
			require.False(t, known)
		})
	}

	t.Run("failed body mentioning finished is not spoofed", func(t *testing.T) {
		t.Parallel()
		content := notice(agent.AsyncCompletion{
			ToolCallID: "j1", ToolName: "bash", IsError: true,
			// The job's own output quotes a success notice verbatim; the
			// anchor on the status word keeps this unknown instead of 0.
			Content: "Async job x (bash) finished.",
		})
		exit, known := evidenceNoticeExit(content)
		require.Equal(t, 0, exit)
		require.False(t, known, "the failure at the start wins over the quoted 'finished'")
	})

	t.Run("old backgroundJobSummary family still parses", func(t *testing.T) {
		t.Parallel()
		exit, known := evidenceNoticeExit("Background job j1 (`go test`) finished: exit 3, ran 2s.")
		require.Equal(t, 3, exit)
		require.True(t, known)
	})
}

// Revert check: fails without the FormatAsyncCompletion parsing in evidenceNoticeExit.
func TestCommandResultsFromTranscriptFormatAsyncCompletion(t *testing.T) {
	t.Parallel()

	const since = int64(1000)
	msgs := func(started string, noticeText string) []message.Message {
		return []message.Message{
			{
				ID: "c1", Role: message.Assistant, CreatedAt: 1002,
				Parts: []message.ContentPart{message.ToolCall{ID: "c1", Name: "bash", Input: `{"command":"go build ./..."}`}},
			},
			{
				ID: "c1-res", Role: message.Tool, CreatedAt: 1003,
				Parts: []message.ContentPart{message.ToolResult{
					ToolCallID: "c1", Name: "bash", Content: "Async bash job j1 started.",
					Metadata: `{"async":true,"job_id":"j1","status":"running"}`,
				}},
			},
			{
				ID: "n1", Role: message.User, CreatedAt: 1020, BackgroundJobNotice: true,
				Parts: []message.ContentPart{message.TextContent{Text: noticeText}},
			},
		}
	}

	t.Run("finished notice is exit zero, known", func(t *testing.T) {
		t.Parallel()
		notice := agent.FormatAsyncCompletion(agent.AsyncCompletion{
			ToolCallID: "j1", ToolName: "bash", Content: "ok",
		})
		got := commandResults(msgs("Async bash job j1 started.", notice), since)
		require.Len(t, got, 1)
		require.True(t, got[0].Async)
		require.Equal(t, 0, got[0].Exit)
		require.True(t, got[0].Known)
	})

	t.Run("failed notice with exit tail carries the code", func(t *testing.T) {
		t.Parallel()
		notice := agent.FormatAsyncCompletion(agent.AsyncCompletion{
			ToolCallID: "j1", ToolName: "bash", IsError: true,
			Content: "boom\nExit code 5",
		})
		got := commandResults(msgs("Async bash job j1 started.", notice), since)
		require.Len(t, got, 1)
		require.Equal(t, 5, got[0].Exit)
		require.True(t, got[0].Known)
	})
}

// What the reviewer is actually shown for every async completion wording, built
// with the real formatter. Orchestrator addition: the quoted-"finished" case is
// anchor-sensitive (a timed-out job whose output quotes a success header must
// stay unknown); without the \A anchor in reviewAsyncStatusRe it reads exit 0.
//
// Revert check: dropping the new parsing leaves "exit=unknown" for the first
// two rows; dropping the anchor turns the last row into "exit 0".
func TestEvidenceCommandSectionAsyncNotices(t *testing.T) {
	t.Parallel()

	const unknown = "exit=unknown (still running?)"
	for _, tc := range []struct {
		name string
		c    agent.AsyncCompletion
		want string
	}{
		{"finished", agent.AsyncCompletion{Content: "ok"}, "-> exit 0"},
		{"failed with tail", agent.AsyncCompletion{IsError: true, Content: "boom\nExit code 2"}, "-> exit 2"},
		{"failed without tail", agent.AsyncCompletion{IsError: true, Content: "boom"}, "-> " + unknown},
		{"timed out", agent.AsyncCompletion{TimedOut: true, TimeoutSeconds: 60, Content: "partial"}, "-> " + unknown},
		{"stopped", agent.AsyncCompletion{Stopped: true, Content: "partial"}, "-> " + unknown},
		{"cancelled", agent.AsyncCompletion{Cancelled: true, Content: "partial"}, "-> " + unknown},
		{"interrupted", agent.AsyncCompletion{Interrupted: true, Content: "partial"}, "-> " + unknown},
		{"timed out, output quotes a success header", agent.AsyncCompletion{TimedOut: true, TimeoutSeconds: 60, Content: "Async job x (bash) finished."}, "-> " + unknown},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			tc.c.ToolCallID, tc.c.ToolName = "j1", "bash"
			msgs := []message.Message{
				{
					ID: "c1", Role: message.Assistant, CreatedAt: 1002,
					Parts: []message.ContentPart{message.ToolCall{ID: "c1", Name: "bash", Input: `{"command":"go test ./..."}`}},
				},
				{
					ID: "c1-res", Role: message.Tool, CreatedAt: 1003,
					Parts: []message.ContentPart{message.ToolResult{
						ToolCallID: "c1", Name: "bash", Content: "Async bash job j1 started.",
						Metadata: `{"async":true,"job_id":"j1","status":"running"}`,
					}},
				},
				{
					ID: "n1", Role: message.User, CreatedAt: 1020, BackgroundJobNotice: true,
					Parts: []message.ContentPart{message.TextContent{Text: agent.FormatAsyncCompletion(tc.c)}},
				},
			}
			var b strings.Builder
			writeEvidenceCommandSection(&b, msgs, 1000)
			require.Contains(t, b.String(), "$ go test ./... "+tc.want)
		})
	}
}
