// Timeout and watchdog-semantics tests: SetTimeoutOptions, the
// watchdogFinishMessage/watchdogToolResultMessage cause mappings, and
// effectiveToolMaxDuration / effectiveToolCleanupGrace precedence.

package agent

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestSetTimeoutOptions verifies that SetTimeoutOptions populates the internal
// fields on the sessionAgent. Fork patch: batch 8.
func TestSetTimeoutOptions(t *testing.T) {
	env := testEnv(t)
	sa := testSessionAgent(env, nil, nil, "test prompt")
	agent := sa.(*sessionAgent)

	require.False(t, agent.timeoutExtendsOnProgress, "default should be false")
	require.Zero(t, agent.timeoutHardCap, "default should be 0")

	sa.SetTimeoutOptions(true, 30*time.Second)

	assert.True(t, agent.timeoutExtendsOnProgress)
	assert.Equal(t, 30*time.Second, agent.timeoutHardCap)
}

// TestWatchdogFinishMessage_HardCapDoesNotBlameProviderOrTool is the
// regression test for task #223: commit 77c1104a fixed the BOOLEAN passed
// to onFire (a hard-cap fire with a tool in flight no longer wrongly
// claimed toolTimeout=true), but until this change agent.go's onFire
// callback still only had two message branches — "Tool timeout" and
// "Stream stalled" — so a genuine --timeout-hard-cap fire (with or without
// a tool in flight) always fell into the "Stream stalled" branch, which
// falsely blames the PROVIDER for silence and cites idleTimeout, a
// completely different configured duration than the one that actually
// fired. This proves watchdogFinishMessage — the pure mapping the runTurn
// AddFinish call now delegates to — gives causeHardCap its own title and
// body that (a) is NOT "Stream stalled", (b) does NOT mention idleTimeout's
// duration or the word "Provider", and (c) DOES cite the actual configured
// hardCap duration that fired.
func TestWatchdogFinishMessage_HardCapDoesNotBlameProviderOrTool(t *testing.T) {
	const toolMaxDuration = 45 * time.Minute
	const hardCap = 20 * time.Minute
	const idleTimeout = 3 * time.Minute
	const provider = "anthropic"

	title, body := watchdogFinishMessage(causeHardCap, toolMaxDuration, hardCap, idleTimeout, provider)

	assert.Equal(t, "Turn timeout", title, "hard-cap fire must get its own title, not reuse Tool timeout/Stream stalled")
	assert.NotEqual(t, streamStalledFinishTitle, title)
	assert.NotContains(t, body, "Provider", "must not blame the provider for a wall-clock ceiling it had nothing to do with")
	assert.NotContains(t, body, provider, "must not name the provider at all — it did not cause this")
	assert.NotContains(t, body, idleTimeout.String(), "must not cite idleTimeout — that's not the duration that fired")
	assert.NotContains(t, body, toolMaxDuration.String(), "must not cite toolMaxDuration — no specific tool caused this")
	assert.Contains(t, body, hardCap.String(), "must cite the actual --timeout-hard-cap duration that fired")
}

// TestWatchdogFinishMessage_AllThreeCausesDistinct proves the three causes
// produce three genuinely different titles, so the resulting finish message
// always names the actual mechanism (tool / turn wall-clock / provider
// idle) that triggered the watchdog, never collapsing two causes into one
// message.
func TestWatchdogFinishMessage_AllThreeCausesDistinct(t *testing.T) {
	const toolMaxDuration = 45 * time.Minute
	const hardCap = 20 * time.Minute
	const idleTimeout = 3 * time.Minute
	const provider = "anthropic"

	toolTitle, toolBody := watchdogFinishMessage(causeToolTimeout, toolMaxDuration, hardCap, idleTimeout, provider)
	hardCapTitle, _ := watchdogFinishMessage(causeHardCap, toolMaxDuration, hardCap, idleTimeout, provider)
	idleTitle, idleBody := watchdogFinishMessage(causeIdleStall, toolMaxDuration, hardCap, idleTimeout, provider)

	assert.Equal(t, "Tool timeout", toolTitle)
	assert.Equal(t, "Turn timeout", hardCapTitle)
	assert.Equal(t, streamStalledFinishTitle, idleTitle)
	assert.NotEqual(t, toolTitle, hardCapTitle)
	assert.NotEqual(t, hardCapTitle, idleTitle)
	assert.NotEqual(t, toolTitle, idleTitle)

	assert.Contains(t, toolBody, toolMaxDuration.String(), "tool-timeout body must cite toolMaxDuration")
	assert.Contains(t, idleBody, idleTimeout.String(), "idle-stall body must cite idleTimeout")
	assert.Contains(t, idleBody, provider, "idle-stall body must name the provider — it genuinely is the cause here")
}

// TestWatchdogFinishMessage_IdleStallTitleMatchesRetryConstant is the
// regression test for task #236: "Stream stalled" used to be hardcoded
// independently in watchdogFinishMessage's causeIdleStall branch AND in
// coordinator.go's streamStalledFinishTitle constant (the value
// shouldRetryTurn's finish-part matching compares against to decide
// whether a stall qualifies for transparent streamStallRetriesDefault
// retry). Two independently-maintained copies of the same string meant a
// future reword of one — e.g. for clarity or i18n — could silently break
// the retry match with no test catching it, since every existing test
// compared watchdogFinishMessage's output against ITS OWN separate
// hardcoded literal rather than against the actual constant coordinator.go
// reads. This test cross-references the two directly: if
// watchdogFinishMessage's idle-stall title and streamStalledFinishTitle
// ever diverge again, this assertion — not a hardcoded string — is what
// catches it.
func TestWatchdogFinishMessage_IdleStallTitleMatchesRetryConstant(t *testing.T) {
	const toolMaxDuration = 45 * time.Minute
	const hardCap = 20 * time.Minute
	const idleTimeout = 3 * time.Minute
	const provider = "anthropic"

	title, _ := watchdogFinishMessage(causeIdleStall, toolMaxDuration, hardCap, idleTimeout, provider)

	assert.Equal(t, streamStalledFinishTitle, title, "watchdogFinishMessage's idle-stall title must match coordinator.go's streamStalledFinishTitle exactly, or transparent stall-retry silently stops matching")
}

// TestWatchdogToolResultMessage_ReflectsRealCause is the regression test for
// task #227 (same root-cause family as task #223's watchdogFinishMessage
// split): the synthetic tool-result content built for the model when a tool
// call is still unfinished at watchdog-fire time used to unconditionally
// read "the provider stream stalled for >idleTimeout" regardless of the
// real cause — so even after 7b48f75a fixed the HUMAN-facing finish
// message, the MODEL was still told the wrong story on a genuine hard-cap
// or tool-timeout fire. This proves watchdogToolResultMessage — what
// agent.go's runTurn now delegates to instead of a hardcoded string — gives
// each cause distinct, correctly-attributed wording:
//   - causeHardCap must NOT mention "provider" or cite idleTimeout; it must
//     cite the actual hardCap duration.
//   - causeToolTimeout must NOT mention "provider" or cite idleTimeout; it
//     must cite the actual toolMaxDuration.
//   - causeIdleStall is the only cause allowed to blame the provider and
//     cite idleTimeout.
func TestWatchdogToolResultMessage_ReflectsRealCause(t *testing.T) {
	const toolMaxDuration = 45 * time.Minute
	const hardCap = 20 * time.Minute
	const idleTimeout = 3 * time.Minute
	const provider = "anthropic"

	t.Run("hard cap does not blame the provider or cite idleTimeout", func(t *testing.T) {
		msg := watchdogToolResultMessage(causeHardCap, toolMaxDuration, hardCap, idleTimeout, provider)
		assert.NotContains(t, msg, provider, "must not name the provider at all — it did not cause this")
		assert.NotContains(t, msg, idleTimeout.String(), "must not cite idleTimeout — that's not the duration that fired")
		assert.Contains(t, msg, hardCap.String(), "must cite the actual --timeout-hard-cap duration that fired")
	})

	t.Run("tool timeout does not blame the provider or cite idleTimeout", func(t *testing.T) {
		msg := watchdogToolResultMessage(causeToolTimeout, toolMaxDuration, hardCap, idleTimeout, provider)
		assert.NotContains(t, msg, provider, "must not name the provider at all — it did not cause this")
		assert.NotContains(t, msg, idleTimeout.String(), "must not cite idleTimeout — that's not the duration that fired")
		assert.Contains(t, msg, toolMaxDuration.String(), "must cite the actual toolMaxDuration that fired")
	})

	t.Run("idle stall is the only cause that blames the provider", func(t *testing.T) {
		msg := watchdogToolResultMessage(causeIdleStall, toolMaxDuration, hardCap, idleTimeout, provider)
		assert.Contains(t, msg, idleTimeout.String(), "idle-stall must cite idleTimeout — the provider genuinely is the cause here")
	})

	t.Run("all three causes produce distinct messages", func(t *testing.T) {
		hardCapMsg := watchdogToolResultMessage(causeHardCap, toolMaxDuration, hardCap, idleTimeout, provider)
		toolMsg := watchdogToolResultMessage(causeToolTimeout, toolMaxDuration, hardCap, idleTimeout, provider)
		idleMsg := watchdogToolResultMessage(causeIdleStall, toolMaxDuration, hardCap, idleTimeout, provider)
		assert.NotEqual(t, hardCapMsg, toolMsg)
		assert.NotEqual(t, toolMsg, idleMsg)
		assert.NotEqual(t, hardCapMsg, idleMsg)
	})
}

// TestEffectiveToolMaxDuration_Default proves the unified cap: with no
// explicit operator override, every run — sub-agent or top-level,
// orchestrating a delegation or not — gets exactly toolExecutionMaxDefault.
// There used to be a separate, larger cap reserved for a top-level run
// orchestrating a worker delegation; that split was removed (see
// toolExecutionMaxDefault's doc) because it produced its own false cutoffs
// — a sub-agent's OWN plain tool call (a slow build/test inside its turn)
// still only got the short cap. One value, no special-casing.
func TestEffectiveToolMaxDuration_Default(t *testing.T) {
	env := testEnv(t)
	sa := testSessionAgent(env, nil, nil, "test prompt")
	agent := sa.(*sessionAgent)

	require.Zero(t, agent.toolMaxDuration, "no explicit operator override by default")

	got := agent.effectiveToolMaxDuration()
	assert.Equal(t, toolExecutionMaxDefault, got,
		"every run must get the same never-freeze backstop by default, regardless of orchestrator/sub-agent status")
}

// TestEffectiveToolMaxDuration_OperatorOverrideWins verifies
// Options.StreamToolTimeoutSeconds (a.toolMaxDuration) always wins over the
// built-in default, in both directions.
func TestEffectiveToolMaxDuration_OperatorOverrideWins(t *testing.T) {
	t.Run("operator override shorter than the default wins", func(t *testing.T) {
		env := testEnv(t)
		sa := testSessionAgent(env, nil, nil, "test prompt")
		agent := sa.(*sessionAgent)
		agent.toolMaxDuration = 5 * time.Minute // shorter than the default

		got := agent.effectiveToolMaxDuration()
		assert.Equal(t, 5*time.Minute, got,
			"an explicit operator override shorter than the default must still win")
	})

	t.Run("operator override longer than the default wins", func(t *testing.T) {
		env := testEnv(t)
		sa := testSessionAgent(env, nil, nil, "test prompt")
		agent := sa.(*sessionAgent)
		agent.toolMaxDuration = 90 * time.Minute // longer than toolExecutionMaxDefault

		got := agent.effectiveToolMaxDuration()
		assert.Equal(t, 90*time.Minute, got,
			"an explicit operator override longer than the default must still win")
	})
}

// TestEffectiveToolCleanupGrace_TopLevelGetsDefault proves a top-level
// (non-sub-agent) session — the one whose watchdog may be waiting on an
// `agent`-tool delegation and needs to give the child's own watchdog a
// chance to fire first — gets toolCleanupGraceDefault with no explicit
// override configured.
func TestEffectiveToolCleanupGrace_TopLevelGetsDefault(t *testing.T) {
	env := testEnv(t)
	sa := testSessionAgent(env, nil, nil, "test prompt")
	agent := sa.(*sessionAgent)
	agent.isSubAgent = false

	require.Zero(t, agent.toolCleanupGrace, "no explicit operator override by default")

	got := agent.effectiveToolCleanupGrace()
	assert.Equal(t, toolCleanupGraceDefault, got,
		"a top-level session must get the default grace so a nested child watchdog gets a chance to fire first")
}

// TestEffectiveToolCleanupGrace_SubAgentGetsNoGrace is the task #205
// regression test: it proves the asymmetry that actually closes the
// parent/child cancellation race. Task #200's original fix applied
// toolCleanupGraceDefault symmetrically to BOTH the parent and the child
// sub-agent's own watchdog, which cancels out of the "child fires before
// parent" inequality algebraically and leaves the parent's unconditional
// head start (it starts timing at OnToolCall, before the child's own turn
// has even begun executing) as the sole deciding factor — so the parent
// still always won. A sub-agent can never itself be waiting on a further
// nested `agent`-tool delegation (excluded from workerToolNames for
// sub-agents), so it is always the deepest watchdog in the chain and must
// get NO grace: it should fire at bare toolMaxDuration, strictly before the
// parent's toolMaxDuration+90s.
func TestEffectiveToolCleanupGrace_SubAgentGetsNoGrace(t *testing.T) {
	env := testEnv(t)
	sa := testSessionAgent(env, nil, nil, "test prompt")
	agent := sa.(*sessionAgent)
	agent.isSubAgent = true

	require.Zero(t, agent.toolCleanupGrace, "no explicit operator override by default")

	got := agent.effectiveToolCleanupGrace()
	assert.Zero(t, got,
		"a sub-agent's own watchdog must get no grace — it is always the deepest watchdog "+
			"in the chain and must fire at bare toolMaxDuration, strictly before the parent's "+
			"toolMaxDuration+90s, to win the parent/child cancellation race")
}

// TestEffectiveToolCleanupGrace_ExplicitOverrideWinsForSubAgent verifies the
// explicit operator/test override (SessionAgentOptions.ToolCleanupGrace)
// still wins even for a sub-agent that opts back into a non-zero grace —
// the isSubAgent-based default only applies when no override is set.
func TestEffectiveToolCleanupGrace_ExplicitOverrideWinsForSubAgent(t *testing.T) {
	env := testEnv(t)
	sa := testSessionAgent(env, nil, nil, "test prompt")
	agent := sa.(*sessionAgent)
	agent.isSubAgent = true
	agent.toolCleanupGrace = 5 * time.Second

	got := agent.effectiveToolCleanupGrace()
	assert.Equal(t, 5*time.Second, got,
		"an explicit override must win even for a sub-agent session")
}

// TestEffectiveToolCleanupGrace_ExplicitOverrideWinsForTopLevel verifies the
// explicit operator/test override also wins for a top-level session,
// consistent with effectiveToolMaxDuration's override precedence.
func TestEffectiveToolCleanupGrace_ExplicitOverrideWinsForTopLevel(t *testing.T) {
	env := testEnv(t)
	sa := testSessionAgent(env, nil, nil, "test prompt")
	agent := sa.(*sessionAgent)
	agent.isSubAgent = false
	agent.toolCleanupGrace = 5 * time.Second

	got := agent.effectiveToolCleanupGrace()
	assert.Equal(t, 5*time.Second, got,
		"an explicit override must win even for a top-level session")
}

// TestEffectiveToolCleanupGrace_ChildWinsCancellationRace_EarlyWedge proves
// the race is resolved for the case toolCleanupGraceDefault actually
// guarantees: the child's tool call wedges EARLY — within
// toolCleanupGraceDefault of the parent delegating — e.g. the child hangs
// during its own init/DB preamble, or on close to the first tool it runs.
// `delta` here models pure parent/child watchdog START skew (init + DB
// preamble before the child's watchdog even begins counting), which really
// is sub-second to low seconds in practice — NOT how long the child worked
// before hitting its stuck tool. See the companion test
// _LateWedgeParentStillWinsKnownLimitation for the case this grace does
// NOT cover, and toolCleanupGraceDefault's doc comment in agent.go for the
// full, honest scope (found overstated by an @oh review of this fix; the
// doc and this test were both corrected together rather than leaving a
// passing-but-misleading test behind).
func TestEffectiveToolCleanupGrace_ChildWinsCancellationRace_EarlyWedge(t *testing.T) {
	env := testEnv(t)

	parentSA := testSessionAgent(env, nil, nil, "parent prompt")
	parent := parentSA.(*sessionAgent)
	parent.isSubAgent = false

	childSA := testSessionAgent(env, nil, nil, "child prompt")
	child := childSA.(*sessionAgent)
	child.isSubAgent = true

	const toolMaxDuration = 45 * time.Minute
	parent.toolMaxDuration = toolMaxDuration
	child.toolMaxDuration = toolMaxDuration

	// Realistic parent/child WATCHDOG-START skew: the child's own watchdog
	// starts only once its turn actually begins executing (after init and
	// the DB preamble) — strictly later than the parent's OnToolCall. This
	// models a child whose stuck tool is (at latest) the first one it runs,
	// not one it hits after working productively for a while.
	for _, delta := range []time.Duration{0, time.Second, 10 * time.Second, 60 * time.Second} {
		delegationStart := time.Now()
		childToolStart := delegationStart.Add(delta)

		parentFireAt := delegationStart.Add(parent.effectiveToolMaxDuration() + parent.effectiveToolCleanupGrace())
		childFireAt := childToolStart.Add(child.effectiveToolMaxDuration() + child.effectiveToolCleanupGrace())

		assert.Truef(t, childFireAt.Before(parentFireAt),
			"delta=%s: child must fire (%s) strictly before parent (%s) so the child's own "+
				"watchdog can unwind cleanly before the parent cancels genCtx out from under it",
			delta, childFireAt, parentFireAt)
	}
}

// TestEffectiveToolCleanupGrace_LateWedgeParentStillWinsKnownLimitation
// documents, as a passing (not failing) test, the case
// toolCleanupGraceDefault does NOT cover: a child that works productively
// for a while — its own watchdog resetting on every tool-call boundary,
// same as the parent's — and only wedges LATE, deep into its turn, well
// past toolCleanupGraceDefault after the parent delegated. In that case the
// parent's watchdog (which counts from the original OnToolCall, not from
// the child's last progress) still fires and force-cancels the delegation
// FIRST, exactly like before task #205's fix. This is accepted, not a bug:
// the parent still terminates correctly and task #197 already made the
// cost-transfer step cancel-immune, so the only loss on this path is
// diagnostic quality (the child's own finish part / goroutine dump), not
// correctness. A structural fix would require the child to push progress
// signals that reset the PARENT's watchdog too — not implemented. This test
// exists so a future reader sees an explicit, intentional boundary instead
// of silently discovering it in production.
func TestEffectiveToolCleanupGrace_LateWedgeParentStillWinsKnownLimitation(t *testing.T) {
	env := testEnv(t)

	parentSA := testSessionAgent(env, nil, nil, "parent prompt")
	parent := parentSA.(*sessionAgent)
	parent.isSubAgent = false

	childSA := testSessionAgent(env, nil, nil, "child prompt")
	child := childSA.(*sessionAgent)
	child.isSubAgent = true

	const toolMaxDuration = 45 * time.Minute
	parent.toolMaxDuration = toolMaxDuration
	child.toolMaxDuration = toolMaxDuration

	// The child worked productively for well over toolCleanupGraceDefault
	// (90s) before its own watchdog even started timing the tool call that
	// eventually wedges — e.g. several minutes of real edits/tests before a
	// hung bash command. skew (watchdog-start delay) is realistic and
	// small; workDuration (prior productive work) is what pushes the
	// child's effective fire time past the parent's.
	const skew = 2 * time.Second
	const workDuration = 5 * time.Minute
	delegationStart := time.Now()
	childToolStart := delegationStart.Add(skew).Add(workDuration)

	parentFireAt := delegationStart.Add(parent.effectiveToolMaxDuration() + parent.effectiveToolCleanupGrace())
	childFireAt := childToolStart.Add(child.effectiveToolMaxDuration() + child.effectiveToolCleanupGrace())

	assert.Truef(t, parentFireAt.Before(childFireAt),
		"known limitation: for a late wedge (skew+workDuration=%s, past toolCleanupGraceDefault=%s), "+
			"the parent (%s) still fires before the child (%s) — this is the case toolCleanupGraceDefault "+
			"does not cover, see its doc comment in agent.go",
		skew+workDuration, toolCleanupGraceDefault, parentFireAt, childFireAt)
}
