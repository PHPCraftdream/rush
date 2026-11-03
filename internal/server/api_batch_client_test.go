package server

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/PHPCraftdream/rush/internal/apiclient"
	"github.com/PHPCraftdream/rush/internal/config"
	"github.com/stretchr/testify/require"
)

// batchAPIClient is the production client (internal/apiclient) pointed at the
// production serve listener of the batch fixture, so the client's wire types are
// checked against what the server really sends.
func (f *batchFixture) apiClient(t *testing.T, token string) *apiclient.Client {
	t.Helper()
	client, _, err := apiclient.New(apiclient.Options{BaseURL: f.base, Token: token, AllowCleartext: true, MaxResponseBytes: 1 << 20})
	require.NoError(t, err)
	previous := apiclient.BatchPollInterval
	apiclient.BatchPollInterval = 20 * time.Millisecond
	t.Cleanup(func() { apiclient.BatchPollInterval = previous })
	return client
}

func clientContext(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 40*time.Second)
	t.Cleanup(cancel)
	return ctx
}

func TestAPIClientTaskLifecycleAgainstTheProductionListener(t *testing.T) {
	f := newBatchFixture(t, batchTextProvider("the task output"))
	token := f.token(t, "client", []string{"read", "run"}, "ask", config.APITokenBinding{})
	client := f.apiClient(t, token)
	ctx := clientContext(t)

	accepted, err := client.CreateTask(ctx, apiclient.TaskRequest{Prompt: "say something"}, "key-1")
	require.NoError(t, err)
	require.NotEmpty(t, accepted.RootID)
	require.False(t, accepted.Replayed)

	replay, err := client.CreateTask(ctx, apiclient.TaskRequest{Prompt: "say something"}, "key-1")
	require.NoError(t, err)
	require.Equal(t, accepted.RootID, replay.RootID)
	require.True(t, replay.Replayed, "the same key and body replays the original root")

	batch, err := client.WaitBatch(ctx, accepted.RootID)
	require.NoError(t, err)
	require.True(t, batch.Done)
	require.Equal(t, "completed", batch.State)
	tasks := 0
	var leaf apiclient.BatchNode
	for _, node := range batch.Nodes {
		if node.Kind == "task" {
			tasks++
			leaf = node
		}
	}
	require.Equal(t, 1, tasks)
	require.Equal(t, "agent", leaf.Tool)
	require.NotEmpty(t, leaf.ChildSessionID)
	require.Contains(t, leaf.Output, "the task output")

	node, err := client.GetBatchNode(ctx, accepted.RootID, leaf.ID)
	require.NoError(t, err)
	require.Equal(t, leaf.ID, node.ID)

	resumed, err := client.CreateTask(ctx, apiclient.TaskRequest{Prompt: "and more", ResumeSessionID: leaf.ChildSessionID}, "")
	require.NoError(t, err)
	again, err := client.WaitBatch(ctx, resumed.RootID)
	require.NoError(t, err)
	for _, n := range again.Nodes {
		if n.Kind == "task" {
			require.Equal(t, leaf.ChildSessionID, n.ChildSessionID, "the resume continued the same child")
		}
	}
}

func TestAPIClientBatchErrorsAgainstTheProductionListener(t *testing.T) {
	f := newBatchFixture(t, batchTextProvider("ok"))
	token := f.token(t, "client", []string{"read", "run"}, "ask", config.APITokenBinding{})
	client := f.apiClient(t, token)
	ctx := clientContext(t)

	_, err := client.GetBatch(ctx, "no-such-root")
	require.True(t, apiclient.IsNotFound(err), "an unknown root is a 404: %v", err)
	require.Equal(t, apiclient.ExitCodeNotFound, apiclient.ExitCodeFor(err))

	_, err = client.CreateBatch(ctx, []byte("{not json"), "")
	var usage *apiclient.UsageError
	require.ErrorAs(t, err, &usage, "a spec that is not JSON never reaches the server")

	_, err = client.CreateBatch(ctx, []byte(`{"tool":"agent","input":{"prompt":"x"}}`), "")
	require.Error(t, err, "a bare task is not a batch")

	_, err = client.CreateTask(ctx, apiclient.TaskRequest{Prompt: "p", ResumeSessionID: "no-such-session"}, "")
	require.Error(t, err)
	require.Equal(t, apiclient.ExitCodeFailure, apiclient.ExitCodeFor(err))
}

func TestAPIClientWaitStopsOnAHeldQuestionAndAnswerResumes(t *testing.T) {
	f := newBatchFixture(t, questionProvider())
	f.grantChildQuestionTools(t)
	token := f.token(t, "ask", []string{"read", "run"}, "ask", config.APITokenBinding{})
	client := f.apiClient(t, token)
	ctx := clientContext(t)

	accepted, err := client.CreateTask(ctx, apiclient.TaskRequest{Prompt: "ask me something"}, "")
	require.NoError(t, err)

	batch, err := client.WaitBatch(ctx, accepted.RootID)
	var paused *apiclient.BatchPausedError
	require.ErrorAs(t, err, &paused, "wait stops on the held question")
	require.Equal(t, apiclient.ExitCodeRunPaused, apiclient.ExitCodeFor(err))
	held := batch.HeldQuestions()
	require.Len(t, held, 1)
	require.Equal(t, "Which port?", held[0].Question)

	require.NoError(t, client.AnswerBatchQuestion(ctx, accepted.RootID, held[0].ID, "8080"))
	done, err := client.WaitBatch(ctx, accepted.RootID)
	require.NoError(t, err)
	require.Equal(t, "completed", done.State)
	require.Contains(t, done.Nodes[len(done.Nodes)-1].Output+done.Nodes[0].Output, "child finished: 8080")

	// The question is answered: a second answer is the API's 409, a plain failure here.
	err = client.AnswerBatchQuestion(ctx, accepted.RootID, held[0].ID, "again")
	require.Error(t, err)
	var failed *apiclient.BatchFailedError
	require.False(t, errors.As(err, &failed))
}
