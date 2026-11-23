package agent

import (
	"context"
	"time"

	"charm.land/fantasy"
	"github.com/PHPCraftdream/rush/internal/agent/runtime/batch/batchcore"
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
	// through it (cancelBatchCall). Required, fails closed without it.
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
			return cancelBatchCall(ctx, d.Runner, root).Err
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
