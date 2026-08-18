package cmd

// The `sessions show` subcommand: detailed single-session inspection in
// text or JSON, with optional message thread and sub-agent transcripts.

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/charmbracelet/crush/internal/message"
	"github.com/charmbracelet/crush/internal/session"
	"github.com/spf13/cobra"
)

var sessionsShowCmd = &cobra.Command{
	Use:   "show <id>",
	Short: "Inspect a single session in detail",
	Long: `Show detailed information about a session including its title, models,
tokens, cost, and optionally all messages.

The default output is human-readable text; use --json for structured format
suitable for parsing. Combine with --with-messages to include the message
thread and system prompt. Use --full with --with-messages to see complete
message content (default truncates to 200 chars per message).`,
	Args: cobra.ExactArgs(1),
	Example: `
# Human-readable inspection
crush sessions show myid-123

# Full session data with all messages
crush sessions show myid-123 --with-messages

# Machine-readable format for scripts
crush sessions show myid-123 --json

# See everything including full message content
crush sessions show myid-123 --with-messages --full --json
  `,
	RunE: sessionsShowCmdRun,
}

func sessionsShowCmdRun(cmd *cobra.Command, args []string) error {
	asJSON, _ := cmd.Flags().GetBool("json")
	withMessages, _ := cmd.Flags().GetBool("with-messages")
	full, _ := cmd.Flags().GetBool("full")
	withSubagents, _ := cmd.Flags().GetBool("with-subagents")
	if full {
		withMessages = true
	}
	// --with-subagents renders child delegation transcripts, which only makes
	// sense alongside the parent's own message thread; imply --with-messages so
	// `show <id> --with-subagents` on its own does the obviously-intended thing.
	if withSubagents {
		withMessages = true
	}

	a, err := setupApp(cmd)
	if err != nil {
		return err
	}
	defer a.Shutdown()

	sess, err := resolveSessionID(cmd.Context(), a.Sessions, args[0])
	if err != nil {
		return err
	}

	type msgItem struct {
		ID           string `json:"id"`
		Role         string `json:"role"`
		Preview      string `json:"preview"`
		FinishReason string `json:"finish_reason,omitempty"`
		// Retried mirrors printMessage's ndjson field: a finish_reason="error"
		// row that was followed by more messages was transiently retried, not
		// a terminal death. Separate boolean so consumers keep the raw enum.
		Retried bool `json:"retried,omitempty"`
	}

	type sessionShowOutput struct {
		ID               string  `json:"id"`
		Hash             string  `json:"hash"`
		Title            string  `json:"title"`
		Purpose          string  `json:"purpose,omitempty"` // first user prompt excerpt
		ParentID         string  `json:"parent_id,omitempty"`
		Provider         string  `json:"provider,omitempty"`
		Model            string  `json:"model,omitempty"`
		Effort           string  `json:"effort,omitempty"`
		CreatedAt        int64   `json:"created_at"`
		UpdatedAt        int64   `json:"updated_at"`
		MessageCount     int64   `json:"message_count"`
		PromptTokens     int64   `json:"prompt_tokens"`
		CompletionTokens int64   `json:"completion_tokens"`
		CostUSD          float64 `json:"cost_usd"`
		EndedReason      string  `json:"ended_reason,omitempty"`
		BudgetMaxCost    float64 `json:"budget_max_cost,omitempty"`
		BudgetMaxTokens  int64   `json:"budget_max_tokens,omitempty"`
		BudgetTimeoutSec int64   `json:"budget_timeout_sec,omitempty"`
		// SubAgentActivity, when non-empty, describes an in-flight sub-agent
		// delegation whose activity is fresher than this session's own —
		// e.g. "assistant activity 3s ago (session abc12345)". Computed from
		// the shared call-tree walk (sessions_activity.go).
		SubAgentActivity string    `json:"sub_agent_activity,omitempty"`
		SystemPrompt     string    `json:"system_prompt,omitempty"`
		Messages         []msgItem `json:"messages,omitempty"`
	}

	// Fetch the first user message as "purpose".
	var purpose string
	messages, msgErr := a.Messages.List(cmd.Context(), sess.ID)
	if msgErr == nil {
		for _, msg := range messages {
			if msg.Role == message.User {
				for _, part := range msg.Parts {
					if tc, ok := part.(message.TextContent); ok && tc.Text != "" {
						purpose = tc.Text
						if len(purpose) > 120 {
							purpose = purpose[:120] + "…"
						}
						break
					}
				}
				break
			}
		}
	}

	output := sessionShowOutput{
		ID:               sess.ID,
		Hash:             session.HashID(sess.ID),
		Title:            sess.Title,
		Purpose:          purpose,
		ParentID:         sess.ParentSessionID,
		Provider:         sess.LargeModelProvider,
		Model:            sess.LargeModelID,
		Effort:           sess.LargeModelReasoningEffort,
		CreatedAt:        sess.CreatedAt,
		UpdatedAt:        sess.UpdatedAt,
		MessageCount:     sess.MessageCount,
		PromptTokens:     sess.PromptTokens,
		CompletionTokens: sess.CompletionTokens,
		CostUSD:          sess.Cost,
		EndedReason:      sess.EndedReason,
		BudgetMaxCost:    sess.BudgetMaxCost,
		BudgetMaxTokens:  sess.BudgetMaxTokens,
		BudgetTimeoutSec: sess.BudgetTimeoutSec,
		SystemPrompt:     sess.SystemPrompt,
	}

	// Sub-agent pulse: surface an in-flight delegation's own last activity
	// so `sessions show` on a session that's blocked waiting on a sub-agent
	// isn't misread as idle. Baseline = the session's own updated_at; the
	// note only appears when a descendant sub-agent session is fresher.
	output.SubAgentActivity = subAgentActivityNote(cmd.Context(), a, sess.ID, sess.UpdatedAt, time.Now())

	if withMessages {
		if msgErr != nil {
			return fmt.Errorf("failed to list messages: %w", msgErr)
		}

		output.Messages = make([]msgItem, len(messages))
		for i, msg := range messages {
			preview := ""
			if full {
				for _, part := range msg.Parts {
					if tc, ok := part.(message.TextContent); ok {
						preview = tc.Text
						break
					}
				}
			} else {
				for _, part := range msg.Parts {
					if tc, ok := part.(message.TextContent); ok {
						preview = truncate(tc.Text, 200)
						break
					}
				}
			}

			finishReason := ""
			retried := false
			if f := msg.FinishPart(); f != nil {
				finishReason = string(f.Reason)
				// A followed-by-later error row is a transient, auto-retried
				// failure — the session went on. messages is the full list in
				// order, so any row but the last has a later message.
				retried = f.Reason == message.FinishReasonError && i < len(messages)-1
			}

			output.Messages[i] = msgItem{
				ID:           msg.ID,
				Role:         string(msg.Role),
				Preview:      preview,
				FinishReason: finishReason,
				Retried:      retried,
			}
		}
	}

	if asJSON {
		enc := json.NewEncoder(os.Stdout)
		return enc.Encode(output)
	}

	fmt.Printf("ID:           %s\n", output.ID)
	fmt.Printf("Hash:         %s\n", short(output.Hash))
	fmt.Printf("Title:        %s\n", output.Title)
	if output.ParentID != "" {
		fmt.Printf("Parent:       %s\n", output.ParentID)
	} else {
		fmt.Printf("Parent:       -\n")
	}
	if output.Provider != "" || output.Model != "" {
		fmt.Printf("Provider:     %s\n", output.Provider+"/"+output.Model)
		if output.Effort != "" {
			fmt.Printf("Effort:       %s\n", output.Effort)
		}
	}
	fmt.Printf("Created:      %s\n", time.Unix(output.CreatedAt, 0).Format(time.RFC3339))
	fmt.Printf("Updated:      %s\n", time.Unix(output.UpdatedAt, 0).Format(time.RFC3339))
	fmt.Printf("Messages:     %d\n", output.MessageCount)
	fmt.Printf("Tokens:       %d prompt, %d completion\n", output.PromptTokens, output.CompletionTokens)
	costLine := fmt.Sprintf("$%.6f USD", output.CostUSD)
	if output.BudgetMaxCost > 0 {
		pct := output.CostUSD / output.BudgetMaxCost * 100
		costLine += fmt.Sprintf(" / $%.2f budget (%.0f%%)", output.BudgetMaxCost, pct)
	}
	fmt.Printf("Cost:         %s\n", costLine)
	if output.BudgetMaxTokens > 0 {
		totalTokens := output.PromptTokens + output.CompletionTokens
		pct := float64(totalTokens) / float64(output.BudgetMaxTokens) * 100
		fmt.Printf("Token budget: %d / %d (%.0f%%)\n", totalTokens, output.BudgetMaxTokens, pct)
	}
	if output.BudgetTimeoutSec > 0 {
		fmt.Printf("Timeout:      %s\n", formatDurationShort(time.Duration(output.BudgetTimeoutSec)*time.Second))
	}
	if output.EndedReason != "" {
		fmt.Printf("Ended:        %s\n", output.EndedReason)
	}
	if output.SubAgentActivity != "" {
		fmt.Printf("Delegating:   %s\n", output.SubAgentActivity)
	}
	if output.Purpose != "" {
		fmt.Printf("Purpose:      %s\n", output.Purpose)
	}
	fmt.Println()
	fmt.Println("System prompt:")
	if output.SystemPrompt == "" {
		fmt.Println("  (none)")
	} else {
		lines := strings.Split(strings.TrimSpace(output.SystemPrompt), "\n")
		if len(lines) > 5 {
			for _, line := range lines[:5] {
				fmt.Printf("  %s\n", line)
			}
			fmt.Printf("  ... (%d more lines; use --with-messages for full)\n", len(lines)-5)
		} else {
			for _, line := range lines {
				fmt.Printf("  %s\n", line)
			}
		}
	}

	if output.Messages != nil {
		fmt.Println()
		fmt.Println("Messages:")
		for i, msg := range output.Messages {
			fmt.Printf("  %d. [%s] %s\n", i+1, msg.Role, truncate(msg.Preview, 60))
			if msg.FinishReason != "" {
				fmt.Printf("     (finished: %s)\n", finishReasonLabel(message.FinishReason(msg.FinishReason), msg.Retried))
			}
		}
	}

	// Opt-in (--with-subagents): after the parent's message summary, render
	// each sub-agent delegation's full transcript as a demarcated, indented
	// block. Default-hidden: without the flag, `show` never prints a child
	// session's message content (only the one-line pulse note above).
	if withSubagents {
		fmt.Println()
		fmt.Println("Sub-agent delegations:")
		printSubAgentTranscripts(cmd.Context(), os.Stdout, a, sess.ID, "text", time.Now())
	}

	return nil
}
