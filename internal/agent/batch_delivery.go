package agent

import (
	"context"
	"time"

	"charm.land/fantasy"
	"github.com/PHPCraftdream/rush/internal/agent/runtime/batch/batchcore"
	"github.com/PHPCraftdream/rush/internal/agent/runtime/batch/batchexec"
	"github.com/PHPCraftdream/rush/internal/agent/runtime/batch/batchlife"
	"github.com/PHPCraftdream/rush/internal/agent/runtime/batch/batchmeta"
	"github.com/PHPCraftdream/rush/internal/session/jobbatch"
)

type (
	batchDoneSource = batchcore.DoneSource
)

type batchDeliveryDeps struct {
	// Owner is the DORMANT DEFAULT delivery fence (R11F P1-1): used only
	// when an invocation carries no per-invocation owner. Flagged installs
	// always deliver under the invoking session (batchDeliveryImpl.owner).
	Owner string
	Done  batchDoneSource
	Elect batchcore.Elector
	// Runner is the engine handle; SYNC caller cancellation submits Stop
	// through it (batchmeta.CancelCall). Required, fails closed without it.
	Runner batchcore.RunnerHandle
	// ArmSupervision arms (or resumes) the owner session's supervision
	// timer when this delivery answers with the STARTED response — the
	// legacy asyncTool arming point restored for the batch runtime (package
	// psv). It is called only after the started election returned
	// successfully AND was won by THIS caller, never for an
	// inline-answered completion, a sync answer, a lost election or a
	// failed election (a loser must not arm: noteWorkStarted's resume
	// branch would spuriously lift another session's supervision pause).
	// Nil-safe: nil (flag-OFF installs and existing tests) arms nothing and
	// keeps delivery behaviour byte-identical.
	ArmSupervision func(context.Context, string)
	// Timer sleeps for the escape window; nil selects time.After (test seam
	// so window tests fire deterministically without sleeps).
	Timer func(time.Duration) <-chan time.Time
	// ToolResultReceipt, when non-nil, registers the fused tool-result
	// receipt in the per-step sink after store.Read validates the root's
	// immutable incarnation (nil keeps delivery behaviour byte-identical).
	ToolResultReceipt func(context.Context, string, jobbatch.ID, string, *toolResultReceiptSink) error
}

// batchDeliveryImpl adapts the invocation and agent-owned cancellation/receipt wiring.
type batchDeliveryImpl struct{ core *batchcore.Delivery }

func newBatchDelivery(d batchDeliveryDeps) (*batchDeliveryImpl, error) {
	deps := batchcore.DeliveryDeps{Owner: d.Owner, Done: d.Done, Elect: d.Elect, Timer: d.Timer, ArmSupervision: d.ArmSupervision}
	if d.Runner != nil {
		deps.Cancel = func(ctx context.Context, root jobbatch.ID) error {
			return batchmeta.CancelCall(ctx, d.Runner, root).Err
		}
	}
	if d.ToolResultReceipt != nil {
		deps.ToolResultReceipt = func(ctx context.Context, owner string, root jobbatch.ID, callID string) error {
			sink, _ := ctx.Value(toolReceiptContextKey{}).(*toolResultReceiptSink)
			if sink == nil {
				return nil
			}
			return d.ToolResultReceipt(ctx, owner, root, callID, sink)
		}
	}
	core, err := batchcore.NewDelivery(deps)
	if err != nil {
		return nil, err
	}
	return &batchDeliveryImpl{core: core}, nil
}

func (b *batchDeliveryImpl) DeliverBatch(ctx context.Context, root jobbatch.ID, inv BatchInvocation) (fantasy.ToolResponse, error) {
	return b.core.DeliverBatch(ctx, root, batchcore.DeliveryRequest{
		SessionID: inv.SessionID, CallID: inv.CallID, Mode: inv.Delivery.Mode,
		Window: inv.Delivery.Window, AutoEscape: inv.BackgroundEscape == batchEscapeAuto,
	})
}

func batchStartedResponse(root jobbatch.ID) fantasy.ToolResponse {
	return batchcore.StartedResponse(root)
}

func isBatchStartedToolResult(result fantasy.ToolResultContent) bool {
	return batchcore.IsStartedToolResult(result)
}

/*
Batch delivery and cancellation mapping (R11D, inert; nothing is installed,
R11F owns the wiring; model tools stay R13).

Old -> new:
  - async wrapper wait-for-completion inside the tool call ->
    batchDeliveryImpl.deliverSync: waits for the root's REAL completion via
    batchDoneSource and forwards the executor's full fantasy.ToolResponse
    (text, image/audio data + media type, metadata, IsError) unchanged.
  - inline-result announcement -> deliverInline: finish-before-ack (LoadDone
    then inline elect), failed ack (bounded retry; completion stays durable
    and unannounced in the outbox), inline-vs-started race (both elections
    are CAS; exactly one delivery, losers return the envelope read-only).
  - async_job started announcement -> deliverDetached / batchStartedResponse:
    started now, completions only as selected notices, never a second launch.
  - background-escape ClaimShell / second-launch behaviour -> windowed
    escape in DeliverBatch: escape=auto with mode sync/inline waits for the
    root's REAL completion up to Delivery.Window and returns its full
    envelope (NO ElectStarted); only when the window elapses does delivery
    go to the background (ElectStarted + started response). No Submit, no
    Stop, no ClaimShell, no second executor; the waiting leaf keeps waiting
    for the real Done and completion arrives via selected notices.
    ESCAPE WINDOW FORMAT: inv.Delivery.Window is a Go duration string parsed
    with time.ParseDuration ("500ms", "2s"); batch_invocation.go is owned by
    a sibling worktree, so the format is documented HERE. Empty or
    unparseable (or non-positive) window with escape=auto is a TYPED ERROR
    (errBatchEscapeWindowRequired / errBatchEscapeWindowInvalid) — never an
    implicit default. escape=never ignores the window entirely; escape=auto
    with mode detached behaves as detached. The window wait races via the
    injectable batchDeliveryDeps.Timer seam (nil = time.After).
  - async cancel-and-wait -> batchmeta.CancelCall: Runner.Stop under a bounded,
    non-cancelled control context detached from the caller's context; the
    outcome returns promptly; a Stop acknowledgement never settles (Settled is
    always false) and held physical termination keeps the admission unit
    occupied until the root's real report. WIRING: deliverSync is the ONLY
    caller — when the caller's ctx is done BEFORE the root completed it calls
    batchmeta.CancelCall exactly once and returns the caller's ctx error promptly
    (a Stop error is joined alongside it); deliverInline and deliverDetached
    NEVER Stop. The Runner is a REQUIRED batchDeliveryDeps field (fail-closed
    construction), and a Stop acknowledgement is never returned as the root's
    completion envelope.
  - web notice pull during running work -> batchWebDrain: bounded, detached
    pull that returns while roots are still running; never waits for Done.

Assumed engine contracts (re-verified against merged code):
  - batchRunnerHandle (batch_runtime.go): Submit returns the caller-supplied
    root; Stop requests cancellation without settling.
  - batchDelivery seam (batch_runtime.go): DeliverBatch(ctx, root, inv).
  - batchDeliveryChoice modes sync/inline/detached and BackgroundEscape
    never/auto are the closed validated sets (batch_invocation.go).
  - Election CAS semantics come from session.BatchStore:
    BatchAnnounceStarted / BatchAnnounceInlineResult return
    ErrBatchAnnouncementLost to losers, who insert no history
    (batch_delivery_store.go). A real transaction error is a FAILED
    ACK, not a lost election, and is retried a bounded number of times.
  - The settled envelope persists in batch_nodes.response_envelope (R5/R11B,
    batch_commit.go) as the jobbatch envelope; the fantasy.ToolResponse form
    travels with the Done source (R11F adapts it), so no SQL is added here.
  - Held termination (design rev 4): cancellation does not settle; the root's
    real report is the only settlement authority.

NOT DONE (later packages):
  - Installation into the runtime, coordinator tools, cli_tool_bridge,
    agentic_fetch_policy and app lifecycle (R11F).
  - batchDoneSource/batchElector production adapters over the Runner and
    session store (R11F).
  - Model-visible batch_* tools and explicit delivery specs/Notify selection
    (R13).
  - Node-bound question delivery (child questions) remains R8-deferred.
*/

const (
	batchOutputTTL = batchmeta.OutputTTL
)

// batchOutputCache keeps existing executor and control callers source-compatible.
type batchOutputCache struct {
	storage *batchmeta.OutputCache
	// retain, when set before use, replaces the default storage sink.
	retain func(id, root jobbatch.ID, chunk jobbatch.OutputChunk, contract batchmeta.OutputCursorContract)
}

func newBatchOutputCache() *batchOutputCache {
	return &batchOutputCache{storage: batchmeta.NewOutputCache()}
}

func (c *batchOutputCache) leaf() *batchmeta.OutputCache {
	if c == nil {
		return nil
	}
	return c.storage
}

func (c *batchOutputCache) putFor(id, root jobbatch.ID, chunk jobbatch.OutputChunk) {
	c.putForContract(id, root, chunk, batchmeta.OutputCursorClamp)
}

func (c *batchOutputCache) putForContract(id, root jobbatch.ID, chunk jobbatch.OutputChunk, contract batchmeta.OutputCursorContract) {
	if storage := c.leaf(); storage != nil {
		if c.retain != nil {
			c.retain(id, root, chunk, contract)
			return
		}
		storage.PutForContract(id, root, chunk, contract)
	}
}

func (c *batchOutputCache) readWithMetadata(id jobbatch.ID, cursor uint64) (jobbatch.OutputChunk, uint64, int, bool, bool) {
	return c.leaf().ReadWithMetadata(id, cursor)
}

type batchArgvOutputReader struct{ process batchexec.ArgvHandle }

func (r batchArgvOutputReader) ReadOutput(ctx context.Context, cursor uint64) (jobbatch.OutputChunk, error) {
	return batchmeta.NewArgvOutputReader(r.process).ReadOutput(ctx, cursor)
}

func batchOutputCapture(cache *batchOutputCache, id, root jobbatch.ID, reader jobbatch.OutputReader, contract batchmeta.OutputCursorContract) error {
	if cache.leaf() == nil {
		return nil
	}
	return batchmeta.CaptureOutput(cache.putForContract, id, root, reader, contract)
}

// batchDriverHints preserves the root method names while batchlife owns the
// ledger. The receiver conversions share the same ledger without copying it.
type batchDriverHints batchlife.DriverHints

func newBatchDriverHints() *batchDriverHints { return (*batchDriverHints)(batchlife.NewDriverHints()) }

func (h *batchDriverHints) bumpHint(owner string) {
	(*batchlife.DriverHints)(h).BumpHint(owner)
}

func (h *batchDriverHints) hintSeqOf(owner string) uint64 {
	return (*batchlife.DriverHints)(h).HintSeqOf(owner)
}

func (h *batchDriverHints) waitForHint(ctx context.Context, owner string, since uint64) bool {
	return (*batchlife.DriverHints)(h).WaitForHint(ctx, owner, since)
}

func (h *batchDriverHints) claimExternalDriver(owner string) {
	(*batchlife.DriverHints)(h).ClaimExternalDriver(owner)
}

func (h *batchDriverHints) releaseExternalDriver(owner string) {
	(*batchlife.DriverHints)(h).ReleaseExternalDriver(owner)
}

func (h *batchDriverHints) isExternalDriver(owner string) bool {
	return (*batchlife.DriverHints)(h).IsExternalDriver(owner)
}

func (h *batchDriverHints) idleCandidates() []string {
	return (*batchlife.DriverHints)(h).IdleCandidates()
}

func (h *batchDriverHints) dropIfIdle(id string) bool {
	return (*batchlife.DriverHints)(h).DropIfIdle(id)
}

func (h *batchDriverHints) subscribe(owner string) (<-chan struct{}, func()) {
	return (*batchlife.DriverHints)(h).Subscribe(owner)
}
