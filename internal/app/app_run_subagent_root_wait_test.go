package app

// The parent-process half of the sub-agent delegation fix: `rush run`'s root
// loop must NOT exit while a delegated sub-agent (or anything that sub-agent
// owns) is still working, and must receive a model turn after the delegation
// result before it is allowed to finish.
//
// Before the park registry (internal/agent/subagent_outcome.go), the root
// loop was released as soon as the CHILD's Run() returned — which is only the
// end of one model turn. A child that started its own background command and
// yielded released the root immediately, so the outer process could emit its
// final JSON envelope and fire the Codex completion callback while the child's
// command was still running, and the child's later result never reached the
// parent at all.
//
// The barrier in this test makes that ordering observable rather than
// timing-dependent: the root's final turn REFUSES to run until the child's
// own owned background job has reached a terminal state. If the root ever
// resumes first (the pre-fix behavior), the model handler fails the request
// and the test fails.
//
// ---------------------------------------------------------------------------
// HARNESS CORRECTNESS NOTES (why the stub below looks the way it does)
// ---------------------------------------------------------------------------
// Every failure mode this test used to be flaky/false-negative on traced back
// to the stub matching whole-request-body substrings. A request body carries
// the session's ENTIRE history, so:
//
//   - "WORKER-MARKER" appears in every ROOT request after turn 1 (it is an
//     argument of the delegation tool call the root itself made), so the root's
//     own post-delegation turn was misrouted into the child branch and started
//     background jobs of its own.
//   - "Background shell started with ID: …" NEVER appears: the bash tool is
//     dispatch-wrapped as async for CLI origin, so the model sees
//     "Async bash job call-bash started. Its result will arrive as a new
//     session message" instead of the inner tool's text.
//   - the title-generation request (the child session has no title yet, and
//     its prompt embeds the delegation prompt) matched the child branch too,
//     producing a tool call a tool-less title agent answered with
//     "tool not found: bash" and a fast/smart retry storm.
//
// So this stub parses the request body and routes on WHO is asking (system
// prompt) and WHAT the newest turn contains (last user message, last tool
// result) instead of on anything that could survive from an earlier turn.

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"runtime"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/PHPCraftdream/rush/internal/config"
	"github.com/PHPCraftdream/rush/internal/message"
	"github.com/stretchr/testify/require"
)

func TestRunNonInteractiveRootWaitsForChildOwnedBackgroundJob(t *testing.T) {
	// The batch runtime is unconditional: the retained scenario is the
	// batch twin (batch_stop on the child's own escaped root id); the
	// legacy job_kill(shell_id) transcript this test used to carry is
	// deleted with the removed tool.
	testRunNonInteractiveRootWaitsForChildOwnedBatchJob(t)
}

func countLabel(order []string, label string) int {
	n := 0
	for _, l := range order {
		if l == label {
			n++
		}
	}
	return n
}

func indexOf(order []string, label string) int {
	for i, l := range order {
		if l == label {
			return i
		}
	}
	return len(order) + 1
}

// grantChildBackgroundShellTools layers bash/run_command/batch_stop onto the
// delegated sub-agent's toolset.
//
// The default Task-agent toolset is read-only (internal/config's
// resolveReadOnlyTools: git_read, glob, grep, ls, sourcegraph, view, fs_*),
// so without this the child answers "tool not found: bash" for its own
// background command — it would never OWN a background shell, and the whole
// park/release contract under test (which is about the child's owned async
// work) could not run at all. This is harness configuration, not a weakening
// of the barrier: the gate is still what decides when the root is resumed.
func grantChildBackgroundShellTools(t *testing.T, application *App) {
	t.Helper()
	cfg := application.config.Config()
	task, ok := cfg.Agents[config.AgentTask]
	require.True(t, ok, "the delegated task agent must be configured")
	tools := slices.Clone(task.AllowedTools)
	// bash/run_command make the child OWN batch work; batch_stop lets it
	// stop its own escaped root. The removed job_kill/job_output tools are
	// not granted: they no longer exist in the batch toolset.
	for _, name := range []string{"bash", "run_command", "batch_stop"} {
		if !slices.Contains(tools, name) {
			tools = append(tools, name)
		}
	}
	application.config.UpdateAgentAllowedTools(config.AgentTask, tools)
}

// longRunningCommand returns a command that outlives the bash tool's 1s
// fast-failure probe, so the child's call registers a LIVE background shell
// instead of completing inline (a fast command is removed by that probe and
// leaves the child owning nothing).
func longRunningCommand() string {
	if runtime.GOOS == "windows" {
		return "ping -n 60 127.0.0.1"
	}
	return "sleep 60"
}

func jsonString(s string) string {
	b, err := json.Marshal(s)
	if err != nil {
		return `""`
	}
	return string(b)
}

// wireRequest / wireMessage mirror the JSON shapes the provider stub sees.
type wireMessage struct {
	Role       string `json:"role"`
	Content    any    `json:"content"`
	ToolCallID string `json:"tool_call_id"`
}

type wireRequest struct {
	Messages []wireMessage `json:"messages"`
}

// routeModelRequest decides which turn a provider request belongs to, from
// the request body alone. It keys on WHO is asking (the system prompt) and on
// the NEWEST content (last user message, last tool result), never on a
// substring that could have survived from an earlier turn of the same
// session — that whole-body matching is exactly what misrouted the root's own
// continuation turns into the child branch (and the title requests into the
// child branch with them).
func routeModelRequest(body []byte) string {
	// A fresh stateless router carries no cross-request state.
	route, _ := newRBRouter().route(body)
	return route
}

type safeRequestOrder struct {
	mu     sync.Mutex
	labels []string
}

func (o *safeRequestOrder) add(label string) {
	o.mu.Lock()
	o.labels = append(o.labels, label)
	o.mu.Unlock()
}

func (o *safeRequestOrder) snapshot() []string {
	o.mu.Lock()
	defer o.mu.Unlock()
	out := make([]string, len(o.labels))
	copy(out, o.labels)
	return out
}

// firstLine returns the first 160 chars of s, collapsed onto one line, so a
// diagnostic log stays readable for a JSON request body.
func firstLine(s string) string {
	s = strings.ReplaceAll(s, "\n", " ")
	s = strings.ReplaceAll(s, "\r", " ")
	if len(s) > 160 {
		return s[:160] + "..."
	}
	return s
}

// syncBuffer is a mutex-free io.Writer good enough for a single-goroutine
// model handler plus the run loop's own writes.
type syncBuffer struct {
	strings.Builder
	mu chan struct{}
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	if b.mu == nil {
		b.mu = make(chan struct{}, 1)
	}
	b.mu <- struct{}{}
	defer func() { <-b.mu }()
	return b.Builder.Write(p)
}

func testRunNonInteractiveRootWaitsForChildOwnedBatchJob(t *testing.T) {
	command, release := recoveryJobCommand(t)
	defer releaseRecoveryJob(t, release)
	var rootID atomic.Value
	rootID.Store("")
	var delegateRoot atomic.Value
	delegateRoot.Store("")
	var childID atomic.Value
	childID.Store("")
	var order safeRequestOrder
	var stopIntentSeen atomic.Bool
	var childNoticeCount, parentFinalCount, startedCount atomic.Int32
	var liveApp atomic.Pointer[App]
	var liveSession atomic.Value
	router := newRBRouter()
	application, sessionID := newAdmissionRaceApp(t, func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		system, lastUser, lastTool := lastTurnParts(body)
		route, seqErr := router.route(body)
		if seqErr != "" {
			http.Error(w, "out-of-sequence batch request: "+seqErr, http.StatusBadRequest)
			return
		}
		switch route {
		case "title":
			admissionWriteSSE(w, []string{admissionSSEText("title", "Root Wait Child"), admissionSSEStop("title", "stop")})
		case "root:delegate":
			order.add("root:delegate")
			admissionWriteSSE(w, []string{admissionSSEToolCall("delegate", "call-agent", "agent", `{"prompt":"WORKER-MARKER do the task"}`), admissionSSEStop("delegate", "tool_calls")})
		case "child:bash":
			order.add("child:bash")
			var id string
			err := liveApp.Load().DB().QueryRowContext(r.Context(), `
SELECT t.child_session_id
FROM batch_nodes r
JOIN batch_nodes t
  ON t.owner_session_id = r.owner_session_id
 AND t.root_id = r.root_id AND t.host_id = r.host_id
 AND t.kind = 'task'
WHERE r.owner_session_id = ? AND r.tool_call_id = 'call-agent'
  AND r.node_id = r.root_id AND r.kind = 'batch'
ORDER BY t.ordinal LIMIT 1`, liveSession.Load().(string)).Scan(&id)
			if err != nil {
				http.Error(w, "delegation child lookup failed: "+err.Error(), http.StatusServiceUnavailable)
				return
			}
			if id == "" {
				http.Error(w, "delegation child lookup returned an empty child id", http.StatusServiceUnavailable)
				return
			}
			childID.Store(id)
			admissionWriteSSE(w, []string{admissionSSEToolCall("child-bash", "call-bash", "bash", `{"command":`+jsonString(command)+`,"description":"held child command","run_in_background":true}`), admissionSSEStop("child-bash", "tool_calls")})
		case "child:started":
			startedCount.Add(1)
			id := batchStartedRootID(lastTool)
			if id == "" {
				t.Errorf("child started tool result has no batch root id: %q", lastTool)
				http.Error(w, "no batch root id", http.StatusServiceUnavailable)
				return
			}
			rootID.Store(id)
			input, err := json.Marshal(map[string]string{"root_id": id})
			if err != nil {
				t.Errorf("marshal batch_stop input: %v", err)
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
			admissionWriteSSE(w, []string{admissionSSEToolCall("stop", "call-stop", "batch_stop", string(input)), admissionSSEStop("stop", "tool_calls")})
		case "child:stop":
			if !strings.Contains(lastTool, `"accepted":true`) || !strings.Contains(lastTool, `"terminal":false`) {
				http.Error(w, "batch_stop must expose cancellation intent, not terminal completion: "+lastTool, http.StatusServiceUnavailable)
				return
			}
			stopIntentSeen.Store(true)
			order.add("child:stop")
			if err := releaseRecoveryJobAsync(release); err != nil {
				t.Errorf("release the recovery job: %v", err)
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
			admissionWriteSSE(w, []string{admissionSSEText("stop-yield", "stop intent acknowledged; command released"), admissionSSEStop("stop-yield", "stop")})
		case "child:stopAck":
			// The stop-ack tool result stays in history; the router replays
			// repeats here so the child can yield until its completion is
			// pulled.
			admissionWriteSSE(w, []string{admissionSSEText("stop-ack", "stop intent already recorded"), admissionSSEStop("stop-ack", "stop")})
		case "child:final":
			// Same-turn deliveries carry the stop ack in history, so the stop
			// intent is provable without a dedicated ack turn.
			var histReq wireRequest
			if json.Unmarshal(body, &histReq) == nil {
				for _, m := range histReq.Messages {
					if m.Role != "tool" {
						continue
					}
					if s, ok := m.Content.(string); ok && strings.Contains(s, `"accepted":true`) && strings.Contains(s, `"terminal":false`) {
						stopIntentSeen.Store(true)
					}
				}
			}
			childNoticeCount.Add(1)
			var state string
			err := liveApp.Load().DB().QueryRowContext(r.Context(), `SELECT state FROM batch_nodes WHERE owner_session_id = ? AND root_id = ? AND task_kind = 'shell'`, childID.Load().(string), rootID.Load().(string)).Scan(&state)
			if err != nil || state != "cancelled" {
				http.Error(w, fmt.Sprintf("selected child notice before terminal state: %s (%v)", state, err), http.StatusServiceUnavailable)
				return
			}
			order.add("child:final")
			admissionWriteSSE(w, []string{admissionSSEText("child-final", "child final: terminal command result received"), admissionSSEStop("child-final", "stop")})
		case "root:yield":
			id := batchStartedRootID(lastTool)
			if id == "" {
				t.Errorf("root started notice has no batch root id: %q", lastTool)
				http.Error(w, "no batch root id", http.StatusServiceUnavailable)
				return
			}
			delegateRoot.Store(id)
			admissionWriteSSE(w, []string{admissionSSEText("root-yield", "waiting for child completion"), admissionSSEStop("root-yield", "stop")})
		case "root:final":
			parentFinalCount.Add(1)
			var state string
			err := liveApp.Load().DB().QueryRowContext(r.Context(), `SELECT state FROM batch_nodes WHERE owner_session_id = ? AND root_id = ? AND task_kind = 'shell'`, childID.Load().(string), rootID.Load().(string)).Scan(&state)
			if err != nil || state != "cancelled" || childNoticeCount.Load() != 1 {
				http.Error(w, fmt.Sprintf("parent completion before child terminal notice: %s notices=%d err=%v", state, childNoticeCount.Load(), err), http.StatusServiceUnavailable)
				return
			}
			order.add("root:final")
			admissionWriteSSE(w, []string{admissionSSEText("root-final", "root final answer"), admissionSSEStop("root-final", "stop")})
		case "child:wait":
			// A replayed started announcement (also persisted as user history):
			// the child just yields until its completion is pulled.
			order.add("child:wait")
			admissionWriteSSE(w, []string{admissionSSEText("child-wait", "child waiting for build"), admissionSSEStop("child-wait", "stop")})
		case "root:wait":
			// A replayed started announcement on the root side: yield until the
			// delegation completion arrives.
			order.add("root:wait")
			admissionWriteSSE(w, []string{admissionSSEText("root-wait", "waiting for child completion"), admissionSSEStop("root-wait", "stop")})
		default:
			http.Error(w, "unexpected batch root-wait route="+route+" system="+system+" lastUser="+lastUser+" lastTool="+lastTool, http.StatusBadRequest)
		}
	})
	liveApp.Store(application)
	liveSession.Store(sessionID)
	grantChildBackgroundShellTools(t, application)
	ctx, cancel := context.WithTimeout(t.Context(), 60*time.Second)
	defer cancel()
	var output syncBuffer
	result, err := application.RunNonInteractiveWithResult(ctx, &output, "start a worker", RunOverrides{Origin: message.OriginCLI}, true, RunModeJSON, sessionID, false)
	require.NoError(t, err)
	require.NotNil(t, result)
	seen := order.snapshot()
	require.Equal(t, 1, countLabel(seen, "root:delegate"))
	require.Equal(t, 1, countLabel(seen, "child:bash"))
	require.Equal(t, 1, countLabel(seen, "child:final"))
	require.Equal(t, 1, countLabel(seen, "root:final"))
	require.Less(t, indexOf(seen, "child:bash"), indexOf(seen, "child:final"))
	require.Less(t, indexOf(seen, "child:final"), indexOf(seen, "root:final"))
	require.EqualValues(t, 1, childNoticeCount.Load())
	require.EqualValues(t, 1, parentFinalCount.Load())
	require.EqualValues(t, 1, startedCount.Load(), "one started identity is consumed")
	require.True(t, stopIntentSeen.Load(), "one durable stop request is issued (dedicated ack turn or same-turn delivery)")
	require.Equal(t, "root final answer", result.FinalText)
	require.Equal(t, 1, strings.Count(output.String(), `"session_id"`))
	require.NotEmpty(t, childID.Load().(string))
}

func batchStartedRootID(text string) string {
	fields := strings.Fields(text)
	if len(fields) < 4 || fields[0] != "batch" || fields[1] != "root" || fields[3] != "started;" {
		return ""
	}
	return fields[2]
}

// releaseRecoveryJobAsync writes the recovery-job release sentinel without
// touching testing state, so a helper goroutine may call it: require (and
// t.Errorf after completion) panics when reached on a goroutine.
func releaseRecoveryJobAsync(release string) error {
	return os.WriteFile(release, nil, 0o644)
}
