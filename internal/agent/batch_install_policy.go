package agent

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/PHPCraftdream/rush/internal/session"
)

// R11F round-2 (P1-4 / P1-5b / P1-5c) helpers: typed bridge-invocation
// context stamping, the per-build policy snapshot for the private-fetch
// arming point, and the live-descendant reader over the R10a store. New
// logic lives in this file; sibling files carry only stated minimal hunks.

type batchAgentLifecycle struct {
	BindChildSession func(ctx context.Context, childSessionID string) error
	NoChildSelected  func()
}

type batchAgentLifecycleContextKey struct{}

type (
	batchAgentQuestionContext    struct{ Depth int }
	batchAgentQuestionContextKey struct{}
)

type batchAgentQuestionBinding struct {
	Ask func(context.Context, int, string) (json.RawMessage, error)
}
type batchAgentQuestionBindingKey struct{}

func withBatchAgentLifecycle(ctx context.Context, lifecycle batchAgentLifecycle) context.Context {
	return context.WithValue(ctx, batchAgentLifecycleContextKey{}, lifecycle)
}

func withBatchAgentQuestionContext(ctx context.Context, depth int) context.Context {
	return context.WithValue(ctx, batchAgentQuestionContextKey{}, batchAgentQuestionContext{Depth: depth})
}

func batchAgentQuestionDepth(ctx context.Context) int {
	if ctx == nil {
		return 0
	}
	v, _ := ctx.Value(batchAgentQuestionContextKey{}).(batchAgentQuestionContext)
	return v.Depth
}

func withBatchAgentQuestionBinding(ctx context.Context, b batchAgentQuestionBinding) context.Context {
	return context.WithValue(ctx, batchAgentQuestionBindingKey{}, b)
}

func batchAgentQuestionBindingFrom(ctx context.Context) (batchAgentQuestionBinding, bool) {
	if ctx == nil {
		return batchAgentQuestionBinding{}, false
	}
	b, ok := ctx.Value(batchAgentQuestionBindingKey{}).(batchAgentQuestionBinding)
	return b, ok && b.Ask != nil
}

// batchBridgeInvocation is the TRUSTED enclosing invocation the CLI bridge
// binds to (P1-4): the pinned registry identity and message id of the
// delegation whose stream is running. It is stamped ONLY by the agent-family
// driver on the delegation leaf's run context — never derived from session
// state or user input.
type batchBridgeInvocation struct {
	RegistryID string
	MessageID  string
}

type batchBridgeInvocationKey struct{}

// withBatchBridgeInvocation stamps one trusted invocation onto ctx.
func withBatchBridgeInvocation(ctx context.Context, inv batchBridgeInvocation) context.Context {
	return context.WithValue(ctx, batchBridgeInvocationKey{}, inv)
}

// batchBridgeInvocationFrom reads the stamped invocation, ok=false when ctx
// carries none (the coordinator's own non-batch path → legacy no-bridge).
func batchBridgeInvocationFrom(ctx context.Context) (batchBridgeInvocation, bool) {
	inv, _ := ctx.Value(batchBridgeInvocationKey{}).(batchBridgeInvocation)
	if ctx == nil || inv.RegistryID == "" {
		return batchBridgeInvocation{}, false
	}
	return inv, true
}

// batchArmingPolicyNote is the P1-5c arming-point policy contract, recorded
// here because the honest finding is negative at BOTH arming points:
//
// The BatchInvocation construction sites (newBatchWorkToolBuild for the
// coordinator family, armPrivateFetchIngress for the private-fetch family)
// carry only RegistryID/RestrictedToolset plus the per-call values filled at
// call time (invokeBatchWithCallContext). The three remaining transport
// fields CANNOT be recovered as the serializable specs the transport wants:
//
//   - FolderScope: CallOptions.FolderScope is the COMPILED
//     permission.FolderScope (permission.BuildFolderScope output); the
//     compiled type keeps no FolderScopeSpec, and permission.FolderScopeSpec
//     cannot be reconstructed from it. At the coordinator arming boundary
//     there is additionally no call context at all — finishToolAssembly
//     receives neither ctx nor CallOptions; a per-call folder scope is
//     already expressed there by toolset composition
//     (applyCallFolderScope strips the fs_* tools a scope does not grant),
//     and the surviving fs_* tools hold their compiled scope internally.
//   - RunAllowlist: the restricted-run allowlist is session/message-keyed
//     state on permission.SessionRunAllowlistManager (session call data:
//     session.SessionAgentCallData.RunAllowlistSpec). It resolves inside the
//     pinned restricted-run wrappers, which run on the executor's REBUILT
//     context — batchLeafRunContext stamps the payload's session id and
//     message id, so the gate resolves the same allowlist entry the
//     in-process path would. A static spec snapshot would go stale across
//     the call's lifetime.
//   - CustomDisk: CallOptions.DiskProvider is arbitrary in-process Go code
//     with no serializable identity. Flag-on ingress now stamps trusted
//     provider presence and refuses it before attach or Submit; Validate
//     refuses non-empty trimmed CustomDisk, and executor admission repeats
//     the marked-payload refusal. Nil providers remain admitted.
//
// Consequently the arming points keep the pinned RegistryID/
// RestrictedToolset identity, and the executor side (P1-5c executor hunks in
// batch_exec_generic/shell/argv) builds the leaf run context EXCLUSIVELY via
// batchLeafRunContext so any PINNED Policy value (FolderScope, RunAllowlist,
// CustomDisk, message id, image support, model name) wins over whatever the
// Start context carries. A future arming point that obtains genuine specs
// can set them on the template without touching the executors.
const batchArmingPolicyNote = "Flag-on work refuses any trusted CallOptions DiskProvider, including explicit OSDisk and non-comparable providers, and any non-empty trimmed CustomDisk before attach, Submit, or leaf execution; executor admission rejects marked payloads. Nil providers remain admitted."

// batchCoordinatorDescendants is the R10a descendant reader for the flagged
// coordinator driver (P1-5b): it reports live descendants of the invocation
// through the BatchLiveReader over the batch store. A nil reader, an error
// or an INCOMPLETE read keeps the scope open (complete=false) — never an
// optimistic true.
type batchCoordinatorDescendants struct {
	live *session.BatchLiveReader
	// child (package S, jb/psc) extends the read with the delegation
	// child's completion-reaction scope; nil keeps the R10a behavior.
	child BatchChildReactionFacts
}

func (d batchCoordinatorDescendants) DescendantsLive(ctx context.Context, req batchAgentRunRequest) (bool, bool) {
	if req.ChildSessionID == "" {
		return false, false
	}
	if d.live == nil {
		return false, false
	}
	res := d.live.BatchLiveForOwner(ctx, req.ChildSessionID)
	return pscCombineDescendantFacts(ctx, d.child, req.ChildSessionID, res.HasLive(), res.Complete)
}

// BatchChildReactionFacts is the delegation child's completion-reaction
// seam (package S, jb/psc; review consultation 4, §4.3 items 2-3). The
// batch delegation's scope used to close on live descendants alone, so a
// child whose shell was terminal but which still owed — or was still
// running — its own completion reaction let the parent settle with a
// yielded/pre-result answer. Every answer is conservative: a failed or
// incomplete read reports the scope OPEN, never idle.
type BatchChildReactionFacts interface {
	// ChildReactionScopeOpen reports whether childSessionID still owes an
	// ACTIONABLE or PACED completion reaction (CLIScope's
	// WorkOpen/DrainOwed/DrainPaced predicate — the same one the legacy
	// release path used) or owns an active reaction turn. Deferred (held
	// question, Stop, policy) and Stuck debt deliberately reports
	// open=false so genuinely non-actionable debt cannot hang a
	// delegation forever. complete=false marks an unreadable read.
	ChildReactionScopeOpen(ctx context.Context, childSessionID string) (open bool, complete bool)
	// ChildFinalOutcome refreshes the captured initial completion with
	// the child's final outcome for THIS delegation (incarnation-exact
	// child identity; reaction-failure markers take priority, then the
	// child's last finished assistant message, a held question delivered
	// as a paused sub-agent). ok=false keeps the captured initial
	// completion.
	ChildFinalOutcome(ctx context.Context, childSessionID string, initial AsyncCompletion) (AsyncCompletion, bool)
}

// pscCombineDescendantFacts merges one live-descendant read with the
// child's reaction scope. The child read can only widen liveness and
// never repairs incompleteness: the delegation stays open while either
// half is open or either half is unreadable.
func pscCombineDescendantFacts(ctx context.Context, child BatchChildReactionFacts, childSessionID string, live, complete bool) (bool, bool) {
	if child == nil || childSessionID == "" {
		return live, complete
	}
	open, childComplete := child.ChildReactionScopeOpen(ctx, childSessionID)
	return live || open, complete && childComplete
}

// pscChildScopeOpen is the arbiter-classified debt predicate, equal to the
// legacy release path's (workLedger.childScopeOpenAcrossProcesses):
// running work on a live host, debt a Drain may run now (Owed), or debt
// held by or paced after a reaction attempt (Paced).
func pscChildScopeOpen(st CLIScopeState) bool {
	return st.WorkOpen || st.Drain == DrainOwed || st.Drain == DrainPaced
}

// pscErrChildScopeUnavailable: no bound coordinator answered the child's
// CLIScope read. The caller treats it as an incomplete read (stay open).
var pscErrChildScopeUnavailable = errors.New("batch child reaction scope: coordinator unavailable")

// pscChildReactionFacts is the production BatchChildReactionFacts over two
// injected functions.
type pscChildReactionFacts struct {
	scope   func(ctx context.Context, childSessionID string) (CLIScopeState, error)
	outcome func(ctx context.Context, childSessionID string, initial AsyncCompletion) (AsyncCompletion, bool)
}

func (f pscChildReactionFacts) ChildReactionScopeOpen(ctx context.Context, childSessionID string) (bool, bool) {
	if f.scope == nil || childSessionID == "" {
		return false, false
	}
	st, err := f.scope(ctx, childSessionID)
	if err != nil {
		return true, false
	}
	return pscChildScopeOpen(st), true
}

func (f pscChildReactionFacts) ChildFinalOutcome(ctx context.Context, childSessionID string, initial AsyncCompletion) (AsyncCompletion, bool) {
	if f.outcome == nil || childSessionID == "" {
		return initial, false
	}
	return f.outcome(ctx, childSessionID, initial)
}

// PscCoordinatorChildReactionFactsLate builds the production child
// reaction facts for an App-level wiring site whose coordinator is
// assigned AFTER the batch install is built: get is re-read on every
// call, so a not-yet-bound coordinator reports the conservative answers
// (scope open/incomplete; initial outcome kept) until it is. The outcome
// half reuses coordinator.refreshSubAgentCompletion — the legacy release
// path's refresh — so reaction-failure markers, the paused-question
// delivery and the empty-final-text notice keep their existing
// semantics.
func PscCoordinatorChildReactionFactsLate(get func() Coordinator) BatchChildReactionFacts {
	if get == nil {
		return nil
	}
	return pscChildReactionFacts{
		scope: func(ctx context.Context, childSessionID string) (CLIScopeState, error) {
			c := get()
			if c == nil {
				return CLIScopeState{}, pscErrChildScopeUnavailable
			}
			src, ok := c.(ReactionDebtSource)
			if !ok {
				return CLIScopeState{}, pscErrChildScopeUnavailable
			}
			return src.CLIScope(ctx, childSessionID)
		},
		outcome: func(ctx context.Context, childSessionID string, initial AsyncCompletion) (AsyncCompletion, bool) {
			cc, ok := get().(*coordinator)
			if !ok || cc == nil {
				return initial, false
			}
			return cc.refreshSubAgentCompletion(childSessionID, initial), true
		},
	}
}

// CoordinatorBatchAgentDepsWithLive builds the flagged agent-family seams
// with the R10a live descendant reader wired (P1-5b). The app install passes
// the reader over the same writer pool the batch store uses. Nil keeps the
// fail-closed incomplete read.
func CoordinatorBatchAgentDepsWithLive(live *session.BatchLiveReader) BatchAgentDeps {
	return batchAgentDeps{
		Runs:        batchCoordinatorAgentRuns{},
		Descendants: batchCoordinatorDescendants{live: live},
	}
}

// CoordinatorBatchAgentDepsWithChildScope is the flagged install seam with
// the child reaction-scope reader wired (package S, jb/psc). Poll is
// raised from the 10ms default: each poll now runs the child's full
// CLIScope read (live scope, debt, arbiter verdict), which must not run
// at 100Hz.
func CoordinatorBatchAgentDepsWithChildScope(live *session.BatchLiveReader, child BatchChildReactionFacts) BatchAgentDeps {
	return batchAgentDeps{
		Runs:        batchCoordinatorAgentRuns{},
		Descendants: batchCoordinatorDescendants{live: live, child: child},
		ChildFacts:  child,
		Poll:        250 * time.Millisecond,
	}
}
