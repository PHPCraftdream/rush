package app

import (
	"strings"
	"testing"

	"github.com/PHPCraftdream/rush/internal/message"
	"github.com/stretchr/testify/require"
)

// batchNotice renders the literal batch completion text production persists
// for a terminal state. The legacy formatter fixture is gone; the wordings
// here are the production carrier texts ("finished." / "failed." / the
// no-exit wordings "timed out", "was stopped", "was cancelled", "was
// interrupted").
func batchNotice(header, content string) string {
	if content == "" {
		return "Async job j1 (bash) " + header
	}
	return "Async job j1 (bash) " + header + "\n\n" + content
}

// Revert check: fails without the status-word parsing in evidenceNoticeExit.
func TestEvidenceNoticeExitBatchCompletion(t *testing.T) {
	t.Parallel()

	t.Run("finished job is exit zero, known", func(t *testing.T) {
		t.Parallel()
		exit, known := evidenceNoticeExit(batchNotice("finished.", "all good"))
		require.Equal(t, 0, exit)
		require.True(t, known)
	})

	t.Run("failed job takes its exit from the output tail", func(t *testing.T) {
		t.Parallel()
		exit, known := evidenceNoticeExit(batchNotice("failed.", "some failure\nExit code 2"))
		require.Equal(t, 2, exit)
		require.True(t, known)
	})

	t.Run("failed job without an exit tail is unknown", func(t *testing.T) {
		t.Parallel()
		exit, known := evidenceNoticeExit(batchNotice("failed.", "crashed hard"))
		require.Equal(t, 0, exit)
		require.False(t, known)
	})

	// Each of these wordings carries no exit code: unknown, never a false 0.
	for name, notice := range map[string]string{
		"timed out":   batchNotice("timed out after 60s and was stopped. Partial output:", "partial"),
		"stopped":     batchNotice("was stopped. Partial output before the stop:", "partial"),
		"cancelled":   batchNotice("was cancelled. Partial output:", "partial"),
		"interrupted": batchNotice("was interrupted: its host process ended before the job finished.", "partial"),
	} {
		t.Run("no-exit wording "+name+" is unknown", func(t *testing.T) {
			t.Parallel()
			exit, known := evidenceNoticeExit(notice)
			require.Equal(t, 0, exit)
			require.False(t, known)
		})
	}

	t.Run("failed body mentioning finished is not spoofed", func(t *testing.T) {
		t.Parallel()
		// The job's own output quotes a success notice verbatim; the anchor
		// on the status word keeps this unknown instead of 0.
		content := batchNotice("failed.", "Async job x (bash) finished.")
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

// Revert check: fails without the status-word parsing in evidenceNoticeExit.
func TestCommandResultsFromTranscriptBatchCompletion(t *testing.T) {
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
		notice := batchNotice("finished.", "ok")
		got := commandResults(msgs("Async bash job j1 started.", notice), since)
		require.Len(t, got, 1)
		require.True(t, got[0].Async)
		require.Equal(t, 0, got[0].Exit)
		require.True(t, got[0].Known)
	})

	t.Run("failed notice with exit tail carries the code", func(t *testing.T) {
		t.Parallel()
		notice := batchNotice("failed.", "boom\nExit code 5")
		got := commandResults(msgs("Async bash job j1 started.", notice), since)
		require.Len(t, got, 1)
		require.Equal(t, 5, got[0].Exit)
		require.True(t, got[0].Known)
	})
}

// What the reviewer is actually shown for every batch completion wording,
// built with the literal production carrier texts. Orchestrator addition: the
// quoted-"finished" case is anchor-sensitive (a timed-out job whose output
// quotes a success header must stay unknown); without the \A anchor in
// reviewAsyncStatusRe it reads exit 0.
//
// Revert check: dropping the new parsing leaves "exit=unknown" for the first
// two rows; dropping the anchor turns the last row into "exit 0".
func TestEvidenceCommandSectionAsyncNotices(t *testing.T) {
	t.Parallel()

	const unknown = "exit=unknown (still running?)"
	for _, tc := range []struct {
		name string
		want string
	}{
		{"finished", "-> exit 0"},
		{"failed with tail", "-> exit 2"},
		{"failed without tail", "-> " + unknown},
		{"timed out", "-> " + unknown},
		{"stopped", "-> " + unknown},
		{"cancelled", "-> " + unknown},
		{"interrupted", "-> " + unknown},
		{"timed out, output quotes a success header", "-> " + unknown},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var notice string
			switch tc.name {
			case "finished":
				notice = batchNotice("finished.", "ok")
			case "failed with tail":
				notice = batchNotice("failed.", "boom\nExit code 2")
			case "failed without tail":
				notice = batchNotice("failed.", "boom")
			case "timed out":
				notice = batchNotice("timed out after 60s and was stopped. Partial output:", "partial")
			case "stopped":
				notice = batchNotice("was stopped. Partial output before the stop:", "partial")
			case "cancelled":
				notice = batchNotice("was cancelled. Partial output:", "partial")
			case "interrupted":
				notice = batchNotice("was interrupted: its host process ended before the job finished.", "partial")
			case "timed out, output quotes a success header":
				notice = batchNotice("timed out after 60s and was stopped. Partial output:", "Async job x (bash) finished.")
			default:
				t.Fatalf("unhandled case %q", tc.name)
			}
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
					Parts: []message.ContentPart{message.TextContent{Text: notice}},
				},
			}
			var b strings.Builder
			writeEvidenceCommandSection(&b, msgs, 1000)
			require.Contains(t, b.String(), "$ go test ./... "+tc.want)
		})
	}
}
