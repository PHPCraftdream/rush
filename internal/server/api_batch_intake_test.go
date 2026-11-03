package server

import (
	"net/http"
	"strings"
	"testing"

	"github.com/PHPCraftdream/rush/internal/config"
	"github.com/PHPCraftdream/rush/internal/message"
	"github.com/stretchr/testify/require"
)

func TestBatchIntakeTaskRunsToCompletionThroughProductionListener(t *testing.T) {
	f := newBatchFixture(t, batchTextProvider("task done"))
	token := f.token(t, "intake", []string{"read", "run"}, "ask", config.APITokenBinding{})
	accepted := f.do(t, token, http.MethodPost, "/tasks", map[string]any{"prompt": "do the thing"}).accepted(t)
	done := f.waitBatchDone(t, token, accepted.RootID)
	require.Equal(t, "completed", done.State)
	require.Equal(t, accepted.RootID, done.RootID)
	require.Len(t, done.Nodes, 2, "the one-item batch is a root and one leaf")
	var leaf apiBatchNode
	for _, node := range done.Nodes {
		if node.Kind == "task" {
			leaf = node
		}
	}
	require.Equal(t, "agent", leaf.Tool)
	require.Equal(t, "completed", leaf.State)
	require.NotEmpty(t, leaf.ChildSessionID, "child_session_id is exposed once bound")
	child, err := f.app.Sessions.Get(t.Context(), leaf.ChildSessionID)
	require.NoError(t, err)
	require.Equal(t, serviceSessionID(token2ID(t, f, token)), child.ParentSessionID, "the child belongs to the token's service session")

	service, err := f.app.Sessions.Get(t.Context(), child.ParentSessionID)
	require.NoError(t, err)
	require.Equal(t, message.OriginAPIService, service.Origin)

	// Node read.
	node := f.do(t, token, http.MethodGet, "/batches/"+accepted.RootID+"/nodes/"+leaf.ID, nil)
	require.Equal(t, http.StatusOK, node.Status, string(node.Body))
	require.NotContains(t, string(node.Body), "task_payload", "DTOs never carry raw rows")
	require.NotContains(t, string(node.Body), "owner_session_id")
}

func token2ID(t *testing.T, f *batchFixture, token string) string {
	t.Helper()
	id, _, _ := strings.Cut(strings.TrimPrefix(token, "rush_"), ".")
	return id
}

func TestBatchIntakeAllowlistRefusesBeforeAnyRowIsWritten(t *testing.T) {
	f := newBatchFixture(t, batchTextProvider("never"))
	token := f.token(t, "allowlist", []string{"read", "run"}, "ask", config.APITokenBinding{})
	body := `{"mode":"parallel","items":[{"tool":"agent","input":{"prompt":"p"}},{"mode":"sequential","items":[{"tool":"run_command","input":{"command":"echo x"}}]}]}`
	resp := f.do(t, token, http.MethodPost, "/batches", body)
	require.Equal(t, http.StatusBadRequest, resp.Status, string(resp.Body))
	require.Contains(t, string(resp.Body), "run_command")

	fac, err := f.app.BatchCLIStore()
	require.NoError(t, err)
	roots, err := fac.Store.BatchAPIRoots(t.Context(), serviceSessionID(token2ID(t, f, token)))
	require.NoError(t, err)
	require.Empty(t, roots, "a refused submission wrote nothing")
	_, err = f.app.Sessions.Get(t.Context(), serviceSessionID(token2ID(t, f, token)))
	require.Error(t, err, "a refused submission did not even create the service session")

	for name, bad := range map[string]string{
		"unknown field": `{"mode":"parallel","bogus":1}`,
		"not a batch":   `{"tool":"agent","input":{"prompt":"p"}}`,
		"trailing":      `{"mode":"parallel","items":[]} {}`,
		"empty":         ``,
	} {
		require.Equal(t, http.StatusBadRequest, f.do(t, token, http.MethodPost, "/batches", bad).Status, name)
	}
	require.Equal(t, http.StatusBadRequest, f.do(t, token, http.MethodPost, "/batches", `{"mode":"parallel","items":[{"tool":"agent","input":{"prompt":"p"}}]}`, "Idempotency-Key", "bad key!").Status)
}

func TestBatchIntakeExactNameOptInThroughBinding(t *testing.T) {
	f := newBatchFixture(t, batchTextProvider("ok"))
	plain := f.token(t, "plain", []string{"run"}, "ask", config.APITokenBinding{})
	opted := f.token(t, "opted", []string{"run"}, "ask", config.APITokenBinding{Kind: "batch", Batch: &config.APITokenBatchBinding{Tools: []string{"bash"}, MaxRoots: 2, MaxParallel: 2}})
	spec := `{"mode":"parallel","items":[{"tool":"bash","input":{"command":"echo hi"}}]}`
	require.Equal(t, http.StatusBadRequest, f.do(t, plain, http.MethodPost, "/batches", spec).Status, "bash is off by default")
	// The opted-in token passes the allowlist; whether the leaf then runs is the engine's business.
	resp := f.do(t, opted, http.MethodPost, "/batches", spec)
	require.NotEqual(t, http.StatusBadRequest, resp.Status, string(resp.Body))
}

func TestBatchIntakeReadonlyTokenCannotSubmitAgents(t *testing.T) {
	f := newBatchFixture(t, batchTextProvider("never"))
	token := f.token(t, "readonly", []string{"run"}, "readonly", config.APITokenBinding{})
	resp := f.do(t, token, http.MethodPost, "/tasks", map[string]any{"prompt": "p"})
	require.Equal(t, http.StatusBadRequest, resp.Status, string(resp.Body))
	require.Contains(t, string(resp.Body), "not available to a read-only token", "the agent tool is refused by name, not by falling off the read-only list")
}

func TestBatchIntakeIdempotencyIsDurableAndBodyBound(t *testing.T) {
	f := newBatchFixture(t, batchTextProvider("ok"))
	token := f.token(t, "idem", []string{"read", "run"}, "ask", config.APITokenBinding{})
	first := f.do(t, token, http.MethodPost, "/tasks", map[string]any{"prompt": "same"}, "Idempotency-Key", "k-1").accepted(t)
	f.waitBatchDone(t, token, first.RootID)
	again := f.do(t, token, http.MethodPost, "/tasks", map[string]any{"prompt": "same"}, "Idempotency-Key", "k-1").accepted(t)
	require.Equal(t, first.RootID, again.RootID, "a replay returns the original root")
	require.True(t, again.Replayed)

	conflict := f.do(t, token, http.MethodPost, "/tasks", map[string]any{"prompt": "different"}, "Idempotency-Key", "k-1")
	require.Equal(t, http.StatusUnprocessableEntity, conflict.Status, string(conflict.Body))

	other := f.do(t, token, http.MethodPost, "/tasks", map[string]any{"prompt": "same"}, "Idempotency-Key", "k-2").accepted(t)
	require.NotEqual(t, first.RootID, other.RootID, "another key is another root")

	fac, err := f.app.BatchCLIStore()
	require.NoError(t, err)
	roots, err := fac.Store.BatchAPIRoots(t.Context(), serviceSessionID(token2ID(t, f, token)))
	require.NoError(t, err)
	require.Len(t, roots, 2)
	for _, root := range roots {
		require.True(t, strings.HasPrefix(root.Ref, "batch-cli:api:"), "the submit reference keeps the batch-cli: family: %s", root.Ref)
		require.LessOrEqual(t, len(root.Ref), 128)
	}
}

func TestBatchIntakeRootsAreConfinedToTheirToken(t *testing.T) {
	f := newBatchFixture(t, batchTextProvider("ok"))
	owner := f.token(t, "owner", []string{"read", "run"}, "ask", config.APITokenBinding{})
	stranger := f.token(t, "stranger", []string{"read", "run"}, "ask", config.APITokenBinding{})
	accepted := f.do(t, owner, http.MethodPost, "/tasks", map[string]any{"prompt": "mine"}).accepted(t)
	f.waitBatchDone(t, owner, accepted.RootID)

	foreign := f.do(t, stranger, http.MethodGet, "/batches/"+accepted.RootID, nil)
	missing := f.do(t, stranger, http.MethodGet, "/batches/no-such-root", nil)
	require.Equal(t, http.StatusNotFound, foreign.Status)
	require.Equal(t, http.StatusNotFound, missing.Status)
	require.Equal(t, problemShape(t, string(missing.Body)), problemShape(t, string(foreign.Body)), "a foreign root is indistinguishable from a missing one")
	require.Equal(t, http.StatusNotFound, f.do(t, stranger, http.MethodPost, "/batches/"+accepted.RootID+"/stop", nil).Status)
	require.Equal(t, http.StatusNotFound, f.do(t, stranger, http.MethodGet, "/batches/"+accepted.RootID+"/nodes/"+accepted.RootID, nil).Status)

	// A read token cannot submit or stop.
	reader := f.token(t, "reader", []string{"read"}, "ask", config.APITokenBinding{})
	require.Equal(t, http.StatusForbidden, f.do(t, reader, http.MethodPost, "/tasks", map[string]any{"prompt": "p"}).Status)
	require.Equal(t, http.StatusForbidden, f.do(t, reader, http.MethodPost, "/batches/"+accepted.RootID+"/stop", nil).Status)

	// A session-pinned token never reaches batch intake.
	pinTarget, err := f.app.Sessions.Create(t.Context(), "pin target")
	require.NoError(t, err)
	pinned := f.token(t, "pinned", []string{"read", "run"}, "ask", config.APITokenBinding{Kind: "session", Pin: &config.APITokenPinBinding{Session: pinTarget.ID, Workspace: f.workspace}})
	require.Equal(t, http.StatusForbidden, f.do(t, pinned, http.MethodPost, "/tasks", map[string]any{"prompt": "p"}).Status)
	require.Equal(t, http.StatusForbidden, f.do(t, pinned, http.MethodGet, "/batches/"+accepted.RootID, nil).Status)
}

func TestBatchServiceSessionIsHiddenFromSessionRoutes(t *testing.T) {
	f := newBatchFixture(t, batchTextProvider("ok"))
	token := f.token(t, "hide", []string{"read", "run"}, "ask", config.APITokenBinding{})
	f.do(t, token, http.MethodPost, "/tasks", map[string]any{"prompt": "p"}).accepted(t)
	service := serviceSessionID(token2ID(t, f, token))

	list := f.do(t, token, http.MethodGet, "/sessions", nil)
	require.Equal(t, http.StatusOK, list.Status)
	require.NotContains(t, string(list.Body), service, "the service session is not listed")
	require.Equal(t, http.StatusNotFound, f.do(t, token, http.MethodGet, "/sessions/"+service, nil).Status)
	require.Equal(t, http.StatusNotFound, f.do(t, token, http.MethodGet, "/sessions/"+service+"/messages", nil).Status)
	require.Equal(t, http.StatusNotFound, f.do(t, token, http.MethodPost, "/sessions/"+service+"/runs", map[string]any{"prompt": "talk"}).Status, "a service session never accepts conversational runs")
	require.Equal(t, http.StatusNotFound, f.do(t, token, http.MethodPost, "/sessions/"+service+"/inject", map[string]any{"message": "hi"}).Status)
}

func TestBatchServiceSessionIdCollisionIsRefused(t *testing.T) {
	f := newBatchFixture(t, batchTextProvider("ok"))
	token := f.token(t, "collide", []string{"read", "run"}, "ask", config.APITokenBinding{})
	id := serviceSessionID(token2ID(t, f, token))
	conversational, err := f.app.Sessions.CreateWithID(t.Context(), id, "squatter")
	require.NoError(t, err)
	require.Equal(t, id, conversational.ID)
	resp := f.do(t, token, http.MethodPost, "/tasks", map[string]any{"prompt": "p"})
	require.GreaterOrEqual(t, resp.Status, 500, "a conversational session squatting the id must not become an owner: %s", resp.Body)
}

func TestAPIBatchResumeContinuesOnlyADirectChildOfTheServiceSession(t *testing.T) {
	f := newBatchFixture(t, batchTextProvider("ok"))
	token := f.token(t, "resume", []string{"read", "run"}, "ask", config.APITokenBinding{})
	other := f.token(t, "other", []string{"read", "run"}, "ask", config.APITokenBinding{})
	first := f.do(t, token, http.MethodPost, "/tasks", map[string]any{"prompt": "first"}).accepted(t)
	firstDone := f.waitBatchDone(t, token, first.RootID)
	var child string
	for _, node := range firstDone.Nodes {
		if node.Kind == "task" {
			child = node.ChildSessionID
		}
	}
	require.NotEmpty(t, child)
	before, err := f.app.Messages.List(t.Context(), child)
	require.NoError(t, err)

	resumed := f.do(t, token, http.MethodPost, "/tasks", map[string]any{"prompt": "follow up", "resume_session_id": child}).accepted(t)
	resumedDone := f.waitBatchDone(t, token, resumed.RootID)
	require.Equal(t, "completed", resumedDone.State)
	for _, node := range resumedDone.Nodes {
		if node.Kind == "task" {
			require.Equal(t, child, node.ChildSessionID, "the resume continued the same child, it did not create another")
		}
	}
	after, err := f.app.Messages.List(t.Context(), child)
	require.NoError(t, err)
	require.Greater(t, len(after), len(before), "resuming appends to the child's conversation")

	// Not the owner's child: another token's child, the service session itself, an unrelated session, a missing one.
	otherRoot := f.do(t, other, http.MethodPost, "/tasks", map[string]any{"prompt": "theirs"}).accepted(t)
	otherDone := f.waitBatchDone(t, other, otherRoot.RootID)
	var foreignChild string
	for _, node := range otherDone.Nodes {
		if node.Kind == "task" {
			foreignChild = node.ChildSessionID
		}
	}
	unrelated, err := f.app.Sessions.Create(t.Context(), "unrelated")
	require.NoError(t, err)
	for name, target := range map[string]string{
		"another token's child": foreignChild,
		"the service session":   serviceSessionID(token2ID(t, f, token)),
		"an unrelated session":  unrelated.ID,
		"a missing session":     "no-such-session",
	} {
		resp := f.do(t, token, http.MethodPost, "/tasks", map[string]any{"prompt": "x", "resume_session_id": target})
		require.Equal(t, http.StatusBadRequest, resp.Status, "%s: %s", name, resp.Body)
		require.Contains(t, string(resp.Body), "not a child")
	}
}
