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
	"io"
	"net/http"
	"runtime"
	"runtime/pprof"
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
	// childJobKilled is closed by the child's job_kill tool call, i.e. the
	// moment the child's OWN background job becomes terminal. The root's
	// final turn is gated on it.
	childJobKilled := make(chan struct{})
	// closeGate closes childJobKilled exactly once: both post-kill child
	// branches below prove the job terminal (see the merged-Drain note on
	// child:final), and which of them serves the child's last turn is a
	// scheduling race, not a contract.
	var closeGate sync.Once
	var childTurns atomic.Int32
	var requests atomic.Int32
	// requestOrder records the phase label of every served model request, in
	// order, so the full root-finishes-last contract can be asserted on the
	// sequence rather than on counters.
	var requestOrder safeRequestOrder
	var rootFinalTurn atomic.Int32

	application, sessionID := newAdmissionRaceApp(t, func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		n := requests.Add(1)
		t.Logf("model request %d: %q", n, firstLine(string(body)))
		route := routeModelRequest(body)
		switch route {
		case "title":
			// Title generation (system prompt is the title prompt). A plain
			// short title — no tool call — so the fast model succeeds on its
			// first attempt instead of failing into a retry storm.
			admissionWriteSSE(w, []string{
				admissionSSEText("title", "Root Wait Child"), admissionSSEStop("title", "stop"),
			})
		case "root:final":
			// The root's post-delegation turn. It must not start until the
			// child's owned job is terminal.
			select {
			case <-childJobKilled:
			case <-time.After(15 * time.Second):
				http.Error(w,
					"root resumed before the delegated child's owned background job was terminal",
					http.StatusServiceUnavailable)
				return
			}
			rootFinalTurn.Add(1)
			requestOrder.add("root:final")
			admissionWriteSSE(w, []string{
				admissionSSEText("root-final", "root final answer"), admissionSSEStop("root-final", "stop"),
			})
		case "child:final":
			// The child's turn that carries the pulled bg-shell-done notice
			// ("Async job call-bash (bash) was stopped (job_kill)"). The
			// notice is only persisted after the shell actually exited, so
			// serving this turn is itself production proof that the child's
			// OWN owned background job is terminal -- release the gate here.
			//
			// This branch also serves the case where the completion Drain
			// MERGED into the child's plain post-job_kill continuation: the
			// merged turn carries BOTH the notice and the "terminated
			// successfully" tool result, and the misroute-sensitive harness
			// used to classify it as child:result without ever closing the
			// gate -- the root then resumed first, the 15s gate refused with
			// 503 twice, and the run died of its own ctx deadline (~61s) --
			// the reported #1083 flake, load/race dependent.
			closeGate.Do(func() { close(childJobKilled) })
			childTurns.Add(1)
			requestOrder.add("child:final")
			admissionWriteSSE(w, []string{
				admissionSSEText("child-final", "child final: gate job reported"), admissionSSEStop("child-final", "stop"),
			})
		case "child:yield":
			// Child turn 3 (unmerged path): the plain post-job_kill
			// continuation, the bg-shell-done notice not pulled yet. The kill
			// tool result is production proof the job is terminal, so the
			// gate is released here too; the completion Drain arrives after
			// this as its own child:final turn.
			closeGate.Do(func() { close(childJobKilled) })
			childTurns.Add(1)
			requestOrder.add("child:yield")
			admissionWriteSSE(w, []string{
				admissionSSEText("child-yield", "child yielded: gate still running"), admissionSSEStop("child-yield", "stop"),
			})
		case "child:kill":
			// Child turn 2: its own background command is up, so it kills it.
			// This is the test-controlled terminal event for the child's OWN
			// owned async work. The inner bash tool registered the shell as
			// "001" (first job in the app's background manager), which is
			// what the child is told to kill.
			childTurns.Add(1)
			requestOrder.add("child:kill")
			admissionWriteSSE(w, []string{
				admissionSSEToolCall("child-kill", "call-kill", "job_kill", `{"shell_id":"001"}`),
				admissionSSEStop("child-kill", "tool_calls"),
			})
		case "child:bash":
			// Child turn 1: the delegation prompt. The child starts its OWN
			// background command — exactly the shape that used to release the
			// parent prematurely. The command must outlive bash's 1s
			// fast-failure probe: a fast command (`go version`) is REMOVED by
			// that probe and never becomes a live background shell, so the
			// child would own nothing at all.
			childTurns.Add(1)
			requestOrder.add("child:bash")
			admissionWriteSSE(w, []string{
				admissionSSEToolCall("child-bash", "call-bash", "bash",
					`{"command":`+jsonString(longRunningCommand())+`,"description":"hold the gate open","run_in_background":true}`),
				admissionSSEStop("child-bash", "tool_calls"),
			})
		case "root:yield":
			// The root's turn 2 — the continuation the model always gets after
			// an async tool call returned. It must YIELD (plain text, end of
			// turn) rather than delegate again; the delegation is already
			// parked and the drain loop is what resumes this session later.
			requestOrder.add("root:yield")
			admissionWriteSSE(w, []string{
				admissionSSEText("root-yield", "root yielded: delegation parked"), admissionSSEStop("root-yield", "stop"),
			})
		default:
			requestOrder.add("root:delegate")
			admissionWriteSSE(w, []string{
				admissionSSEToolCall("delegate", "call-agent", "agent", `{"prompt":"WORKER-MARKER do the task"}`),
				admissionSSEStop("delegate", "tool_calls"),
			})
		}
	})
	grantChildBackgroundShellTools(t, application)

	// A rare full-package-only hang (61s = the ctx timeout below) left no
	// evidence; on failure dump the served-request order and every goroutine.
	t.Cleanup(func() {
		if !t.Failed() {
			return
		}
		t.Logf("DIAG request order: %v", requestOrder.snapshot())
		var buf strings.Builder
		_ = pprof.Lookup("goroutine").WriteTo(&buf, 1)
		dump := buf.String()
		if len(dump) > 200_000 {
			dump = dump[:200_000]
		}
		t.Logf("DIAG goroutines:\n%s", dump)
	})

	ctx, cancel := context.WithTimeout(t.Context(), 60*time.Second)
	defer cancel()
	var output syncBuffer
	result, err := application.RunNonInteractiveWithResult(ctx, &output, "start a worker", RunOverrides{
		Origin: message.OriginCLI,
	}, true, RunModeJSON, sessionID, false)
	require.NoError(t, err)
	require.NotNil(t, result)

	// THE FULL CONTRACT, asserted on request order and exit_reason:
	//
	//  1. root turn-1 yields after delegation,
	//  2. the gate holds (no final JSON is produced while the child's
	//     owned job is still live — enforced by the barrier in the
	//     handler, which refuses to serve the root's final turn until the
	//     child's job_kill has landed),
	//  3. the child-owned job completes and that completion is CONSUMED as
	//     the root's turn-2 input,
	//  4. the root then finishes,
	//  5. the final JSON envelope is emitted exactly once, and nothing
	//     further runs afterwards.
	//
	// The order is asserted by index rather than by exact sequence: the
	// root's post-delegation yield turn (a real request — the model is always
	// called again after an async tool result) races the child's first turn,
	// so its position is not deterministic. The child's post-kill shape is
	// likewise racy: the completion Drain may merge into the plain
	// continuation (a single child:final turn) or follow it (child:yield,
	// then child:final). What IS contractual is the relative order of the
	// phases: exactly one root:delegate first, the child's phases in order,
	// and root:final LAST.
	order := requestOrder.snapshot()
	require.Contains(t, order, "root:delegate", "the root must delegate exactly once: %v", order)
	require.Equal(t, "root:delegate", order[0], "the delegation must be the first served request: %v", order)
	require.Equal(t, "root:final", order[len(order)-1], "the root must be the LAST phase to run: %v", order)
	require.Equal(t, 1, countLabel(order, "root:delegate"), "the root must delegate exactly once: %v", order)
	require.Equal(t, 1, countLabel(order, "root:final"), "the root must be resumed exactly once by the delegation result: %v", order)
	require.Equal(t, 1, countLabel(order, "child:bash"), "the child must start exactly one background job: %v", order)
	require.Equal(t, 1, countLabel(order, "child:kill"), "the child must kill its job exactly once: %v", order)
	require.Less(t, indexOf(order, "child:bash"), indexOf(order, "child:kill"),
		"the child must start its background job before killing it: %v", order)
	// The post-kill phase: at least one child:final (the turn carrying the
	// pulled bg-shell-done notice, merged or standalone), optionally preceded
	// by the unmerged plain continuation. Every child phase precedes the
	// root's final turn.
	require.GreaterOrEqual(t, countLabel(order, "child:final"), 1,
		"the child's final turn (carrying the pulled job-done notice) must be served: %v", order)
	require.Less(t, indexOf(order, "child:kill"), indexOf(order, "child:final"),
		"the child must kill its job before its final turn: %v", order)
	require.Less(t, indexOf(order, "child:final"), indexOf(order, "root:final"),
		"the child's answer to its own job result must reach the parent "+
			"before the root's final turn: %v", order)

	require.EqualValues(t, 1, rootFinalTurn.Load(),
		"the root must be resumed exactly once by the delegation result")
	require.Equal(t, "root final answer", result.FinalText)

	// The root ran to its own clean completion, not to a queued/canceled or
	// error exit caused by the descendant gate holding it open.
	require.Equal(t, "end_turn", result.ExitReason,
		"the root must finish with its OWN end_turn, not an artifact of the "+
			"transitive gate")
	require.Empty(t, result.Error,
		"holding the root open for descendant work must not surface as an error")

	// Exactly one JSON envelope, emitted after the child's owned job was
	// terminal. "session_id" appears once per envelope, so more than one
	// means the root returned early and was resumed again.
	require.Equal(t, 1, strings.Count(output.String(), `"session_id"`),
		"exactly one final JSON envelope may be emitted")

	// Exactly one delegation notice, and it carries the child's own result.
	msgs, listErr := application.Messages.List(t.Context(), sessionID)
	require.NoError(t, listErr)
	var notices int
	for _, item := range msgs {
		if !item.BackgroundJobNotice {
			continue
		}
		notices++
		require.Contains(t, item.FullText(), "child final",
			"the parent's only delegation notice must carry the child's final answer")
	}
	require.Equal(t, 1, notices, "exactly one delegation notice may reach the parent session")
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

// grantChildBackgroundShellTools layers bash/job_kill/job_output onto the
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
	for _, name := range []string{"bash", "job_kill", "job_output"} {
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
	Role    string `json:"role"`
	Content any    `json:"content"`
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
	var req wireRequest
	if err := json.Unmarshal(body, &req); err != nil {
		return "root:delegate"
	}
	system := ""
	if len(req.Messages) > 0 && req.Messages[0].Role == "system" {
		if s, ok := req.Messages[0].Content.(string); ok {
			system = s
		}
	}
	lastUser, lastTool := "", ""
	for _, m := range req.Messages {
		switch m.Role {
		case "user":
			if s, ok := m.Content.(string); ok {
				lastUser = s
			}
		case "tool":
			if s, ok := m.Content.(string); ok {
				lastTool = s
			}
		}
	}

	// Ambiguity note: the completion Drain can MERGE into the plain
	// post-job_kill continuation, making that turn carry BOTH the pulled
	// bg-shell-done notice (lastUser) and the "terminated successfully" tool
	// result (lastTool). The notice check must therefore win: that merged
	// turn is the child's final turn (child:final), not a loss of the kill
	// phase. Ordering within a session is stable, so once the kill tool
	// result exists it stays the last tool result for every later child
	// turn; the notice's presence is what distinguishes final from yield.
	switch {
	case strings.Contains(system, "short title"):
		return "title"
	case strings.Contains(lastUser, "Async job call-agent (agent) finished"):
		return "root:final"
	case strings.Contains(lastUser, "Async job call-bash (bash)"):
		return "child:final"
	case strings.Contains(lastTool, "terminated successfully"):
		return "child:yield"
	case strings.Contains(lastTool, "Async bash job"):
		return "child:kill"
	case strings.Contains(lastUser, "WORKER-MARKER do the task"):
		return "child:bash"
	case strings.Contains(lastTool, "Async agent job call-agent started"):
		return "root:yield"
	default:
		return "root:delegate"
	}
}

// safeRequestOrder records the phase label of each served model request, in
// arrival order, so a test can assert the exact sequence the root and its
// descendants ran in. The handler runs on the provider's goroutine while the
// assertions run on the test goroutine, hence the mutex.
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
