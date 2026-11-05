package server

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"runtime/pprof"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/PHPCraftdream/rush/internal/config"
	"github.com/stretchr/testify/require"
)

// The batch tests have failed once each on CI runners and never again locally
// (a 202 where a 429 was expected, a root that finished before the client saw
// it, a question that never reached awaiting_answer). A bare assertion message
// cannot say which side was slow, so the fixture records what happened and
// prints it when a test fails: a timeline of every HTTP exchange and provider
// request, the service's reservations, the durable state of every root the
// test's tokens hold, and the stacks of the goroutines inside the batch code.

// batchTrace is a timestamped, bounded event log shared by the fixture's
// HTTP client and its in-process model provider.
type batchTrace struct {
	start time.Time

	mu    sync.Mutex
	lines []string

	providerStarted atomic.Int64
	providerDone    atomic.Int64
}

const batchTraceCap = 300

func newBatchTrace() *batchTrace { return &batchTrace{start: time.Now()} }

func (tr *batchTrace) add(format string, args ...any) {
	line := fmt.Sprintf("+%7.3fs %s", time.Since(tr.start).Seconds(), fmt.Sprintf(format, args...))
	tr.mu.Lock()
	defer tr.mu.Unlock()
	if len(tr.lines) >= batchTraceCap {
		tr.lines = append(tr.lines[:0], tr.lines[len(tr.lines)-batchTraceCap/2:]...)
	}
	tr.lines = append(tr.lines, line)
}

func (tr *batchTrace) String() string {
	tr.mu.Lock()
	defer tr.mu.Unlock()
	return strings.Join(tr.lines, "\n")
}

// traceProvider wraps the fixture's model provider so every request it serves
// is on the timeline, with how many are in flight.
func (tr *batchTrace) traceProvider(inner http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		started := tr.providerStarted.Add(1)
		tr.add("provider request #%d %s %s (in flight %d)", started, r.Method, r.URL.Path, started-tr.providerDone.Load())
		defer func() {
			done := tr.providerDone.Add(1)
			tr.add("provider request #%d finished (done %d of %d started)", started, done, tr.providerStarted.Load())
		}()
		inner(w, r)
	}
}

func truncateForTrace(raw []byte, limit int) string {
	s := strings.ReplaceAll(string(raw), "\n", " ")
	if len(s) > limit {
		return s[:limit] + "..."
	}
	return s
}

// goroutinesMatching returns the stacks of goroutines containing any key,
// capped for a readable CI log.
func goroutinesMatching(limit int, keys ...string) string {
	var buf bytes.Buffer
	_ = pprof.Lookup("goroutine").WriteTo(&buf, 2)
	var keep []string
	for _, g := range strings.Split(buf.String(), "\n\n") {
		for _, k := range keys {
			if strings.Contains(g, k) {
				keep = append(keep, g)
				break
			}
		}
	}
	out := fmt.Sprintf("goroutines matching %v: %d\n%s", keys, len(keep), strings.Join(keep, "\n\n"))
	if len(out) > limit {
		out = out[:limit] + "\n...(truncated)"
	}
	return out
}

// ownerOf maps an API token to its hidden service session, the owner the
// batch store files that token's roots under.
func (f *batchFixture) ownerOf(token string) (string, error) {
	id, _, ok := strings.Cut(strings.TrimPrefix(token, "rush_"), ".")
	if !ok || id == "" {
		return "", fmt.Errorf("token %q has no id part", truncateForTrace([]byte(token), 12))
	}
	return serviceSessionID(id), nil
}

// fetchBatch reads a root over the listener without failing the test, so the
// failure report itself can never abort.
func (f *batchFixture) fetchBatch(token, root string) (apiBatch, string) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, f.base+"/api/v1/batches/"+root, nil)
	if err != nil {
		return apiBatch{}, err.Error()
	}
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return apiBatch{}, err.Error()
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	var out apiBatch
	if resp.StatusCode != http.StatusOK || json.Unmarshal(raw, &out) != nil {
		return apiBatch{}, fmt.Sprintf("status %d: %s", resp.StatusCode, truncateForTrace(raw, 200))
	}
	return out, ""
}

// diagnostics renders the whole picture of the fixture at this instant.
func (f *batchFixture) diagnostics() string {
	var b strings.Builder
	fmt.Fprintf(&b, "=== batch fixture state at +%.3fs ===\n", time.Since(f.trace.start).Seconds())
	fmt.Fprintf(&b, "provider requests: started=%d finished=%d\n", f.trace.providerStarted.Load(), f.trace.providerDone.Load())

	f.service.mu.Lock()
	watching := make(map[string]int, len(f.service.watching))
	for k, v := range f.service.watching {
		watching[k] = v
	}
	answered := len(f.service.answered)
	f.service.mu.Unlock()
	roots := make([]string, 0, len(watching))
	for k := range watching {
		roots = append(roots, k)
	}
	sort.Strings(roots)
	fmt.Fprintf(&b, "service: watching=%d %v (root -> reserved parallel), answered questions=%d\n", len(watching), roots, answered)

	f.tokensMu.Lock()
	tokens := make(map[string]string, len(f.tokens))
	for k, v := range f.tokens {
		tokens[k] = v
	}
	f.tokensMu.Unlock()
	names := make([]string, 0, len(tokens))
	for n := range tokens {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, name := range names {
		f.describeTokenRoots(&b, name, tokens[name])
	}

	b.WriteString("--- timeline (HTTP exchanges and provider requests) ---\n")
	b.WriteString(f.trace.String())
	b.WriteString("\n--- ")
	b.WriteString(goroutinesMatching(16000, "apiBatchService", "internal/agent.(*BatchCLI", "internal/jobbatch", "RunQueuePump"))
	b.WriteString("\n")
	return b.String()
}

// describeTokenRoots lists the durable roots a token's service session holds,
// with every node's state, cause and outcome.
func (f *batchFixture) describeTokenRoots(b *strings.Builder, name, token string) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	owner, err := f.ownerOf(token)
	if err != nil {
		fmt.Fprintf(b, "token %q: owner unknown: %v\n", name, err)
		return
	}
	fac, err := f.app.BatchCLIStore()
	if err != nil {
		fmt.Fprintf(b, "token %q: batch store unavailable: %v\n", name, err)
		return
	}
	held, err := fac.Store.BatchAPIRoots(ctx, owner)
	if err != nil {
		fmt.Fprintf(b, "token %q: roots read failed: %v\n", name, err)
		return
	}
	fmt.Fprintf(b, "token %q (owner %s): %d root(s)\n", name, owner, len(held))
	for _, root := range held {
		fmt.Fprintf(b, "  root %s state=%s\n", root.Root, root.State)
		view, problem := f.fetchBatch(token, root.Root)
		if problem != "" {
			fmt.Fprintf(b, "    read over the listener failed: %s\n", problem)
			continue
		}
		fmt.Fprintf(b, "    listener view: state=%s done=%v nodes=%d\n", view.State, view.Done, len(view.Nodes))
		for _, n := range view.Nodes {
			fmt.Fprintf(b, "    node %s kind=%s tool=%s state=%s cause=%q outcome=%s/%s %q awaiting=%v child=%s\n",
				n.ID, n.Kind, n.Tool, n.State, n.Cause, n.OutcomeState, n.OutcomeCode, truncateForTrace([]byte(n.OutcomeSummary), 160), n.AwaitingAnswer, n.ChildSessionID)
		}
	}
}

// The report is the only evidence a one-off CI failure leaves, so pin what it
// carries: the timeline of HTTP exchanges and provider requests, and the
// durable state of the root the test's token holds.
func TestBatchFixtureDiagnosticsCarryTimelineAndRootState(t *testing.T) {
	f := newBatchFixture(t, batchTextProvider("diag"))
	token := f.token(t, "diag-token", []string{"read", "run"}, "ask", config.APITokenBinding{})
	accepted := f.do(t, token, http.MethodPost, "/tasks", map[string]any{"prompt": "p"}).accepted(t)
	f.waitBatchDone(t, token, accepted.RootID)

	report := f.diagnostics()
	require.Contains(t, report, "provider requests: started=")
	require.Contains(t, report, "provider request #1 POST")
	require.Contains(t, report, "provider request #1 finished")
	require.Contains(t, report, "HTTP POST /tasks -> 202")
	require.Contains(t, report, "HTTP GET /batches/"+accepted.RootID+" -> 200")
	require.Contains(t, report, `token "diag-token"`)
	require.Contains(t, report, "root "+accepted.RootID+" state=")
	require.Contains(t, report, "listener view: state=completed done=true")
	require.Contains(t, report, "node ")
	require.Contains(t, report, "service: watching=")
	require.Contains(t, report, "goroutines matching")
}

func TestBatchTraceKeepsTheNewestEventsWhenFull(t *testing.T) {
	t.Parallel()

	tr := newBatchTrace()
	for i := 0; i < batchTraceCap*2; i++ {
		tr.add("event %d", i)
	}
	out := tr.String()
	require.Contains(t, out, fmt.Sprintf("event %d", batchTraceCap*2-1), "the newest event must survive")
	require.NotContains(t, out, "event 0\n", "the oldest events are dropped")
	require.LessOrEqual(t, strings.Count(out, "\n")+1, batchTraceCap)
}
