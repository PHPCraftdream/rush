package agent

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

type gatedQuestionPublication struct {
	entered   chan struct{}
	release   chan struct{}
	committed atomic.Bool
}

func (p *gatedQuestionPublication) PublishChildQuestion(ctx context.Context, _ batchChildQuestionNotice) error {
	close(p.entered)
	select {
	case <-p.release:
		p.committed.Store(true)
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

type publicationOrderResolver struct {
	publication *gatedQuestionPublication
	calls       atomic.Int32
}

func (r *publicationOrderResolver) ResolveChildQuestions(ctx context.Context, _, _, _ string, _, _ int64) error {
	r.calls.Add(1)
	if err := ctx.Err(); err != nil {
		return err
	}
	if !r.publication.committed.Load() {
		return errors.New("answer resolution preceded question publication")
	}
	return nil
}

func TestBatchQuestionPublicationSerializesWithAnswerAcceptance(t *testing.T) {
	tl := newQuestionTestTimeline(t)
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	pub := &gatedQuestionPublication{entered: make(chan struct{}), release: make(chan struct{})}
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(pub.release) }) }
	defer release()
	resolver := &publicationOrderResolver{publication: pub}
	e := &batchAgentExecution{
		scope: newBatchAgentScope(),
		req:   batchAgentRunRequest{NodeID: "root.1", Token: 7, SessionID: "owner", ChildSessionID: "child"},
		deps:  batchAgentDeps{QuestionPublisher: pub, QuestionResolver: resolver},
	}
	type askResult struct {
		answer json.RawMessage
		err    error
	}
	asked := make(chan askResult, 1)
	go func() { answer, err := e.askBatchQuestion(ctx, 1, "continue?"); asked <- askResult{answer, err} }()
	select {
	case <-pub.entered:
	case <-ctx.Done():
		t.Fatal("publisher did not reach commit barrier")
	}
	// Белый ящик: выбранная граница линеаризации обязана удерживать scope mutex.
	unlocked := e.scope.mu.TryLock()
	if unlocked {
		e.scope.mu.Unlock()
	}
	tl.addf("publication barrier: scope_unlocked=%v committed=%v", unlocked, pub.committed.Load())
	require.False(t, unlocked, "publication must serialize with held-scope answer acceptance")
	injected := make(chan error, 1)
	go func() { injected <- e.Inject(ctx, json.RawMessage(`{"depth":1,"sequence":1,"text":"accepted"}`)) }()
	release()
	select {
	case err := <-injected:
		tl.addf("inject returned: %v", err)
		require.NoError(t, err)
	case <-ctx.Done():
		t.Fatal("inject did not finish after publication")
	}
	select {
	case result := <-asked:
		require.NoError(t, result.err)
		require.JSONEq(t, `"accepted"`, string(result.answer))
	case <-ctx.Done():
		t.Fatal("the original ask did not receive its answer")
	}
	require.EqualValues(t, 1, resolver.calls.Load())
}

func TestBatchScopeZeroSequenceCannotResolveUnpublishedHold(t *testing.T) {
	tl := newQuestionTestTimeline(t)
	scope := newBatchAgentScope()
	hold, err := scope.hold(1)
	require.NoError(t, err)
	called := false
	err = scope.deliverWithCommit(batchAgentInjectMessage{Depth: 1, Sequence: 0}, func() error { called = true; return nil })
	tl.addf("zero-sequence answer: err=%v committed=%v", err, called)
	require.ErrorIs(t, err, ErrBatchAgentNotHeld)
	require.False(t, called)
	scope.mu.Lock()
	retained := scope.held == hold
	scope.mu.Unlock()
	require.True(t, retained)
}
