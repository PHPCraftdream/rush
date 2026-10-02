package agent

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync/atomic"
	"testing"

	"charm.land/fantasy"

	"github.com/PHPCraftdream/rush/internal/agent/tools"
	"github.com/stretchr/testify/require"
)

// errUnknownTool mirrors fantasy's own answer to a call whose tool is not in
// the step's set.
var errUnknownTool = errors.New("tool not found: read")

const guardOraclePath = "f.go"

// The oracle of docs/plans/2026-10-01-in-turn-progress-guard.md §1.4: every
// row of the table, driven through the same code production runs — the
// stepGuard snapshot prepareStep builds, the real classifyStepCall and
// applyStep. Both each step's verdict AND the whole streak series are
// compared, not just the final number: a streak that reaches the right value
// by the wrong route is exactly as wrong as one that does not reach it.
//
// Revert-checks: (a) nextStreak counting neutral as "+1" turns row 6 red —
// the turn would stop on the 5th step even though the model was polling
// `todos`; (b) keeping read coverage across an act turns row 5 red
// (1 2 0 1 instead of 1 2 0 0).
func TestTurnProgressOracleTable(t *testing.T) {
	t.Parallel()

	asyncTag := `{"async":true,"job_id":"j","status":"running","claim_id":"claim-1"}`

	// Row 4 (A22): one full read, then windows of 3 sliding forward by one a
	// time — fresh by arguments, stale by content, so loop_detection stays mute.
	var sliding []oracleCall
	for i := 0; i < 5; i++ {
		sliding = append(sliding, win(68+i, 68+i+3))
	}

	rows := []oracleRow{
		{
			name:        "1_wait_series_without_own_work",
			async:       true,
			steps:       []oracleStep{step(wait), step(wait), step(wait), step(wait), step(wait)},
			wantStreak:  []int{1, 2, 3, 4, 5},
			wantRefused: []bool{false, false, true, true, true},
			wantStop:    true,
		},
		{
			name:  "2_sleep_then_waits_with_own_work",
			async: true,
			steps: append([]oracleStep{step(wait)},
				stepOwn(wait), stepOwn(wait), stepOwn(wait), stepOwn(wait)),
			wantStreak:  []int{1, 2, 3, 4, 5},
			wantRefused: []bool{false, true, true, true, true},
			wantStop:    true,
		},
		{
			name:        "3_waits_with_live_own_work",
			async:       true,
			steps:       []oracleStep{stepOwn(wait), stepOwn(wait), stepOwn(wait), stepOwn(wait), stepOwn(wait)},
			wantStreak:  []int{1, 2, 3, 4, 5},
			wantRefused: []bool{true, true, true, true, true},
			wantStop:    true,
		},
		{
			name:        "4_reread_series_A22",
			async:       true,
			steps:       []oracleStep{step(win(0, 163)), step(sliding[0]), step(sliding[1]), step(sliding[2]), step(sliding[3]), step(sliding[4])},
			wantStreak:  []int{0, 1, 2, 3, 4, 5},
			wantRefused: []bool{false, false, false, true, true, true},
			wantStop:    true,
		},
		{
			name:        "5_act_clears_coverage",
			async:       true,
			steps:       []oracleStep{step(win(68, 71)), step(win(68, 71)), step(act), step(win(68, 71))},
			seed:        []readSpan{{start: 68, end: 71}},
			wantStreak:  []int{1, 2, 0, 0},
			wantRefused: []bool{false, false, false, false},
		},
		{
			name:        "6_neutral_does_not_extend",
			async:       true,
			steps:       []oracleStep{step(wait), step(wait), step(neutral), step(neutral), step(neutral), step(wait)},
			wantStreak:  []int{1, 2, 2, 2, 2, 3},
			wantRefused: []bool{false, false, false, false, false, true},
		},
		{
			name:  "7_progress_in_one_step_wins",
			async: true,
			steps: []oracleStep{
				stepOwn(wait, act), stepOwn(wait, act), stepOwn(wait, act),
				stepOwn(wait, act), stepOwn(wait, act), stepOwn(wait, act),
				stepOwn(wait, act),
			},
			wantStreak:  []int{0, 0, 0, 0, 0, 0, 0},
			wantRefused: []bool{true, true, true, true, true, true, true},
		},
		{
			name:        "8_notice_does_not_reset_streak",
			async:       true,
			steps:       []oracleStep{step(wait), step(wait), stepBoundary(wait)},
			wantStreak:  []int{1, 2, 3},
			wantRefused: []bool{false, false, true},
		},
		{
			name:        "9_notice_clears_coverage",
			async:       true,
			steps:       []oracleStep{step(win(0, 163)), step(win(68, 71)), stepBoundary(win(68, 71))},
			wantStreak:  []int{0, 1, 0},
			wantRefused: []bool{false, false, false},
		},
		{
			name:        "10_trim_clears_coverage",
			async:       true,
			steps:       []oracleStep{step(win(68, 71)), step(win(68, 71)), stepBoundary(win(68, 71))},
			seed:        []readSpan{{start: 68, end: 71}},
			wantStreak:  []int{1, 2, 0},
			wantRefused: []bool{false, false, false},
		},
		{
			name:        "11_sdk_waits_are_acts",
			async:       false,
			steps:       []oracleStep{stepOwn(wait), stepOwn(wait), stepOwn(wait), stepOwn(wait), stepOwn(wait), stepOwn(wait), stepOwn(wait)},
			wantStreak:  []int{0, 0, 0, 0, 0, 0, 0},
			wantRefused: []bool{false, false, false, false, false, false, false},
		},
		{
			name:        "12_single_timer_then_text",
			async:       true,
			steps:       []oracleStep{step(wait), step()},
			wantStreak:  []int{1, 1},
			wantRefused: []bool{false, false},
		},
		{
			name:        "13_refusal_beside_a_read_is_progress",
			async:       true,
			steps:       []oracleStep{step(refused, win(400, 420))},
			wantStreak:  []int{0},
			wantRefused: []bool{true},
		},
		{
			name:  "14_drain_leg_of_pure_refusals",
			async: true,
			skip:  "#1113's business: a reaction-chain leg, not the in-turn streak",
		},
		{
			name:        "15_unknown_tool_is_neutral",
			async:       true,
			steps:       []oracleStep{step(unknown), step(unknown), step(unknown)},
			wantStreak:  []int{0, 0, 0},
			wantRefused: []bool{false, false, false},
		},
	}

	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()
			if row.skip != "" {
				t.Skip(row.skip)
			}
			ts := &turnStream{call: SessionAgentCall{SessionID: "oracle-" + row.name}}
			for _, span := range row.seed {
				ts.progress.seen = make(readCoverage)
				ts.progress.seen.add(guardOraclePath, span)
			}

			var gotStreak []int
			var gotRefused []bool
			for i, oracleStep := range row.steps {
				_ = i
				// prepareStep: a boundary that inserted or trimmed clears
				// coverage BEFORE the snapshot, so the snapshot is what this
				// step's prompt actually carries.
				if oracleStep.boundary {
					ts.progress.seen.clear()
				}
				guard := stepGuardFor(&ts.progress, oracleStep.ownWork, row.async)
				guard.canEdit, guard.canDelegate = true, true
				ts.progress.guard = guard

				var calls []fantasy.ToolCallContent
				var results []fantasy.ToolResultContent
				refused := false
				for _, call := range oracleStep.calls {
					callContent, result := oracleCallContent(call, row.async, asyncTag)
					calls = append(calls, callContent)
					if oracleRefuses(ts.progress.seen, guard, call) {
						refused = true
						result.ClientMetadata = progressGuardMetadataJSON(refusedKindOf(call))
					}
					results = append(results, result)
				}
				ts.recordStepProgress(makeStep(calls, results))
				gotStreak = append(gotStreak, ts.progress.streak)
				gotRefused = append(gotRefused, refused)
			}

			require.Equal(t, row.wantStreak, gotStreak, "streak after every step of row %q", row.name)
			require.Equal(t, row.wantRefused, gotRefused, "per-step refusal verdicts of row %q", row.name)
			require.Equal(t, row.wantStop, ts.progress.stopped, "stopped at the end of row %q", row.name)
		})
	}
}

// oracleCall is one modelled tool call of one step.
type oracleCall struct {
	kind  byte
	spans []readSpan // only for callWindow
}

// oracleCallKind letters the §1.4 vocabulary: W wait launch, V windowed read
// (the classifier decides read vs. re-read from the turn's coverage), A act,
// N neutral, U call of a tool absent from the set, X a guard refusal.
const (
	callWait    = 'W'
	callWindow  = 'V'
	callAct     = 'A'
	callNeutral = 'N'
	callUnknown = 'U'
	callRefused = 'X'
)

var (
	wait    = oracleCall{kind: callWait}
	act     = oracleCall{kind: callAct}
	neutral = oracleCall{kind: callNeutral}
	unknown = oracleCall{kind: callUnknown}
	refused = oracleCall{kind: callRefused}
)

// win is a windowed read of [start, end) of the oracle file.
func win(start, end int) oracleCall {
	return oracleCall{kind: callWindow, spans: []readSpan{{start: start, end: end}}}
}

func (c oracleCall) named() []namedSpan {
	out := make([]namedSpan, 0, len(c.spans))
	for _, sp := range c.spans {
		out = append(out, namedSpan{path: guardOraclePath, span: sp})
	}
	return out
}

// oracleStep is one step of a turn. ownWork models the session having its own
// undelivered work at the step's start; boundary models a step whose boundary
// inserted a notice or trimmed the window — both clear read coverage, and a
// boundary event never resets the streak, else an echo cycle is endless.
type oracleStep struct {
	ownWork  bool
	boundary bool
	calls    []oracleCall
}

func step(calls ...oracleCall) oracleStep         { return oracleStep{calls: calls} }
func stepOwn(calls ...oracleCall) oracleStep      { return oracleStep{calls: calls, ownWork: true} }
func stepBoundary(calls ...oracleCall) oracleStep { return oracleStep{calls: calls, boundary: true} }

// oracleRow is one row of §1.4's table. async models the entry-channel origin:
// CLI/Web launch commands as background jobs (async), while an SDK sleep
// blocks for real (act). seed records windows read before the row's steps, for
// the rows that start from an already-covered file.
type oracleRow struct {
	name        string
	async       bool
	steps       []oracleStep
	seed        []readSpan
	wantStreak  []int
	wantRefused []bool
	wantStop    bool
	skip        string
}

// oracleRefuses is the guard's own verdict, exactly as the wrapper applies it:
// a wait launch is refused by refuseWait, a windowed read by refuseReread, and
// a modelled refusal needs no decision.
func oracleRefuses(cov readCoverage, guard stepGuard, call oracleCall) bool {
	switch call.kind {
	case callWait:
		return guard.refuseWaitCall()
	case callWindow:
		return guard.refuseRereadCall(call.named())
	}
	return call.kind == callRefused
}

func refusedKindOf(call oracleCall) string {
	if call.kind == callWindow {
		return "reread"
	}
	return "wait"
}

// oracleCallContent builds the tool call and the result the classifier judges.
// async controls a wait launch's metadata: an async tag makes it a wait, its
// absence makes it a real (blocking) act.
func oracleCallContent(call oracleCall, async bool, asyncTag string) (fantasy.ToolCallContent, fantasy.ToolResultContent) {
	var (
		content fantasy.ToolCallContent
		result  fantasy.ToolResultContent
	)
	switch call.kind {
	case callWait, callRefused:
		content = fantasy.ToolCallContent{ToolCallID: "c", ToolName: "bash", Input: `{"command":"echo x"}`}
		result = fantasy.ToolResultContent{
			ToolCallID: "c",
			ToolName:   "bash",
			Result:     fantasy.ToolResultOutputContentText{Text: "started"},
		}
		if async {
			result.ClientMetadata = asyncTag
		}
	case callWindow:
		input, err := json.Marshal(map[string]any{
			"file_path": guardOraclePath,
			"offset":    call.spans[0].start,
			"limit":     call.spans[0].end - call.spans[0].start,
		})
		if err != nil {
			panic(err)
		}
		content = fantasy.ToolCallContent{ToolCallID: "c", ToolName: "view", Input: string(input)}
		result = fantasy.ToolResultContent{
			ToolCallID: "c",
			ToolName:   "view",
			Result:     fantasy.ToolResultOutputContentText{Text: "1\tline"},
		}
	case callAct:
		content = fantasy.ToolCallContent{ToolCallID: "c", ToolName: "edit", Input: `{"file_path":"f.go","old_string":"a","new_string":"b"}`}
		result = fantasy.ToolResultContent{
			ToolCallID: "c",
			ToolName:   "edit",
			Result:     fantasy.ToolResultOutputContentText{Text: "edited"},
		}
	case callNeutral:
		content = fantasy.ToolCallContent{ToolCallID: "c", ToolName: "todos", Input: `{"todos":[]}`}
		result = fantasy.ToolResultContent{
			ToolCallID: "c",
			ToolName:   "todos",
			Result:     fantasy.ToolResultOutputContentText{Text: "listed"},
		}
	case callUnknown:
		content = fantasy.ToolCallContent{ToolCallID: "c", ToolName: "read", Input: `{"path":"f.go"}`}
		result = fantasy.ToolResultContent{
			ToolCallID: "c",
			ToolName:   "read",
			Result:     fantasy.ToolResultOutputContentError{Error: errUnknownTool},
		}
	}
	return content, result
}

// guardStepModel is a provider double that yields exactly one tool call of a
// caller-chosen name/input on its FIRST Stream, and a plain text finish on
// every later one. One tool-calling round is all the wrapper needs to be
// exercised through fantasy's own executeSingleTool — which is what copies
// ToolResponse.Metadata into ToolResultContent.ClientMetadata — and staying
// at one round keeps the spy from running twice.
type guardStepModel struct {
	mockModel
	toolName, toolInput string
	rounds              int
}

func (m *guardStepModel) Stream(context.Context, fantasy.Call) (fantasy.StreamResponse, error) {
	m.rounds++
	first := m.rounds == 1
	return func(yield func(fantasy.StreamPart) bool) {
		if first {
			if !yield(fantasy.StreamPart{
				Type:          fantasy.StreamPartTypeToolCall,
				ID:            "guard-call-1",
				ToolCallName:  m.toolName,
				ToolCallInput: m.toolInput,
			}) {
				return
			}
		} else if !yield(fantasy.StreamPart{
			Type: fantasy.StreamPartTypeTextDelta,
		}) {
			return
		}
		yield(fantasy.StreamPart{
			Type:         fantasy.StreamPartTypeFinish,
			FinishReason: fantasy.FinishReasonStop,
			Usage:        fantasy.Usage{InputTokens: 1, OutputTokens: 1},
		})
	}, nil
}

// runGuardTool runs ONE call of name/input through a progressGuardTool-wrapped
// spy tool, via fantasy's real agent loop, and returns the ToolResultContent
// fantasy produced. spy counts the inner tool's real executions.
func runGuardTool(t *testing.T, guard stepGuard, name, input string, spy *atomic.Int32) fantasy.ToolResultContent {
	t.Helper()
	spyTool := fantasy.NewAgentTool(name, "spy: flips a counter if it really runs",
		func(context.Context, struct{}, fantasy.ToolCall) (fantasy.ToolResponse, error) {
			spy.Add(1)
			return fantasy.NewTextResponse("spy ran"), nil
		})
	agent := fantasy.NewAgent(
		&guardStepModel{mockModel: mockModel{}, toolName: name, toolInput: input},
		fantasy.WithTools(&progressGuardTool{inner: spyTool, guard: guard}),
		fantasy.WithStopConditions(fantasy.StepCountIs(3)),
	)
	var got fantasy.ToolResultContent
	_, err := agent.Stream(context.Background(), fantasy.AgentStreamCall{
		Messages: []fantasy.Message{fantasy.NewUserMessage("go")},
		OnToolResult: func(result fantasy.ToolResultContent) error {
			got = result
			return nil
		},
	})
	require.NoError(t, err)
	require.NotEmpty(t, got.ToolCallID, "fantasy must have executed the call")
	return got
}

func guardErrorText(t *testing.T, result fantasy.ToolResultContent) string {
	t.Helper()
	errRes, ok := fantasy.AsToolResultOutputType[fantasy.ToolResultOutputContentError](result.Result)
	require.True(t, ok, "a guard refusal must come back as an error result")
	return errRes.Error.Error()
}

// TestProgressGuardToolRefusesWait is T4's A23 leg: with the session's own work
// running, a pure wait launch never reaches the tool at streak 0 — the spy
// stays at zero, and the refusal carries both the wording that tells the model
// how to end its turn and the metadata tag classifyStepCall reads.
//
// Revert-check: dropping ownWork from refuseWait's condition makes the spy run
// and this test red (see run_guard_revert_checks.md for the recorded failure).
func TestProgressGuardToolRefusesWait(t *testing.T) {
	t.Parallel()

	var spy atomic.Int32
	result := runGuardTool(t, stepGuard{streak: 0, ownWork: true, async: true},
		tools.BashToolName, `{"command":"echo x"}`, &spy)

	require.Zero(t, spy.Load(), "the inner tool must never run on a refusal — no ledger Start, no notice, no claim")
	text := guardErrorText(t, result)
	require.Contains(t, text, "does not wait", "refusal text must say the command does not wait")
	require.Contains(t, text, "NO tool call", "refusal text must tell the model how to end the turn")
	require.Contains(t, result.ClientMetadata, `"progress_guard"`, "refusal must be tagged across fantasy")
	require.Contains(t, result.ClientMetadata, `"refused":"wait"`)
}

// TestProgressGuardToolPassesThrough covers the three legs a refusal must NOT
// touch: a wait with no own work and an empty streak (row 1's sanctioned single
// timer), and a command with actual substance. In each the spy runs and the
// guard leaves the result alone.
func TestProgressGuardToolPassesThrough(t *testing.T) {
	t.Parallel()

	t.Run("wait_without_own_work_passess", func(t *testing.T) {
		t.Parallel()
		var spy atomic.Int32
		result := runGuardTool(t, stepGuard{streak: 0, ownWork: false, async: true},
			tools.BashToolName, `{"command":"echo x"}`, &spy)
		require.Equal(t, int32(1), spy.Load(), "a first wait with no own work of this session runs")
		require.Equal(t, "spy ran", textOf(result))
	})

	t.Run("real_command_runs", func(t *testing.T) {
		t.Parallel()
		var spy atomic.Int32
		result := runGuardTool(t, stepGuard{streak: 0, ownWork: true, async: true},
			tools.BashToolName, `{"command":"go build ./..."}`, &spy)
		require.Equal(t, int32(1), spy.Load(), "a command with substance is not a wait")
		require.Equal(t, "spy ran", textOf(result))
	})

	t.Run("sync_origin_wait_runs", func(t *testing.T) {
		t.Parallel()
		var spy atomic.Int32
		result := runGuardTool(t, stepGuard{streak: 4, ownWork: true, async: false},
			tools.BashToolName, `{"command":"echo x"}`, &spy)
		require.Equal(t, int32(1), spy.Load(), "an SDK sleep blocks for real — it is act, not wait")
		require.Equal(t, "spy ran", textOf(result))
	})
}

// TestProgressGuardToolRefusesReread is T4's A22 leg: past the threshold, a
// windowed read whose every window was already returned in this turn is
// refused, and in orchestrator mode (no edit tools, but `agent`) the refusal
// says how to get the change made instead. The last free step also warns that
// the guard ends the turn next.
func TestProgressGuardToolRefusesReread(t *testing.T) {
	t.Parallel()

	covered := readCoverage{guardOraclePath: {{start: 0, end: 163}}}

	t.Run("covered_window", func(t *testing.T) {
		t.Parallel()
		var spy atomic.Int32
		result := runGuardTool(t, stepGuard{streak: 2, seen: covered, canEdit: true, canDelegate: true},
			"view", `{"file_path":"f.go","offset":68,"limit":3}`, &spy)
		require.Zero(t, spy.Load(), "the inner tool must never run on a refusal")
		text := guardErrorText(t, result)
		require.Contains(t, text, "already returned in this turn")
		require.Contains(t, text, "lines 69–71", "the refusal must name the lines it already holds")
		require.Contains(t, result.ClientMetadata, `"refused":"reread"`)
		require.NotContains(t, text, "delegate the change", "an editing session needs no delegation hint")
	})

	t.Run("fresh_window_runs", func(t *testing.T) {
		t.Parallel()
		var spy atomic.Int32
		result := runGuardTool(t, stepGuard{streak: 2, seen: covered, canEdit: true, canDelegate: true},
			"view", `{"file_path":"f.go","offset":170,"limit":3}`, &spy)
		require.Equal(t, int32(1), spy.Load(), "an uncovered window is a fresh read")
		require.Equal(t, "spy ran", textOf(result))
	})

	t.Run("orchestrator_mode_delegation_hint", func(t *testing.T) {
		t.Parallel()
		var spy atomic.Int32
		result := runGuardTool(t, stepGuard{streak: 2, seen: covered, canEdit: false, canDelegate: true},
			"view", `{"file_path":"f.go","offset":68,"limit":3}`, &spy)
		require.Zero(t, spy.Load())
		require.Contains(t, guardErrorText(t, result), "delegate the change with the `agent` tool")
	})

	t.Run("last_free_step_warns_of_the_stop", func(t *testing.T) {
		t.Parallel()
		var spy atomic.Int32
		result := runGuardTool(t, stepGuard{streak: inTurnStopAfter - 1, seen: covered, canEdit: true, canDelegate: true},
			"view", `{"file_path":"f.go","offset":68,"limit":3}`, &spy)
		require.Zero(t, spy.Load())
		require.Contains(t, guardErrorText(t, result), "rush ends the turn now")
	})
}

// TestGuardStopDetailsText pins the assistant finish details of a stopped turn
// (what it counted, whether the session had its own work, what to do next) and
// the exported recognizer the CLI step 3 will use.
func TestGuardStopDetailsText(t *testing.T) {
	t.Parallel()

	progress := turnProgress{streak: inTurnStopAfter, waits: 3, rereads: 2, stopped: true}
	progress.guard = stepGuard{streak: inTurnStopAfter, ownWork: true, canEdit: false, canDelegate: true}
	details := guardStopDetails(progress)
	require.Contains(t, details, "5 consecutive steps")
	require.Contains(t, details, "3 wait call(s)")
	require.Contains(t, details, "2 already-read window(s)")
	require.Contains(t, details, "delegate the remaining change")

	require.True(t, IsInTurnGuardStop(InTurnGuardStopTitle))
	require.True(t, IsInTurnGuardStop(InTurnGuardStopTitle+": tail"))
	require.False(t, IsInTurnGuardStop("Stopped: loop detected"))
	require.False(t, IsInTurnGuardStop(""))
}

func textOf(result fantasy.ToolResultContent) string {
	text, ok := fantasy.AsToolResultOutputType[fantasy.ToolResultOutputContentText](result.Result)
	if !ok {
		return ""
	}
	return text.Text
}

// --- prepareStep's two coverage resets, driven on the REAL method ---
//
// TurnProgress's oracle table above models both resets as `oracleStep.
// boundary` — a boolean the row's author sets by hand. These two tests take
// the other side: they run the real prepareStep on a real turnStream and check
// that the production branch really does fire, so a mutant that drops either
// `ts.progress.seen.clear()` call inside prepareStep goes red here rather than
// only in a comment.

const (
	// guardSplicePath/guardTrimPath are the files the two legs read. The
	// window is the same (0,163) the A22 oracle row reads in one go.
	guardSplicePath = "guard_m7.go"
	guardTrimPath   = "guard_m6.go"
)

// guardProbeView is the call the guard judges: the FIRST window of path, the
// one a turn that already returned it has no reason to ask for again.
func guardProbeView(path string) fantasy.ToolCallContent {
	input, err := json.Marshal(map[string]any{"file_path": path, "offset": 0, "limit": 163})
	if err != nil {
		panic(err)
	}
	return fantasy.ToolCallContent{ToolCallID: "guard-probe", ToolName: tools.ViewToolName, Input: string(input)}
}

// guardCoverageVerdict is the OBSERVABLE consequence of a step's guard
// snapshot: the same repeated view call, classified by the real
// classifyStepCall against that snapshot, plus the real refusal verdict a step
// at the refusal threshold reaches for it. Coverage that survived the step
// boundary makes the call a re-read the guard refuses; coverage the boundary
// cleared makes it a fresh read that runs. Reading ts.progress.guard.seen (the
// snapshot prepareStep handed the step) rather than the live map is the point:
// a reset that landed too late — after armProgressGuard — would pass a live-map
// check and still be wrong.
func guardCoverageVerdict(ts *turnStream, path string) (class stepCallClass, refuses bool) {
	call := guardProbeView(path)
	result := fantasy.ToolResultContent{
		ToolCallID: call.ToolCallID,
		ToolName:   call.ToolName,
		Result:     fantasy.ToolResultOutputContentText{Text: "1\tpackage agent"},
	}
	return classifyStepCall(call, result, stepClassifyCtx{seen: ts.progress.guard.seen}),
		(stepGuard{streak: inTurnRefuseAfter, seen: ts.progress.guard.seen}).refuseRereadCall(readSpans(call))
}

// guardSeedCoverage records one full read of path's first window in the turn's
// coverage — the state a real read of that window leaves behind (§1.2).
func guardSeedCoverage(ts *turnStream, path string) {
	ts.progress.seen = make(readCoverage)
	ts.progress.seen.add(path, readSpan{start: 0, end: 163})
}

// guardFillerMessages are three long user messages plus a short one: the tiny
// window the trim leg runs on overflows the long ones, so the sliding-window
// trim really cuts the front of the prompt instead of returning it unchanged.
func guardFillerMessages() []fantasy.Message {
	return []fantasy.Message{
		fantasy.NewUserMessage(strings.Repeat("guard trim filler ", 60)),
		fantasy.NewUserMessage(strings.Repeat("guard trim filler ", 60)),
		fantasy.NewUserMessage(strings.Repeat("guard trim filler ", 60)),
		fantasy.NewUserMessage("the newest message"),
	}
}

// guardPrepareStream assembles the turnStream one prepareStep run needs from
// the Drain-attempt fixture: the real *sessionAgent, the real session row and
// the real model, every other input stubbed. No provider and no fantasy loop
// are involved — the step is prepared directly, so a coverage reset inside
// prepareStep is observed in isolation instead of through a whole turn.
// promptTokens is what currentSession's own recorded usage claims, which is
// what the sliding-window branch subtracts from the context window.
func guardPrepareStream(t *testing.T, f *attemptFixture, model Model, promptTokens int64) *turnStream {
	t.Helper()
	sess, err := f.env.sessions.Get(context.Background(), f.sessID)
	require.NoError(t, err)
	sess.PromptTokens = promptTokens
	return newTurnStream(turnStreamConfig{
		a: f.sa, ctx: context.Background(), genCtx: context.Background(), cancel: func() {},
		call: SessionAgentCall{SessionID: f.sessID}, smartModel: model, currentSession: sess,
		bumpActivity: func() {}, toolStarted: func() {}, toolFinished: func() {},
		startCheckpoint: func() {}, stopCheckpoint: func() {},
		notifyUI: func() error { return nil }, drainPendingUI: func() {},
	})
}

// TestPrepareStepSpliceClearsReadCoverage pins prepareStep's splice reset: a
// step boundary that inserts a message (mailbox inject, cross-process inject
// or notice pull) may be the very reason a file changed, so the windows read
// earlier in the turn are dropped BEFORE the step's guard snapshot is taken
// (agent_turn_step.go's `if len(stepSplices) > 0` branch, §1.2). The notice
// pull is what makes stepSplices non-empty here: seedDebt(pulled=false) leaves
// a terminal, announced job whose notice row is still delivery='pending', so
// the step-boundary pull moves it into history.
//
// The control leg is the same fixture with nothing pending: no splice, so the
// coverage must survive into the step. Without it the mutant leg would pass
// against an empty snapshot either way.
func TestPrepareStepSpliceClearsReadCoverage(t *testing.T) {
	t.Parallel()

	t.Run("boundary_without_a_splice_keeps_coverage", func(t *testing.T) {
		t.Parallel()
		f := newAttemptFixture(t, "guard-splice-control", attemptFixtureOpts{noIdle: true})
		ts := guardPrepareStream(t, f, f.model, 0)
		guardSeedCoverage(ts, guardSplicePath)

		_, _, err := ts.prepareStep(context.Background(), fantasy.PrepareStepFunctionOptions{
			Messages: []fantasy.Message{fantasy.NewUserMessage("continue")},
		})
		require.NoError(t, err)

		require.Empty(t, ts.carriedSplices, "nothing was pending: the boundary inserted nothing")
		require.False(t, ts.silentCompactNeeded, "the fixture never enables the sliding window")
		class, refuses := guardCoverageVerdict(ts, guardSplicePath)
		require.Equal(t, stepCallReread, class, "coverage survives a boundary that spliced nothing")
		require.True(t, refuses, "the repeated window is refused with the coverage intact")
	})

	t.Run("boundary_with_a_splice_clears_coverage", func(t *testing.T) {
		t.Parallel()
		f := newAttemptFixture(t, "guard-splice", attemptFixtureOpts{noIdle: true})
		f.seedDebt(context.Background(), "guard-splice-notice", false)
		ts := guardPrepareStream(t, f, f.model, 0)
		guardSeedCoverage(ts, guardSplicePath)

		_, _, err := ts.prepareStep(context.Background(), fantasy.PrepareStepFunctionOptions{
			Messages: []fantasy.Message{fantasy.NewUserMessage("continue")},
		})
		require.NoError(t, err)

		require.Len(t, ts.carriedSplices, 1, "the pulled notice was spliced into this step's prompt")
		class, refuses := guardCoverageVerdict(ts, guardSplicePath)
		require.Equal(t, stepCallRead, class, "a boundary that spliced a message dropped the coverage")
		require.False(t, refuses, "the same window is a fresh read once the boundary cleared it")
	})
}

// TestPrepareStepTrimClearsReadCoverage pins prepareStep's trim reset: content
// the sliding window cut is no longer in the prompt, so what was read from it
// is no longer "already in your context" either (agent_turn_step.go's
// `if remaining <= slideThreshold` branch, §1.2). The mutant leg is the
// fixture's own model narrowed to a 100-token window against a session whose
// recorded usage already exceeds the slide threshold; the control leg is the
// same agent with the model's real window, which has slack and trims nothing.
//
// Revert-check performed: dropping `ts.progress.seen.clear()` from the trim
// branch makes the sliding_window_trim_clears_coverage leg red — the repeated
// window still classifies as a re-read and is still refused at the threshold.
func TestPrepareStepTrimClearsReadCoverage(t *testing.T) {
	t.Parallel()

	t.Run("window_with_slack_keeps_coverage", func(t *testing.T) {
		t.Parallel()
		f := newAttemptFixture(t, "guard-trim-control", attemptFixtureOpts{noIdle: true})
		f.sa.disableAutoSummarize = false
		ts := guardPrepareStream(t, f, f.model, 0)
		guardSeedCoverage(ts, guardTrimPath)

		_, _, err := ts.prepareStep(context.Background(), fantasy.PrepareStepFunctionOptions{
			Messages: guardFillerMessages(),
		})
		require.NoError(t, err)

		require.False(t, ts.silentCompactNeeded, "the fixture's window has slack: nothing is trimmed")
		class, refuses := guardCoverageVerdict(ts, guardTrimPath)
		require.Equal(t, stepCallReread, class, "coverage survives a step that trimmed nothing")
		require.True(t, refuses, "the repeated window is refused with the coverage intact")
	})

	t.Run("sliding_window_trim_clears_coverage", func(t *testing.T) {
		t.Parallel()
		f := newAttemptFixture(t, "guard-trim", attemptFixtureOpts{noIdle: true})
		f.sa.disableAutoSummarize = false
		narrow := f.model
		narrow.CatwalkCfg.ContextWindow = 100
		ts := guardPrepareStream(t, f, narrow, 90)
		guardSeedCoverage(ts, guardTrimPath)

		_, _, err := ts.prepareStep(context.Background(), fantasy.PrepareStepFunctionOptions{
			Messages: guardFillerMessages(),
		})
		require.NoError(t, err)

		require.True(t, ts.silentCompactNeeded, "the trimmed window also flagged the silent compact")
		class, refuses := guardCoverageVerdict(ts, guardTrimPath)
		require.Equal(t, stepCallRead, class, "a step that trimmed the prompt dropped the coverage")
		require.False(t, refuses, "the same window is a fresh read once the trim cleared it")
	})
}
