package agent

import (
	"context"

	"github.com/PHPCraftdream/rush/internal/session"
)

// R11F round-2 (P1-4 / P1-5b / P1-5c) helpers: typed bridge-invocation
// context stamping, the per-build policy snapshot for the private-fetch
// arming point, and the live-descendant reader over the R10a store. New
// logic lives in this file; sibling files carry only stated minimal hunks.

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
//   - CustomDisk: CallOptions.DiskProvider is an arbitrary in-process
//     tools.DiskProvider with no serializable identity (call_options.go:
//     "a DiskProvider is arbitrary in-process Go code"); a call carrying
//     one must never reach the durable queue at all.
//
// Consequently the arming points keep the pinned RegistryID/
// RestrictedToolset identity, and the executor side (P1-5c executor hunks in
// batch_exec_generic/shell/argv) builds the leaf run context EXCLUSIVELY via
// batchLeafRunContext so any PINNED Policy value (FolderScope, RunAllowlist,
// CustomDisk, message id, image support, model name) wins over whatever the
// Start context carries. A future arming point that obtains genuine specs
// can set them on the template without touching the executors.
const batchArmingPolicyNote = ""

// batchCoordinatorDescendants is the R10a descendant reader for the flagged
// coordinator driver (P1-5b): it reports live descendants of the invocation
// through the BatchLiveReader over the batch store. A nil reader, an error
// or an INCOMPLETE read keeps the scope open (complete=false) — never an
// optimistic true.
type batchCoordinatorDescendants struct {
	live *session.BatchLiveReader
}

func (d batchCoordinatorDescendants) DescendantsLive(ctx context.Context, req batchAgentRunRequest) (bool, bool) {
	if d.live == nil {
		// No store wired: the read is incomplete by construction, so the
		// scope stays open (fail closed), never optimistic.
		return false, false
	}
	res := d.live.BatchLiveForOwner(ctx, req.SessionID)
	return res.HasLiveDescendants(), res.Complete
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
