package agent

import (
	"context"

	"charm.land/fantasy"

	"github.com/PHPCraftdream/rush/internal/agent/runtime/exec/turnstep"
)

// The in-turn progress guard (§2 of docs/plans/2026-10-01-in-turn-progress-guard.md).
// The POLICY — thresholds, streak folding, refusal verdicts, refusal/finish
// texts and the read-window math — lives in the leaf package
// (runtime/exec/turnstep). This file keeps the guard's original private root
// state (turnProgress, stepGuard) and the test seams the root tests and
// turnStream require; every shim below delegates to the leaf.
const (
	// inTurnRefuseAfter (S): from this many consecutive no-progress steps a
	// wait launch or an already-covered re-read is refused instead of run.
	inTurnRefuseAfter = turnstep.InTurnRefuseAfter
	// inTurnStopAfter (H): the turn is finished by the guard once the streak
	// reaches this length.
	inTurnStopAfter = turnstep.InTurnStopAfter
)

// InTurnGuardStopTitle opens the assistant finish message of a turn the guard
// stopped. UI/CLI recognize it through IsInTurnGuardStop, exactly as the run
// loop recognizes a coordinator continuation prompt.
const InTurnGuardStopTitle = turnstep.InTurnGuardStopTitle

// IsInTurnGuardStop reports whether title was written by the in-turn progress
// guard (see InTurnGuardStopTitle).
func IsInTurnGuardStop(title string) bool {
	return turnstep.IsInTurnGuardStop(title)
}

// refuseWait delegates the wait-leg verdict to the leaf policy
// (turnstep.RefuseWait).
func refuseWait(streak int, ownWork, async bool) bool {
	return turnstep.RefuseWait(streak, ownWork, async)
}

// refuseReread delegates the re-read leg verdict to the leaf policy
// (turnstep.RefuseReread).
func refuseReread(streak int) bool {
	return turnstep.RefuseReread(streak)
}

// nextStreak delegates the streak fold to the leaf policy (turnstep.NextStreak).
func nextStreak(streak int, class stepCallClass) int {
	return turnstep.NextStreak(streak, class)
}

// stopAfter delegates the hard-stop threshold to the leaf policy
// (turnstep.StopAfter).
func stopAfter(streak int) bool {
	return turnstep.StopAfter(streak)
}

// classOfStep delegates the per-step ranking to the leaf policy
// (turnstep.ClassOfStep).
func classOfStep(classes []stepCallClass) stepCallClass {
	return turnstep.ClassOfStep(classes)
}

func hasClass(classes []stepCallClass, want stepCallClass) bool {
	return turnstep.HasClass(classes, want)
}

// turnProgress is the guard's per-turn state (§1.3). One turn only; a new turn
// starts from zero. Written by prepareStep (coverage resets, guard snapshot)
// and by recordStepHistory (classes, streak, stopped), read by StopWhen — all
// sequential callbacks of one agent.Stream loop, so no lock.
type turnProgress struct {
	streak   int
	seen     readCoverage // line windows already returned in this turn; lazily built
	waits    int          // wait calls, for the finish text
	rereads  int          // re-read calls, for the finish text
	noWrites int          // native edit/multiedit calls proven not to write
	stopped  bool
	guard    stepGuard
}

// leafGuard lifts the root snapshot into the leaf's value type for the leaf
// text builders (they read only the streak/mode fields).
func leafGuard(g stepGuard) turnstep.StepGuard {
	return turnstep.StepGuard{
		Streak:      g.streak,
		OwnWork:     g.ownWork,
		Async:       g.async,
		CanEdit:     g.canEdit,
		CanDelegate: g.canDelegate,
	}
}

// leafTurnProgress lifts the root turn state into the leaf's value type for
// the finish text (which reads no coverage).
func leafTurnProgress(p turnProgress) turnstep.TurnProgress {
	return turnstep.TurnProgress{
		Streak:   p.streak,
		Waits:    p.waits,
		Rereads:  p.rereads,
		NoWrites: p.noWrites,
		Stopped:  p.stopped,
		Guard:    leafGuard(p.guard),
	}
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

// refuseWaitCall reports whether this step refuses an async wait-only launch
// (leaf verdict turnstep.RefuseWait).
func (g stepGuard) refuseWaitCall() bool {
	return turnstep.RefuseWait(g.streak, g.ownWork, g.async)
}

// refuseRereadCall reports whether this step refuses a windowed read whose
// every window is already covered (leaf verdict turnstep.RefuseReread plus the
// per-path coverage check).
func (g stepGuard) refuseRereadCall(spans []namedSpan) bool {
	return refuseReread(g.streak) && coveredAll(g.seen, spans)
}

// coveredAll reports whether every window lies inside cov, short-circuiting on
// the first uncovered span. A nil map (no snapshot taken) never covers
// anything, so no call is ever accused of re-reading.
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
		case stepCallNoWrite:
			p.noWrites++
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

// progressGuardMetadataJSON marshals the ClientMetadata tag stamped on a
// refusal; the classifier reads it to mark the call as refused (the tool never
// ran, so it neither progressed nor launched anything). The encode itself
// lives in the leaf package (runtime/exec/turnstep.ProgressGuardMetadataJSON).
func progressGuardMetadataJSON(refused string) string {
	return turnstep.ProgressGuardMetadataJSON(refused)
}

// progressGuardTool wraps one fantasy.AgentTool of one step and refuses, on
// that step's snapshot, the calls the plan §2.2 forbids: an async wait-only
// launch while the session has its own work (or past the threshold), and a
// windowed read whose windows were all already returned in this turn. Nothing
// runs on a refusal — no batch launch, no hook, no permission prompt — the
// model sees an ordinary error result carrying the guard's tag. The refusal
// VERDICTS and TEXTS come from the leaf policy; the wrapper itself stays in
// the root so policy_leaf.go's leaf-execution assertion can unwrap it and the
// test seams keep their concrete type.
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

// refuseWaitText delegates the wait refusal text to the leaf
// (turnstep.RefuseWaitText).
func refuseWaitText(g stepGuard, call fantasy.ToolCall) string {
	return turnstep.RefuseWaitText(leafGuard(g), call)
}

// refuseRereadText delegates the re-read refusal text to the leaf
// (turnstep.RefuseRereadText).
func refuseRereadText(g stepGuard, first namedSpan) string {
	return turnstep.RefuseRereadText(leafGuard(g), turnstep.NamedSpan{Path: first.path, Span: toLeafSpan(first.span)})
}

// guardStopDetails delegates the finish details to the leaf
// (turnstep.GuardStopDetails).
func guardStopDetails(p turnProgress) string {
	return turnstep.GuardStopDetails(leafTurnProgress(p))
}
