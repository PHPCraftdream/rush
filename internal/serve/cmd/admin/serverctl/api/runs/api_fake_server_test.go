package apiruns_test

import (
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
	t.Cleanup(f.Close)
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
