package app

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/PHPCraftdream/rush/internal/jobbatch"
	"github.com/PHPCraftdream/rush/internal/session"
	"github.com/stretchr/testify/require"
)

func TestBatchSubmit_AgentItemCreatesThenResumesChild(t *testing.T) {
	t.Setenv("RUSH_GLOBAL_DIR", t.TempDir())
	t.Setenv("RUSH_GLOBAL_DATA", t.TempDir())
	t.Setenv("RUSH_GLOBAL_CONFIG", t.TempDir())
	var childRequests atomic.Int32
	application, ownerID := newAdmissionRaceApp(t, func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		system, lastUser, _ := lastTurnParts(body)
		if strings.Contains(lastUser, "Generate a concise title") {
			admissionWriteSSE(w, []string{admissionSSEText("title", "Batch Premise"), admissionSSEStop("title", "stop")})
			return
		}
		if strings.Contains(system, "an agent for Rush") {
			childRequests.Add(1)
		}
		admissionWriteSSE(w, []string{admissionSSEText("leaf", "leaf complete"), admissionSSEStop("leaf", "stop")})
	})
	ctx, cancel := context.WithTimeout(t.Context(), 45*time.Second)
	defer cancel()
	owner, err := application.Sessions.Get(ctx, ownerID)
	require.NoError(t, err)
	secondOwner, err := application.Sessions.Create(ctx, "second batch owner")
	require.NoError(t, err)

	submit := func(owner, prompt, resume string) (string, session.Session) {
		t.Helper()
		input := map[string]any{"prompt": prompt}
		if resume != "" {
			input["resume_session_id"] = resume
		}
		payload, marshalErr := json.Marshal(map[string]any{"tool": "agent", "input": input})
		require.NoError(t, marshalErr)
		root, submitErr := application.BatchSubmit(ctx, owner, jobbatch.Spec{Batch: &jobbatch.BatchSpec{
			Mode:  jobbatch.ModeSequential,
			Items: []jobbatch.Spec{{Task: &jobbatch.TaskSpec{Payload: payload}}},
		}})
		require.NoError(t, submitErr)
		_, waitErr := application.BatchWait(ctx, owner, root)
		require.NoError(t, waitErr)
		snapshot, readErr := application.BatchCLIStore()
		require.NoError(t, readErr)
		batch, readErr := snapshot.Store.Read(ctx, owner, root)
		require.NoError(t, readErr)
		require.Len(t, batch.Nodes, 2)
		node := batch.Nodes[1]
		if node.ChildSessionID.Valid {
			child, getErr := application.Sessions.Get(ctx, node.ChildSessionID.String)
			require.NoError(t, getErr)
			return root, child
		}
		return root, session.Session{}
	}

	firstRoot, child := submit(owner.ID, "first child task", "")
	require.NotEmpty(t, child.ID)
	firstSnapshot, err := application.BatchCLIStore()
	require.NoError(t, err)
	firstBatch, err := firstSnapshot.Store.Read(ctx, owner.ID, firstRoot)
	require.NoError(t, err)
	require.Equal(t, string(jobbatch.StateCompleted), firstBatch.Nodes[1].State)
	require.Equal(t, owner.ID, child.ParentSessionID)
	firstMessages, err := application.Messages.List(ctx, child.ID)
	require.NoError(t, err)
	resumedRoot, resumedChild := submit(owner.ID, "resume child task", child.ID)
	require.Equal(t, child.ID, resumedChild.ID)
	resumedSnapshot, err := application.BatchCLIStore()
	require.NoError(t, err)
	resumedBatch, err := resumedSnapshot.Store.Read(ctx, owner.ID, resumedRoot)
	require.NoError(t, err)
	require.Equal(t, string(jobbatch.StateCompleted), resumedBatch.Nodes[1].State)
	resumedMessages, err := application.Messages.List(ctx, child.ID)
	require.NoError(t, err)
	require.Greater(t, len(resumedMessages), len(firstMessages), "resuming the direct child must append messages")

	_, foreignChild := submit(secondOwner.ID, "foreign child task", "")
	require.NotEmpty(t, foreignChild.ID)
	foreignRoot, _ := submit(owner.ID, "reject foreign child", foreignChild.ID)
	foreignSnapshot, err := application.BatchCLIStore()
	require.NoError(t, err)
	foreignBatch, err := foreignSnapshot.Store.Read(ctx, owner.ID, foreignRoot)
	require.NoError(t, err)
	require.Len(t, foreignBatch.Nodes, 2)
	require.Equal(t, string(jobbatch.StateFailed), foreignBatch.Nodes[1].State)
	require.Contains(t, foreignBatch.Nodes[1].OutcomeSummary, "not a child of the current session")
	require.Greater(t, childRequests.Load(), int32(0))
	require.NotEmpty(t, firstRoot)
}

func TestBatchHeldChildQuestion_WakesConversationalOwner(t *testing.T) {
	t.Setenv("RUSH_GLOBAL_DIR", t.TempDir())
	t.Setenv("RUSH_GLOBAL_DATA", t.TempDir())
	t.Setenv("RUSH_GLOBAL_CONFIG", t.TempDir())
	// Goes through BatchSubmit's batch-cli: reference and observes production
	// question publication and the owner's provider calls.
	var application *App
	var ownerID string
	var ownerCalls atomic.Int32
	ownerCalled := make(chan struct{}, 1)
	childAsked := make(chan struct{})
	var childAskOnce atomic.Bool
	application, ownerID = newAdmissionRaceApp(t, func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		system, lastUser, _ := lastTurnParts(body)
		if strings.Contains(lastUser, "Generate a concise title") {
			admissionWriteSSE(w, []string{admissionSSEText("title", "Batch Question"), admissionSSEStop("title", "stop")})
			return
		}
		if strings.Contains(system, "an agent for Rush") {
			if childAskOnce.CompareAndSwap(false, true) {
				close(childAsked)
			}
			admissionWriteSSE(w, []string{
				admissionSSEToolCall("child-ask", "call-ask", "ask_question", `{"question":"Which port?","options":["8080"]}`),
				admissionSSEStop("child-ask", "tool_calls"),
			})
			return
		}
		if r.Header.Get("x-session-id") == session.HashID(ownerID) {
			ownerCalls.Add(1)
			select {
			case ownerCalled <- struct{}{}:
			default:
			}
		}
		admissionWriteSSE(w, []string{admissionSSEText("owner", "unexpected owner turn"), admissionSSEStop("owner", "stop")})
	})
	grantChildWakeTools(t, application)
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	t.Cleanup(cancel)
	owner, err := application.Sessions.Get(ctx, ownerID)
	require.NoError(t, err)
	payload, err := json.Marshal(map[string]any{"tool": "agent", "input": map[string]any{"prompt": "ask a question"}})
	require.NoError(t, err)
	_, err = application.BatchSubmit(ctx, owner.ID, jobbatch.Spec{Batch: &jobbatch.BatchSpec{
		Mode:  jobbatch.ModeSequential,
		Items: []jobbatch.Spec{{Task: &jobbatch.TaskSpec{Payload: payload}}},
	}})
	require.NoError(t, err)
	select {
	case <-childAsked:
	case <-ctx.Done():
		require.FailNow(t, "child did not reach ask_question", "%v", ctx.Err())
	}
	// The real wake path is wired (no recorder replaces it). Premise experiment
	// result: a held child question DOES start a model turn of the owner session
	// (the question text is published into the owner conversation and the owner is
	// woken). That is the intended CLI behaviour; API service sessions must not do
	// it (see the API service-session wake gate).
	select {
	case <-ownerCalled:
	case <-time.After(pwWakeTimeout):
		require.FailNow(t, "a held child question must wake the conversational batch owner")
	}
	require.Positive(t, ownerCalls.Load())
}
