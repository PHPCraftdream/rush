package cmd

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeAPICall is one request the fake server saw.
type fakeAPICall struct {
	Method, Path, Query, LastEventID, Body string
}

// fakeRunServer speaks the REAL wire shapes of the run, event, message and
// permission routes (internal/server/api_*.go, internal/apiclient types), so
// the CLI's run/events/permissions paths can be driven without a provider.
type fakeRunServer struct {
	*httptest.Server
	mu          sync.Mutex
	calls       []fakeAPICall
	runStates   []string // consumed per GET /runs/r1; the last one repeats
	runError    string   // failed-run summary
	events      []string // raw SSE frames; the stream stays open afterwards
	permissions bool     // GET /permissions returns one pending request
}

func newFakeRunServer(t *testing.T, states ...string) *fakeRunServer {
	t.Helper()
	f := &fakeRunServer{runStates: states}
	f.Server = httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.Server.Close)
	return f
}

func (f *fakeRunServer) record(r *http.Request) string {
	body, _ := io.ReadAll(r.Body)
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, fakeAPICall{r.Method, r.URL.Path, r.URL.RawQuery, r.Header.Get("Last-Event-ID"), string(body)})
	return string(body)
}

func (f *fakeRunServer) seen(method, pathSuffix string) []fakeAPICall {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []fakeAPICall
	for _, call := range f.calls {
		if call.Method == method && strings.HasSuffix(call.Path, pathSuffix) {
			out = append(out, call)
		}
	}
	return out
}

func (f *fakeRunServer) nextState() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.runStates) == 0 {
		return "running"
	}
	state := f.runStates[0]
	if len(f.runStates) > 1 {
		f.runStates = f.runStates[1:]
	}
	return state
}

func writeFakeJSON(w http.ResponseWriter, status int, body string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = io.WriteString(w, body)
}

func (f *fakeRunServer) serve(w http.ResponseWriter, r *http.Request) {
	body := f.record(r)
	path := strings.TrimPrefix(r.URL.Path, "/api/v1")
	switch {
	case r.Method == http.MethodPost && path == "/sessions/s1/runs":
		writeFakeJSON(w, http.StatusAccepted, `{"run_id":"r1","session_id":"s1","state":"queued"}`)
	case r.Method == http.MethodGet && path == "/runs/r1":
		state := f.nextState()
		if state == "running" || state == "queued" {
			time.Sleep(50 * time.Millisecond) // a long poll must not spin
		}
		run := fmt.Sprintf(`{"id":"r1","session_id":"s1","state":%q,"created_at":"2026-01-01T00:00:00Z"`, state)
		switch state {
		case "succeeded":
			run += `,"message_id":"m1"`
		case "failed":
			run += fmt.Sprintf(`,"error":{"code":"provider_error","summary":%q}`, f.runError)
		}
		writeFakeJSON(w, http.StatusOK, run+"}")
	case r.Method == http.MethodGet && path == "/sessions/s1/messages":
		writeFakeJSON(w, http.StatusOK, `{"items":[{"id":"m1","session_id":"s1","role":"assistant","parts":[{"type":"text","text":"final "},{"type":"text","text":"answer"}],"created_at":"2026-01-01T00:00:01Z"}],"next_after":null}`)
	case r.Method == http.MethodGet && path == "/sessions/s1/events":
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flusher, _ := w.(http.Flusher)
		for _, frame := range f.events {
			_, _ = io.WriteString(w, frame)
			flusher.Flush()
		}
		<-r.Context().Done()
	case r.Method == http.MethodGet && path == "/permissions":
		if f.permissions {
			writeFakeJSON(w, http.StatusOK, `{"items":[{"id":"p1","session_id":"s1","run_id":"r1","tool":"bash","action":"execute","description":"run ls","path":"","created_at":"2026-01-01T00:00:00Z"}]}`)
		} else {
			writeFakeJSON(w, http.StatusOK, `{"items":[]}`)
		}
	case r.Method == http.MethodPost && path == "/permissions/p1":
		_ = body
		writeFakeJSON(w, http.StatusOK, `{}`)
	case r.Method == http.MethodPost && strings.HasPrefix(path, "/permissions/"):
		w.Header().Set("Content-Type", "application/problem+json")
		writeFakeJSON(w, http.StatusNotFound, `{"type":"about:blank","title":"permission not found","status":404}`)
	case r.Method == http.MethodPost && path == "/runs/r1/cancel":
		writeFakeJSON(w, http.StatusOK, `{"id":"r1","session_id":"s1","state":"cancelled","created_at":"2026-01-01T00:00:00Z"}`)
	case r.Method == http.MethodPost && path == "/sessions/s1/inject":
		writeFakeJSON(w, http.StatusAccepted, `{}`)
	default:
		writeFakeJSON(w, http.StatusNotFound, `{"type":"about:blank","title":"not found","status":404}`)
	}
}

func sseFrame(id int, event, data string) string {
	return fmt.Sprintf("id: %d\nevent: %s\ndata: %s\n\n", id, event, data)
}

// newRunFixture builds an isolated fixture whose default profile points at the fake.
func newRunFixture(t *testing.T, fake *fakeRunServer) *apiProcessFixture {
	t.Helper()
	f := newAPIProcessFixture(t)
	addFaultRemote(t, f, fake.URL)
	return f
}

func TestAPICLIRunPrintsFinalText(t *testing.T) {
	fake := newFakeRunServer(t, "queued", "running", "succeeded")
	f := newRunFixture(t, fake)
	stdout, stderr, exit := f.runWithEnv(apiFaultEnv, "", "api", "run", "s1", "hello world", "--role", "smart")
	if exit != 0 || stdout != "final answer\n" {
		t.Fatalf("exit %d stdout=%q stderr=%q", exit, stdout, stderr)
	}
	created := fake.seen(http.MethodPost, "/sessions/s1/runs")
	if len(created) != 1 {
		t.Fatalf("create calls = %d", len(created))
	}
	var request map[string]string
	if err := json.Unmarshal([]byte(created[0].Body), &request); err != nil || request["prompt"] != "hello world" || request["role"] != "smart" {
		t.Fatalf("create body = %q (%v)", created[0].Body, err)
	}
	f.assertNoSecrets(t, []string{apiFaultToken}, stdout, stderr)
}

func TestAPICLIRunFailedExitsOneWithoutPayload(t *testing.T) {
	fake := newFakeRunServer(t, "running", "failed")
	fake.runError = "provider exploded"
	f := newRunFixture(t, fake)
	stdout, stderr, exit := f.runWithEnv(apiFaultEnv, "", "api", "run", "s1", "x")
	if exit != 1 || stdout != "" || !strings.Contains(strings.ToLower(stderr), "provider exploded") {
		t.Fatalf("exit %d stdout=%q stderr=%q", exit, stdout, stderr)
	}
}

func TestAPICLIRunPausedForPermissionExitsSix(t *testing.T) {
	fake := newFakeRunServer(t, "running", "awaiting_answer")
	fake.permissions = true
	f := newRunFixture(t, fake)
	stdout, stderr, exit := f.runWithEnv(apiFaultEnv, "", "api", "run", "s1", "x")
	if exit != 6 || stdout != "" {
		t.Fatalf("exit %d stdout=%q stderr=%q", exit, stdout, stderr)
	}
	if !strings.Contains(stderr, "permission pending: p1") || !strings.Contains(stderr, "rush api permissions allow p1") {
		t.Fatalf("missing the resolve hint: %q", stderr)
	}
}

func TestAPICLIRunNoWaitPrintsRunIDOnly(t *testing.T) {
	fake := newFakeRunServer(t, "succeeded")
	f := newRunFixture(t, fake)
	stdout, stderr, exit := f.runWithEnv(apiFaultEnv, "", "api", "run", "s1", "x", "--no-wait")
	if exit != 0 || stdout != "r1\n" {
		t.Fatalf("exit %d stdout=%q stderr=%q", exit, stdout, stderr)
	}
	if got := fake.seen(http.MethodGet, "/runs/r1"); len(got) != 0 {
		t.Fatalf("--no-wait polled the run %d times", len(got))
	}
	stdout, _, exit = f.runWithEnv(apiFaultEnv, "", "api", "run", "s1", "x", "--no-wait", "--json")
	var accepted map[string]string
	if exit != 0 || json.Unmarshal([]byte(stdout), &accepted) != nil || accepted["run_id"] != "r1" {
		t.Fatalf("--json exit %d stdout=%q", exit, stdout)
	}
	if _, _, exit = f.runWithEnv(apiFaultEnv, "", "api", "run", "s1", "x", "--no-wait", "--stream"); exit != 2 {
		t.Fatalf("--no-wait with --stream exit = %d, want 2", exit)
	}
}

func TestAPICLIRunStreamEndsOnTerminalRunEvent(t *testing.T) {
	fake := newFakeRunServer(t, "running", "succeeded")
	fake.events = []string{
		sseFrame(1, "message", `{"n":1}`),
		sseFrame(2, "run", `{"state":"running"}`),
		sseFrame(3, "run", `{"state":"succeeded"}`),
	}
	f := newRunFixture(t, fake)
	stdout, stderr, exit := f.runWithEnv(apiFaultEnv, "", "api", "run", "s1", "x", "--stream")
	if exit != 0 {
		t.Fatalf("exit %d stdout=%q stderr=%q", exit, stdout, stderr)
	}
	if !strings.Contains(stdout, `{"n":1}`) || !strings.Contains(stdout, `"state":"succeeded"`) {
		t.Fatalf("stream output = %q", stdout)
	}
}

func TestAPICLIRunStreamFallsBackToPollingWhenNoRunEventArrives(t *testing.T) {
	fake := newFakeRunServer(t, "succeeded") // the stream stays silent and open
	f := newRunFixture(t, fake)
	start := time.Now()
	stdout, stderr, exit := f.runWithEnv(apiFaultEnv, "", "api", "run", "s1", "x", "--stream")
	if exit != 0 {
		t.Fatalf("exit %d stdout=%q stderr=%q (after %s)", exit, stdout, stderr, time.Since(start))
	}
}

func TestAPICLIRunStreamPausedExitsSix(t *testing.T) {
	fake := newFakeRunServer(t, "awaiting_answer")
	fake.permissions = true
	fake.events = []string{sseFrame(1, "run", `{"state":"awaiting_answer"}`)}
	f := newRunFixture(t, fake)
	_, stderr, exit := f.runWithEnv(apiFaultEnv, "", "api", "run", "s1", "x", "--stream")
	if exit != 6 || !strings.Contains(stderr, "rush api permissions allow p1") {
		t.Fatalf("exit %d stderr=%q", exit, stderr)
	}
}

func TestAPICLIRunTimeoutExitsFive(t *testing.T) {
	fake := newFakeRunServer(t, "running")
	f := newRunFixture(t, fake)
	_, stderr, exit := f.runWithEnv(apiFaultEnv, "", "api", "run", "s1", "x", "--timeout", "700ms")
	if exit != 5 {
		t.Fatalf("exit %d stderr=%q, want 5", exit, stderr)
	}
}

func TestAPICLICancelInjectAndRunsCommands(t *testing.T) {
	fake := newFakeRunServer(t, "running")
	f := newRunFixture(t, fake)
	for _, args := range [][]string{{"api", "cancel", "r1", "--json"}, {"api", "runs", "cancel", "r1", "--json"}} {
		stdout, stderr, exit := f.runWithEnv(apiFaultEnv, "", args...)
		var run map[string]any
		if exit != 0 || json.Unmarshal([]byte(stdout), &run) != nil || run["state"] != "cancelled" {
			t.Fatalf("%v: exit %d stdout=%q stderr=%q", args, exit, stdout, stderr)
		}
	}
	if got := fake.seen(http.MethodPost, "/runs/r1/cancel"); len(got) != 2 {
		t.Fatalf("cancel calls = %d, want 2", len(got))
	}
	stdout, _, exit := f.runWithEnv(apiFaultEnv, "", "api", "runs", "get", "r1", "--json")
	var run map[string]any
	if exit != 0 || json.Unmarshal([]byte(stdout), &run) != nil || run["id"] != "r1" {
		t.Fatalf("runs get exit %d stdout=%q", exit, stdout)
	}
	if _, _, exit = f.runWithEnv(apiFaultEnv, "", "api", "inject", "s1", "more context"); exit != 0 {
		t.Fatalf("inject exit %d", exit)
	}
	injected := fake.seen(http.MethodPost, "/sessions/s1/inject")
	if len(injected) != 1 || !strings.Contains(injected[0].Body, `"message":"more context"`) || !strings.Contains(injected[0].Body, `"mode":"merge"`) {
		t.Fatalf("inject calls = %+v", injected)
	}
}

func TestAPICLIPermissionsListAllowDeny(t *testing.T) {
	fake := newFakeRunServer(t)
	fake.permissions = true
	f := newRunFixture(t, fake)
	stdout, stderr, exit := f.runWithEnv(apiFaultEnv, "", "api", "permissions", "list")
	if exit != 0 || !strings.Contains(stdout, "p1") || !strings.Contains(stdout, "bash") {
		t.Fatalf("list exit %d stdout=%q stderr=%q", exit, stdout, stderr)
	}
	if _, _, exit = f.runWithEnv(apiFaultEnv, "", "api", "permissions", "allow", "p1"); exit != 0 {
		t.Fatalf("allow exit %d", exit)
	}
	if _, _, exit = f.runWithEnv(apiFaultEnv, "", "api", "permissions", "deny", "p1"); exit != 0 {
		t.Fatalf("deny exit %d", exit)
	}
	answers := fake.seen(http.MethodPost, "/permissions/p1")
	if len(answers) != 2 || !strings.Contains(answers[0].Body, `"decision":"allow"`) || !strings.Contains(answers[1].Body, `"decision":"deny"`) {
		t.Fatalf("answers = %+v", answers)
	}
	if _, _, exit = f.runWithEnv(apiFaultEnv, "", "api", "permissions", "allow", "p404"); exit != 4 {
		t.Fatalf("unknown permission exit = %d, want 4", exit)
	}
}

func TestAPICLIEventsReplayExitsAfterIdleAndResumesFromAfter(t *testing.T) {
	fake := newFakeRunServer(t)
	fake.events = []string{sseFrame(8, "message", `{"n":8}`), sseFrame(9, "message", `{"n":9}`)}
	f := newRunFixture(t, fake)
	stdout, stderr, exit := f.runWithEnv(apiFaultEnv, "", "api", "events", "s1", "--after", "7", "--idle", "400ms")
	if exit != 0 || !strings.Contains(stdout, `{"n":8}`) || !strings.Contains(stdout, `{"n":9}`) {
		t.Fatalf("exit %d stdout=%q stderr=%q", exit, stdout, stderr)
	}
	gets := fake.seen(http.MethodGet, "/sessions/s1/events")
	if len(gets) == 0 || gets[0].LastEventID != "7" {
		t.Fatalf("event requests = %+v", gets)
	}
	if _, _, exit = f.runWithEnv(apiFaultEnv, "", "api", "events", "s1", "--after", "not-a-number"); exit != 2 {
		t.Fatalf("bad --after exit = %d, want 2", exit)
	}
}
