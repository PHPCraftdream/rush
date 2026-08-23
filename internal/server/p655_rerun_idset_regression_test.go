package server

// Task #655 (fourteenth review, P2-1 + M-2): rerun prompt recreation must
// survive row deletions below the prompt and panics between onHandoff and
// createUserMessage.
//
// P2-1: #651 (commit 28f37afc) replaced a row-COUNT check with a POSITION
// scan (`for i := baselineCount; ...`). A position is invalidated by ANY
// deletion below the replacement turn's own prompt row — exactly what
// in-turn compaction does when it deletes summarised rows. On the error
// path the explicit recreateRerunPromptIfLost call (ungated, unlike the
// defer) then scans a window that no longer contains the prompt, finds
// nothing, and appends a spurious second copy of the operator's prompt.
// The old count check handled 1-2 deleted rows; the position check breaks
// at 1+.
//
// M-2: #651 gated the recreate defer on releaseOnBailout, which onHandoff
// clears. onHandoff fires BEFORE the turn's createUserMessage runs, so a
// panic in that narrow window unwinds with the defer disarmed and the
// prompt lost.

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"charm.land/fantasy"
	"github.com/charmbracelet/crush/internal/agent"
	appPkg "github.com/charmbracelet/crush/internal/app"
	"github.com/charmbracelet/crush/internal/message"
	"github.com/stretchr/testify/require"
)

// Compile-time guarantee that the fakes satisfy agent.Coordinator.
var _ agent.Coordinator = (*compactingErrorCoordinator)(nil)
var _ agent.Coordinator = (*postHandoffPanicCoordinator)(nil)

// compactingErrorCoordinator simulates a replacement turn that fires the
// real onHandoff, creates the prompt + reply rows (mimicking
// createUserMessage), performs in-turn compaction (creates a summary row,
// then deletes two OLDER rows below the prompt, mimicking
// runSummarizeSilent's commit loop), and then returns an error — the
// "auto-compaction mid-tool-use continuing to a second turn that then
// fails" shape from the fourteenth review's P2-1.
type compactingErrorCoordinator struct {
	cancellableHoldCoordinator
	a *appPkg.App
}

func (m *compactingErrorCoordinator) RunWithReservedOwnership(ctx context.Context, sessionID, prompt string, epoch uint64, cancel context.CancelFunc, onHandoff func(), smart, fast *agent.ModelOverride, attachments ...message.Attachment) (*fantasy.AgentResult, error) {
	// Fire the handoff exactly as agent_run.go does, BEFORE any row is
	// created (clears the handler's releaseOnBailout gate).
	if onHandoff != nil {
		onHandoff()
	}
	// The real turn installs its ownership-release defer right at the
	// handoff point; mirror that so the reservation is freed on error.
	defer m.ReleaseExclusive(sessionID, epoch, cancel)

	// createUserMessage: the turn's own prompt row.
	if _, err := m.a.Messages.Create(ctx, sessionID, message.CreateMessageParams{
		Role:  message.User,
		Parts: []message.ContentPart{message.TextContent{Text: prompt}},
	}); err != nil {
		return nil, err
	}

	// Partial assistant reply before the failure.
	reply, err := m.a.Messages.Create(ctx, sessionID, message.CreateMessageParams{
		Role:  message.Assistant,
		Parts: []message.ContentPart{message.TextContent{Text: "partial reply"}},
	})
	if err != nil {
		return nil, err
	}
	reply.AddFinish(message.FinishReasonEndTurn, "", "")
	if err := m.a.Messages.Update(ctx, reply); err != nil {
		return nil, err
	}

	// In-turn compaction: a summary row replaces the summarised history...
	if _, err := m.a.Messages.Create(ctx, sessionID, message.CreateMessageParams{
		Role:  message.User,
		Parts: []message.ContentPart{message.TextContent{Text: "summary of earlier conversation"}},
	}); err != nil {
		return nil, err
	}

	// ...and the summarised rows (both BELOW the prompt row) are deleted,
	// shifting the prompt below any position-based baseline. d=2.
	allMsgs, err := m.a.Messages.List(ctx, sessionID)
	if err != nil {
		return nil, err
	}
	for _, row := range allMsgs {
		if (row.Role == message.User && row.Content().Text == "q1") ||
			(row.Role == message.Assistant && row.Content().Text == "a1") {
			if delErr := m.a.Messages.Delete(ctx, row.ID); delErr != nil {
				return nil, delErr
			}
		}
	}

	return nil, errors.New("provider stream failed")
}

// TestHandleRerunMessage_ErrorPathAfterCompactionDoesNotDuplicatePrompt is
// the fourteenth review's P2-1 reproduction: a rerun whose replacement turn
// errors AFTER in-turn compaction deleted two rows below the prompt must
// end with exactly ONE user message carrying the prompt text.
//
// History setup (targetIdx=2):
//
//	user0: "q1"
//	assistant0: "a1" (finished)
//	userMsg: "rerun me" (rerun target)
//	tail: "old reply" (finished; deleted by the handler's tail delete)
//
// The fake creates prompt "rerun me", reply "partial reply", summary
// "summary of earlier conversation", deletes "q1" and "a1" (both below the
// prompt), and returns an error. Final rows: [prompt, reply, summary].
// The position-based scan starts at baselineCount=2 and only sees the
// summary row, so the buggy code appends a second "rerun me".
//
// Revert-check: restore the `for i := baselineCount; i < len(allMsgs); i++`
// positional scan window in recreateRerunPromptIfLost and this test fails
// with count=2 instead of 1.
func TestHandleRerunMessage_ErrorPathAfterCompactionDoesNotDuplicatePrompt(t *testing.T) {
	// Cannot use t.Parallel() because newAttachmentsTestApp calls t.Setenv.
	workingDir := t.TempDir()
	dataDir := t.TempDir()
	a := newAttachmentsTestApp(t, workingDir, dataDir)
	sess, err := a.Sessions.Create(t.Context(), "test-655-error-compaction")
	require.NoError(t, err)
	sessionID := sess.ID
	ctx := t.Context()

	user0, err := a.Messages.Create(ctx, sessionID, message.CreateMessageParams{
		Role:  message.User,
		Parts: []message.ContentPart{message.TextContent{Text: "q1"}},
	})
	require.NoError(t, err)
	assistant0, err := a.Messages.Create(ctx, sessionID, message.CreateMessageParams{
		Role:  message.Assistant,
		Parts: []message.ContentPart{message.TextContent{Text: "a1"}},
	})
	require.NoError(t, err)
	assistant0.AddFinish(message.FinishReasonEndTurn, "", "")
	require.NoError(t, a.Messages.Update(ctx, assistant0))

	userMsg, err := a.Messages.Create(ctx, sessionID, message.CreateMessageParams{
		Role:  message.User,
		Parts: []message.ContentPart{message.TextContent{Text: "rerun me"}},
	})
	require.NoError(t, err)

	tailMsg, err := a.Messages.Create(ctx, sessionID, message.CreateMessageParams{
		Role:  message.Assistant,
		Parts: []message.ContentPart{message.TextContent{Text: "old reply"}},
	})
	require.NoError(t, err)
	tailMsg.AddFinish(message.FinishReasonEndTurn, "", "")
	require.NoError(t, a.Messages.Update(ctx, tailMsg))

	initialMsgs, err := a.Messages.List(ctx, sessionID)
	require.NoError(t, err)
	require.Len(t, initialMsgs, 4, "precondition: q1, a1, target, tail")
	targetIdx := -1
	for i, m := range initialMsgs {
		if m.ID == userMsg.ID {
			targetIdx = i
		}
	}
	require.Equal(t, 2, targetIdx, "precondition: target is at index 2")

	mockCoord := &compactingErrorCoordinator{a: a}
	a.AgentCoordinator = mockCoord

	hub := newHub()
	client := newClient(hub, nil)
	client.send = make(chan []byte, 10)
	payload, err := json.Marshal(RerunMessagePayload{MessageID: userMsg.ID})
	require.NoError(t, err)

	done := make(chan struct{})
	go func() {
		defer close(done)
		handleRerunMessage(ctx, a, client, WSMessage{ID: "req-1", Type: CmdRerunMessage, Payload: payload})
	}()

	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("rerun handler never returned — something blocked unboundedly")
	}

	env := decodeReply(t, client)
	require.Equal(t, EventError, env.Type,
		"the fake returns an error, so the handler must reply with an error")
	require.Contains(t, env.Error, "provider stream failed")

	// The decisive assertion: exactly ONE user message with text "rerun me".
	msgs, err := a.Messages.List(ctx, sessionID)
	require.NoError(t, err)
	count := 0
	for _, m := range msgs {
		if m.Role == message.User && m.Content().Text == "rerun me" {
			count++
		}
	}
	require.Equal(t, 1, count,
		"the handler must NOT create a duplicate prompt on the error path after compaction; "+
			"the position-based scan window no longer contains the prompt after rows below it were deleted (count=%d, expected 1) — "+
			"this is fourteenth-review P2-1", count)

	// The compaction shape actually happened: q1/a1 gone, summary present.
	_, getErr := a.Messages.Get(ctx, user0.ID)
	require.Error(t, getErr, "compaction must have deleted q1")
	_, getErr = a.Messages.Get(ctx, tailMsg.ID)
	require.Error(t, getErr, "tail should be deleted by the handler's step 2")
	hasSummary := false
	for _, m := range msgs {
		if m.Content().Text == "summary of earlier conversation" {
			hasSummary = true
		}
	}
	require.True(t, hasSummary, "the compaction summary row should exist")
}

// postHandoffPanicCoordinator fires the real onHandoff and then panics
// BEFORE creating any row — the narrow window between onHandoff (which
// clears releaseOnBailout) and createUserMessage (fourteenth-review M-2).
type postHandoffPanicCoordinator struct {
	cancellableHoldCoordinator
}

func (m *postHandoffPanicCoordinator) RunWithReservedOwnership(ctx context.Context, sessionID, prompt string, epoch uint64, cancel context.CancelFunc, onHandoff func(), smart, fast *agent.ModelOverride, attachments ...message.Attachment) (*fantasy.AgentResult, error) {
	if onHandoff != nil {
		onHandoff()
	}
	// runOwned's ownership defer takes over at the handoff point; mirror it
	// so the reservation is still released on the panic unwind.
	defer m.ReleaseExclusive(sessionID, epoch, cancel)
	panic("boom - panic between handoff and createUserMessage")
}

// TestHandleRerunMessage_PanicBetweenHandoffAndCreateUserMessagePreservesPrompt
// is the fourteenth review's M-2: a panic in the window where onHandoff has
// already fired (disarming the releaseOnBailout-gated recreate defer) but
// createUserMessage has not yet run must NOT lose the operator's prompt.
// Before #651 the defer's count check caught this; #651's gate let it slip.
//
// Revert-check: gate the recreate defer on releaseOnBailout alone (drop the
// runReturned term) and this test fails with zero matching user messages.
func TestHandleRerunMessage_PanicBetweenHandoffAndCreateUserMessagePreservesPrompt(t *testing.T) {
	// Cannot use t.Parallel() because newAttachmentsTestApp calls t.Setenv.
	workingDir := t.TempDir()
	dataDir := t.TempDir()
	a := newAttachmentsTestApp(t, workingDir, dataDir)
	sess, err := a.Sessions.Create(t.Context(), "test-655-posthandoff-panic")
	require.NoError(t, err)
	sessionID := sess.ID
	ctx := t.Context()

	promptText := "rerun me"
	userMsg, err := a.Messages.Create(ctx, sessionID, message.CreateMessageParams{
		Role:  message.User,
		Parts: []message.ContentPart{message.TextContent{Text: promptText}},
	})
	require.NoError(t, err)

	tailMsg, err := a.Messages.Create(ctx, sessionID, message.CreateMessageParams{
		Role:  message.Assistant,
		Parts: []message.ContentPart{message.TextContent{Text: "old reply"}},
	})
	require.NoError(t, err)
	tailMsg.AddFinish(message.FinishReasonEndTurn, "", "")
	require.NoError(t, a.Messages.Update(ctx, tailMsg))

	mockCoord := &postHandoffPanicCoordinator{}
	a.AgentCoordinator = mockCoord

	hub := newHub()
	client := newClient(hub, nil)
	client.send = make(chan []byte, 10)
	payload, err := json.Marshal(RerunMessagePayload{MessageID: userMsg.ID})
	require.NoError(t, err)

	done := make(chan struct{})
	go func() {
		defer close(done)
		defer func() {
			if r := recover(); r != nil {
				// Expected panic — mirrors hub.runRecovered.
			}
		}()
		handleRerunMessage(ctx, a, client, WSMessage{ID: "req-1", Type: CmdRerunMessage, Payload: payload})
	}()

	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("handler never returned after panic")
	}

	// The decisive assertion: the operator's prompt must have been
	// recreated during the panic unwind — exactly one matching row.
	msgs, err := a.Messages.List(ctx, sessionID)
	require.NoError(t, err)
	count := 0
	for _, m := range msgs {
		if m.Role == message.User && m.Content().Text == promptText {
			count++
		}
	}
	require.Equal(t, 1, count,
		"the prompt must survive a panic between onHandoff and createUserMessage "+
			"(the recreate defer must stay armed until the run RETURNS, not just until the handoff); "+
			"got %d matching user messages, expected 1 — this is fourteenth-review M-2", count)

	// The fake's release ran; the session must not be stuck busy.
	require.False(t, a.AgentCoordinator.IsSessionBusy(sessionID),
		"the fake releases ownership at panic unwind; the session must not stay busy")
}

// ---- Direct unit tests for the baseline-ID semantics ----
// The handler-level tests above drive the real handleRerunMessage; the four
// below pin recreateRerunPromptIfLost's own decision table, including the
// "second source" from the fourteenth review (step 3's target delete fails,
// the original row survives) that has no failure-injection seam at the
// handler level.

// TestRecreateRerunPromptIfLost_SurvivingTargetDoesNotRecreate: if step 3's
// target delete failed, the operator's row survives with its original ID —
// necessarily IN the baseline set (the capture List sees it). The helper
// must treat it as "already present" and not add a second copy.
//
// Revert-check: drop the `|| m.ID == targetID` disjunct from the qualifying
// condition and this test fails with count=2.
func TestRecreateRerunPromptIfLost_SurvivingTargetDoesNotRecreate(t *testing.T) {
	// Cannot use t.Parallel() because newAttachmentsTestApp calls t.Setenv.
	workingDir := t.TempDir()
	dataDir := t.TempDir()
	a := newAttachmentsTestApp(t, workingDir, dataDir)
	sess, err := a.Sessions.Create(t.Context(), "test-655-surviving-target")
	require.NoError(t, err)
	sessionID := sess.ID
	ctx := t.Context()

	target, err := a.Messages.Create(ctx, sessionID, message.CreateMessageParams{
		Role:  message.User,
		Parts: []message.ContentPart{message.TextContent{Text: "rerun me"}},
	})
	require.NoError(t, err)

	// Baseline captured with the survivor present, exactly as the handler
	// would capture it after a failed target delete.
	baseline := map[string]struct{}{target.ID: {}}

	recreateRerunPromptIfLost(ctx, a, sessionID, baseline, target.ID, "rerun me")

	msgs, err := a.Messages.List(ctx, sessionID)
	require.NoError(t, err)
	count := 0
	for _, m := range msgs {
		if m.Role == message.User && m.Content().Text == "rerun me" {
			count++
		}
	}
	require.Equal(t, 1, count,
		"a surviving target row (failed step-3 delete) is already present; the helper must not duplicate it")
}

// TestRecreateRerunPromptIfLost_EarlierIdenticalPromptDoesNotSuppress: an
// earlier identical prompt that survived the tail delete (#644's shape) is
// in the baseline set and is not the target — it must not suppress the
// recreate. The target ID passed here is a row that no longer exists (it
// was deleted at step 3), so no row can match it.
//
// Revert-check: make the scan suppress on ANY text-matching row regardless
// of ID membership and this test fails with count=1.
func TestRecreateRerunPromptIfLost_EarlierIdenticalPromptDoesNotSuppress(t *testing.T) {
	// Cannot use t.Parallel() because newAttachmentsTestApp calls t.Setenv.
	workingDir := t.TempDir()
	dataDir := t.TempDir()
	a := newAttachmentsTestApp(t, workingDir, dataDir)
	sess, err := a.Sessions.Create(t.Context(), "test-655-earlier-identical")
	require.NoError(t, err)
	sessionID := sess.ID
	ctx := t.Context()

	earlier, err := a.Messages.Create(ctx, sessionID, message.CreateMessageParams{
		Role:  message.User,
		Parts: []message.ContentPart{message.TextContent{Text: "continue"}},
	})
	require.NoError(t, err)

	baseline := map[string]struct{}{earlier.ID: {}}

	recreateRerunPromptIfLost(ctx, a, sessionID, baseline, "deleted-target-id", "continue")

	msgs, err := a.Messages.List(ctx, sessionID)
	require.NoError(t, err)
	count := 0
	for _, m := range msgs {
		if m.Role == message.User && m.Content().Text == "continue" {
			count++
		}
	}
	require.Equal(t, 2, count,
		"an earlier identical prompt in the baseline set must not suppress the recreate")
}

// TestRecreateRerunPromptIfLost_NewPromptRowSuppressesDespiteEarlierIdentical:
// when the replacement turn DID create the prompt (its ID is not in the
// baseline), the helper must suppress the recreate even though an earlier
// identical row also exists.
func TestRecreateRerunPromptIfLost_NewPromptRowSuppressesDespiteEarlierIdentical(t *testing.T) {
	// Cannot use t.Parallel() because newAttachmentsTestApp calls t.Setenv.
	workingDir := t.TempDir()
	dataDir := t.TempDir()
	a := newAttachmentsTestApp(t, workingDir, dataDir)
	sess, err := a.Sessions.Create(t.Context(), "test-655-new-row-suppresses")
	require.NoError(t, err)
	sessionID := sess.ID
	ctx := t.Context()

	earlier, err := a.Messages.Create(ctx, sessionID, message.CreateMessageParams{
		Role:  message.User,
		Parts: []message.ContentPart{message.TextContent{Text: "continue"}},
	})
	require.NoError(t, err)
	fresh, err := a.Messages.Create(ctx, sessionID, message.CreateMessageParams{
		Role:  message.User,
		Parts: []message.ContentPart{message.TextContent{Text: "continue"}},
	})
	require.NoError(t, err)
	require.NotEqual(t, earlier.ID, fresh.ID)

	baseline := map[string]struct{}{earlier.ID: {}}

	recreateRerunPromptIfLost(ctx, a, sessionID, baseline, "deleted-target-id", "continue")

	msgs, err := a.Messages.List(ctx, sessionID)
	require.NoError(t, err)
	require.Len(t, msgs, 2,
		"the fresh (non-baseline) prompt row proves the turn recreated it; no third copy may appear")
}

// TestRecreateRerunPromptIfLost_NilBaselineCreatesUnconditionally: a nil
// baseline set means the watermark is unknown (the baseline List failed);
// the helper errs toward recreating even though a matching row exists —
// a visible duplicate beats silently losing the operator's words.
func TestRecreateRerunPromptIfLost_NilBaselineCreatesUnconditionally(t *testing.T) {
	// Cannot use t.Parallel() because newAttachmentsTestApp calls t.Setenv.
	workingDir := t.TempDir()
	dataDir := t.TempDir()
	a := newAttachmentsTestApp(t, workingDir, dataDir)
	sess, err := a.Sessions.Create(t.Context(), "test-655-nil-baseline")
	require.NoError(t, err)
	sessionID := sess.ID
	ctx := t.Context()

	target, err := a.Messages.Create(ctx, sessionID, message.CreateMessageParams{
		Role:  message.User,
		Parts: []message.ContentPart{message.TextContent{Text: "rerun me"}},
	})
	require.NoError(t, err)

	recreateRerunPromptIfLost(ctx, a, sessionID, nil, target.ID, "rerun me")

	msgs, err := a.Messages.List(ctx, sessionID)
	require.NoError(t, err)
	count := 0
	for _, m := range msgs {
		if m.Role == message.User && m.Content().Text == "rerun me" {
			count++
		}
	}
	require.Equal(t, 2, count,
		"unknown watermark (nil baseline) must err toward recreating, not suppressing")
}
