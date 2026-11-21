package state

import (
	"context"
	"errors"
	"time"

	"github.com/PHPCraftdream/rush/internal/agent/runtime/batch/batchexec/batchdispatch"
	"github.com/PHPCraftdream/rush/internal/session/batch/sessionbatchread"
)

// ArmingPolicyNote records the original P1-5c arming-point contract; current
// disk-provider admission is implemented by ingress, not by this note.
// At the original arming points:
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
//     core.SessionAgentCallData.RunAllowlistSpec). It resolves inside the
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
const ArmingPolicyNote = "Flag-on work refuses any trusted CallOptions DiskProvider, including explicit OSDisk and non-comparable providers, and any non-empty trimmed CustomDisk before attach, Submit, or leaf execution; executor admission rejects marked payloads. Nil providers remain admitted."

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
	ChildFinalOutcome(ctx context.Context, childSessionID string, initial batchdispatch.AsyncCompletion) (batchdispatch.AsyncCompletion, bool)
}

// CombineDescendantFacts merges one live-descendant read with the
// child's reaction scope. The child read can only widen liveness and
// never repairs incompleteness: the delegation stays open while either
// half is open or either half is unreadable.
func CombineDescendantFacts(ctx context.Context, child ReactionScopeReader, childSessionID string, live, complete bool) (bool, bool) {
	if child == nil || childSessionID == "" {
		return live, complete
	}
	open, childComplete := child.ChildReactionScopeOpen(ctx, childSessionID)
	return live || open, complete && childComplete
}

// ReactionScopeReader	is the portion of the child facts used during polling.
type ReactionScopeReader interface {
	ChildReactionScopeOpen(ctx context.Context, childSessionID string) (open, complete bool)
}

// Descendants combines the live batch rows with the child reaction scope.
type Descendants struct {
	Live  *sessionbatchread.BatchLiveReader
	Child ReactionScopeReader
}

func (d Descendants) DescendantsLive(ctx context.Context, childSessionID string) (bool, bool) {
	if childSessionID == "" || d.Live == nil {
		return false, false
	}
	res := d.Live.BatchLiveForOwner(ctx, childSessionID)
	return CombineDescendantFacts(ctx, d.Child, childSessionID, res.HasLive(), res.Complete)
}

// ChildScope projects the classified debt that can keep a child open.
type ChildScope struct {
	WorkOpen, DrainOwed, DrainPaced bool
}

func (s ChildScope) Open() bool { return s.WorkOpen || s.DrainOwed || s.DrainPaced }

// ErrChildScopeUnavailable means no bound coordinator could answer the read.
var ErrChildScopeUnavailable = errors.New("batch child reaction scope: coordinator unavailable")

// ReactionFacts reads current scope and final outcome on demand.
type ReactionFacts struct {
	Scope   func(ctx context.Context, childSessionID string) (ChildScope, error)
	Outcome func(ctx context.Context, childSessionID string, initial batchdispatch.AsyncCompletion) (batchdispatch.AsyncCompletion, bool)
}

func (f ReactionFacts) ChildReactionScopeOpen(ctx context.Context, childSessionID string) (bool, bool) {
	if f.Scope == nil || childSessionID == "" {
		return false, false
	}
	scope, err := f.Scope(ctx, childSessionID)
	if err != nil {
		return true, false
	}
	return scope.Open(), true
}

func (f ReactionFacts) ChildFinalOutcome(ctx context.Context, childSessionID string, initial batchdispatch.AsyncCompletion) (batchdispatch.AsyncCompletion, bool) {
	if f.Outcome == nil || childSessionID == "" {
		return initial, false
	}
	return f.Outcome(ctx, childSessionID, initial)
}

// ChildScopePollInterval limits full coordinator scope reads while waiting.
const ChildScopePollInterval = 250 * time.Millisecond
