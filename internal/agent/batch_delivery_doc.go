package agent

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
  - async cancel-and-wait -> cancelBatchCall: Runner.Stop under a bounded,
    non-cancelled control context detached from the caller's context; the
    outcome returns promptly; a Stop acknowledgement never settles (Settled is
    always false) and held physical termination keeps the admission unit
    occupied until the root's real report. WIRING: deliverSync is the ONLY
    caller — when the caller's ctx is done BEFORE the root completed it calls
    cancelBatchCall exactly once and returns the caller's ctx error promptly
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
    (batch_ack.go, batch_inline.go). A real transaction error is a FAILED
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
