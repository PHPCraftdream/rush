package drainctl

import (
	"sync/atomic"

	"github.com/PHPCraftdream/rush/internal/session"
)

func (a *DrainAttempt) CoordinatorDrainAttemptMarker() any { return a }

// DrainAttempt is one Drain leg (one turn-loop iteration of a Drain call).
type DrainAttempt struct {
	SessionID string
	// HintAt is the hint counter read BEFORE the turn-start pull: a fact that
	// lands during the attempt is never absorbed by it.
	HintAt uint64
	// Snapshot is retained struct shape only: batch accounting reads
	// BatchSnapshot exclusively (att.Snapshot stays zero).
	Snapshot session.BatchDebtSnapshot
	Outcome  DrainOutcome
	// PendingLeft (no-turn): pending-inclusive debt remains -- the pull fails.
	PendingLeft bool
	// CommitNo (no-turn with visible debt): the policy/gate verdict.
	CommitNo DrainVerdict
	// Stalled: the stream watchdog fired (surfaces as context.Canceled).
	Stalled atomic.Bool
	// CapAbort: --max-cost/--max-tokens ended the turn (surfaces as Canceled).
	CapAbort atomic.Bool
	// TurnCtxDone: the turn's OWN context was done (cancelled or past its
	// deadline) when its error surfaced. A deadline error is an operator stop
	// only when this is true: net/http timeouts satisfy
	// errors.Is(err, context.DeadlineExceeded) too.
	TurnCtxDone atomic.Bool
	// StreamErr is the error agent.Stream returned: runTurn hands a cancelled
	// leg to the next queued call with a nil error, so the accounting reads
	// the cause here.
	StreamErr error
	// Provider is the provider id of the model the attempt called, and
	// Credentialed says the call carries its own per-call credentials (which a
	// refresh of the shared config cannot repair): set just before the stream,
	// read by the 401 handling of the accounting.
	Provider     string
	Credentialed bool
	// ChainIdleClaims holds the claim ids of the pure wait commands (bash
	// sleep/echo, run_command sleep/timeout) THIS leg launched async; each
	// completion becomes a debt row keyed by its claim. ChainProgress says
	// the leg also did a real action (edit, a bash command with substance,
	// a delegation, ...). Written by recordStepHistory, read by the reaction
	// chain guard's accounting (#1113).
	ChainIdleClaims []string
	ChainProgress   bool
	// ChainBatchRoots holds the root tool_call_ids of the batch launches
	// THIS leg started whose classification was a pure wait (the started
	// tool response's own identity: the launched root's tool_call_id IS the
	// call id). Each root's selected completion becomes a node debt row
	// keyed by that root id; read by the reaction chain guard's batch
	// accounting.
	ChainBatchRoots []string
	// BatchSnapshot is the batch twin of Snapshot: the visible batch debt
	// captured right after the leg's own turn-start pull -- the ONLY set
	// this leg's accounting may increment on or settle. BatchCaptured says
	// the capture succeeded; a failed capture is NEVER re-read late at
	// accounting time (a late capture would widen the acted-on set with
	// rows that arrived after the pull).
	BatchSnapshot session.BatchDebtSnapshot
	BatchCaptured bool
	// BatchDebtCallIDs is BatchSnapshot's node rows resolved to their
	// ROOT tool_call_ids (the chain identity vocabulary), computed by the
	// accounting right before the chain guard reads it.
	BatchDebtCallIDs []string
	Closed           bool
}
