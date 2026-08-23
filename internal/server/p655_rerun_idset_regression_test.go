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
//
// Task #658 (fifteenth review, P3-1): a transient failure of the
// baseline-capture List must not make the error-path recreate create blind;
// the handler-level regression below pins that the helper still scans
// (and thus recognizes the turn's own prompt row) when the baseline falls
// back to the pre-delete listing.
//
// Task #659 (sixteenth review, M-2): the seed's CONTENT — not just its
// non-nilness — is pinned at the handler level by
// TestHandleRerunMessage_SeedContentPinsEarlierIdenticalPromptOnFailedListPath;
// deleting the seed loop at the capture site keeps every other test green,
// and only that test goes red.
//
// Task #660 (seventeenth review, M-1): the post-delete UNION's content —
// the mirror image of #659's seed finding, one loop further down — is
// pinned at the handler level by
// TestHandleRerunMessage_UnionLoopPinsForeignRowInsideWindowOnSuccessPath;
// deleting the union loop's body at the capture site keeps every other
// test green, and only that test goes red.

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
var _ agent.Coordinator = (*promptThenErrorCoordinator)(nil)
var _ agent.Coordinator = (*handoffOnlyErrorCoordinator)(nil)
var _ message.Service = (*listCallFailingMessages)(nil)

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

// TestRecreateRerunPromptIfLost_PreDeleteOnlyBaselineStillScans: when the
// post-delete baseline List failed at the capture site, the set falls back
// to the PRE-DELETE seed (task #658) instead of nil, so the helper must
// still SCAN rather than create unconditionally: the replacement turn's
// own prompt row — an ID absent from the pre-delete seed — must suppress
// the recreate even though the set carries no post-delete contribution.
//
// The target row is deleted up front (a successful step 3) so suppression
// can come only from the fresh row's non-baseline ID — otherwise the
// targetID disjunct alone would make this pass vacuously. The seed also
// carries a deleted tail row's ID, mirroring what the capture site's seed
// contains and proving an ID of a row that no longer exists changes
// nothing.
//
// Revert-check: make the helper skip the scan and create unconditionally
// (what the pre-#658 nil-baseline branch did) and this test fails with
// count=2. Restoring the nil fallback alone does NOT redden this test —
// it passes a non-nil seed, which even the old helper scanned correctly;
// the capture-site half of #658 is pinned at the handler level by
// TestHandleRerunMessage_TransientBaselineListFailureDoesNotDuplicatePrompt.
func TestRecreateRerunPromptIfLost_PreDeleteOnlyBaselineStillScans(t *testing.T) {
	// Cannot use t.Parallel() because newAttachmentsTestApp calls t.Setenv.
	workingDir := t.TempDir()
	dataDir := t.TempDir()
	a := newAttachmentsTestApp(t, workingDir, dataDir)
	sess, err := a.Sessions.Create(t.Context(), "test-655-predelete-only-baseline")
	require.NoError(t, err)
	sessionID := sess.ID
	ctx := t.Context()

	earlier, err := a.Messages.Create(ctx, sessionID, message.CreateMessageParams{
		Role:  message.User,
		Parts: []message.ContentPart{message.TextContent{Text: "q1"}},
	})
	require.NoError(t, err)
	target, err := a.Messages.Create(ctx, sessionID, message.CreateMessageParams{
		Role:  message.User,
		Parts: []message.ContentPart{message.TextContent{Text: "rerun me"}},
	})
	require.NoError(t, err)
	tail, err := a.Messages.Create(ctx, sessionID, message.CreateMessageParams{
		Role:  message.Assistant,
		Parts: []message.ContentPart{message.TextContent{Text: "old reply"}},
	})
	require.NoError(t, err)
	tail.AddFinish(message.FinishReasonEndTurn, "", "")
	require.NoError(t, a.Messages.Update(ctx, tail))

	// The handler's step 2 (tail delete) and step 3 (target delete) both
	// succeeded — only the baseline-capture List failed.
	require.NoError(t, a.Messages.Delete(ctx, tail.ID))
	require.NoError(t, a.Messages.Delete(ctx, target.ID))

	// The replacement turn's own createUserMessage row, written after
	// every listing.
	fresh, err := a.Messages.Create(ctx, sessionID, message.CreateMessageParams{
		Role:  message.User,
		Parts: []message.ContentPart{message.TextContent{Text: "rerun me"}},
	})
	require.NoError(t, err)
	require.NotEqual(t, target.ID, fresh.ID)

	// The pre-delete-only seed, exactly what the redesigned capture site
	// builds when the post-delete List fails: every pre-delete row's ID,
	// including IDs of rows the deletes have since removed.
	baseline := map[string]struct{}{
		earlier.ID: {},
		target.ID:  {},
		tail.ID:    {},
	}

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
		"a pre-delete-only baseline must still suppress the recreate when the turn's own prompt row (an ID not in the seed) exists")
	require.Len(t, msgs, 2, "earlier row + the turn's prompt row; no duplicate may be appended")
}

// listCallFailingMessages wraps message.Service and fails exactly ONE List
// call — the n-th (1-based) — returning a transient error for it while
// passing every other call through untouched. Used to reproduce the
// fifteenth review's P3-1: a baseline-capture List failure (call #2) that
// clears before the helper's own List (call #3) runs.
type listCallFailingMessages struct {
	message.Service
	failOnNthList int // 1-based ordinal of the List call to fail
	listCalls     int // number of List calls seen so far
	failedAt      int // ordinal that actually failed (0 = none yet)
}

func (m *listCallFailingMessages) List(ctx context.Context, sessionID string) ([]message.Message, error) {
	m.listCalls++
	if m.listCalls == m.failOnNthList {
		m.failedAt = m.listCalls
		return nil, errors.New("transient DB error (probe)")
	}
	return m.Service.List(ctx, sessionID)
}

// promptThenErrorCoordinator fires the real onHandoff, creates the prompt
// + reply rows (mimicking createUserMessage), and then returns an error —
// the "replacement turn creates its own prompt row then errors" shape from
// the fifteenth review's P3-1. Unlike compactingErrorCoordinator, it does
// NOT perform in-turn compaction; the failure shape is purely "provider
// error on first turn".
type promptThenErrorCoordinator struct {
	cancellableHoldCoordinator
	a *appPkg.App
}

func (m *promptThenErrorCoordinator) RunWithReservedOwnership(ctx context.Context, sessionID, prompt string, epoch uint64, cancel context.CancelFunc, onHandoff func(), smart, fast *agent.ModelOverride, attachments ...message.Attachment) (*fantasy.AgentResult, error) {
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
		Parts: []message.ContentPart{message.TextContent{Text: "first turn reply"}},
	})
	if err != nil {
		return nil, err
	}
	reply.AddFinish(message.FinishReasonEndTurn, "", "")
	if err := m.a.Messages.Update(ctx, reply); err != nil {
		return nil, err
	}

	return nil, errors.New("second-turn provider error")
}

// handoffOnlyErrorCoordinator fires the real onHandoff and then returns an
// error WITHOUT creating any row — the "replacement turn dies before its
// createUserMessage" shape. Where promptThenErrorCoordinator proves the
// turn's own prompt row suppresses the recreate, this one leaves nothing
// new in the DB, so the only same-text row the helper's scan can find is
// one that predates the rerun entirely.
type handoffOnlyErrorCoordinator struct {
	cancellableHoldCoordinator
}

func (m *handoffOnlyErrorCoordinator) RunWithReservedOwnership(ctx context.Context, sessionID, prompt string, epoch uint64, cancel context.CancelFunc, onHandoff func(), smart, fast *agent.ModelOverride, attachments ...message.Attachment) (*fantasy.AgentResult, error) {
	// Fire the handoff exactly as agent_run.go does, BEFORE anything else
	// (clears the handler's releaseOnBailout gate).
	if onHandoff != nil {
		onHandoff()
	}
	// The real turn installs its ownership-release defer right at the
	// handoff point; mirror that so the reservation is freed on error.
	defer m.ReleaseExclusive(sessionID, epoch, cancel)
	return nil, errors.New("provider error before any prompt row")
}

// TestHandleRerunMessage_TransientBaselineListFailureDoesNotDuplicatePrompt is
// the fifteenth review's P3-1 reproduction: the baseline-capture List
// (handlers_agent.go:849) fails transiently, the DB recovers, the replacement
// run creates its own prompt row and then errors; the error-path recreate must
// NOT append a second copy.
//
// History setup (targetIdx=0):
//
//	userMsg: "rerun me" (rerun target)
//	tailMsg: "old reply" (finished; deleted by the handler's tail delete)
//
// The decorator fails List call #2 of exactly 3 (call #1 = step 2's tail list
// at :704, call #2 = baseline capture at :849, call #3 = the helper's own List
// at :1010 which MUST succeed — that is the "transient" premise). The fake
// creates prompt "rerun me", reply "first turn reply", and returns an error.
// Final rows: [prompt, reply]. The nil baseline (capture List failed) makes the
// buggy code skip the scan and call Create unconditionally, appending a second
// "rerun me".
//
// Revert-check: restore the nil-baseline fallback (skip the scan +
// unconditional Create when the capture List fails) and this test fails with
// count=2.
func TestHandleRerunMessage_TransientBaselineListFailureDoesNotDuplicatePrompt(t *testing.T) {
	// Cannot use t.Parallel() because newAttachmentsTestApp calls t.Setenv.
	workingDir := t.TempDir()
	dataDir := t.TempDir()
	a := newAttachmentsTestApp(t, workingDir, dataDir)
	sess, err := a.Sessions.Create(t.Context(), "test-655-transient-baseline-list")
	require.NoError(t, err)
	sessionID := sess.ID
	ctx := t.Context()

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
	require.Len(t, initialMsgs, 2, "precondition: target, tail")
	targetIdx := -1
	for i, m := range initialMsgs {
		if m.ID == userMsg.ID {
			targetIdx = i
		}
	}
	require.Equal(t, 0, targetIdx, "precondition: target is at index 0")

	mockCoord := &promptThenErrorCoordinator{a: a}
	a.AgentCoordinator = mockCoord

	// Install the decorator AFTER all setup rows are created and BEFORE
	// launching the handler.
	msgSvc := &listCallFailingMessages{Service: a.Messages, failOnNthList: 2}
	a.Messages = msgSvc

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
	require.Contains(t, env.Error, "second-turn provider error")

	// Assert the transient premise held: exactly three List calls, the
	// failure hit the capture call (#2), and the helper's own List (#3)
	// succeeded.
	require.Equal(t, 3, msgSvc.listCalls,
		"exactly three List calls must have occurred: step 2 tail list, baseline capture, helper's own scan")
	require.Equal(t, 2, msgSvc.failedAt,
		"the failure must have hit the baseline-capture List (call #2), not any other")

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
		"the handler must NOT create a duplicate prompt when the baseline-capture List fails transiently; "+
			"the helper must scan against the pre-delete baseline seed and recognize the turn's own prompt row; "+
			"got %d matching user messages, expected 1 — this is fifteenth-review P3-1 (a spurious duplicate was appended)", count)

	// The tail message must have been deleted by the handler's step 2.
	_, getErr := a.Messages.Get(ctx, tailMsg.ID)
	require.Error(t, getErr, "tail should be deleted by the handler's step 2")

	// An assistant row with text "first turn reply" must exist.
	hasReply := false
	for _, m := range msgs {
		if m.Content().Text == "first turn reply" {
			hasReply = true
		}
	}
	require.True(t, hasReply, "the replacement turn's assistant reply should exist")
}

// TestHandleRerunMessage_SeedContentPinsEarlierIdenticalPromptOnFailedListPath
// is the sixteenth review's M-2: nothing else in the suite pins the CONTENT
// of the pre-delete seed. Delete only the three-line seed loop at the
// capture site (baselineIDs stays non-nil but empty; the post-delete union
// loop is intact) and every other test stays green — including
// TestHandleRerunMessage_TransientBaselineListFailureDoesNotDuplicatePrompt,
// whose suppressing row is the replacement turn's OWN prompt, outside the
// seed regardless of whether the seed loop ran. This test is the one that
// goes red.
//
// #644's exact shape: an EARLIER identical prompt above the rerun target.
// History setup (targetIdx=1):
//
//	earlier: "rerun me" (User — the earlier identical prompt; above the
//	         target, so the tail delete spares it)
//	userMsg: "rerun me" (User, rerun target — deleted at step 3)
//	tailMsg: "old reply" (Assistant, finished — deleted by the tail delete)
//
// The decorator fails List call #2 of exactly 3 (call #1 = step 2's tail
// list, call #2 = baseline capture, call #3 = the helper's own scan, which
// must succeed — the "transient" premise). The fake coordinator fires
// onHandoff and errors WITHOUT creating any row, so when the helper scans,
// the only "rerun me" row left is the EARLIER one.
//
// Against the real code the seed contains earlier's ID: the scan reads it
// as pre-existing (in the baseline, not the target) and the recreate fires
// — count=2. With the seed loop deleted the set is empty, the earlier
// row's ID is "not in the baseline", it is misread as the already
// recreated prompt, and the recreate is suppressed — count=1: #644's
// prompt loss silently reintroduced on a green suite.
//
// Revert-check: delete the seed loop at the capture site in
// handleRerunMessage and this test fails with count=1.
func TestHandleRerunMessage_SeedContentPinsEarlierIdenticalPromptOnFailedListPath(t *testing.T) {
	// Cannot use t.Parallel() because newAttachmentsTestApp calls t.Setenv.
	workingDir := t.TempDir()
	dataDir := t.TempDir()
	a := newAttachmentsTestApp(t, workingDir, dataDir)
	sess, err := a.Sessions.Create(t.Context(), "test-655-seed-content-pins")
	require.NoError(t, err)
	sessionID := sess.ID
	ctx := t.Context()

	promptText := "rerun me"
	earlier, err := a.Messages.Create(ctx, sessionID, message.CreateMessageParams{
		Role:  message.User,
		Parts: []message.ContentPart{message.TextContent{Text: promptText}},
	})
	require.NoError(t, err)
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

	initialMsgs, err := a.Messages.List(ctx, sessionID)
	require.NoError(t, err)
	require.Len(t, initialMsgs, 3, "precondition: earlier, target, tail")
	targetIdx := -1
	for i, m := range initialMsgs {
		if m.ID == userMsg.ID {
			targetIdx = i
		}
	}
	require.Equal(t, 1, targetIdx, "precondition: target is at index 1, below the earlier identical prompt")
	require.NotEqual(t, earlier.ID, userMsg.ID, "precondition: earlier and target are distinct rows")

	mockCoord := &handoffOnlyErrorCoordinator{}
	a.AgentCoordinator = mockCoord

	// Install the decorator AFTER all setup rows are created and BEFORE
	// launching the handler.
	msgSvc := &listCallFailingMessages{Service: a.Messages, failOnNthList: 2}
	a.Messages = msgSvc

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
	require.Contains(t, env.Error, "provider error before any prompt row")

	// Assert the transient premise held: exactly three List calls, the
	// failure hit the capture call (#2), and the helper's own List (#3)
	// succeeded.
	require.Equal(t, 3, msgSvc.listCalls,
		"exactly three List calls must have occurred: step 2 tail list, baseline capture, helper's own scan")
	require.Equal(t, 2, msgSvc.failedAt,
		"the failure must have hit the baseline-capture List (call #2), not any other")

	// The handler's deletes did their job: tail and target gone, the
	// earlier identical prompt (above the target) survives.
	_, getErr := a.Messages.Get(ctx, tailMsg.ID)
	require.Error(t, getErr, "tail should be deleted by the handler's step 2")
	_, getErr = a.Messages.Get(ctx, userMsg.ID)
	require.Error(t, getErr, "target should be deleted by the handler's step 3")
	_, getErr = a.Messages.Get(ctx, earlier.ID)
	require.NoError(t, getErr, "the earlier identical prompt sits above the target and must survive")

	// The decisive assertion: the recreate must FIRE — count=2 (the
	// earlier row plus the recreated prompt). An empty seed (the
	// mutation) misreads `earlier` as the already-recreated row,
	// suppresses the recreate, and silently loses the operator's rerun
	// prompt: count=1.
	msgs, err := a.Messages.List(ctx, sessionID)
	require.NoError(t, err)
	count := 0
	for _, m := range msgs {
		if m.Role == message.User && m.Content().Text == promptText {
			count++
		}
	}
	require.Equal(t, 2, count,
		"the pre-delete seed must identify the earlier identical prompt as PRE-EXISTING so the recreate fires; "+
			"got %d matching user messages, expected 2 — with an empty seed the earlier row is misread as the "+
			"already-recreated prompt and the operator's rerun prompt is silently lost (sixteenth-review M-2, #644's shape)", count)
}

// TestHandleRerunMessage_UnionLoopPinsForeignRowInsideWindowOnSuccessPath is
// the seventeenth review's M-1: nothing else in the suite pins the CONTENT
// of the post-delete UNION. Delete only the union loop's body at the capture
// site (keep the post-delete List call and its else warning branch) and
// every other test stays green — including #659's failed-List-path seed
// test, where the union never executes, and
// TestHandleRerunMessage_ConcurrentUnrelatedWriterDoesNotSuppressRecreate,
// whose concurrent row's text deliberately does not match. This test is the
// one that goes red.
//
// The success-path mirror of #659's scenario: a FOREIGN User row with the
// SAME text as the rerun prompt lands INSIDE the tail-delete window
// (created at rerunTailDeleteSeam(0), i.e. after the pre-delete listing and
// before the post-delete List) — exactly a concurrent handleInjectMessage
// or `crush sessions inject` landing mid-delete. The post-delete List
// SUCCEEDS (no decorator). The fake coordinator is handoffOnlyErrorCoordinator:
// it fires onHandoff and errors without writing any row.
//
// History setup (targetIdx=0):
//
//	userMsg: "rerun me" (User, rerun target — deleted at step 3)
//	tailMsg: "old reply" (Assistant, finished — deleted by the tail delete)
//	foreign: "rerun me" (User — created by the seam at i==0, mid-delete)
//
// Against the real code the union puts foreign's ID in the baseline, so the
// helper's scan reads it as pre-existing (in the baseline, not the target)
// and the recreate fires — count=2 (foreign + the recreated prompt). With
// the union loop's body deleted the foreign row's ID is not in the set, its
// text matches, and the helper's !inBaseline disjunct misreads it as the
// already-recreated prompt — the recreate is suppressed and the operator's
// prompt is silently replaced by the foreign row: count=1, #644's loss
// class on the success path this time.
//
// Revert-check: delete the union loop's body at the capture site in
// handleRerunMessage and this test fails with count=1.
func TestHandleRerunMessage_UnionLoopPinsForeignRowInsideWindowOnSuccessPath(t *testing.T) {
	// Cannot use t.Parallel() because newAttachmentsTestApp calls t.Setenv.
	workingDir := t.TempDir()
	dataDir := t.TempDir()
	a := newAttachmentsTestApp(t, workingDir, dataDir)
	sess, err := a.Sessions.Create(t.Context(), "test-660-union-pins-foreign-row")
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

	initialMsgs, err := a.Messages.List(ctx, sessionID)
	require.NoError(t, err)
	require.Len(t, initialMsgs, 2, "precondition: target, tail")
	targetIdx := -1
	for i, m := range initialMsgs {
		if m.ID == userMsg.ID {
			targetIdx = i
		}
	}
	require.Equal(t, 0, targetIdx, "precondition: target is at index 0")

	mockCoord := &handoffOnlyErrorCoordinator{}
	a.AgentCoordinator = mockCoord

	// The concurrent writer: a foreign same-text User row landing inside
	// the tail-delete window — after the pre-delete listing (call #1),
	// before the post-delete List (call #2). The seam fires at the top of
	// tail iteration 0, strictly after allMsgs was captured and strictly
	// before the tail's Delete runs. The row is therefore NOT in the tail
	// slice the loop deletes (sliced from allMsgs) and not the target: it
	// survives both deletes, exactly like a real handleInjectMessage /
	// `crush sessions inject` row.
	foreignCreated := make(chan message.Message, 1)
	rerunTailDeleteSeam = func(i int) {
		if i != 0 {
			return
		}
		foreign, createErr := a.Messages.Create(ctx, sessionID, message.CreateMessageParams{
			Role:  message.User,
			Parts: []message.ContentPart{message.TextContent{Text: promptText}},
		})
		if createErr != nil {
			t.Errorf("failed to create foreign row inside the window: %v", createErr)
			return
		}
		foreignCreated <- foreign
	}
	t.Cleanup(func() { rerunTailDeleteSeam = nil })

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
	require.Contains(t, env.Error, "provider error before any prompt row")

	// The seam fired and the foreign row landed inside the window.
	var foreign message.Message
	select {
	case foreign = <-foreignCreated:
	default:
		t.Fatal("rerunTailDeleteSeam never fired at i==0; the test did not exercise the mid-delete window")
	}
	require.NotEqual(t, userMsg.ID, foreign.ID, "precondition: the foreign row is distinct from the rerun target")

	// The handler's deletes did their job; the foreign row survived them.
	_, getErr := a.Messages.Get(ctx, tailMsg.ID)
	require.Error(t, getErr, "tail should be deleted by the handler's step 2")
	_, getErr = a.Messages.Get(ctx, userMsg.ID)
	require.Error(t, getErr, "target should be deleted by the handler's step 3")
	_, getErr = a.Messages.Get(ctx, foreign.ID)
	require.NoError(t, getErr, "the foreign same-text row lands after the pre-delete listing, outside the tail slice; it must survive")

	// The decisive assertion: the recreate must FIRE — count=2 (the
	// foreign row plus the recreated prompt). A missing union leaves the
	// foreign row's ID out of the baseline; its text then matches and its
	// ID reads as new, so the helper misreads it as the already-recreated
	// prompt and the operator's rerun prompt is silently lost: count=1.
	msgs, err := a.Messages.List(ctx, sessionID)
	require.NoError(t, err)
	count := 0
	for _, m := range msgs {
		if m.Role == message.User && m.Content().Text == promptText {
			count++
		}
	}
	require.Equal(t, 2, count,
		"the post-delete union must identify the foreign same-text row (created between the two listings) as PRE-EXISTING so the recreate fires; "+
			"got %d matching user messages, expected 2 — without the union the foreign row is misread as the already-recreated prompt "+
			"and the operator's rerun prompt is silently lost (seventeenth-review M-1, #644's shape on the success path)", count)
	require.Len(t, msgs, 2, "foreign row + recreated prompt; nothing else may remain")
}
