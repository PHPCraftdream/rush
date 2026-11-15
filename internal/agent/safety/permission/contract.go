package permission

import (
	"context"

	"github.com/PHPCraftdream/rush/internal/kit/sync/pubsub"
	"github.com/PHPCraftdream/rush/internal/store/db"
)

type CreatePermissionRequest struct {
	SessionID   string `json:"session_id"`
	ToolCallID  string `json:"tool_call_id"`
	ToolName    string `json:"tool_name"`
	Description string `json:"description"`
	Action      string `json:"action"`
	Params      any    `json:"params"`
	Path        string `json:"path"`
}

type PermissionNotification struct {
	ToolCallID string
	Granted    bool
	Denied     bool
}

type PermissionRequest struct {
	ID          string
	SessionID   string
	ToolCallID  string
	ToolName    string
	Description string
	Action      string
	Params      any
	Path        string
}

type Service interface {
	pubsub.Subscriber[PermissionRequest]
	GrantPersistent(permission PermissionRequest)
	Grant(permission PermissionRequest)
	Deny(permission PermissionRequest)
	Request(ctx context.Context, opts CreatePermissionRequest) (bool, error)
	AutoApproveSession(sessionID string)
	// InheritSessionAutoApprove propagates parentID's auto-approve status
	// (if any) to childID, atomically. Sub-agent delegations run under
	// their OWN child session id, so without this a non-interactive
	// `rush run` — which auto-approves only the root session it was
	// given — leaves every delegated sub-agent unapproved, and the
	// sub-agent's first non-safe tool call blocks forever on a UI prompt
	// that does not exist in that mode. Deliberately inheritance rather
	// than a blanket auto-approve: an INTERACTIVE parent's sub-agent must
	// still go through the normal prompt path.
	InheritSessionAutoApprove(parentID, childID string)
	SetSkipRequests(skip bool)
	SkipRequests() bool
	// SetRunAllowlist arms the restricted-run allowlist used by
	// `rush run`. Pass the zero value (or call with IsRestricted ==
	// false) to restore the legacy auto-approve-everything behaviour.
	// The allowlist only governs the non-interactive auto-approve path;
	// it never affects interactive (TUI / web) permission flows.
	SetRunAllowlist(allowlist RunAllowlist)
	SubscribeNotifications(ctx context.Context) <-chan pubsub.Event[PermissionNotification]
	ListSessionPermissions(ctx context.Context, sessionID string) ([]db.SessionPermission, error)
	UpdatePermissionEnabled(ctx context.Context, ruleID string, enabled bool) error
	DeletePermission(ctx context.Context, ruleID string) error
}

// RestrictedRunAuthorizer lets command tools check a per-call restricted-run
// policy without entering the interactive permission flow. Implementations
// are optional so lightweight permission test doubles remain valid.
type RestrictedRunAuthorizer interface {
	AuthorizeRestrictedRun(opts CreatePermissionRequest) (restricted bool, allowed bool)
}

type RestrictedRunContextAuthorizer interface {
	AuthorizeRestrictedRunContext(ctx context.Context, opts CreatePermissionRequest) (restricted bool, allowed bool)
}

// RestrictedRunToolAuthorizer checks the coarse tool gate before dispatch.
// Tool-specific permission requests still enforce their action and path.
type RestrictedRunToolAuthorizer interface {
	AuthorizeRestrictedTool(opts CreatePermissionRequest) (restricted bool, allowed bool)
}

// RestrictedRunDispatchAuthorizer is the central pre-Request gate. A denied
// dispatch publishes the same decided notification Request would publish;
// an allowed dispatch leaves publication to the underlying tool Request.
type RestrictedRunDispatchAuthorizer interface {
	AuthorizeRestrictedDispatch(ctx context.Context, opts CreatePermissionRequest) (restricted bool, allowed bool)
}

// SessionRunAllowlistManager is the OPTIONAL per-session extension of the
// restricted-run gate (R1-1). Consumers type-assert on it instead of it
// living on Service so every existing Service test fake keeps compiling
// (same consuming-interface pattern as the agent package's
// credentialRunner).
//
//   - SetSessionRunAllowlist arms allowlist for sessionID ONLY: Request
//     consults it before the process-wide gate armed by SetRunAllowlist.
//     This is what lets two concurrent non-interactive runs on one host
//     carry different restricted-run policies without racing for a single
//     shared value.
//   - ClearSessionRunAllowlist drops sessionID's entry (run-end cleanup);
//     the session then falls back to the process-wide gate.
//   - InheritSessionRunAllowlist propagates parentID's entry to childID
//     (sub-agent sessions), mirroring InheritSessionAutoApprove: a
//     restricted run's delegated sub-agent must not silently escape the
//     restriction by carrying a child session id the gate has no entry
//     for. No parent entry => child gets none (interactive children keep
//     the normal prompt path).
type SessionRunAllowlistManager interface {
	SetSessionRunAllowlist(sessionID string, allowlist RunAllowlist)
	ClearSessionRunAllowlist(sessionID string)
	InheritSessionRunAllowlist(parentID, childID string)

	// SetSessionRunAllowlistForEpoch arms allowlist for sessionID, bound to
	// the mailbox ownership epoch the caller holds for that session
	// (round-2 review R2-1). Call it only AFTER winning the reservation
	// (ReserveExclusive) so a policy is never installed for a run that has
	// not been admitted yet.
	SetSessionRunAllowlistForEpoch(sessionID string, allowlist RunAllowlist, ownerEpoch uint64)
	// ClearSessionRunAllowlistForEpoch drops sessionID's entry ONLY if the
	// stored entry still carries ownerEpoch — the permission-layer
	// analogue of mailbox.abandonOwnership's epoch check. A stale run's
	// deferred cleanup therefore can never delete a NEWER owner's freshly
	// armed policy, and an entry armed by a run that lost admission is
	// never created in the first place.
	ClearSessionRunAllowlistForEpoch(sessionID string, ownerEpoch uint64)

	// SetSessionRunAllowlistForCall arms allowlist for sessionID bound to
	// the owning turn's LogicalCallID (round-3 review R3-4). This is the
	// per-turn activation mechanism: the CALLER arms the policy only when
	// its call actually becomes the session's active turn — never at
	// queue time — so a queued call's policy can neither leak into the
	// currently running turn nor be armed for a turn that has not
	// started.
	SetSessionRunAllowlistForCall(sessionID string, allowlist RunAllowlist, ownerCallID string)
	// ClearSessionRunAllowlistForCall drops sessionID's entry ONLY if the
	// stored entry still carries ownerCallID — the same
	// compare-and-delete idiom as ClearSessionRunAllowlistForEpoch, keyed
	// by the logical call id instead of the mailbox epoch. The mailbox
	// epoch cannot serve this role on the queueing path: one owner's
	// dispatch loop runs EVERY queued turn under the SAME epoch
	// (runOwned's epoch parameter does not change across its
	// call=next iterations), so consecutive turns are distinguishable
	// only by their own LogicalCallID. A stale clear (a later owner, a
	// newer turn's policy) therefore never deletes another turn's entry.
	ClearSessionRunAllowlistForCall(sessionID string, ownerCallID string)

	// InheritSessionRunAllowlistForGeneration propagates parentID's
	// per-session gate to childID exactly like InheritSessionRunAllowlist,
	// but binds the copy to generation (a delegation driver's registration
	// id, internal/agent's subAgentDriverRegistry -- async phase 3 §6.2) so
	// ClearSessionRunAllowlistForGeneration can later remove ONLY that
	// specific copy. Used at the three call sites that re-inherit a
	// delegated child's allowlist on every delegation/wake
	// (coordinator_subagents.go's runSubAgent, async_tool.go's Run,
	// agent_drain.go's drainCallFor) instead of the ungoverned
	// InheritSessionRunAllowlist, so their entry can finally be cleared once
	// the child's scope closes without risking the §6.3 race a plain
	// Set/ClearSessionRunAllowlist pair would reopen.
	InheritSessionRunAllowlistForGeneration(parentID, childID string, generation uint64)
	// ClearSessionRunAllowlistForGeneration drops childID's entry ONLY if it
	// still carries generation -- the same compare-and-delete idiom as
	// ClearSessionRunAllowlistForEpoch, keyed by the delegation driver's
	// registration generation instead of a mailbox ownership epoch. A stale
	// clear from an OLD generation (the delegation's scope closing after a
	// resume_session_id has already re-armed a NEWER generation) therefore
	// never deletes the newer copy.
	ClearSessionRunAllowlistForGeneration(childID string, generation uint64)
}

// SessionRunAllowlistBaselineManager is the per-session DEMOTED
// legacy-row fallback gate. Durable run-queue rows now persist each
// call's own policy spec (session.SessionAgentCallData.RunAllowlistSpec)
// and the run-queue pump's RebuildSessionAgentCall recompiles it (R4-1),
// so every NEW row's restarted turn arms its own per-call entry (R3-4
// SetSessionRunAllowlistForCall) that Request consults BEFORE this
// baseline. The baseline can therefore no longer decide any new row's
// policy and cannot exhibit the cross-call policy mix-up — or the
// unrestricted-sub-agent leak (R4-2 closes the residual window in
// InheritSessionRunAllowlist) — that a per-SESSION last-writer-wins map
// once could. Its only remaining role: turns rebuilt from rows persisted
// BEFORE the spec field existed (their JSON carries no spec), for which
// nothing better can be reconstructed in this process. Consumers
// type-assert on it instead of it living on Service, exactly like
// SessionRunAllowlistManager, so every existing Service test fake keeps
// compiling.
//
// SetSessionRunAllowlistBaseline pins allowlist as sessionID's FALLBACK
// restricted-run policy: it is consulted only for auto-approved requests
// that arrive when the session has NO per-call entry in the R3-4
// SetSessionRunAllowlistForCall map — i.e. legacy rows only. Two honest
// residuals remain for those rows: (1) a legacy row judged only by the
// baseline inherits whichever run armed the baseline LAST for that
// session, not necessarily its own caller's policy; (2) after a real
// process restart the in-memory baseline is gone too, and such a row
// falls to the unrestricted process-wide gate — the pre-baseline status
// quo, healed by any re-run of the turn through ExecuteRun.
//
// Deliberately NOT cleared at run end: the durable restart can happen
// long after the arming run returned, so the baseline's lifetime matches
// the session's auto-approve flag, which is also never revoked. A later
// ExecuteRun on the same session overwrites it. The COMPILED allowlist is
// stored (not the spec) because the baseline, like the per-call entries,
// is consumed only inside this process.
type SessionRunAllowlistBaselineManager interface {
	SetSessionRunAllowlistBaseline(sessionID string, allowlist RunAllowlist)
}

// sessionRunAllowlistEntry is one per-session restricted-run gate entry:
// the compiled allowlist plus its binding. ownerEpoch 0 marks a
// legacy/epoch-less entry armed without an ownership epoch (the mailbox
// never grants epoch 0 — beginCompact bumps an idle mailbox to 1 — so a
// reserved run's epoch-aware clear can never match a legacy entry by
// accident). ownerCallID "" marks an entry not bound to a logical call.
// ownerDriverGen 0 marks an entry not bound to a delegation driver's
// registration generation (subAgentDriverRegistry never issues generation
// 0 either — async phase 3 §6.2). The three bindings are independent: the
// epoch-scoped pair (Set/ClearSessionRunAllowlistForEpoch) matches only
// ownerEpoch, the call-scoped pair (Set/ClearSessionRunAllowlistForCall)
// matches only ownerCallID, the generation-scoped pair
// (InheritSessionRunAllowlistForGeneration/ClearSessionRunAllowlistForGeneration)
// matches only ownerDriverGen, so a stale clear from one mechanism can never
// delete another mechanism's entry.
type sessionRunAllowlistEntry struct {
	allowlist      RunAllowlist
	ownerEpoch     uint64
	ownerCallID    string
	ownerDriverGen uint64
}
