package agent

import (
	"context"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/PHPCraftdream/rush/internal/config"
	"github.com/PHPCraftdream/rush/internal/message"
	"github.com/stretchr/testify/require"
)

type silentNoopSummaryTimestampService struct {
	message.Service
	calls             atomic.Int32
	mu                sync.Mutex
	createdSummaryIDs []string
	deletedIDs        []string
}

func (s *silentNoopSummaryTimestampService) SetSummaryCreatedAt(context.Context, string, string, int64) (bool, error) {
	s.calls.Add(1)
	return false, nil
}

func (s *silentNoopSummaryTimestampService) Create(ctx context.Context, sessionID string, params message.CreateMessageParams) (message.Message, error) {
	created, err := s.Service.Create(ctx, sessionID, params)
	if err == nil && params.IsSummaryMessage {
		s.mu.Lock()
		s.createdSummaryIDs = append(s.createdSummaryIDs, created.ID)
		s.mu.Unlock()
	}
	return created, err
}

func (s *silentNoopSummaryTimestampService) Delete(ctx context.Context, id string) error {
	s.mu.Lock()
	s.deletedIDs = append(s.deletedIDs, id)
	s.mu.Unlock()
	return s.Service.Delete(ctx, id)
}

func (s *silentNoopSummaryTimestampService) cleanupTrace() (created, deleted []string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.createdSummaryIDs...), append([]string(nil), s.deletedIDs...)
}

func TestCodexRecoveryZeroRowSummaryTimestampStopsWithoutDeletingHistory(t *testing.T) {
	// Install failure-only bounded diagnostics before setup/assertions. Only
	// request status/size/body hashes and message-ID hashes are reported.
	var h *recoveryFailureHarness
	var stamped *silentNoopSummaryTimestampService
	var priorSummary message.Message
	var before map[string]message.Message
	var pinnedID, tailID string
	const currentPrompt = "Report the scheduled work briefly."
	var runErr error
	t.Cleanup(func() {
		if !t.Failed() || h == nil {
			return
		}
		d := h.trace
		d.mu.Lock()
		stages := append([]string(nil), d.stages...)
		models := append([]string(nil), d.models...)
		statuses := append([]int(nil), d.statuses...)
		bodyBytes := append([]int(nil), d.bodyBytes...)
		bodyHashes := append([]string(nil), d.bodyHashes...)
		d.mu.Unlock()
		diagCtx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		pointerHash, promptTokens, completionTokens, ownCost := "", int64(0), int64(0), float64(0)
		if after, getErr := h.f.env.sessions.Get(diagCtx, h.f.sessID); getErr == nil {
			pointerHash = recoveryFailureHash(after.SummaryMessageID)
			promptTokens, completionTokens, ownCost = after.PromptTokens, after.CompletionTokens, after.OwnCost
		}
		var survivingOld, summaries int
		var currentUser, pinned, tail bool
		if rows, listErr := h.f.env.messages.List(diagCtx, h.f.sessID); listErr == nil {
			for _, row := range rows {
				if _, exists := before[row.ID]; exists {
					survivingOld++
				}
				if row.IsSummaryMessage {
					summaries++
				}
				currentUser = currentUser || row.Role == message.User && row.FullText() == currentPrompt
				pinned = pinned || row.ID == pinnedID && row.Pinned
				tail = tail || row.ID == tailID
			}
		}
		stampCalls := int32(0)
		var createdIDs, deletedIDs []string
		if stamped != nil {
			stampCalls = stamped.calls.Load()
			createdIDs, deletedIDs = stamped.cleanupTrace()
		}
		t.Errorf("redacted timestamp-regression trace: codex=%d worker=%d stages=%v models=%v statuses=%v body_bytes=%v body_hashes=%v stamp_calls=%d timestamp_error=%t prior_pointer=%s after_pointer=%s old_rows=%d surviving_old_rows=%d summary_rows=%d created_summary_ids=%v deleted_message_ids=%v current_user=%t pinned=%t retained_tail=%t prompt_tokens=%d completion_tokens=%d own_cost=%.9f ownership_busy=%t",
			d.codexCount.Load(), d.workerCount.Load(), stages, models, statuses, bodyBytes, bodyHashes, stampCalls,
			runErr != nil && strings.Contains(strings.ToLower(runErr.Error()), "timestamp"), recoveryFailureHash(priorSummary.ID), pointerHash,
			len(before), survivingOld, summaries, hashMessageIDs(createdIDs), hashMessageIDs(deletedIDs), currentUser, pinned, tail,
			promptTokens, completionTokens, ownCost, h.f.sa.IsSessionBusy(h.f.sessID))
	})

	// Isolate the --global-dir path as well as the config/data/XDG locations
	// before constructing the real recovery fixture.
	t.Setenv(config.GlobalDirEnv, "")
	isolateAllGlobalConfigPaths(t)
	h = newRecoveryFailureHarness(t, "success", "safe-http-400")
	ctx := t.Context()

	// Existing non-empty summary pointer plus eligible older rows.
	priorSummary, err := h.f.env.messages.Create(ctx, h.f.sessID, message.CreateMessageParams{
		Role: message.Assistant, Parts: []message.ContentPart{message.TextContent{Text: "prior persisted summary"}},
		Model: "prior-worker", Provider: "fixture", IsSummaryMessage: true, Hidden: true,
	})
	require.NoError(t, err)
	_, err = h.f.env.conn.ExecContext(ctx,
		`UPDATE messages SET created_at = (SELECT MIN(created_at) - 1 FROM messages WHERE session_id = ? AND id <> ?) WHERE id = ?`,
		h.f.sessID, priorSummary.ID, priorSummary.ID)
	require.NoError(t, err)
	require.NoError(t, h.f.env.sessions.SetSummaryAndUsage(ctx, h.f.sessID, priorSummary.ID, 0, 0))

	// Make real worker usage yield a visible, single cost charge.
	workerCfg, ok := h.f.coord.cfg.Config().Providers.Get("failure-worker")
	require.True(t, ok)
	workerCfg.Models[0].CostPer1MIn = 2
	workerCfg.Models[0].CostPer1MOut = 3
	h.f.coord.cfg.Config().Providers.Set("failure-worker", workerCfg)

	beforeRows, err := h.f.env.messages.List(ctx, h.f.sessID)
	require.NoError(t, err)
	before = make(map[string]message.Message, len(beforeRows))
	for _, row := range beforeRows {
		before[row.ID] = row
		if row.Pinned {
			pinnedID = row.ID
		}
		tailID = row.ID
	}
	require.NotEmpty(t, pinnedID)
	require.NotEmpty(t, tailID)

	// All normal operations use the real DB-backed message service; the one
	// override simulates SetSummaryCreatedAt returning (false, nil).
	stamped = &silentNoopSummaryTimestampService{Service: h.f.env.messages}
	h.f.sa.messages = stamped
	h.f.coord.messages = stamped
	runErr = h.execute(t, currentPrompt)
	afterSession, sessionErr := h.f.env.sessions.Get(ctx, h.f.sessID)
	afterRows, listErr := h.f.env.messages.List(ctx, h.f.sessID)
	currentUser, pinnedSurvived, tailSurvived := false, false, false
	newSummaryCount := 0
	oldRowsSurvived := make(map[string]message.Message, len(before))
	for _, row := range afterRows {
		if _, exists := before[row.ID]; exists {
			oldRowsSurvived[row.ID] = row
		}
		if row.IsSummaryMessage && row.ID != priorSummary.ID {
			newSummaryCount++
		}
		currentUser = currentUser || row.Role == message.User && row.FullText() == currentPrompt
		pinnedSurvived = pinnedSurvived || row.ID == pinnedID && row.Pinned
		tailSurvived = tailSurvived || row.ID == tailID
	}

	require.Error(t, runErr)
	require.Contains(t, strings.ToLower(runErr.Error()), "timestamp", "zero affected rows must report an explicit timestamp failure")
	require.EqualValues(t, 1, h.trace.codexCount.Load(), "no Codex continuation despite non-empty old summary pointer")
	require.EqualValues(t, 1, h.trace.workerCount.Load(), "real worker summarizer runs exactly once")
	require.EqualValues(t, 1, stamped.calls.Load(), "verify the zero-row timestamp result")
	require.NoError(t, sessionErr)
	require.NoError(t, listErr)
	require.Equal(t, priorSummary.ID, afterSession.SummaryMessageID, "failed stamp leaves old pointer unchanged")
	require.Zero(t, newSummaryCount, "failed summary orphan is removed")
	require.Len(t, oldRowsSurvived, len(before), "all pre-existing history survives")
	for id, old := range before {
		got, exists := oldRowsSurvived[id]
		require.True(t, exists, "original message %s survives", recoveryFailureHash(id))
		require.Equal(t, old.FullText(), got.FullText(), "original message %s content", recoveryFailureHash(id))
		require.Equal(t, old.Pinned, got.Pinned, "original message %s pin state", recoveryFailureHash(id))
	}
	require.True(t, currentUser, "current user message remains")
	require.True(t, pinnedSurvived, "pinned message remains pinned")
	require.True(t, tailSurvived, "accepted tail remains")
	require.EqualValues(t, 80, afterSession.PromptTokens, "worker prompt usage is accounted")
	require.EqualValues(t, 12, afterSession.CompletionTokens, "worker completion usage is accounted")
	require.InDelta(t, 0.000196, afterSession.OwnCost, 1e-12, "worker usage cost is charged exactly once")
	createdIDs, deletedIDs := stamped.cleanupTrace()
	require.Len(t, createdIDs, 1, "recovery creates exactly one summary")
	require.Equal(t, createdIDs, deletedIDs, "failed stamp deletes only its orphan summary")
	require.False(t, h.f.sa.IsSessionBusy(h.f.sessID), "failed recovery releases session ownership")
}

func hashMessageIDs(ids []string) []string {
	out := make([]string, len(ids))
	for i, id := range ids {
		out[i] = recoveryFailureHash(id)
	}
	return out
}
