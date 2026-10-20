package agent

import (
	"context"
	"encoding/json"

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
}

func (d batchCoordinatorDescendants) DescendantsLive(ctx context.Context, req batchAgentRunRequest) (bool, bool) {
	if req.ChildSessionID == "" {
		return false, false
	}
	if d.live == nil {
		return false, false
	}
	res := d.live.BatchLiveForOwner(ctx, req.ChildSessionID)
	return res.HasLive(), res.Complete
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
