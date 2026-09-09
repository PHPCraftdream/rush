package server

// Regression tests for recreateRerunPromptIfLost in isolation: when a surviving target must not be recreated, and how an earlier identical prompt interacts with a newer prompt row. Split out of p655_rerun_idset_regression_test.go when the 1000-line file limit landed.

import (
	"testing"

	"github.com/PHPCraftdream/rush/internal/message"
	"github.com/stretchr/testify/require"
)

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
