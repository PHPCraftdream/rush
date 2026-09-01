package permission

// Fork patch: permission rules are persisted per-session in the SQLite store
// (see DB migration `20260308000002_add_session_permissions.sql` and the
// `enabled` flag from `20260312000002`). The Service interface adds
// ListSessionPermissions / UpdatePermissionEnabled / DeletePermission for the
// web UI's permissions modal. Upstream keeps the rules in memory only.
//
// The PermissionRequest / PermissionNotification structs lost their JSON tags
// on purpose: the web wire format is defined in `internal/server/protocol.go`,
// not on these in-memory types — keeping the tags would cause subtle drift
// between the two layers.
//
// See CHANGELOG.fork.md section 4.C (DB migrations) and section 4.A
// (WebSocket protocol) before resolving a merge conflict in this file.

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"sync/atomic"

	"github.com/PHPCraftdream/rush/internal/csync"
	"github.com/PHPCraftdream/rush/internal/db"
	"github.com/PHPCraftdream/rush/internal/pubsub"
	"github.com/google/uuid"
)

// hookApprovalKey is the unexported context key used to mark a tool call as
// pre-approved by a PreToolUse hook. The value is the tool call ID so an
// approval can't be reused across calls that happen to share a context.
type hookApprovalKey struct{}

// WithHookApproval returns a context that marks the given tool call ID as
// pre-approved by a hook. When the permission service sees a matching
// request it short-circuits the normal prompt and grants immediately.
func WithHookApproval(ctx context.Context, toolCallID string) context.Context {
	return context.WithValue(ctx, hookApprovalKey{}, toolCallID)
}

// hookApproved reports whether the context carries a hook approval for the
// given tool call ID.
func hookApproved(ctx context.Context, toolCallID string) bool {
	if toolCallID == "" {
		return false
	}
	v, _ := ctx.Value(hookApprovalKey{}).(string)
	return v == toolCallID
}

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
}

// sessionRunAllowlistEntry is one per-session restricted-run gate entry:
// the compiled allowlist plus its binding. ownerEpoch 0 marks a
// legacy/epoch-less entry armed without an ownership epoch (the mailbox
// never grants epoch 0 — beginCompact bumps an idle mailbox to 1 — so a
// reserved run's epoch-aware clear can never match a legacy entry by
// accident). ownerCallID "" marks an entry not bound to a logical call.
// The two bindings are independent: the epoch-scoped pair
// (Set/ClearSessionRunAllowlistForEpoch) matches only ownerEpoch, the
// call-scoped pair (Set/ClearSessionRunAllowlistForCall) matches only
// ownerCallID, so a stale clear from one mechanism can never delete the
// other mechanism's entry.
type sessionRunAllowlistEntry struct {
	allowlist   RunAllowlist
	ownerEpoch  uint64
	ownerCallID string
}

type permissionService struct {
	*pubsub.Broker[PermissionRequest]

	notificationBroker *pubsub.Broker[PermissionNotification]
	workingDir         string
	// Fork patch (concurrency): the upstream in-memory grant cache was
	// removed. Request now consults the DB on every call via
	// MatchSessionPermission so a grant created in another rush process
	// (parallel `rush run`) is immediately visible without restart. See
	// CHANGELOG.fork.md and the original fork note about why this used
	// to be a []PermissionRequest slice.
	pendingRequests       *csync.Map[string, chan bool]
	autoApproveSessions   map[string]bool
	autoApproveSessionsMu sync.RWMutex
	skip                  atomic.Bool
	allowedTools          []string
	q                     *db.Queries

	// Fork patch (concurrency, H-4): requestMu used to serialize the whole
	// Request() body — including the blocking wait for a human response —
	// across ALL sessions. That was correct for the single upstream TUI
	// (only one modal could ever be on screen, so only one Request needed
	// to be "live" at a time), but the fork replaced the TUI with a web UI
	// that has no such restriction, and is explicitly designed to drive N
	// concurrent sessions (see CLAUDE.md). Under the old code, an
	// unanswered permission prompt in session A blocked every other
	// session's permission request from even being published until A's
	// context expired.
	//
	// pendingRequests (csync.Map, keyed by permission ID) was already the
	// real per-request synchronization primitive: each Request gets its
	// own response channel, and Grant/Deny/GrantPersistent atomically
	// Take() the entry for the specific ID they're resolving (see #132's
	// fix, 43f8328f). Nothing about that requires a global lock.
	//
	// activeRequest previously held a single *PermissionRequest so
	// GrantPersistent could reconstruct the full record when the
	// production caller sends only the ID (see GrantPersistent). With
	// requestMu gone, more than one Request can be between "publish" and
	// "resolved" at once, so activeRequest is now keyed by permission ID
	// (csync.Map) instead of being a single shared pointer — otherwise a
	// second session's request would clobber the first's activeRequest
	// entry before it's read back.
	activeRequests *csync.Map[string, *PermissionRequest]

	// runAllowlistGate gates the non-interactive auto-approve path. When
	// its compiled allowlist IsRestricted, AutoApproveSession'd sessions
	// no longer get blanket approval — each request must clear the
	// allowlist instead, or it is denied cleanly without waiting for a
	// UI that isn't there. See runallowlist.go.
	runAllowlistGate runAllowlistGate

	// runAllowlistBySession is the per-session restricted-run gate
	// (R1-1): the process-wide runAllowlistGate above is ONE value for
	// the whole service, so two concurrent non-interactive runs with
	// different policies raced for it — a restricted tenant's tool call
	// could be approved under another tenant's unrestricted gate (or
	// vice versa) whenever the second run's SetRunAllowlist landed
	// between the first run's arm and its first permission check.
	// Entries are keyed by the requesting session id (already carried by
	// CreatePermissionRequest, just previously unused by the gate) and
	// take precedence over the shared gate; absent entries fall back to
	// it, so legacy SetRunAllowlist callers behave exactly as before. Since
	// round-2 review R2-1 each entry also carries the mailbox ownership epoch
	// it was armed under, so a stale run's epoch-aware clear can never delete
	// a newer owner's policy.
	runAllowlistBySession   map[string]sessionRunAllowlistEntry
	runAllowlistBySessionMu sync.RWMutex
}

func (s *permissionService) GrantPersistent(permission PermissionRequest) {
	// The handler may send only the ID; fill in the rest from activeRequests.
	// This lookup must happen before Take so we still know what to publish/
	// persist if we win the race below — activeRequests is independent of
	// pendingRequests and reading it doesn't decide a winner.
	if active, ok := s.activeRequests.Get(permission.ID); ok {
		permission = *active
	}

	// Fix (P2.3): Take must run BEFORE any publish or DB write. Take is the
	// atomic get+delete that decides which single concurrent Grant/Deny/
	// GrantPersistent call "wins" for this ID (see #132, 43f8328f). If we
	// published/persisted first and only then took, a losing call could
	// still have broadcast its (wrong, non-winning) outcome to subscribers,
	// or — for GrantPersistent specifically — written a persistent grant to
	// the DB for a request another concurrent call had already denied.
	respCh, ok := s.pendingRequests.Take(permission.ID)
	if !ok {
		slog.Debug("Permission request already resolved", "id", permission.ID)
		return
	}
	respCh <- true

	s.notificationBroker.Publish(pubsub.CreatedEvent, PermissionNotification{
		ToolCallID: permission.ToolCallID,
		Granted:    true,
	})

	// Fork patch (concurrency): the in-memory append was dropped — the DB
	// is the single source of truth so other processes see this grant on
	// their next Request without a restart. See CHANGELOG.fork.md.
	//
	// session_id is intentionally stored as "" so the grant matches
	// requests from ANY session. This preserves the upstream loader's
	// behaviour (which used to overwrite session_id with "" when reading
	// rows into the in-memory cache) and the cross-session contract
	// exercised by TestAlwaysAllow_CrossSession. MatchSessionPermission's
	// WHERE clause (session_id = '' OR session_id = ?) handles the read.
	// Guard: only persist if we have a valid permission (activeRequests matched).
	if s.q != nil && permission.ToolName != "" && permission.Action != "" {
		if err := s.q.CreateSessionPermission(context.Background(), db.CreateSessionPermissionParams{
			ID:        uuid.New().String(),
			SessionID: "",
			ToolName:  permission.ToolName,
			Action:    permission.Action,
			Path:      permission.Path,
		}); err != nil {
			slog.Warn("permission: failed to persist grant", "err", err)
		}
	}

	s.activeRequests.Del(permission.ID)
}

func (s *permissionService) Grant(permission PermissionRequest) {
	// Fix (P2.3): see GrantPersistent for why Take must come first.
	respCh, ok := s.pendingRequests.Take(permission.ID)
	if !ok {
		slog.Debug("Permission request already resolved", "id", permission.ID)
		return
	}
	respCh <- true

	s.notificationBroker.Publish(pubsub.CreatedEvent, PermissionNotification{
		ToolCallID: permission.ToolCallID,
		Granted:    true,
	})

	s.activeRequests.Del(permission.ID)
}

func (s *permissionService) Deny(permission PermissionRequest) {
	// Fix (P2.3): see GrantPersistent for why Take must come first.
	respCh, ok := s.pendingRequests.Take(permission.ID)
	if !ok {
		slog.Debug("Permission request already resolved", "id", permission.ID)
		return
	}
	respCh <- false

	s.notificationBroker.Publish(pubsub.CreatedEvent, PermissionNotification{
		ToolCallID: permission.ToolCallID,
		Granted:    false,
		Denied:     true,
	})

	s.activeRequests.Del(permission.ID)
}

func (s *permissionService) Request(ctx context.Context, opts CreatePermissionRequest) (bool, error) {
	if s.skip.Load() {
		return true, nil
	}

	// Check if the tool/action combination is in the allowlist
	commandKey := opts.ToolName + ":" + opts.Action
	if slices.Contains(s.allowedTools, commandKey) || slices.Contains(s.allowedTools, opts.ToolName) {
		return true, nil
	}

	// A PreToolUse hook that returned decision=allow stamps the context
	// with the tool call ID. Treat that as a pre-approval and skip the
	// prompt entirely. We still publish a granted notification so the UI
	// and audit subscribers see the outcome.
	if hookApproved(ctx, opts.ToolCallID) {
		s.notificationBroker.Publish(pubsub.CreatedEvent, PermissionNotification{
			ToolCallID: opts.ToolCallID,
			Granted:    true,
		})
		return true, nil
	}

	// tell the UI that a permission was requested
	s.notificationBroker.Publish(pubsub.CreatedEvent, PermissionNotification{
		ToolCallID: opts.ToolCallID,
	})

	s.autoApproveSessionsMu.RLock()
	autoApprove := s.autoApproveSessions[opts.SessionID]
	s.autoApproveSessionsMu.RUnlock()

	if autoApprove {
		// Restricted-run gate. In a non-interactive `rush run` the
		// session is auto-approve, but if the operator armed a
		// restricted allowlist (--restrict-run / permissions.run.restrict)
		// we must not blanket-grant. Consult the allowlist; unmatched
		// requests are denied cleanly here so the agent sees a fast
		// "no" instead of hanging on a UI that doesn't exist.
		// R1-1: the session-keyed entry, when present, wins — it is THIS
		// run's own policy, immune to a concurrent run re-arming the
		// process-wide gate. opts.SessionID was always available here;
		// the gate just never consulted it before.
		s.runAllowlistBySessionMu.RLock()
		sessionEntry, hasSessionGate := s.runAllowlistBySession[opts.SessionID]
		s.runAllowlistBySessionMu.RUnlock()
		gate := s.runAllowlistGate.load()
		if hasSessionGate {
			gate = sessionEntry.allowlist
		}
		if gate.IsRestricted() && !gate.allowsRequest(opts) {
			s.notificationBroker.Publish(pubsub.CreatedEvent, PermissionNotification{
				ToolCallID: opts.ToolCallID,
				Granted:    false,
				Denied:     true,
			})
			return false, nil
		}
		s.notificationBroker.Publish(pubsub.CreatedEvent, PermissionNotification{
			ToolCallID: opts.ToolCallID,
			Granted:    true,
		})
		return true, nil
	}

	fileInfo, err := os.Stat(opts.Path)
	dir := opts.Path
	if err == nil {
		if fileInfo.IsDir() {
			dir = opts.Path
		} else {
			dir = filepath.Dir(opts.Path)
		}
	}

	if dir == "." {
		dir = s.workingDir
	}
	permission := PermissionRequest{
		ID:          uuid.New().String(),
		Path:        dir,
		SessionID:   opts.SessionID,
		ToolCallID:  opts.ToolCallID,
		ToolName:    opts.ToolName,
		Description: opts.Description,
		Action:      opts.Action,
		Params:      opts.Params,
	}

	// Fork patch (concurrency): query the persistent-grant table directly
	// on every Request instead of consulting an in-memory cache that was
	// populated only at startup. Under parallel `rush run` processes,
	// the old cache made an "always allow" granted in process A invisible
	// to process B until B restarted, causing B to re-prompt (or block in
	// non-interactive mode). Query cost is one indexed SELECT; the cache
	// scan it replaces was O(N) anyway. See CHANGELOG.fork.md.
	if s.q != nil {
		if _, err := s.q.MatchSessionPermission(ctx, db.MatchSessionPermissionParams{
			ToolName:  permission.ToolName,
			Action:    permission.Action,
			Path:      permission.Path,
			SessionID: permission.SessionID,
		}); err == nil {
			s.notificationBroker.Publish(pubsub.CreatedEvent, PermissionNotification{
				ToolCallID: opts.ToolCallID,
				Granted:    true,
			})
			return true, nil
		}
	}

	// activeRequests lets GrantPersistent reconstruct the full record from
	// an ID-only response (see GrantPersistent). Keyed by permission ID
	// (rather than a single shared slot) since, with requestMu gone,
	// multiple sessions' requests can be pending here concurrently. Grant/
	// Deny/GrantPersistent remove their own entry on the happy path; this
	// defer is the backstop for ctx.Done() and any other early return.
	s.activeRequests.Set(permission.ID, &permission)
	defer s.activeRequests.Del(permission.ID)

	respCh := make(chan bool, 1)
	s.pendingRequests.Set(permission.ID, respCh)
	defer s.pendingRequests.Del(permission.ID)

	// Publish the request
	s.Publish(pubsub.CreatedEvent, permission)

	select {
	case <-ctx.Done():
		s.notificationBroker.Publish(pubsub.CreatedEvent, PermissionNotification{
			ToolCallID: permission.ToolCallID,
			Denied:     true,
		})
		return false, ctx.Err()
	case granted := <-respCh:
		return granted, nil
	}
}

func (s *permissionService) AutoApproveSession(sessionID string) {
	s.autoApproveSessionsMu.Lock()
	s.autoApproveSessions[sessionID] = true
	s.autoApproveSessionsMu.Unlock()
}

// InheritSessionAutoApprove — see the Service interface comment for the
// full rationale. Read and write happen under ONE write-lock hold so a
// concurrent AutoApproveSession/Request on either id can't interleave
// between the check and the propagation.
//
// A childID that would inherit nothing (parent not auto-approved) is
// left absent from the map rather than written as false: Request reads
// the map with a plain index expression, so absent and false behave
// identically, and not writing keeps the map from growing one entry per
// sub-agent delegation in interactive sessions.
func (s *permissionService) InheritSessionAutoApprove(parentID, childID string) {
	if parentID == "" || childID == "" || parentID == childID {
		return
	}
	s.autoApproveSessionsMu.Lock()
	defer s.autoApproveSessionsMu.Unlock()
	if s.autoApproveSessions[parentID] {
		s.autoApproveSessions[childID] = true
	}
}

func (s *permissionService) SubscribeNotifications(ctx context.Context) <-chan pubsub.Event[PermissionNotification] {
	return s.notificationBroker.Subscribe(ctx)
}

func (s *permissionService) SetSkipRequests(skip bool) {
	s.skip.Store(skip)
}

func (s *permissionService) SkipRequests() bool {
	return s.skip.Load()
}

// SetRunAllowlist arms or clears the restricted-run allowlist. The
// allowlist is consulted on the auto-approve path only (see Request),
// so interactive sessions are unaffected.
func (s *permissionService) SetRunAllowlist(allowlist RunAllowlist) {
	s.runAllowlistGate.store(allowlist)
}

// SetSessionRunAllowlist arms allowlist for sessionID only (R1-1). See
// SessionRunAllowlistManager for the contract.
func (s *permissionService) SetSessionRunAllowlist(sessionID string, allowlist RunAllowlist) {
	if sessionID == "" {
		return
	}
	s.runAllowlistBySessionMu.Lock()
	s.runAllowlistBySession[sessionID] = sessionRunAllowlistEntry{allowlist: allowlist}
	s.runAllowlistBySessionMu.Unlock()
}

// ClearSessionRunAllowlist drops sessionID's entry so the session falls
// back to the process-wide gate (R1-1 run-end cleanup; ExecuteRun calls
// it when the run finishes so a long-lived host does not accumulate one
// entry per run). Deleting rather than storing an inert value keeps the
// map from growing per session and keeps the fallback semantics
// explicit. Session auto-approve state is NOT touched — it has no
// cleanup by design (a web session's approval must survive across
// turns), unlike a run-scoped allowlist which is re-armed by every run.
func (s *permissionService) ClearSessionRunAllowlist(sessionID string) {
	if sessionID == "" {
		return
	}
	s.runAllowlistBySessionMu.Lock()
	delete(s.runAllowlistBySession, sessionID)
	s.runAllowlistBySessionMu.Unlock()
}

// SetSessionRunAllowlistForEpoch arms allowlist for sessionID under the
// given ownership epoch (R2-1). See SessionRunAllowlistManager.
func (s *permissionService) SetSessionRunAllowlistForEpoch(sessionID string, allowlist RunAllowlist, ownerEpoch uint64) {
	if sessionID == "" {
		return
	}
	s.runAllowlistBySessionMu.Lock()
	s.runAllowlistBySession[sessionID] = sessionRunAllowlistEntry{allowlist: allowlist, ownerEpoch: ownerEpoch}
	s.runAllowlistBySessionMu.Unlock()
}

// ClearSessionRunAllowlistForEpoch drops sessionID's entry only when it
// still carries ownerEpoch (R2-1): the same epoch-comparison idiom
// mailbox.abandonOwnership uses before mutating shared state. A mismatch
// means a later owner re-armed the entry after this run's era ended, and
// this stale cleanup must not touch it.
func (s *permissionService) ClearSessionRunAllowlistForEpoch(sessionID string, ownerEpoch uint64) {
	if sessionID == "" {
		return
	}
	s.runAllowlistBySessionMu.Lock()
	if entry, ok := s.runAllowlistBySession[sessionID]; ok && entry.ownerEpoch == ownerEpoch {
		delete(s.runAllowlistBySession, sessionID)
	}
	s.runAllowlistBySessionMu.Unlock()
}

// SetSessionRunAllowlistForCall arms allowlist for sessionID under the
// logical call id of the turn that owns it (R3-4).
func (s *permissionService) SetSessionRunAllowlistForCall(sessionID string, allowlist RunAllowlist, ownerCallID string) {
	s.runAllowlistBySessionMu.Lock()
	s.runAllowlistBySession[sessionID] = sessionRunAllowlistEntry{allowlist: allowlist, ownerCallID: ownerCallID}
	s.runAllowlistBySessionMu.Unlock()
}

// ClearSessionRunAllowlistForCall drops sessionID's entry only when it
// still carries ownerCallID (R3-4): a stale clear can never delete a
// newer turn's freshly armed policy.
func (s *permissionService) ClearSessionRunAllowlistForCall(sessionID string, ownerCallID string) {
	s.runAllowlistBySessionMu.Lock()
	if entry, ok := s.runAllowlistBySession[sessionID]; ok && entry.ownerCallID == ownerCallID {
		delete(s.runAllowlistBySession, sessionID)
	}
	s.runAllowlistBySessionMu.Unlock()
}

// InheritSessionRunAllowlist propagates parentID's per-session gate to
// childID under one lock hold (R1-1): a restricted run's sub-agent works
// under its OWN child session id, and without inheritance its first
// non-allowlisted tool call would consult the process-wide gate — whatever
// a concurrent run last armed there — instead of its parent's policy.
// Mirrors InheritSessionAutoApprove's atomicity and its "inherit nothing
// when the parent has nothing" rule.
func (s *permissionService) InheritSessionRunAllowlist(parentID, childID string) {
	if parentID == "" || childID == "" || parentID == childID {
		return
	}
	s.runAllowlistBySessionMu.Lock()
	if entry, ok := s.runAllowlistBySession[parentID]; ok {
		s.runAllowlistBySession[childID] = entry
	}
	s.runAllowlistBySessionMu.Unlock()
}

func NewPermissionService(ctx context.Context, workingDir string, skip bool, allowedTools []string, q *db.Queries) Service {
	svc := &permissionService{
		Broker:                pubsub.NewBroker[PermissionRequest](),
		notificationBroker:    pubsub.NewBroker[PermissionNotification](),
		workingDir:            workingDir,
		autoApproveSessions:   make(map[string]bool),
		runAllowlistBySession: make(map[string]sessionRunAllowlistEntry),
		allowedTools:          allowedTools,
		pendingRequests:       csync.NewMap[string, chan bool](),
		activeRequests:        csync.NewMap[string, *PermissionRequest](),
		q:                     q,
	}
	// Fork merge note (origin/main 6b312bee "fix: potential data race on
	// permissionService"): upstream made skip atomic.Bool and initialises it
	// after struct construction. Their pattern preserved.
	svc.skip.Store(skip)

	// Fork patch (concurrency): startup pre-load into an in-memory cache
	// was removed. Request now queries the DB directly on every call so
	// grants from other processes are immediately visible. See
	// CHANGELOG.fork.md.

	return svc
}

func (s *permissionService) ListSessionPermissions(ctx context.Context, sessionID string) ([]db.SessionPermission, error) {
	return s.q.ListSessionPermissions(ctx, sessionID)
}

func (s *permissionService) UpdatePermissionEnabled(ctx context.Context, ruleID string, enabled bool) error {
	var enabledInt int64
	if enabled {
		enabledInt = 1
	}
	return s.q.UpdatePermissionEnabled(ctx, db.UpdatePermissionEnabledParams{
		Enabled: enabledInt,
		ID:      ruleID,
	})
}

func (s *permissionService) DeletePermission(ctx context.Context, ruleID string) error {
	return s.q.DeletePermission(ctx, ruleID)
}
