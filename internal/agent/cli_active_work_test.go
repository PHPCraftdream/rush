package agent

import (
	"context"
	"fmt"
	"net/http"
	"sync"
	"testing"

	"charm.land/fantasy"
	"github.com/PHPCraftdream/rush/internal/agent/tools"
	"github.com/PHPCraftdream/rush/internal/session"
	"github.com/stretchr/testify/require"
)

// Revert-check: changing CLIActiveWork's `if !persisted {` to `if !persisted && false {` permits early exit.
func TestCLIActiveWorkHeldQuestionPersistence(t *testing.T) {
	ctx := context.Background()
	f, parent, child, _ := childDelegationBase(t, attemptFixtureOpts{
		tools:   []fantasy.AgentTool{tools.NewAskQuestionTool()},
		handler: textThenAskQuestionResponse,
		noIdle:  true,
	})
	_, err := f.sa.Run(ctx, SessionAgentCall{SessionID: child, Prompt: "do work", NonInteractive: true})
	require.ErrorAs(t, err, new(*AwaitingAnswerError))
	require.True(t, f.coord.autoResumeSuspended(child))
	require.False(t, f.sa.IsSessionBusy(child))

	// Use the ledger's existing latches to hold only the delegation and
	// reserve its notice for the channel-controlled asynchronous writer.
	job := jobOf(f.ledger, parent, "delegate-1")
	f.ledger.mu.Lock()
	job.answerHold = true
	job.questionNoticed = true
	f.ledger.mu.Unlock()
	f.ledger.armDelegation(job, jobResult{content: "first turn text"})
	require.Same(t, job, f.ledger.heldQuestionJob(child))
	live, incomplete := f.store.LiveJobs(ctx, parent)
	require.False(t, incomplete)
	require.Len(t, live, 1, "only the held delegation may be live")
	require.Equal(t, child, live[0].ChildSessionID)
	require.False(t, f.ledger.running(child))
	require.Nil(t, f.coord.background)
	require.Empty(t, childQuestionRows(t, f, parent))
	_, question, ok := f.ledger.childAwaitingQuestion(child)
	require.True(t, ok)

	ready := make(chan struct{})
	commit := make(chan struct{})
	committed := make(chan error, 1)
	done := make(chan struct{})
	var release sync.Once
	releaseWriter := func() { release.Do(func() { close(commit) }) }
	t.Cleanup(func() {
		releaseWriter()
		<-done
	})
	go func() {
		defer close(done)
		close(ready)
		<-commit
		committed <- f.store.InsertSessionNotice(ctx, parent, session.NoticeKindChildQuestion,
			childQuestionNoticeText("", question, child, job.toolCallID, 0), true, job.toolCallID)
	}()
	<-ready
	active, err := f.coord.CLIActiveWork(ctx, parent)
	require.NoError(t, err)
	require.True(t, active, "held question must keep CLI active before its notice commits")
	require.Empty(t, childQuestionRows(t, f, parent))

	releaseWriter()
	require.NoError(t, <-committed)
	notices := childQuestionRows(t, f, parent)
	require.Len(t, notices, 1)
	require.Contains(t, notices[0].Text, "SUB-AGENT QUESTION (session "+child+")")
	require.Contains(t, notices[0].Text, "job "+job.toolCallID+" stays open")
	require.Same(t, job, f.ledger.heldQuestionJob(child))
	active, err = f.coord.CLIActiveWork(ctx, parent)
	require.NoError(t, err)
	require.False(t, active, "durably announced question alone is not active CLI work")
}

// Revert-check: removing IsHardQuotaLimit's status guard misclassifies auth errors.
func TestIsHardQuotaLimit(t *testing.T) {
	for _, tc := range []struct {
		status int
		text   string
		want   bool
	}{
		{429, "quota exhausted", true},
		{429, "usage limit", true},
		{429, "server overloaded", false},
		{401, "quota exhausted", false},
		{402, "quota exhausted", false},
		{403, "quota exhausted", false},
	} {
		err := fmt.Errorf("wrapped: %w", &fantasy.ProviderError{StatusCode: tc.status, Message: tc.text})
		require.Equal(t, tc.want, IsHardQuotaLimit(err))
	}
	require.False(t, IsHardQuotaLimit(nil))
	require.False(t, IsHardQuotaLimit(fmt.Errorf("quota exhausted HTTP %d", http.StatusTooManyRequests)))
}
