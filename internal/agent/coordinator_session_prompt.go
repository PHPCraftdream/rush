// The per-session system prompt, reconciled with the mode of the call that is
// about to run it (A24, docs/plans/2026-10-01-in-turn-progress-guard.md §4).
//
// A session's system prompt is persisted once and then reused by every later
// turn, while a call's tool set is rebuilt from the live config every turn. In
// the wguard turn (774 steps) the prompt had been saved before the operator
// configured a worker, so it had no rule 7 "Orchestrator mode" — and the turn
// that inherited it had no edit tool to obey, only a saved instruction to use
// one. The two generations disagreed for a whole turn and the model crawled
// files instead of delegating.
//
// A stored prompt records its own mode by carrying (or not carrying) the rule
// 7 marker, and a call states its own mode through resolvedOverrides
// .orchestrator — the same bool that went into prompt.Build for the tool set
// it pinned. One mismatch costs one rebuild-and-re-save per mode change, and
// after that the per-session prompt is a working cache again. The priority
// order of resolveTurnConfig is untouched: this decides what BECOMES the
// per-session prompt, not what wins over a pinned one.

package agent

import (
	"context"
	"log/slog"
	"strings"

	"github.com/PHPCraftdream/rush/internal/agent/prompt"
)

// sessionPromptForCall returns the system prompt this call's turn must run on:
// the session's stored prompt when it was built in the same mode as the call's
// tool set, and the call's own pinned.systemPrompt persisted over it when the
// two disagree (one cache miss per mode change). A session that has no stored
// prompt yet, or a pinned snapshot that could not build one, falls back to
// resolveSessionSystemPrompt — the resolve-and-persist path, unchanged.
func (c *coordinator) sessionPromptForCall(ctx context.Context, sessionID string, pinned *resolvedOverrides) string {
	if pinned != nil {
		sess, err := c.sessions.Get(ctx, sessionID)
		if err != nil {
			return ""
		}
		if sess.SystemPrompt != "" {
			storedMode := strings.Contains(sess.SystemPrompt, prompt.OrchestratorRuleMarker)
			if storedMode == pinned.orchestrator {
				return sess.SystemPrompt
			}
			if pinned.systemPrompt != "" {
				if err := c.sessions.UpdateSystemPrompt(ctx, sessionID, pinned.systemPrompt); err != nil {
					// The turn still runs on the prompt that matches its tool
					// set; a failed save only costs a rebuild next turn.
					slog.Warn("coordinator: failed to re-save system prompt for a mode change", "sessionID", sessionID, "err", err)
				}
				return pinned.systemPrompt
			}
		}
	}
	return c.resolveSessionSystemPrompt(ctx, sessionID)
}
