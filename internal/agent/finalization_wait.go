package agent

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/PHPCraftdream/rush/internal/agent/runtime/batch/batchcore/finalize"
	"github.com/PHPCraftdream/rush/internal/agent/runtime/batch/batchinst/gate"
	"github.com/PHPCraftdream/rush/internal/session"
	coresession "github.com/PHPCraftdream/rush/internal/session/service"
)

const (
	// defaultFinalizationPollInterval covers changes that are invisible to the
	// in-process change signal (work in other processes, quiet durable moves).
	defaultFinalizationPollInterval = 2 * time.Second
	// defaultFinalizationMaxRecompositions bounds a runaway recompose loop.
	defaultFinalizationMaxRecompositions = 8
	// finalizationHeldQuestionText is the stable pause question surfaced to
	// the operator when the driver holds an open question.
	finalizationHeldQuestionText = "An owned question is awaiting an answer."
)

// FinalizationWaitPhase names one observable phase of the FinalizeTurn loop.
type FinalizationWaitPhase string

const (
	FinalizationPhaseSealing   FinalizationWaitPhase = "sealing"
	FinalizationPhaseWaiting   FinalizationWaitPhase = "waiting"
	FinalizationPhaseComposing FinalizationWaitPhase = "composing"
	FinalizationPhasePaused    FinalizationWaitPhase = "paused"
	FinalizationPhaseAccepted  FinalizationWaitPhase = "accepted"
	FinalizationPhaseFailed    FinalizationWaitPhase = "failed"
)

// FinalizationWaitStatus is one observable transition of the wait loop; the
// typed refusal cause is the seal reason derived from the facts the gate saw.
type FinalizationWaitStatus struct {
	Phase         FinalizationWaitPhase
	Reason        coresession.AgentFinalizationSealReason
	Descendants   int
	Debt          int
	PhysicalHolds int
	QueuedCalls   int
	HeldQuestion  bool
	Cancelled     bool
}

// FinalizationWaitDeps carries the collaborators of the FinalizeTurn hook.
type FinalizationWaitDeps struct {
	// Settler settles reaction debts once per wait entry (lane L3).
	Settler session.AgentFinalizationDebtSettler
	// Compose rebuilds the provisional answer; nil uses the built-in
	// recomposition (see finalization_recompose.go).
	Compose FinalizationComposeFunc
	// PollInterval bounds every wait; <=0 means defaultFinalizationPollInterval.
	PollInterval time.Duration
	// MaxRecompositions bounds recompositions; <0 means the default, 0
	// legally forbids recomposition.
	MaxRecompositions int
	// OnState is an optional observer of every phase transition.
	OnState func(FinalizationWaitStatus)
}

// ErrFinalizationRecomposeLimit refuses to keep recomposing a candidate that
// can never become fresh enough; it is a fail-closed stop, not a wait.
var ErrFinalizationRecomposeLimit = errors.New("finalization recomposition limit reached")

// mapFinalizationSealReason derives the typed seal refusal cause from the
// facts the gate observed; a held question is a pause path, not a reason.
func mapFinalizationSealReason(f gate.Facts) coresession.AgentFinalizationSealReason {
	switch {
	case f.Cancelled:
		return coresession.AgentFinalizationSealCancelled
	case !f.Complete:
		return coresession.AgentFinalizationSealScopeIncomplete
	case f.Descendants > 0:
		return coresession.AgentFinalizationSealDescendantsActive
	case f.Debt > 0:
		return coresession.AgentFinalizationSealReactionDebt
	case f.PhysicalHolds > 0:
		return coresession.AgentFinalizationSealPhysicalHolds
	case f.QueuedCalls > 0:
		return coresession.AgentFinalizationSealQueuedCalls
	default:
		return coresession.AgentFinalizationSealNone
	}
}

func (d FinalizationWaitDeps) status(phase FinalizationWaitPhase, f gate.Facts) FinalizationWaitStatus {
	return FinalizationWaitStatus{
		Phase:         phase,
		Reason:        mapFinalizationSealReason(f),
		Descendants:   f.Descendants,
		Debt:          f.Debt,
		PhysicalHolds: f.PhysicalHolds,
		QueuedCalls:   f.QueuedCalls,
		HeldQuestion:  f.HeldQuestion,
		Cancelled:     f.Cancelled,
	}
}

func (d FinalizationWaitDeps) emit(phase FinalizationWaitPhase, f gate.Facts) {
	st := d.status(phase, f)
	if d.OnState != nil {
		d.OnState(st)
	}
	slog.Debug("Finalization wait entered",
		"phase", string(phase),
		"reason", st.Reason.String(),
		"descendants", st.Descendants,
		"debt", st.Debt,
		"physical_holds", st.PhysicalHolds,
		"queued_calls", st.QueuedCalls,
		"held_question", st.HeldQuestion,
		"cancelled", st.Cancelled,
	)
}

// NewFinalizationFinalizeTurnHook builds the L5 FinalizeTurn hook: the
// wait/compose/seal loop that runs at the end of a finalization-aware turn.
func NewFinalizationFinalizeTurnHook(deps FinalizationWaitDeps) func(context.Context, *turnStream) error {
	return func(ctx context.Context, ts *turnStream) error {
		return runFinalizationWait(ctx, ts, deps)
	}
}

// runFinalizationWait loops trySeal until the candidate is accepted, the turn
// pauses on a held question, the context is done, or a fail-closed error stops
// the loop. Every wait is bounded by the poll timer and the context.
func runFinalizationWait(ctx context.Context, ts *turnStream, deps FinalizationWaitDeps) error {
	execution := ts.finalization
	if execution == nil {
		return fmt.Errorf("%w: finalization execution unavailable", gate.ErrFinalizationFactsUnavailable)
	}
	poll := deps.PollInterval
	if poll <= 0 {
		poll = defaultFinalizationPollInterval
	}
	allowed := deps.MaxRecompositions
	if allowed < 0 {
		allowed = defaultFinalizationMaxRecompositions
	}
	// scopeChanged remembers that the last publication refused as a durable
	// "not yet"; that compose verdict must not burn recomposition budget.
	// The closure runs only inside gate.TrySeal under its mutex.
	scopeChanged := false
	publish := finalizationPublishClosure(ts, execution, &scopeChanged)
	for {
		// Status facts are read directly, without re-entering the gate:
		// that helper requires the gate's mutex, which this loop does not
		// hold; trySeal re-reads and refuses under the lock.
		facts, err := execution.Read(ctx)
		if err != nil {
			facts = gate.Facts{}
		}
		if err := ctx.Err(); err != nil {
			deps.emit(FinalizationPhaseFailed, facts)
			return ctx.Err()
		}
		deps.emit(FinalizationPhaseSealing, facts)
		verdict, err := execution.Gate.TrySeal(ctx, ts.finalizationStamp, ts.finalizationCandidate.Candidate, execution.Read, publish)
		if err != nil {
			// An already sealed gate answers Accepted plus ErrFinalizationSealed;
			// sealing twice is idempotent and must not publish again.
			if verdict == gate.Accepted && errors.Is(err, gate.ErrFinalizationSealed) {
				deps.emit(FinalizationPhaseAccepted, facts)
				return nil
			}
			deps.emit(FinalizationPhaseFailed, facts)
			return fmt.Errorf("finalization seal failed: %w", err)
		}
		switch verdict {
		case gate.Accepted:
			deps.emit(FinalizationPhaseAccepted, facts)
			return nil
		case gate.PauseQuestion:
			deps.emit(FinalizationPhasePaused, facts)
			return &AwaitingAnswerError{Question: finalizationHeldQuestionText, SessionID: ts.call.SessionID}
		case gate.Compose:
			if scopeChanged {
				scopeChanged = false
				deps.emit(FinalizationPhaseWaiting, facts)
				// The durable side said "not yet": time heals, recomposition
				// cannot, so settle debts and wait instead of burning budget.
				if err := finalize.SettleFinalizationDebts(ctx, deps.Settler, execution.SessionID); err != nil {
					return err
				}
				if err := waitFinalizationRound(ctx, poll, nil); err != nil {
					return err
				}
				continue
			}
			if allowed <= 0 {
				deps.emit(FinalizationPhaseFailed, facts)
				return fmt.Errorf("%w: at most %d recompositions are allowed", ErrFinalizationRecomposeLimit, allowed)
			}
			allowed--
			deps.emit(FinalizationPhaseComposing, facts)
			if err := composeFinalizationCandidate(ctx, ts, execution, deps); err != nil {
				deps.emit(FinalizationPhaseFailed, facts)
				return err
			}
		case gate.WaitWork:
			deps.emit(FinalizationPhaseWaiting, facts)
			if err := finalize.SettleFinalizationDebts(ctx, deps.Settler, execution.SessionID); err != nil {
				return err
			}
			waitVerdict, changed, err := execution.Gate.WaitState(ctx, execution.Read)
			if err != nil {
				deps.emit(FinalizationPhaseFailed, facts)
				return fmt.Errorf("finalization wait state read failed: %w", err)
			}
			if waitVerdict != gate.WaitWork {
				// The facts moved while arming the signal; re-evaluate from the
				// top so trySeal sees the fresh state.
				continue
			}
			if changed == nil {
				deps.emit(FinalizationPhaseFailed, facts)
				return fmt.Errorf("%w: finalization change signal unavailable", gate.ErrFinalizationFactsUnavailable)
			}
			if err := waitFinalizationRound(ctx, poll, changed); err != nil {
				return err
			}
		}
	}
}

// waitFinalizationRound sleeps until the change signal, the poll timer, or the
// context wakes the loop; a nil signal means only the timer and context.
func waitFinalizationRound(ctx context.Context, poll time.Duration, changed <-chan struct{}) error {
	timer := time.NewTimer(poll)
	defer timer.Stop()
	if changed == nil {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-timer.C:
			return nil
		}
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-changed:
		return nil
	case <-timer.C:
		return nil
	}
}
