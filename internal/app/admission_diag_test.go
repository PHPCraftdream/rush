package app

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"runtime/pprof"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/PHPCraftdream/rush/internal/config"
	"github.com/stretchr/testify/require"
)

// newAdmissionRaceApp backs 50-odd tests, and the batch ones among them have
// failed once each on a CI runner for reasons that left no trace ("coder tool
// configuration unavailable" after a config reload dropped the provider). The
// fixture now records every request its in-process model provider serves and,
// when the test fails, prints that timeline with the config, the batch roots
// and the stacks of the batch goroutines, taken before the App shuts down.

type admissionTrace struct {
	start time.Time

	mu    sync.Mutex
	lines []string
}

const admissionTraceCap = 200

func newAdmissionTrace() *admissionTrace { return &admissionTrace{start: time.Now()} }

func (tr *admissionTrace) add(format string, args ...any) {
	line := fmt.Sprintf("+%7.3fs %s", time.Since(tr.start).Seconds(), fmt.Sprintf(format, args...))
	tr.mu.Lock()
	defer tr.mu.Unlock()
	if len(tr.lines) >= admissionTraceCap {
		tr.lines = append(tr.lines[:0], tr.lines[len(tr.lines)-admissionTraceCap/2:]...)
	}
	tr.lines = append(tr.lines, line)
}

func (tr *admissionTrace) String() string {
	tr.mu.Lock()
	defer tr.mu.Unlock()
	return strings.Join(tr.lines, "\n")
}

func admissionShort(s string, limit int) string {
	s = strings.ReplaceAll(s, "\n", " ")
	if len(s) > limit {
		return s[:limit] + "..."
	}
	return s
}

// wrap records each provider request (which agent's system prompt it carries
// and the tail of the last user turn) and then serves it through inner.
func (tr *admissionTrace) wrap(inner http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		r.Body = io.NopCloser(bytes.NewReader(body))
		system, lastUser, lastTool := lastTurnParts(body)
		tr.add("provider %s %s bytes=%d system=%q lastUser=%q lastTool=%q", r.Method, r.URL.Path, len(body),
			admissionShort(system, 40), admissionShort(lastUser, 60), admissionShort(lastTool, 40))
		inner(w, r)
	}
}

// describeAdmissionApp renders what the test could not see: the timeline, the
// provider configuration the live store holds now, the coordinator, and every
// batch root the owner holds with each node's state and outcome.
func describeAdmissionApp(application *App, store *config.ConfigStore, ownerID string, tr *admissionTrace) string {
	var b strings.Builder
	fmt.Fprintf(&b, "=== admission app state at +%.3fs ===\n", time.Since(tr.start).Seconds())

	cfg := store.Config()
	var providers []string
	if cfg.Providers != nil {
		for name := range cfg.Providers.Seq2() {
			providers = append(providers, name)
		}
	}
	sort.Strings(providers)
	fmt.Fprintf(&b, "config: IsConfigured=%v providers=%v coordinatorPresent=%v\n", cfg.IsConfigured(), providers, application.AgentCoordinator != nil)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	fac, err := application.BatchCLIStore()
	if err != nil {
		fmt.Fprintf(&b, "batch store unavailable: %v\n", err)
	} else if roots, rootsErr := fac.Store.BatchAPIRoots(ctx, ownerID); rootsErr != nil {
		fmt.Fprintf(&b, "batch roots read failed: %v\n", rootsErr)
	} else {
		fmt.Fprintf(&b, "batch roots of owner %s: %d\n", ownerID, len(roots))
		for _, root := range roots {
			fmt.Fprintf(&b, "  root %s state=%s\n", root.Root, root.State)
			snap, readErr := fac.Store.Read(ctx, ownerID, root.Root)
			if readErr != nil {
				fmt.Fprintf(&b, "    read failed: %v\n", readErr)
				continue
			}
			for _, n := range snap.Nodes {
				fmt.Fprintf(&b, "    node %s kind=%s state=%s cause=%q outcome=%s/%s %q child=%s\n",
					n.NodeID, n.Kind, n.State, n.Cause, n.OutcomeState, n.OutcomeCode, admissionShort(n.OutcomeSummary, 160), n.ChildSessionID.String)
			}
		}
	}

	b.WriteString("--- provider timeline ---\n")
	b.WriteString(tr.String())

	var buf bytes.Buffer
	_ = pprof.Lookup("goroutine").WriteTo(&buf, 2)
	var keep []string
	for _, g := range strings.Split(buf.String(), "\n\n") {
		if strings.Contains(g, "internal/agent.(*BatchCLI") || strings.Contains(g, "internal/jobbatch") || strings.Contains(g, "buildPolicyTools") {
			keep = append(keep, g)
		}
	}
	dump := strings.Join(keep, "\n\n")
	if len(dump) > 12000 {
		dump = dump[:12000] + "\n...(truncated)"
	}
	fmt.Fprintf(&b, "\n--- batch goroutines: %d ---\n%s\n", len(keep), dump)
	return b.String()
}

func logAdmissionAppOnFailure(t *testing.T, application *App, store *config.ConfigStore, ownerID string, tr *admissionTrace) {
	t.Helper()
	t.Cleanup(func() {
		if t.Failed() {
			t.Log(describeAdmissionApp(application, store, ownerID, tr))
		}
	})
}

func TestAdmissionAppDiagnosticsShowTimelineConfigAndRoots(t *testing.T) {
	var served int
	var mu sync.Mutex
	application, ownerID := newAdmissionRaceApp(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		mu.Lock()
		served++
		mu.Unlock()
		admissionWriteSSE(w, []string{admissionSSEText("x", "ok"), admissionSSEStop("x", "stop")})
	})
	require.NotNil(t, application)

	tr := newAdmissionTrace()
	srvHandler := tr.wrap(func(w http.ResponseWriter, r *http.Request) {
		admissionWriteSSE(w, []string{admissionSSEText("y", "ok"), admissionSSEStop("y", "stop")})
	})
	rec := &responseSink{header: http.Header{}}
	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, "/chat/completions", strings.NewReader(`{"messages":[{"role":"user","content":"hello there"}]}`))
	require.NoError(t, err)
	srvHandler(rec, req)

	report := describeAdmissionApp(application, application.config, ownerID, tr)
	require.Contains(t, report, "admission app state at")
	require.Contains(t, report, "config: IsConfigured=true providers=")
	require.Contains(t, report, "openaicompat")
	require.Contains(t, report, "batch roots of owner "+ownerID+": 0")
	require.Contains(t, report, "provider POST /chat/completions")
	require.Contains(t, report, "batch goroutines:")
}

// responseSink is a minimal ResponseWriter for driving a handler directly.
type responseSink struct {
	header http.Header
	body   bytes.Buffer
}

func (r *responseSink) Header() http.Header         { return r.header }
func (r *responseSink) Write(p []byte) (int, error) { return r.body.Write(p) }
func (r *responseSink) WriteHeader(int)             {}
