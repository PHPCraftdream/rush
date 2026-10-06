package app

import (
	"testing"

	"github.com/PHPCraftdream/rush/internal/agent"
	"github.com/PHPCraftdream/rush/internal/message"
	"github.com/stretchr/testify/require"
)

func TestCommandResultsAsyncRealNoticeShapes(t *testing.T) {
	t.Parallel()

	const since = int64(1000)
	transcript := func(jobID, notice string) []message.Message {
		return []message.Message{
			{
				ID:        "call-async",
				Role:      message.Assistant,
				CreatedAt: 1002,
				Parts: []message.ContentPart{
					message.ToolCall{
						ID:    "call-async",
						Name:  "bash",
						Input: `{"command":"go test ./..."}`,
					},
				},
			},
			{
				ID:        "call-async-result",
				Role:      message.Tool,
				CreatedAt: 1003,
				Parts: []message.ContentPart{
					message.ToolResult{
						ToolCallID: "call-async",
						Name:       "bash",
						Content:    "Async bash job " + jobID + " started.",
						Metadata:   `{"async":true,"job_id":"` + jobID + `","status":"running"}`,
					},
				},
			},
			{
				ID:                  "notice",
				Role:                message.User,
				CreatedAt:           1020,
				BackgroundJobNotice: true,
				Parts: []message.ContentPart{
					message.TextContent{Text: notice},
				},
			},
		}
	}

	tests := []struct {
		name, jobID, notice string
		exit                int
		known               bool
	}{
		{
			name: "header call id wins over the shell id in the body", jobID: "call-async",
			// Real shape: the FormatAsyncCompletion header names the call id, the
			// body is backgroundJobSummary's headline naming the shell id "001".
			// Revert check: trying the Background-job regex first keys the notice
			// under "001", so the lookup by call id misses (exit unknown).
			notice: agent.FormatAsyncCompletion(agent.AsyncCompletion{
				ToolCallID: "call-async", ToolName: "bash", IsError: true,
				Content: "Background job 001 (`go test ./...`) finished: exit 3, ran 2s.\n\nFAIL",
			}),
			exit: 3, known: true,
		},
		{
			name: "finished header content ending Exit code 2", jobID: "call-async",
			// Revert check: preserves finished-header tail parsing.
			notice: agent.FormatAsyncCompletion(agent.AsyncCompletion{ToolCallID: "call-async", ToolName: "bash", Content: "result\nExit code 2"}),
			exit:   2, known: true,
		},
		{
			name: "finished uses final code after middle code", jobID: "call-async",
			// Revert check: middle output code must not override the tail.
			notice: agent.FormatAsyncCompletion(agent.AsyncCompletion{ToolCallID: "call-async", ToolName: "bash", Content: "Exit code 0\nmore output\nExit code 5"}),
			exit:   5, known: true,
		},
		{
			name: "plain finished success without tail", jobID: "call-async",
			// Revert check: finished notices without a tail remain known-zero.
			notice: agent.FormatAsyncCompletion(agent.AsyncCompletion{ToolCallID: "call-async", ToolName: "bash", Content: "ok"}),
			exit:   0, known: true,
		},
		{
			name: "formatted failed with Bash exit tail", jobID: "call-async",
			notice: agent.FormatAsyncCompletion(agent.AsyncCompletion{ToolCallID: "call-async", ToolName: "bash", IsError: true, Content: "failed\nExit code 7"}),
			exit:   7, known: true,
		},
		{
			name: "exit mention before the final tail is not trusted", jobID: "call-async",
			notice: agent.FormatAsyncCompletion(agent.AsyncCompletion{ToolCallID: "call-async", ToolName: "bash", IsError: true, Content: "Exit code 4\ncrashed"}),
			known:  false,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := commandResults(transcript(tc.jobID, tc.notice), since)
			require.Len(t, got, 1)
			require.True(t, got[0].Async)
			require.Equal(t, tc.known, got[0].Known)
			if tc.known {
				require.Equal(t, tc.exit, got[0].Exit)
			}
		})
	}
}
