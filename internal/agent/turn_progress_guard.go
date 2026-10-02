package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"strings"

	"charm.land/fantasy"

	"github.com/PHPCraftdream/rush/internal/agent/tools"
)

// The in-turn progress guard (§2 of docs/plans/2026-10-01-in-turn-progress-guard.md).
// #1113 counts a reaction CHAIN across turns; nothing watched what happens
// INSIDE one turn, where the model burned 56–520 steps polling sleeps and
// re-reading the same windows (A22/A23). This guard is a second, independent
// law: a per-turn streak of steps that made no progress, a soft refusal at S
// and a hard stop at H.
const (
	// inTurnRefuseAfter (S): from this many consecutive no-progress steps a
	// wait launch or an already-covered re-read is refused instead of run.
	inTurnRefuseAfter = 2
	// inTurnStopAfter (H): the turn is finished by the guard once the streak
	// reaches this length.
	inTurnStopAfter = 5
)

// InTurnGuardStopTitle opens the assistant finish message of a turn the guard
// stopped. UI/CLI recognize it through IsInTurnGuardStop, exactly as the run
// loop recognizes a coordinator continuation prompt.
const InTurnGuardStopTitle = "Stopped: no progress in this turn"

// IsInTurnGuardStop reports whether title was written by the in-turn progress
// guard (see InTurnGuardStopTitle).
func IsInTurnGuardStop(title string) bool {
	return strings.HasPrefix(title, InTurnGuardStopTitle)
}

// refuseWait is the wait-leg verdict (§3's table): only an ASYNC launch waits
// (a synchronous SDK sleep really blocks, so it stays act), and it is refused
// either because the session has its own undelivered work — whose completion
// already wakes it — or because the streak is past S.
func refuseWait(streak int, ownWork, async bool) bool {
	return async && (ownWork || streak >= inTurnRefuseAfter)
}

// refuseReread is the re-read leg verdict: every window already returned in
// this turn is in the model's context, so asking for it again learns nothing.
func refuseReread(streak int) bool {
	return streak >= inTurnRefuseAfter
}

// nextStreak folds one step's class into the streak (§1.1): progress resets it,
// a wait/re-read/refusal extends it, a neutral step (`todos`, `job_output`,
// an unknown tool) leaves it alone.
func nextStreak(streak int, class stepCallClass) int {
	switch class {
	case stepCallAct, stepCallRead:
		return 0
	case stepCallReread, stepCallWait, stepCallRefused:
		return streak + 1
	default:
		return streak
	}
}

// stopAfter reports whether the streak is long enough for the guard to end the
// turn (§2.3).
func stopAfter(streak int) bool {
	return streak >= inTurnStopAfter
}

// classOfStep ranks one step's calls into the single class the streak is
// computed from: act/read (progress) outrank any no-progress call, which
// outranks neutral. A step that mixes a refusal with a real read still
// progressed (§1.4 line 13).
func classOfStep(classes []stepCallClass) stepCallClass {
	acc := stepCallNeutral
	for _, class := range classes {
		switch class {
		case stepCallAct, stepCallRead:
			return stepCallAct
		case stepCallReread, stepCallWait, stepCallRefused:
			if acc == stepCallNeutral {
				acc = class
			}
		}
	}
	return acc
}

// turnProgress is the guard's per-turn state (§1.3). One turn only; a new turn
// starts from zero. Written by prepareStep (coverage resets, guard snapshot)
// and by recordStepHistory (classes, streak, stopped), read by StopWhen — all
// sequential callbacks of one agent.Stream loop, so no lock.
type turnProgress struct {
	streak  int
	seen    readCoverage // line windows already returned in this turn; lazily built
	waits   int          // wait calls, for the finish text
	rereads int          // re-read calls, for the finish text
	stopped bool
	guard   stepGuard
}

// stepGuard is the immutable snapshot of one step's worth of guard state,
// built in prepareStep BEFORE the assistant message and handed to the tool
// wrappers of that step. Parallel calls of one step read one value — no lock,
// no shared mutable state (§1.3).
type stepGuard struct {
	streak               int
	ownWork, async       bool
	canEdit, canDelegate bool
	seen                 readCoverage // copy, only when a re-read could be refused
}

// stepGuardFor snapshots the turn state for one step. The coverage is copied
// at every step, not only from the refusal threshold on: recordStepHistory
// classifies each call against this very snapshot, and the streak must count a
// re-read as "no progress" from the FIRST one (§1.4 row 4) even when it is too
// early to refuse it. Files are small, so the copy is a plain map rebuild.
func stepGuardFor(p *turnProgress, ownWork, async bool) stepGuard {
	return stepGuard{
		streak:  p.streak,
		ownWork: ownWork,
		async:   async,
		seen:    p.seen.snapshot(),
	}
}

// refuseWaitCall reports whether this step refuses an async wait-only launch.
func (g stepGuard) refuseWaitCall() bool {
	return refuseWait(g.streak, g.ownWork, g.async)
}

// refuseRereadCall reports whether this step refuses a windowed read whose
// every window is already covered.
func (g stepGuard) refuseRereadCall(spans []namedSpan) bool {
	return refuseReread(g.streak) && coveredAll(g.seen, spans)
}

// coveredAll reports whether every window lies inside cov. A nil map (no
// snapshot taken) never covers anything, so no call is ever accused of
// re-reading.
func coveredAll(cov readCoverage, spans []namedSpan) bool {
	if cov == nil || len(spans) == 0 {
		return false
	}
	for _, named := range spans {
		if !cov.covered(named.path, named.span) {
			return false
		}
	}
	return true
}

// applyStep folds one finished step's per-call classes into the turn state and
// returns how many of the step's calls the guard had refused. classes and
// spans are parallel: every read/re-read contributes the windows it asked for,
// added AFTER the classification — which must run against the coverage as of
// the step's START, not against this step's own reads (§1.2).
func (p *turnProgress) applyStep(classes []stepCallClass, spans [][]namedSpan) (refused int) {
	for i, class := range classes {
		switch class {
		case stepCallRead, stepCallReread:
			for _, named := range spans[i] {
				if p.seen == nil {
					p.seen = make(readCoverage)
				}
				p.seen.add(named.path, named.span)
			}
			if class == stepCallReread {
				p.rereads++
			}
		case stepCallWait:
			p.waits++
		case stepCallRefused:
			refused++
		}
	}
	// An act, a message insert or a trim all mean "the file may have
	// changed": coverage is dropped whatever the step also read (§1.2).
	if hasClass(classes, stepCallAct) {
		p.seen.clear()
	}
	p.streak = nextStreak(p.streak, classOfStep(classes))
	p.stopped = stopAfter(p.streak)
	return refused
}

func hasClass(classes []stepCallClass, want stepCallClass) bool {
	for _, class := range classes {
		if class == want {
			return true
		}
	}
	return false
}

// progressGuardMetadataJSON marshals the ClientMetadata tag stamped on a
// refusal; the classifier reads it to mark the call as refused (the tool never
// ran, so it neither progressed nor launched anything).
func progressGuardMetadataJSON(refused string) string {
	var meta progressGuardMetadata
	meta.ProgressGuard.Refused = refused
	data, err := json.Marshal(meta)
	if err != nil {
		return ""
	}
	return string(data)
}

// progressGuardTool wraps one fantasy.AgentTool of one step and refuses, on
// that step's snapshot, the calls the plan §2.2 forbids: an async wait-only
// launch while the session has its own work (or past the threshold), and a
// windowed read whose windows were all already returned in this turn. Nothing
// runs on a refusal — no workLedger.Start, no hook, no permission prompt — the
// model sees an ordinary error result carrying the guard's tag. Info and every
// provider-facing seam are delegated, so the request, the cache prefix and
// R3-1 are unchanged.
type progressGuardTool struct {
	inner fantasy.AgentTool
	guard stepGuard
}

// wrapProgressGuard returns a tool slice with each entry wrapped in a
// progressGuardTool carrying the same step snapshot.
func wrapProgressGuard(tools []fantasy.AgentTool, guard stepGuard) []fantasy.AgentTool {
	out := make([]fantasy.AgentTool, len(tools))
	for i, tool := range tools {
		out[i] = &progressGuardTool{inner: tool, guard: guard}
	}
	return out
}

func (t *progressGuardTool) Info() fantasy.ToolInfo {
	return t.inner.Info()
}

func (t *progressGuardTool) ProviderOptions() fantasy.ProviderOptions {
	return t.inner.ProviderOptions()
}

func (t *progressGuardTool) SetProviderOptions(opts fantasy.ProviderOptions) {
	t.inner.SetProviderOptions(opts)
}

// RestrictedRunAction delegates through the inner tool, exactly as hookedTool
// does, so a tool that exposes it keeps exposing it.
func (t *progressGuardTool) RestrictedRunAction() string {
	provider, ok := t.inner.(interface{ RestrictedRunAction() string })
	if !ok {
		return ""
	}
	return provider.RestrictedRunAction()
}

func (t *progressGuardTool) Run(ctx context.Context, call fantasy.ToolCall) (fantasy.ToolResponse, error) {
	input := fantasy.ToolCallContent{ToolName: call.Name, Input: call.Input}
	if isWaitOnlyCall(input) && t.guard.refuseWaitCall() {
		return t.refuse("wait", refuseWaitText(t.guard, call))
	}
	if spans := readSpans(input); t.guard.refuseRereadCall(spans) {
		return t.refuse("reread", refuseRereadText(t.guard, spans[0]))
	}
	return t.inner.Run(ctx, call)
}

// refuse builds the error result of a blocked call. fantasy copies
// ToolResponse.Metadata straight into ToolResultContent.ClientMetadata
// (agent.go's executeSingleTool, error and success path alike), which is where
// classifyStepCall looks for the tag.
func (t *progressGuardTool) refuse(refused, text string) (fantasy.ToolResponse, error) {
	resp := fantasy.NewTextErrorResponse(text)
	resp.Metadata = progressGuardMetadataJSON(refused)
	return resp, nil
}

// guardStopHint warns, on the last free step, that the next no-progress step
// ends the turn (§2.2). It closes the refusal text: the model reads it after
// the instruction it must follow, not before.
func guardStopHint(streak int) string {
	if streak == inTurnStopAfter-1 {
		return " If this step makes no progress, rush ends the turn now."
	}
	return ""
}

// waitCommandLabel names the refused command as the model wrote it.
func waitCommandLabel(call fantasy.ToolCall) string {
	switch call.Name {
	case tools.BashToolName:
		var params struct {
			Command string `json:"command"`
		}
		if json.Unmarshal([]byte(call.Input), &params) == nil && strings.TrimSpace(params.Command) != "" {
			return "`" + strings.TrimSpace(params.Command) + "`"
		}
	case tools.RunCommandToolName:
		var params struct {
			Program string `json:"program"`
		}
		if json.Unmarshal([]byte(call.Input), &params) == nil && strings.TrimSpace(params.Program) != "" {
			return "`run_command " + strings.TrimSpace(params.Program) + "`"
		}
	}
	return "`" + call.Name + "`"
}

// refuseWaitText is §2.2's wait refusal: the command never waits (it starts as
// a background job and returns at once), so the model must end its turn
// WITHOUT a tool call — a no-op tool call is what keeps the turn going.
func refuseWaitText(g stepGuard, call fantasy.ToolCall) string {
	text := fmt.Sprintf("Refused: %s does not wait — commands here start as background jobs and return at once. "+
		"Your running job/delegation reports by itself as a session message and starts your next turn. "+
		"End your turn now: reply with a short status and NO tool call — "+
		"a no-op tool call keeps the turn going.", waitCommandLabel(call))
	if !g.ownWork {
		text += fmt.Sprintf(" ...and your last %d steps made no progress. To wait for time, call `wakein` and end your turn; "+
			"otherwise continue the task or end your turn with no tool call.", g.streak)
	}
	return text + guardStopHint(g.streak)
}

// refuseRereadText is §2.2's re-read refusal, naming the lines so the model
// can see what it already holds. In orchestrator mode (a set without the edit
// tools but with `agent`) it says how to get the change made instead.
func refuseRereadText(g stepGuard, first namedSpan) string {
	text := fmt.Sprintf("Refused: lines %s of %s were already returned in this turn and are in your context. "+
		"Act on them or end your turn. "+
		"If you are waiting for a running job to write this file, end your turn — its completion wakes you.",
		spanLines(first.span), "`"+first.path+"`")
	if !g.canEdit && g.canDelegate {
		text += " This session cannot edit files (orchestrator mode): delegate the change with the `agent` tool — " +
			"file path, the exact change, how to verify it."
	}
	return text + guardStopHint(g.streak)
}

// spanLines renders a read window as the 1-based inclusive lines it covers.
func spanLines(sp readSpan) string {
	if sp.end <= sp.start {
		return "the whole file"
	}
	if sp.end == math.MaxInt {
		return fmt.Sprintf("%d–the end of the file", sp.start+1)
	}
	return fmt.Sprintf("%d–%d", sp.start+1, sp.end)
}

// guardStopDetails is the assistant finish details of a guard-stopped turn
// (§2.3): what it counted, whether the session had its own work, and what the
// next turn should do.
func guardStopDetails(p turnProgress) string {
	details := fmt.Sprintf("The in-turn progress guard stopped this turn: %d consecutive steps with no progress, "+
		"%d wait call(s) and %d already-read window(s). ", p.streak, p.waits, p.rereads)
	if p.guard.ownWork {
		details += "Your own job/delegation was running; its completion reports by itself and starts your next turn. "
	} else {
		details += "No job of this session was running. "
	}
	if p.guard.canDelegate && !p.guard.canEdit {
		details += "This session cannot edit files (orchestrator mode): delegate the remaining change with the `agent` tool. "
	}
	return details + "End the turn with a short status and NO tool call."
}
