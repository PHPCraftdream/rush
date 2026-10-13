package session

/*
Batch commit hook mapping (R11B, inert; no production caller wiring).

NewBatchCommitHook adapts one accepted jobbatch.Transition into store
projections. It owns no execution, delivers no history and publishes nothing:
the Runner alone dispatches effects after a nil error, and already faults the
Runner on hook error, panic or timeout (ErrCommitFailed), so every error here
is returned as-is, unwrapped-unchanged.

Transition field -> column mapping (batch_nodes unless noted):

  Transition.Sequence     root_sequence + projection_sequence of each touched
                          root, fenced by root_sequence/projection_sequence <
                          @sequence and runner_id = @runner_id; overflow at or
                          above 2^63 is ErrBatchValueOverflow, never truncated.
  Changed[].ID            node_id of the ProjectBatchNodeParams update (or the
                          reservation path for Submit/Add-created nodes).
  Changed[].State/Cause   state / cause (first-latched: the Runner's effective
                          view is written verbatim, never re-derived).
  Changed[].Token/Attempts/Control/ControlError/ControlPending
                          token / attempts / control_state / control_error /
                          control_pending (empty Control maps to 'none').
  Changed[].Outcome.*     outcome_state / outcome_code / outcome_summary.
  NodeView.Batch fields   reserved at Submit/Add insert time: mode, on_fail,
                          notify, max_parallel.
  NodeView.Task fields    reserved at insert time (R5(e)): task_kind,
                          task_group, task_payload, timeout_ns. Changed views
                          never describe them; Launch alone cannot, so never-
                          admitted tasks are durable from reservation on.
  Event.Spec / Items      the reservation tree; Submit reserves under the
                          Runner's exact root ID, Add inserts at the accepted
                          parent ordinals inside Project (no second root).
  Notices[i]              notice_* columns; ordinal = i, the position in
                          Transition.Notices, never a structural ordinal.
                          delivery='pending', wake=1, except HostClosing
                          notices which carry wake=0 and never rewrite an
                          already-latched cause.
  Event.Result (settled)  full envelope JSON (state, code, summary, details)
                          in response_envelope, plus reference "<node>#<seq>"
                          in response_envelope_ref. R5(d) proof: the only
                          pre-existing envelope column, response_envelope_ref,
                          holds a reference string; outcome_summary holds the
                          inspection Summary alone and is CHECK-free but
                          semantically the outcome, not an envelope; Details
                          had no column at all. Additive column, no backfill.
  child_session_id        written by the caller that owns that fact; the
                          update COALESCEs envelope, reference and
                          child_session_id so unrelated events preserve them.

Sequence identity is (host_id, runner_id, sequence): host_id and runner_id
fence every node update, root advance, notice selection and control answer.
runner_id is a fresh UUID per Runner instance; the root UUID stays the
execution incarnation. batch_runner_checkpoints records the same identity for
rootless transitions (capacity_available) and Final (event_type='final',
final=1); Release checkpoints too and leaves retention to R9 ordering.

Submit attachment keys on (owner, tool_call_id=Event.Ref) with
input_hash = sha256(canonical Event.Spec): the Transition carries no trusted
tool-call identity or input bytes of its own, so ingress (R11 package C) pins
them into Ref and the Spec. Same host, different runner incarnation on a live
root is ErrBatchRunnerMismatch — recovery is R9 work, never silent reattach;
a live root on a different host is ErrBatchForeignLiveHost and routes to the
owning Runner, never resubmits here.

Dependency gaps, resolved here / deferred / out of scope:
  R8  post-announcement sync consumption, elected-answer handling, envelope
      loading and node-bound question delivery: deferred to R8 follow-ups and
      R11 package D; this step only makes the envelope durable.
  R9  host registration/probes, partial-commit reconciliation, retention and
      Release ordering, durable owner-side Stop acceptance, cross-incarnation
      recovery: deferred to R9; runner_id fencing here gives it its key.
  R10a conservative captured-root/Done lookup, incarnation routing and
      durable-live rows not present locally: deferred to R10a; RootOwner and
      the runner_id columns are the seams it needs.
Out of scope: any new terminal authority, audit of Commands (Start/Stop are
effects, not durable facts), and SettledRoots (derivable from projected rows).

Inert boundary: no production code calls NewBatchCommitHook, ReserveIdentity,
RootOwner or the new checkpoint query; legacy Reserve keeps its
store-generated-UUID behavior, and every existing caller's observable behavior
is unchanged. No new tests here; the second worker owns acceptance tests.
*/
