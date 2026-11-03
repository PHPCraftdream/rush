package cmd

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/PHPCraftdream/rush/internal/apiclient"
	"github.com/spf13/cobra"
)

// fakeBatchServer speaks the real wire shapes of the batch routes
// (internal/server/api_batch_handlers.go); the client side of those shapes is
// checked against the production listener in internal/server.
type fakeBatchServer struct {
	*httptest.Server
	mu      sync.Mutex
	calls   []fakeBatchCall
	batches []string // consumed per GET /batches/r1; the last one repeats
}

type fakeBatchCall struct{ Method, Path, Body, Key string }

func newFakeBatchServer(t *testing.T, batches ...string) *fakeBatchServer {
	t.Helper()
	previous := apiclient.BatchPollInterval
	apiclient.BatchPollInterval = 10 * time.Millisecond
	t.Cleanup(func() { apiclient.BatchPollInterval = previous })
	f := &fakeBatchServer{batches: batches}
	f.Server = httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.Close)
	return f
}

func (f *fakeBatchServer) seen(method, pathSuffix string) []fakeBatchCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []fakeBatchCall
	for _, call := range f.calls {
		if call.Method == method && strings.HasSuffix(call.Path, pathSuffix) {
			out = append(out, call)
		}
	}
	return out
}

func (f *fakeBatchServer) nextBatch() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.batches) == 0 {
		return batchJSON("running", false)
	}
	body := f.batches[0]
	if len(f.batches) > 1 {
		f.batches = f.batches[1:]
	}
	return body
}

func (f *fakeBatchServer) serve(w http.ResponseWriter, r *http.Request) {
	raw, _ := io.ReadAll(r.Body)
	f.mu.Lock()
	f.calls = append(f.calls, fakeBatchCall{r.Method, r.URL.Path, string(raw), r.Header.Get("Idempotency-Key")})
	f.mu.Unlock()
	path := strings.TrimPrefix(r.URL.Path, "/api/v1")
	switch {
	case r.Method == http.MethodPost && (path == "/tasks" || path == "/batches"):
		writeFakeJSON(w, http.StatusAccepted, `{"root_id":"r1","state":"pending"}`)
	case r.Method == http.MethodGet && path == "/batches/r1":
		writeFakeJSON(w, http.StatusOK, f.nextBatch())
	case r.Method == http.MethodPost && strings.HasSuffix(path, "/stop"):
		writeFakeJSON(w, http.StatusAccepted, `{"request_id":"q1","state":"queued"}`)
	case r.Method == http.MethodPost && strings.HasSuffix(path, "/answer"):
		writeFakeJSON(w, http.StatusAccepted, `{"node_id":"r1.1","delivered":true}`)
	default:
		w.Header().Set("Content-Type", "application/problem+json")
		writeFakeJSON(w, http.StatusNotFound, `{"type":"about:blank","title":"Not Found","status":404,"detail":"batch not found"}`)
	}
}

func runBatchInProcess(t *testing.T, f *fakeBatchServer, args ...string) (int, string, string) {
	t.Helper()
	return runInProcess(t, &fakeRunServer{Server: f.Server}, args...)
}

func batchJSON(state string, done bool, nodes ...string) string {
	if len(nodes) == 0 {
		nodes = []string{taskNodeJSON("r1.1", 1, state, "")}
	}
	return fmt.Sprintf(`{"root_id":"r1","state":%q,"done":%t,"created_at":1,"updated_at":2,"nodes":[{"id":"r1","kind":"batch","ordinal":0,"state":%q},%s]}`, state, done, state, strings.Join(nodes, ","))
}

func taskNodeJSON(id string, ordinal int, state, extra string) string {
	if extra != "" {
		extra = "," + extra
	}
	return fmt.Sprintf(`{"id":%q,"parent_id":"r1","kind":"task","ordinal":%d,"state":%q,"tool":"agent","child_session_id":"child-%s"%s}`, id, ordinal, state, id, extra)
}

func TestAPICLITaskWaitsAndPrintsTheOutput(t *testing.T) {
	f := newFakeBatchServer(t, batchJSON("running", false), batchJSON("completed", true, taskNodeJSON("r1.1", 1, "completed", `"output":"the answer\n"`)))
	code, stdout, stderr := runBatchInProcess(t, f, "task", "--resume", "child-0", "--task-timeout", "90s", "say hi")
	if code != 0 || stdout != "the answer\n" || stderr != "" {
		t.Fatalf("code=%d stdout=%q stderr=%q", code, stdout, stderr)
	}
	posts := f.seen(http.MethodPost, "/tasks")
	if len(posts) != 1 {
		t.Fatalf("task submissions = %d", len(posts))
	}
	var sent apiclient.TaskRequest
	if err := json.Unmarshal([]byte(posts[0].Body), &sent); err != nil {
		t.Fatalf("task body %q: %v", posts[0].Body, err)
	}
	if sent != (apiclient.TaskRequest{Prompt: "say hi", ResumeSessionID: "child-0", TimeoutSeconds: 90}) {
		t.Fatalf("task request = %#v", sent)
	}
	if !strings.HasPrefix(posts[0].Key, "rushapi-") {
		t.Fatalf("a task submission carries a generated idempotency key, got %q", posts[0].Key)
	}
}

func TestAPICLITaskNoWaitPrintsOnlyTheRoot(t *testing.T) {
	f := newFakeBatchServer(t)
	code, stdout, stderr := runBatchInProcess(t, f, "task", "--no-wait", "go")
	if code != 0 || stdout != "r1\n" || stderr != "" {
		t.Fatalf("code=%d stdout=%q stderr=%q", code, stdout, stderr)
	}
	if got := f.seen(http.MethodGet, "/batches/r1"); len(got) != 0 {
		t.Fatalf("--no-wait read the batch %d times", len(got))
	}
}

func TestAPICLIBatchRunSubmitsTheSpecVerbatimAndLabelsMultipleNodes(t *testing.T) {
	spec := `{"batch":{"mode":"parallel","items":[{"task":{"tool":"agent","input":{"prompt":"a"}}}]}}`
	path := filepath.Join(t.TempDir(), "spec.json")
	if err := os.WriteFile(path, []byte(spec), 0o600); err != nil {
		t.Fatal(err)
	}
	two := batchJSON("completed", true, taskNodeJSON("r1.2", 2, "completed", `"output":"second"`), taskNodeJSON("r1.1", 1, "completed", `"output":"first"`))
	f := newFakeBatchServer(t, two)
	code, stdout, stderr := runBatchInProcess(t, f, "batch", "run", path)
	if code != 0 || stderr != "" {
		t.Fatalf("code=%d stderr=%q", code, stderr)
	}
	if want := "== r1.1 ==\nfirst\n== r1.2 ==\nsecond\n"; stdout != want {
		t.Fatalf("stdout = %q, want task nodes in execution order with headers %q", stdout, want)
	}
	posts := f.seen(http.MethodPost, "/batches")
	if len(posts) != 1 || posts[0].Body != spec {
		t.Fatalf("posts = %#v, want the spec sent as is", posts)
	}
}

func TestAPICLIBatchRunSpecErrors(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "absent.json")
	empty := filepath.Join(t.TempDir(), "empty.json")
	if err := os.WriteFile(empty, []byte("  \n"), 0o600); err != nil {
		t.Fatal(err)
	}
	notJSON := filepath.Join(t.TempDir(), "bad.json")
	if err := os.WriteFile(notJSON, []byte("{nope"), 0o600); err != nil {
		t.Fatal(err)
	}
	for name, tc := range map[string]struct{ path, want string }{
		"missing file": {missing, "open batch spec"},
		"empty file":   {empty, "batch spec is empty"},
		"not json":     {notJSON, "not valid JSON"},
	} {
		t.Run(name, func(t *testing.T) {
			f := newFakeBatchServer(t)
			code, _, stderr := runBatchInProcess(t, f, "batch", "run", tc.path)
			if code != 2 || !strings.Contains(stderr, tc.want) {
				t.Fatalf("code=%d stderr=%q, want exit 2 with %q", code, stderr, tc.want)
			}
			if len(f.seen(http.MethodPost, "/batches")) != 0 {
				t.Fatal("a refused spec must not be submitted")
			}
		})
	}
}

func TestReadAPIBatchSpecReadsStandardInput(t *testing.T) {
	cmd := &cobra.Command{}
	cmd.SetIn(strings.NewReader(`{"batch":{}}`))
	for _, args := range [][]string{nil, {"-"}} {
		cmd.SetIn(strings.NewReader(`{"batch":{}}`))
		spec, err := readAPIBatchSpec(cmd, args)
		if err != nil || string(spec) != `{"batch":{}}` {
			t.Fatalf("args=%v spec=%q err=%v", args, spec, err)
		}
	}
	cmd.SetIn(strings.NewReader(strings.Repeat("x", 1<<20+1)))
	if _, err := readAPIBatchSpec(cmd, nil); err == nil || !strings.Contains(err.Error(), "larger than 1 MiB") {
		t.Fatalf("an oversized spec must be refused, err=%v", err)
	}
}

func TestAPICLIHeldQuestionExitsSixWithTheAnswerCommand(t *testing.T) {
	held := batchJSON("running", false, taskNodeJSON("r1.1", 1, "running", `"awaiting_answer":true,"question":"Which port?"`))
	f := newFakeBatchServer(t, held)
	code, _, stderr := runBatchInProcess(t, f, "task", "ask me")
	if code != 6 {
		t.Fatalf("code=%d stderr=%q", code, stderr)
	}
	for _, want := range []string{"question pending on node r1.1: Which port?", `rush api batch answer r1 r1.1 "<text>"`} {
		if !strings.Contains(stderr, want) {
			t.Fatalf("stderr %q lacks %q", stderr, want)
		}
	}
}

func TestAPICLIFailedBatchExitsOneAndNamesTheNode(t *testing.T) {
	failed := batchJSON("failed", true, taskNodeJSON("r1.1", 1, "failed", `"outcome_code":"provider_error","outcome_summary":"model refused"`))
	f := newFakeBatchServer(t, failed)
	code, _, stderr := runBatchInProcess(t, f, "batch", "wait", "r1")
	if code != 1 {
		t.Fatalf("code=%d stderr=%q", code, stderr)
	}
	for _, want := range []string{"batch r1 failed", "node r1.1 failed provider_error model refused"} {
		if !strings.Contains(stderr, want) {
			t.Fatalf("stderr %q lacks %q", stderr, want)
		}
	}
}

func TestAPICLIInterruptStopsTheRemoteBatchAndExits130(t *testing.T) {
	f := newFakeBatchServer(t, batchJSON("running", false))
	interrupt := interruptHarness(t)
	go func() {
		waitForBatchCall(t, f, http.MethodGet, "/batches/r1")
		interrupt()
	}()
	code, _, stderr := runBatchInProcess(t, f, "task", "long job")
	if code != 130 {
		t.Fatalf("exit %d, want 130; stderr=%q", code, stderr)
	}
	if got := f.seen(http.MethodPost, "/batches/r1/stop"); len(got) != 1 {
		t.Fatalf("remote stop calls = %d, want 1", len(got))
	}
}

func waitForBatchCall(t *testing.T, f *fakeBatchServer, method, suffix string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if len(f.seen(method, suffix)) > 0 {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Errorf("fake never saw %s %s", method, suffix)
}

func TestAPICLIBatchWaitInterruptLeavesTheBatchRunning(t *testing.T) {
	f := newFakeBatchServer(t, batchJSON("running", false))
	interrupt := interruptHarness(t)
	go func() {
		waitForBatchCall(t, f, http.MethodGet, "/batches/r1")
		interrupt()
	}()
	code, _, stderr := runBatchInProcess(t, f, "batch", "wait", "r1")
	if code != 130 {
		t.Fatalf("exit %d, want 130; stderr=%q", code, stderr)
	}
	if got := f.seen(http.MethodPost, "/stop"); len(got) != 0 {
		t.Fatalf("wait must not stop a batch it did not submit, saw %d stop calls", len(got))
	}
}

func TestAPICLIBatchShowOutputStopAnswer(t *testing.T) {
	done := batchJSON("completed", true, taskNodeJSON("r1.1", 1, "completed", `"output":"out one"`), taskNodeJSON("r1.2", 2, "completed", `"output_expired":true,"outcome_code":"ok","outcome_summary":"done"`))
	f := newFakeBatchServer(t, done)

	code, stdout, stderr := runBatchInProcess(t, f, "batch", "show", "r1")
	if code != 0 || stderr != "" || !strings.Contains(stdout, "batch r1 completed") || !strings.Contains(stdout, "child-r1.1") {
		t.Fatalf("show code=%d stdout=%q stderr=%q", code, stdout, stderr)
	}
	code, stdout, _ = runBatchInProcess(t, f, "batch", "show", "r1", "--json")
	if code != 0 || !strings.Contains(stdout, `"root_id":"r1"`) {
		t.Fatalf("show --json code=%d stdout=%q", code, stdout)
	}

	code, stdout, stderr = runBatchInProcess(t, f, "batch", "output", "r1")
	if code != 0 || !strings.Contains(stdout, "== r1.1 ==\nout one\n") || !strings.Contains(stderr, "output of node r1.2 has expired") {
		t.Fatalf("output code=%d stdout=%q stderr=%q", code, stdout, stderr)
	}
	code, stdout, _ = runBatchInProcess(t, f, "batch", "output", "r1", "r1.1")
	if code != 0 || stdout != "out one\n" {
		t.Fatalf("single node output code=%d stdout=%q", code, stdout)
	}
	code, _, stderr = runBatchInProcess(t, f, "batch", "output", "r1", "r1.9")
	if code != 4 || !strings.Contains(stderr, "node r1.9 not found") {
		t.Fatalf("unknown node code=%d stderr=%q", code, stderr)
	}

	code, stdout, _ = runBatchInProcess(t, f, "batch", "stop", "r1")
	if code != 0 || stdout != "stop queued\n" || len(f.seen(http.MethodPost, "/batches/r1/stop")) != 1 {
		t.Fatalf("stop code=%d stdout=%q", code, stdout)
	}
	code, _, _ = runBatchInProcess(t, f, "batch", "stop", "r1", "r1.1")
	if code != 0 || len(f.seen(http.MethodPost, "/batches/r1/nodes/r1.1/stop")) != 1 {
		t.Fatalf("node stop code=%d", code)
	}
	code, stdout, _ = runBatchInProcess(t, f, "batch", "answer", "r1", "r1.1", "8080")
	answers := f.seen(http.MethodPost, "/batches/r1/nodes/r1.1/answer")
	if code != 0 || stdout != "answered r1.1\n" || len(answers) != 1 || answers[0].Body != `{"text":"8080"}` {
		t.Fatalf("answer code=%d stdout=%q answers=%#v", code, stdout, answers)
	}
}

func TestAPICLIBatchUnknownRootExitsFour(t *testing.T) {
	f := newFakeBatchServer(t)
	code, _, _ := runBatchInProcess(t, f, "batch", "show", "ghost")
	if code != 4 {
		t.Fatalf("exit %d, want 4 for an unknown root", code)
	}
}

func TestAPICLITaskRejectsANegativeTaskTimeout(t *testing.T) {
	f := newFakeBatchServer(t)
	code, _, stderr := runBatchInProcess(t, f, "task", "--task-timeout", "-1s", "x")
	if code != 2 || !strings.Contains(stderr, "must not be negative") {
		t.Fatalf("code=%d stderr=%q", code, stderr)
	}
	if len(f.seen(http.MethodPost, "/tasks")) != 0 {
		t.Fatal("a refused task must not be submitted")
	}
}
