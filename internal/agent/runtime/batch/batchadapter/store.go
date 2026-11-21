package batchadapter

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"charm.land/fantasy"
	"github.com/PHPCraftdream/rush/internal/agent/runtime/batch/batchcore"
	"github.com/PHPCraftdream/rush/internal/agent/runtime/batch/batchlife"
	"github.com/PHPCraftdream/rush/internal/session"
	"github.com/PHPCraftdream/rush/internal/session/jobbatch"
	coresession "github.com/PHPCraftdream/rush/internal/session/service"
	"github.com/PHPCraftdream/rush/internal/store/message"
)

// StoreDoneSource implements the R11D completion seam over the in-process
// Runner and the durable response envelope. It NEVER invents a result: every
// returned envelope is the executor's own ToolResponse, or the store's
// persisted projection of it.
type StoreDoneSource struct {
	store *session.BatchStore
	// batches resolves a root started in this process, or returns nil.
	batches func(jobbatch.ID) *jobbatch.Batch
	// poll is the durable re-read cadence for roots without a live Batch;
	// zero selects batchDonePollInterval.
	poll time.Duration
	// finalizationGate, when non-nil, withholds completion reads until the
	// owner's finalization record leaves its active phases (W1a wires it).
	finalizationGate coresession.FinalizationTerminalGate
}

var _ batchcore.DoneSource = (*StoreDoneSource)(nil)

// NewStoreDoneSource requires a non-nil batches callback.
// If the callback returns nil, the source uses only the durable store.
func NewStoreDoneSource(store *session.BatchStore, batches func(jobbatch.ID) *jobbatch.Batch, poll time.Duration) *StoreDoneSource {
	return &StoreDoneSource{store: store, batches: batches, poll: poll}
}

// BindFinalizationResultGate sets the gate before the source is used.
func (d *StoreDoneSource) BindFinalizationResultGate(gate coresession.FinalizationTerminalGate) {
	d.finalizationGate = gate
}

// batchDonePollInterval is the fallback durable poll cadence.
const batchDonePollInterval = 250 * time.Millisecond

// AwaitDone waits for the root's REAL completion: a live Batch blocks on the
// engine (never a stop acknowledgement), then loads the persisted envelope;
// a foreign root polls the durable envelope only. ok=false means held
// termination or a caller-cancelled wait, never a fabricated settlement.
func (d *StoreDoneSource) AwaitDone(ctx context.Context, owner string, root jobbatch.ID) (fantasy.ToolResponse, bool, error) {
	if err := ctx.Err(); err != nil {
		return fantasy.ToolResponse{}, false, err
	}
	if b := d.batches(root); b != nil {
		if _, err := b.Wait(ctx); err != nil {
			return fantasy.ToolResponse{}, false, err
		}
		resp, ok, err := d.LoadDone(ctx, owner, root)
		return resp, ok, callerCancelOr(ctx, err)
	}
	if d.finalizationGate != nil {
		// Gated durable wait: hold until the owner's record leaves the active
		// phases, then read once; no busy poll under the gate.
		e, ok, err := coresession.FinalizationAwaitGatedBatchResult(ctx, d.finalizationGate, owner, string(root), d.readEnvelope)
		if err != nil || !ok {
			return fantasy.ToolResponse{}, ok, callerCancelOr(ctx, err)
		}
		resp, convErr := d.envelopeResult(e)
		if convErr != nil {
			return fantasy.ToolResponse{}, false, convErr
		}
		return resp, true, nil
	}
	tick := time.NewTicker(d.pollInterval())
	defer tick.Stop()
	for {
		if err := ctx.Err(); err != nil {
			return fantasy.ToolResponse{}, false, err
		}
		resp, ok, err := d.LoadDone(ctx, owner, root)
		if err != nil || ok {
			return resp, ok, callerCancelOr(ctx, err)
		}
		select {
		case <-ctx.Done():
			return fantasy.ToolResponse{}, false, ctx.Err()
		case <-tick.C:
		}
	}
}

// LoadDone reads an already-persisted completion without waiting (R11F
// P1-2): the one-item completion read — the root envelope when present, else
// the settled single task child's envelope (the Settled commit hook writes
// the envelope on the settled NODE, root.1, not the root). Envelope
// details carry the executor's full ToolResponse (batchEnvelope); a settled
// row without details is projected honestly from its stored summary and
// outcome state (state failed => IsError), never fabricated.
// When the finalization gate is set, the read is withheld until the owner's
// finalization record seals.
func (d *StoreDoneSource) LoadDone(ctx context.Context, owner string, root jobbatch.ID) (fantasy.ToolResponse, bool, error) {
	e, ok, err := coresession.FinalizationGatedBatchResult(ctx, d.finalizationGate, owner, string(root), d.readEnvelope)
	if err != nil || !ok {
		return fantasy.ToolResponse{}, ok, err
	}
	resp, convErr := d.envelopeResult(e)
	if convErr != nil {
		return fantasy.ToolResponse{}, false, convErr
	}
	return resp, true, nil
}

// envelopeResult projects one persisted completion envelope into the
// executor response: the full ToolResponse from Details when present, else
// the honest projection from the stored summary and outcome state.
func (d *StoreDoneSource) envelopeResult(e coresession.BatchDeliveryEnvelope) (fantasy.ToolResponse, error) {
	if len(e.Details) > 0 {
		var resp fantasy.ToolResponse
		if err := json.Unmarshal(e.Details, &resp); err != nil {
			return fantasy.ToolResponse{}, fmt.Errorf("batch done source: envelope %s: %w", e.Ref, err)
		}
		return resp, nil
	}
	resp := fantasy.NewTextResponse(e.Summary)
	// interrupted is the recovery outcome of a lost host: an error-shaped result, never a success.
	resp.IsError = e.State == "failed" || e.State == "timed_out" || e.State == "cancelled" || e.State == "interrupted"
	return resp, nil
}

// readEnvelope matches coresession.FinalizationBatchResultRead so the
// finalization gate helpers can wrap the plain completion read.
func (d *StoreDoneSource) readEnvelope(ctx context.Context, owner, root string) (coresession.BatchDeliveryEnvelope, bool, error) {
	return d.store.BatchDeliveryOneItemCompletion(ctx, owner, root)
}

func (d *StoreDoneSource) pollInterval() time.Duration {
	if d.poll > 0 {
		return d.poll
	}
	return batchDonePollInterval
}

// StoreElector implements the R8 election seam over the typed store
// elections. CAS semantics: exactly one winner, losers report false and
// insert no history (session.ErrBatchAnnouncementLost maps to won=false).
type StoreElector struct {
	store    *session.BatchStore
	messages message.Service
}

// NewStoreElector wires the durable election and history services.
func NewStoreElector(store *session.BatchStore, messages message.Service) StoreElector {
	return StoreElector{store: store, messages: messages}
}

// ElectStarted wins-or-loses the durable started history for the root.
func (e StoreElector) ElectStarted(ctx context.Context, owner string, root jobbatch.ID) (bool, error) {
	params := message.CreateMessageParams{
		Role:                message.User,
		Parts:               []message.ContentPart{message.TextContent{Text: startedNoticeText(root)}},
		AutoResumed:         true,
		BackgroundJobNotice: true,
	}
	_, won, err := e.store.BatchDeliveryElectStarted(ctx, e.messages, owner, string(root), params)
	return won, err
}

var _ batchcore.Elector = StoreElector{}

// ElectInline publishes the elected completion exactly once. The history
// message carries the envelope's content; a media-only response is serialised
// so no payload is lost.
func (e StoreElector) ElectInline(ctx context.Context, owner string, root jobbatch.ID, envelope fantasy.ToolResponse) (bool, error) {
	_, won, err := e.store.BatchDeliveryElectInline(ctx, e.messages, owner, string(root),
		batchCompletionMessageParams(batchEnvelopeText(envelope)))
	return won, err
}

// startedNoticeText is the started history message text.
func startedNoticeText(root jobbatch.ID) string {
	return fmt.Sprintf("batch root %s started; completions arrive as selected notices. Call await_tasks to wait for it.", root)
}

func batchCompletionMessageParams(text string) message.CreateMessageParams {
	return message.CreateMessageParams{
		Role:                message.User,
		Parts:               []message.ContentPart{message.TextContent{Text: text}},
		AutoResumed:         true,
		BackgroundJobNotice: true,
	}
}

// batchEnvelopeText renders a completion envelope for history: the content
// when present, else the serialised envelope (media/metadata preserved).
func batchEnvelopeText(envelope fantasy.ToolResponse) string {
	if envelope.Content != "" {
		return envelope.Content
	}
	raw, err := json.Marshal(envelope)
	if err != nil {
		return ""
	}
	return string(raw)
}

// StoreNoticePull implements the drain seam over the R8 outbox pull.
// Pull consumes only ALREADY-SELECTED notices; it never waits for running
// batch work.
type StoreNoticePull struct {
	store    *session.BatchStore
	messages message.Service
	owner    string
}

// NewStoreNoticePull wires the selected notice pull for owner.
func NewStoreNoticePull(store *session.BatchStore, messages message.Service, owner string) StoreNoticePull {
	return StoreNoticePull{store: store, messages: messages, owner: owner}
}

// Pull drains the owner's pending selected notices and reports the count
// actually delivered into history.
func (p StoreNoticePull) Pull(ctx context.Context) (int, error) {
	pulled, err := p.store.BatchPullNotices(ctx, p.messages, p.owner, batchPulledNoticeParams)
	if err != nil {
		return 0, err
	}
	return len(pulled), nil
}

var _ batchlife.NoticePull = StoreNoticePull{}

// batchPulledNoticeParams formats one selected notice for history from the
// row's own facts; nothing is re-derived from live state.
func batchPulledNoticeParams(row session.BatchNoticeRow) message.CreateMessageParams {
	origin := message.OriginWeb
	if row.OriginCLI {
		origin = message.OriginCLI
	}
	return message.CreateMessageParams{
		Role:                message.User,
		Parts:               []message.ContentPart{message.TextContent{Text: batchNoticeText(row)}},
		AutoResumed:         true,
		BackgroundJobNotice: true,
		Origin:              origin,
	}
}

func batchNoticeText(row session.BatchNoticeRow) string {
	var b strings.Builder
	fmt.Fprintf(&b, "batch %s node %s: kind %q state %q", row.RootID, row.NodeID, row.Kind, row.State)
	if row.Cause != "" {
		fmt.Fprintf(&b, " cause %q", row.Cause)
	}
	if row.OutcomeCode != "" || row.OutcomeSummary != "" {
		fmt.Fprintf(&b, " outcome %s %s", row.OutcomeCode, row.OutcomeSummary)
	}
	fmt.Fprintf(&b, " counts pending=%d active=%d completed=%d failed=%d cancelled=%d timed_out=%d interrupted=%d skipped=%d",
		row.Pending, row.Active, row.Completed, row.Failed, row.Cancelled, row.TimedOut, row.Interrupted, row.Skipped)
	return b.String()
}

// callerCancelOr maps a read error to ctx.Err() once the caller's context is
// done. A cancel that lands while the durable read is in flight surfaces from
// the SQLite driver as "interrupted (9)", not as a wrapped context error, so
// AwaitDone would otherwise report a caller-cancelled wait as a store failure.
// A nil error and a live context pass through unchanged.
func callerCancelOr(ctx context.Context, err error) error {
	if err == nil {
		return nil
	}
	if ctxErr := ctx.Err(); ctxErr != nil {
		return ctxErr
	}
	return err
}
