package batchcore

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"charm.land/fantasy"
	"github.com/PHPCraftdream/rush/internal/session/jobbatch"
)

// Typed delivery errors. Sentinels, distinguishable via errors.Is.
var (
	ErrDeliveryDeps         = errors.New("batch delivery: done source, elector and runner deps are required")
	ErrDeliveryOwner        = errors.New("batch delivery: owner session id is required")
	ErrDoneUnavailable      = errors.New("batch delivery: root completion envelope unavailable")
	ErrInlineElectionFailed = errors.New("batch delivery: inline election retries exhausted")
	ErrDeliveryUnknownMode  = errors.New("batch delivery: unknown delivery mode")
	// ErrEscapeWindowRequired / ErrEscapeWindowInvalid: escape=auto
	// needs an EXPLICIT window; there is no implicit default. The window is a
	// Go duration string (time.ParseDuration format: "500ms", "2s"),
	// documented in batch_delivery_doc.go.
	ErrEscapeWindowRequired = errors.New("batch delivery: escape=auto requires an explicit delivery window")
	ErrEscapeWindowInvalid  = errors.New("batch delivery: escape=auto window is not a valid Go duration")
)

const (
	// StartedResponseSuffix is the detached started-response suffix.
	StartedResponseSuffix = "started; completions arrive as selected notices. Call await_tasks to wait for it."
	// InlineElectionAttempts bounds failed-ack retries, never unbounded.
	InlineElectionAttempts = 4
)

// DoneSource is the completion seam. AwaitDone blocks for the root's
// REAL completion (never a stop acknowledgement) and returns its executor
// envelope unchanged — text, image/audio data, media type and IsError are
// preserved, never collapsed. ok=false with a nil error means the source
// cannot produce a completion (held termination); it is never a settlement.
type DoneSource interface {
	AwaitDone(ctx context.Context, owner string, root jobbatch.ID) (fantasy.ToolResponse, bool, error)
	// LoadDone reads an already-persisted completion without waiting
	// (finish-before-ack: the root finished before this caller's ack).
	LoadDone(ctx context.Context, owner string, root jobbatch.ID) (fantasy.ToolResponse, bool, error)
}

// Elector is the R8 history-election seam. Every method is CAS: exactly
// one caller wins, losers report false and insert no history. A real error is
// a failed transaction (failed ack), NOT a lost election.
type Elector interface {
	// ElectStarted wins-or-loses the durable started history (R8 ack).
	ElectStarted(ctx context.Context, owner string, root jobbatch.ID) (bool, error)
	// ElectInline publishes the elected completion exactly once. Won=true
	// means THIS caller created and published the single completion message.
	ElectInline(ctx context.Context, owner string, root jobbatch.ID, envelope fantasy.ToolResponse) (bool, error)
}

// DeliveryDeps carries the delivery dependencies; both are required.
type DeliveryDeps struct {
	// Owner is the DORMANT DEFAULT delivery fence (R11F P1-1): used only
	// when an invocation carries no per-invocation owner. Flagged installs
	// always deliver under the invoking session (Delivery.owner).
	Owner string
	Done  DoneSource
	Elect Elector
	// Cancel submits Stop for sync caller cancellation; it does not settle the root.
	Cancel func(context.Context, jobbatch.ID) error
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
	ToolResultReceipt func(context.Context, string, jobbatch.ID, string) error
}

// Delivery implements the batchDelivery seam defined in
// batch_runtime.go. It is a value object: no goroutines, no globals.
type Delivery struct {
	deps DeliveryDeps
}

// owner is the delivery fence of ONE invocation (R11F P1-1): the
// PER-INVOCATION owner — the invoking session (inv.SessionID) — so elections,
// loads and attach agree on one identity. deps.Owner is only the dormant
// default for installs without a per-invocation owner; an invocation without
// one fails closed in Validate before delivery ever runs.
type DeliveryRequest struct {
	SessionID, CallID string
	Mode, Window      string
	AutoEscape        bool
}

func (b *Delivery) owner(inv DeliveryRequest) string {
	if inv.SessionID != "" {
		return inv.SessionID
	}
	return b.deps.Owner
}

// NewDelivery validates the required delivery ports.
func NewDelivery(d DeliveryDeps) (*Delivery, error) {
	if d.Done == nil || d.Elect == nil || d.Cancel == nil {
		return nil, fmt.Errorf("%w", ErrDeliveryDeps)
	}
	if d.Owner == "" {
		return nil, fmt.Errorf("%w", ErrDeliveryOwner)
	}
	return &Delivery{deps: d}, nil
}

// DeliverBatch delivers the started root per the invocation's mode and
// returns its tool response. BackgroundEscape=auto is a WINDOWED escape:
// delivery waits for the root's REAL completion up to inv.Window and
// returns its full envelope; only when the window elapses does the already
// running execution continue in the background (ElectStarted + started
// response). Escape NEVER switches execution: no second Submit, no Stop, no
// ClaimShell, no second executor; the waiting leaf keeps waiting for the real
// Done and completion arrives later via selected notices. escape=never
// ignores the window entirely; escape=auto with Mode detached is detached.
func (b *Delivery) DeliverBatch(ctx context.Context, root jobbatch.ID, inv DeliveryRequest) (fantasy.ToolResponse, error) {
	if err := ctx.Err(); err != nil {
		return fantasy.ToolResponse{}, err
	}
	mode := inv.Mode
	// Windowed escape for sync/inline only; detached is already detached.
	if inv.AutoEscape && mode != "detached" {
		window, err := batchEscapeWindow(inv.Window)
		if err != nil {
			return fantasy.ToolResponse{}, err
		}
		return b.deliverWithEscapeWindow(ctx, b.owner(inv), root, window, inv.CallID, mode == "sync")
	}
	switch mode {
	case "sync":
		return b.deliverSync(ctx, b.owner(inv), root, inv.CallID)
	case "inline":
		return b.deliverInline(ctx, b.owner(inv), root)
	case "detached":
		return b.deliverDetached(ctx, b.owner(inv), root)
	default:
		return fantasy.ToolResponse{}, fmt.Errorf("%w: %q", ErrDeliveryUnknownMode, mode)
	}
}

// deliverSync waits for the root's real completion and returns its executor
// envelope unchanged: the full fantasy.ToolResponse (type, content, media
// data/type, metadata, IsError) is forwarded, never collapsed to text.
//
// Caller cancellation: if the CALLER's ctx is done BEFORE the root completed,
// cancelBatchCall submits Stop exactly once under a bounded noncancelled
// control context and the caller's ctx error is returned promptly — never a
// Stop-derived envelope, and Settled stays false. ONLY sync does this: an
// inline or detached caller leaving never cancels the root.
func (b *Delivery) deliverSync(ctx context.Context, owner string, root jobbatch.ID, callID string) (fantasy.ToolResponse, error) {
	envelope, ok, err := b.deps.Done.AwaitDone(ctx, owner, root)
	if err != nil {
		if ctx.Err() != nil {
			// Caller gone before completion: one Stop, prompt return.
			if err := b.deps.Cancel(ctx, root); err != nil {
				return fantasy.ToolResponse{}, errors.Join(ctx.Err(), err)
			}
			return fantasy.ToolResponse{}, ctx.Err()
		}
		return fantasy.ToolResponse{}, err
	}
	if !ok {
		return fantasy.ToolResponse{}, fmt.Errorf("%w: root %s", ErrDoneUnavailable, root)
	}
	if err := b.registerToolResultReceipt(ctx, owner, root, callID); err != nil {
		return fantasy.ToolResponse{}, err
	}
	return envelope, nil
}

// deliverInline elects durable completion or started history atomically.
//
//   - Finish-before-ack: LoadDone finds the persisted completion and the
//     caller elects it inline — exactly one completion is delivered.
//   - Failed ack: a real election error is retried a bounded number of times;
//     the completion stays durable and unannounced in the outbox, so it is
//     neither lost nor duplicated.
//   - Inline-vs-started race: started election is CAS, so exactly one caller
//     elects the started history; the completion election is CAS, so exactly
//     one delivery commits. A loser returns the envelope as a READ, never a
//     second election.
func (b *Delivery) deliverInline(ctx context.Context, owner string, root jobbatch.ID) (fantasy.ToolResponse, error) {
	if err := ctx.Err(); err != nil {
		return fantasy.ToolResponse{}, err
	}
	envelope, done, err := b.deps.Done.LoadDone(ctx, owner, root)
	if err != nil {
		return fantasy.ToolResponse{}, err
	}
	if done {
		return b.electCompletion(ctx, owner, root, envelope)
	}
	started, err := b.deps.Elect.ElectStarted(ctx, owner, root)
	if err != nil {
		return fantasy.ToolResponse{}, err
	}
	if !started {
		// Lost started election: another caller owns the started history
		// and the delivery; wait for the real completion and return it
		// read-only.
		envelope, ok, err := b.deps.Done.AwaitDone(ctx, owner, root)
		if err != nil {
			return fantasy.ToolResponse{}, err
		}
		if !ok {
			return StartedResponse(root), nil
		}
		return envelope, nil
	}
	envelope, ok, err := b.deps.Done.AwaitDone(ctx, owner, root)
	if err != nil {
		return fantasy.ToolResponse{}, err
	}
	if !ok {
		// Held termination: the root never reported. Return the already
		// elected started history; the completion stays with the outbox.
		// The work stays live in the background: arm supervision (psv),
		// the legacy inline started-fallback arming point.
		b.psvArmSupervision(ctx, owner)
		return StartedResponse(root), nil
	}
	return b.electCompletion(ctx, owner, root, envelope)
}

// deliverWithEscapeWindow implements the windowed escape: race the REAL
// AwaitDone against the window timer. Completion inside the window returns the
// full envelope with NO started election; window elapse elects the started
// history (CAS) and returns the started response without Stop, without a
// second Submit and without a second executor — the waiter keeps waiting for
// the real Done and completion arrives via selected notices. If both resolve
// together the select commits to exactly ONE outcome.
func (b *Delivery) deliverWithEscapeWindow(ctx context.Context, owner string, root jobbatch.ID, window time.Duration, callID string, syncMode bool) (fantasy.ToolResponse, error) {
	type awaited struct {
		envelope fantasy.ToolResponse
		ok       bool
		err      error
	}
	done := make(chan awaited, 1)
	go func() {
		envelope, ok, err := b.deps.Done.AwaitDone(ctx, owner, root)
		done <- awaited{envelope, ok, err}
	}()
	select {
	case a := <-done:
		if a.err != nil {
			if syncMode && ctx.Err() != nil {
				if err := b.deps.Cancel(ctx, root); err != nil {
					return fantasy.ToolResponse{}, errors.Join(ctx.Err(), err)
				}
				return fantasy.ToolResponse{}, ctx.Err()
			}
			return fantasy.ToolResponse{}, a.err
		}
		if !a.ok {
			return fantasy.ToolResponse{}, fmt.Errorf("%w: root %s", ErrDoneUnavailable, root)
		}
		if err := b.registerToolResultReceipt(ctx, owner, root, callID); err != nil {
			return fantasy.ToolResponse{}, err
		}
		return a.envelope, nil
	case <-b.after(window):
		started, err := b.deps.Elect.ElectStarted(ctx, owner, root)
		if err != nil {
			return fantasy.ToolResponse{}, err
		}
		if started {
			// The execution escaped to the background: arm supervision
			// (psv), exactly like the legacy window-elapsed fallback.
			b.psvArmSupervision(ctx, owner)
		}
		return StartedResponse(root), nil
	case <-callerDone(ctx, syncMode):
		// Sync cancellation retains the ordinary sync Stop behavior; inline
		// escape does not convert cancellation into a root cancellation.
		if err := b.deps.Cancel(ctx, root); err != nil {
			return fantasy.ToolResponse{}, errors.Join(ctx.Err(), err)
		}
		return fantasy.ToolResponse{}, ctx.Err()
	}
}

func callerDone(ctx context.Context, enabled bool) <-chan struct{} {
	if enabled {
		return ctx.Done()
	}
	return nil
}

// after sleeps for one window via the injectable timer seam (nil = time.After).
func (b *Delivery) after(d time.Duration) <-chan time.Time {
	if b.deps.Timer != nil {
		return b.deps.Timer(d)
	}
	return time.After(d)
}

// batchEscapeWindow parses the escape window. The format is a Go duration
// string (time.ParseDuration: "500ms", "2s") — documented in
// batch_delivery_doc.go because batch_invocation.go is owned by a sibling
// worktree. Empty or non-positive is a TYPED ERROR, never an implicit default.
func batchEscapeWindow(window string) (time.Duration, error) {
	if window == "" {
		return 0, fmt.Errorf("%w", ErrEscapeWindowRequired)
	}
	d, err := time.ParseDuration(window)
	if err != nil || d <= 0 {
		return 0, fmt.Errorf("%w: %q", ErrEscapeWindowInvalid, window)
	}
	return d, nil
}

func (b *Delivery) registerToolResultReceipt(ctx context.Context, owner string, root jobbatch.ID, callID string) error {
	if b.deps.ToolResultReceipt == nil {
		return nil
	}
	return b.deps.ToolResultReceipt(ctx, owner, root, callID)
}

// electCompletion elects the completion envelope inline with bounded retries
// on FAILED ACKS only. Won: this caller delivered the single completion. Lost:
// the single delivery already happened elsewhere and this return is a read.
func (b *Delivery) electCompletion(ctx context.Context, owner string, root jobbatch.ID, envelope fantasy.ToolResponse) (fantasy.ToolResponse, error) {
	var lastErr error
	for attempt := 0; attempt < InlineElectionAttempts; attempt++ {
		won, err := b.deps.Elect.ElectInline(ctx, owner, root, envelope)
		if err == nil {
			// Won or lost, the completion was delivered exactly once; the
			// caller's own tool response is the envelope (a loser's return
			// is a read, never a second election).
			_ = won
			return envelope, nil
		}
		lastErr = err
	}
	return fantasy.ToolResponse{}, fmt.Errorf("%w: root %s: %w", ErrInlineElectionFailed, root, lastErr)
}

// deliverDetached returns the started response immediately. Later completions
// arrive ONLY through selected notices (R8), never as a second launch.
func (b *Delivery) deliverDetached(ctx context.Context, owner string, root jobbatch.ID) (fantasy.ToolResponse, error) {
	started, err := b.deps.Elect.ElectStarted(ctx, owner, root)
	if err != nil {
		return fantasy.ToolResponse{}, err
	}
	if started {
		// Won started election: work continues detached in the
		// background — arm supervision (psv), the legacy detached
		// launch arming point. A lost election must NOT arm: the winner
		// owns the delivery, and arming would spuriously resume a
		// paused session.
		b.psvArmSupervision(ctx, owner)
	}
	return StartedResponse(root), nil
}

// StartedResponse is the detached/inline started tool response.
func StartedResponse(root jobbatch.ID) fantasy.ToolResponse {
	return fantasy.NewTextResponse(fmt.Sprintf("batch root %s %s", root, StartedResponseSuffix))
}

// IsStartedToolResult reports whether a tool result IS the production
// batch started response above. The started response carries no
// ClientMetadata, so the launched root's identity travels in its own text;
// the caller pairs this with the tool call whose id named the root.
func IsStartedToolResult(result fantasy.ToolResultContent) bool {
	if result.Result == nil || result.Result.GetType() != fantasy.ToolResultContentTypeText {
		return false
	}
	text, ok := fantasy.AsToolResultOutputType[fantasy.ToolResultOutputContentText](result.Result)
	if !ok {
		return false
	}
	trimmed := strings.TrimSpace(text.Text)
	return strings.HasPrefix(trimmed, "batch root ") && strings.HasSuffix(trimmed, StartedResponseSuffix)
}
