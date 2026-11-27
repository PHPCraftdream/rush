package agent

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"sync"
	"time"

	"charm.land/fantasy"
	"github.com/PHPCraftdream/rush/internal/agent/runtime/batch/batchadapter"
	"github.com/PHPCraftdream/rush/internal/agent/runtime/batch/batchcore"
	"github.com/PHPCraftdream/rush/internal/agent/runtime/batch/batchcore/modelcontrol"
	"github.com/PHPCraftdream/rush/internal/agent/runtime/batch/batchexec/batchdispatch"
	"github.com/PHPCraftdream/rush/internal/agent/runtime/batch/batchexec/childagent"
	"github.com/PHPCraftdream/rush/internal/agent/runtime/batch/batchflow"
	"github.com/PHPCraftdream/rush/internal/agent/runtime/batch/batchinst"
	toolwrap "github.com/PHPCraftdream/rush/internal/agent/runtime/policy/toolwrap"
	"github.com/PHPCraftdream/rush/internal/config"
	"github.com/PHPCraftdream/rush/internal/kit/sys/shell/shellbg"
	"github.com/PHPCraftdream/rush/internal/session"
	"github.com/PHPCraftdream/rush/internal/session/jobbatch"
	coresession "github.com/PHPCraftdream/rush/internal/session/service"
	sessionhosts "github.com/PHPCraftdream/rush/internal/session/state/sessionhosts"
)

type batchCredentialKey struct {
	registryID string
	sessionID  string
	messageID  string
	callID     string
	tool       string
}

type batchCredentialLeaf struct {
	fantasy.AgentTool
	escrow *batchCredentialEscrow
}

func (l *batchCredentialLeaf) credentialEscrow() *batchCredentialEscrow { return l.escrow }

// PolicyLeafTool is the production role-introspection seam: HEAD role
// introspection unwraps this wrapper to reach the leaf tool underneath.
func (l *batchCredentialLeaf) PolicyLeafTool() fantasy.AgentTool { return l.AgentTool }

// UnwrapCapturedCredentialLeaf is the narrow credential-escrow capability the
// moved captured walk (toolwrap.CapturedToolFrom) uses: BatchLeafRegistryStore
// .Register wraps every agent leaf, so the executor-side captured lookup must
// see through this exact wrapper.
func (l *batchCredentialLeaf) UnwrapCapturedCredentialLeaf() fantasy.AgentTool {
	return l.AgentTool
}

type batchCredentialEscrow struct{ *batchadapter.CredentialEscrow }

func newBatchCredentialEscrow() *batchCredentialEscrow {
	return &batchCredentialEscrow{batchadapter.NewCredentialEscrow()}
}

func (k batchCredentialKey) adapterKey() batchadapter.CredentialKey {
	return batchadapter.CredentialKey{RegistryID: k.registryID, SessionID: k.sessionID, MessageID: k.messageID, CallID: k.callID, Tool: k.tool}
}

func (e *batchCredentialEscrow) adapterEscrow() *batchadapter.CredentialEscrow {
	if e == nil {
		return nil
	}
	return e.CredentialEscrow
}

func (e *batchCredentialEscrow) consume(key batchCredentialKey, nodeID string) (*CredentialSet, func(), error) {
	return e.adapterEscrow().Consume(key.adapterKey(), nodeID)
}

func (e *batchCredentialEscrow) releaseRoot(root string) { e.adapterEscrow().ReleaseRoot(root) }

type batchCredentialRegistration struct {
	key   batchCredentialKey
	root  string
	creds *CredentialSet
}

func batchCredentialKeyFor(p batchLeafPayload) batchCredentialKey {
	messageID := ""
	if p.Policy != nil {
		messageID = p.Policy.MessageID
	}
	key := batchadapter.CredentialKeyFor(p.RegistryID, p.SessionID, messageID, p.ToolCallID, p.Tool)
	return batchCredentialKey{registryID: key.RegistryID, sessionID: key.SessionID, messageID: key.MessageID, callID: key.CallID, tool: key.Tool}
}

func stampBatchCallCredentials(ctx context.Context, spec jobbatch.Spec) (jobbatch.Spec, error) {
	return batchadapter.StampCallCredentials(spec, batchdispatch.KindAgent, callCredentialsFrom(ctx) != nil, decodeBatchCredentialMarker)
}

func decodeBatchCredentialMarker(raw json.RawMessage) (string, bool, func() (json.RawMessage, error), error) {
	var p batchLeafPayload
	if err := json.Unmarshal(raw, &p); err != nil {
		return "", false, nil, err
	}
	return p.Tool, p.CallCredentials, func() (json.RawMessage, error) {
		p.CallCredentials = true
		return json.Marshal(p)
	}, nil
}

func decodeBatchCredentialRegistration(raw json.RawMessage) (string, bool, func() batchadapter.CredentialKey, error) {
	var p batchLeafPayload
	if err := json.Unmarshal(raw, &p); err != nil {
		return "", false, nil, err
	}
	return p.Tool, p.CallCredentials, func() batchadapter.CredentialKey { return batchCredentialKeyFor(p).adapterKey() }, nil
}

type batchModelDelivery interface {
	DeliverBatch(context.Context, jobbatch.ID, BatchInvocation) (fantasy.ToolResponse, error)
}

type installedBatchModelControl struct {
	outputCache        *batchOutputCache
	runner             batchModelRunner
	delivery           batchModelDelivery
	store              *session.BatchStore
	live               *session.BatchLiveReader
	sessions           coresession.Service
	host, runnerID     string
	profile            *BatchProfile
	resolver           batchinst.MCPCapabilityResolver
	rootLeases         *batchRootLeaseStore
	runMu              *sync.Mutex
	maxDepth, maxNodes int
}

var _ BatchModelControl = (*installedBatchModelControl)(nil)

func NewInstalledBatchModelControl(runner batchModelRunner, delivery batchModelDelivery, store *session.BatchStore, live *session.BatchLiveReader, sessions coresession.Service, host, runnerID string, profile *BatchProfile, resolver batchinst.MCPCapabilityResolver, leases ...*batchRootLeaseStore) BatchModelControl {
	control := &installedBatchModelControl{runner: runner, delivery: delivery, store: store, live: live, sessions: sessions, host: host, runnerID: runnerID, profile: profile, resolver: resolver, runMu: &sync.Mutex{}, maxDepth: batchcore.DefaultMaxDepth, maxNodes: batchcore.DefaultMaxNodes}
	if adapter, ok := runner.(interface{ OutputCache() *batchOutputCache }); ok {
		control.outputCache = adapter.OutputCache()
	}
	if len(leases) > 0 {
		control.rootLeases = leases[0]
	}
	return control
}

func (c *installedBatchModelControl) InstallBatchModelControls(reg *toolwrap.PolicyLeafRegistry, inv BatchInvocation) ([]fantasy.AgentTool, error) {
	if reg == nil {
		return nil, errors.New("batch controls require calling pinned registry")
	}
	return newBatchModelTools(c, inv, reg), nil
}

func (c *installedBatchModelControl) Run(ctx context.Context, inv BatchInvocation, reg *toolwrap.PolicyLeafRegistry, raw modelcontrol.Spec) (jobbatch.ID, error) {
	return c.modelMutations().Run(ctx, inv, reg, raw)
}

func (c *installedBatchModelControl) Add(ctx context.Context, caller, root, parent string, inv BatchInvocation, reg *toolwrap.PolicyLeafRegistry, items []modelcontrol.Item) ([]jobbatch.ID, error) {
	return c.modelMutations().Add(ctx, caller, root, parent, inv, reg, items)
}

func (c *installedBatchModelControl) modelMutations() modelcontrol.Mutations[BatchInvocation] {
	deps := modelcontrol.Mutations[BatchInvocation]{
		Access: c.modelAccess(), Runner: c.runner, RunMu: c.runMu,
		Invocation: func(inv BatchInvocation) modelcontrol.MutationInvocation {
			return modelcontrol.MutationInvocation{
				SessionID: inv.SessionID, CallID: inv.CallID, RegistryID: inv.RegistryID,
				PolicyRegistryID: inv.Policy.RegistryID, RestrictedToolset: inv.Policy.RestrictedToolset, DiskBindingID: inv.Policy.DiskBindingID,
				AgentDepth: inv.AgentDepth, AgentDepthSet: inv.AgentDepthSet, WaiterRank: inv.WaiterRank,
				RoleConfig: inv.roleConfig, RoleContract: inv.Policy.RoleContract,
			}
		},
		Configure: func(inv *BatchInvocation, depth int, cfg *config.Config) {
			inv.AgentDepth, inv.AgentDepthSet, inv.roleConfig = depth, true, cfg
		},
		Validate: (*BatchInvocation).Validate, ParentRoleConfig: parentRoleConfigFrom, DefaultDepth: DefaultAgentDepth,
		Build: func(raw modelcontrol.Spec, inv BatchInvocation, reg *toolwrap.PolicyLeafRegistry, depth int) (jobbatch.Spec, error) {
			inv.AgentDepth = depth
			return modelBatchSpecForAdmission(raw, inv, reg, c.resolver)
		},
		AdmitRoles: admitBatchRoleContracts, RewriteLeaf: rewriteModelBatchLeaf, Measure: measureModelBatchSpecs,
		ValidateSubmit: func(rank int, spec jobbatch.Spec) error {
			return ValidateSubmit(c.profile, batchinst.RankContext{Rank: rank}, nil, spec, c.maxDepth, c.maxNodes, c.resolver)
		},
		ValidateAdd: func(rank int, root jobbatch.ID, spec jobbatch.Spec, depth, used int) error {
			return ValidateAdd(c.profile, batchinst.RankContext{Rank: rank, Root: root}, nil, spec, depth, used, c.maxDepth, c.maxNodes, c.resolver)
		},
		StampCredentials: stampBatchCallCredentials, Hash: batchInvocationHash,
		RoleUnconfigured: ErrAgentRoleUnconfigured, DiskUnavailable: ErrDiskProviderUnavailable, MissingCallID: errBatchMissingCallID,
	}
	if c.store != nil {
		deps.Probe = c.store.BatchProbeLive
	}
	if c.delivery != nil {
		deps.Deliver = c.delivery.DeliverBatch
	}
	if l := c.rootLeases; l != nil {
		deps.Leases = modelcontrol.RootLeases{Matches: l.Matches, Acquire: l.acquire, Release: l.Release, Adopt: l.Adopt}
	}
	return deps
}

// modelAccess adapts the installed root dependencies without storing a second
// copy of their state. In particular, typed nil readers must remain absent.
func (c *installedBatchModelControl) modelAccess() modelcontrol.Access {
	var store modelcontrol.Store
	if c.store != nil {
		store = c.store
	}
	var live modelcontrol.LiveReader
	if c.live != nil {
		live = c.live
	}
	return modelcontrol.NewAccess(store, live, c.sessions, c.runner, c.outputCache.readWithMetadata, c.host, c.runnerID)
}

func (c *installedBatchModelControl) StatusNode(ctx context.Context, caller, root, node string) (any, error) {
	access := c.modelAccess()
	return access.StatusNode(ctx, caller, root, node)
}

func (c *installedBatchModelControl) Output(ctx context.Context, caller, root, node string, cursor uint64) (any, error) {
	access := c.modelAccess()
	return access.Output(ctx, caller, root, node, cursor)
}

func (c *installedBatchModelControl) Stop(ctx context.Context, caller, root, node string) (any, error) {
	access := c.modelAccess()
	return access.Stop(ctx, caller, root, node)
}

// Rehomed retained in-process state from the deleted workLedger
// (work_ledger_reaction.go, work_ledger_driver.go): the per-session fact-hint
// counter + waiter set, the external-driver registration mirror, the durable
// foreign-driver read, and the SessionNoticeStore binding the completion
// notice writers (supervision tick, reaction-chain marker, bg-shell done,
// held child question) use. None of this is ledger state: the hint counter
// and the driver mirror are per-process coordination memory that outlives the
// ledger, the driver READ is the retained session_drivers host marker, and
// session_notices is retained notice infrastructure.
// foreignLiveDriver reports whether another live process drives owner's
// session (a `rush run` loop whose durable session_drivers marker names a
// host that is alive or not provably dead). Rehome of work_ledger_driver.go:
// the read goes through the bound HostStore (SetBatchDriverClaimer wires it;
// the claim/release and the foreign read are the same store in production).
// A coordinator without a HostStore-shaped claimer (bare fixtures) reports
// false -- the conservative answer is live/fact-driven, but a fixture has no
// other process to be foreign.
func (c *coordinator) foreignLiveDriver(ctx context.Context, owner string) (bool, error) {
	return (&batchRehomeCoordRuntimeComponent{host: c}).foreignLiveDriver(ctx, owner)
}

// sessionNoticeStore returns the bound SessionNoticeStore, or nil.
func (c *coordinator) sessionNoticeStore() *session.SessionNoticeStore {
	return (&batchRehomeCoordRuntimeComponent{host: c}).sessionNoticeStore()
}

type batchRehomeCoordRuntimeHost interface {
	foreignDriverClaimHostStore() *sessionhosts.HostStore
	boundSessionNoticeStore() *session.SessionNoticeStore
}

type batchRehomeCoordRuntimeComponent struct{ host batchRehomeCoordRuntimeHost }

func (w *batchRehomeCoordRuntimeComponent) foreignLiveDriver(ctx context.Context, owner string) (bool, error) {
	hs := w.host.foreignDriverClaimHostStore()
	if hs == nil {
		return false, nil
	}
	_, foreign, err := hs.ForeignLiveDriver(ctx, owner)
	return foreign, err
}

func (w *batchRehomeCoordRuntimeComponent) sessionNoticeStore() *session.SessionNoticeStore {
	return w.host.boundSessionNoticeStore()
}

func (c *coordinator) foreignDriverClaimHostStore() *sessionhosts.HostStore {
	if c == nil {
		return nil
	}
	hs, ok := c.batchDriverClaimer.(*sessionhosts.HostStore)
	if !ok {
		return nil
	}
	return hs
}

func (c *coordinator) boundSessionNoticeStore() *session.SessionNoticeStore {
	if c == nil {
		return nil
	}
	return c.noticeStore
}

// Batch rehome (H4 lane P5): production helpers that survived the work-ledger
// deletion but whose bodies lived in deleted files (coordinator_await_tasks.go,
// work_ledger_bgshell_kill.go, turn_stall_tool_detach.go). Every call site in
// the batch runtime is unchanged; the bodies are re-homed here.
// clearSleepAll disarms the session's await_tasks sleep and cancels its
// pending max_wait once schedule. Called on EVERY drain launch that is
// allowed, on a human message (resetConsecutiveResume) and on Stop
// (Cancel). Errors are logged, never fatal: the sleep flag itself is
// already cleared by the time the cancel runs.
func (c *coordinator) clearSleepAll(sessionID string) {
	(&batchRehomeRuntimeComponent{host: c}).clearSleepAll(sessionID)
}

func (c *coordinator) cancelAwaitTimer(sessionID, id string) {
	(&batchRehomeRuntimeComponent{host: c}).cancelAwaitTimer(sessionID, id)
}

// bgShellRowCancelled reports whether a stop control has already cancelled
// shellID in sessionID: its completion must then produce neither a wake
// notice nor an auto-resume slot, because the stop's own result IS the
// shell's answer. It is also called under the coordinator's bgArrival gate
// from persistBGShellCompletion, after the gate a winning stop decision
// holds.
//
// Batch-world representation: the work-ledger bg_shell row (state
// "cancelled") is gone, and the batch runtime's batch_stop control targets
// batch roots, never SDK background shells. A background shell leaves the
// coordinator's manager ONLY through an explicit cancellation control --
// BackgroundShellManager.Kill/KillOwned (tools/bash.go's caller-cancellation
// path) or KillAll (session stop) -- each of which Takes the shell out of
// m.shells before its completion callback can fire; a shell that finished on
// its own stays in the manager until a later bash tool call Removes it, and
// such a synchronous completion never reaches notifyBackgroundJobDone at
// all. Absence from the manager is therefore this build's only honest fact
// of "cancelled by a stop control".
func (c *coordinator) bgShellRowCancelled(sessionID, shellID string) bool {
	return (&batchRehomeRuntimeComponent{host: c}).bgShellRowCancelled(sessionID, shellID)
}

type batchRehomeRuntimeHost interface {
	arbClearSleepAll(sessionID string) string
	wakeSchedulerFor() (*wakeScheduler, error)
	bgShellManager() *shellbg.BackgroundShellManager
}

type batchRehomeRuntimeComponent struct{ host batchRehomeRuntimeHost }

func (w *batchRehomeRuntimeComponent) clearSleepAll(sessionID string) {
	w.cancelAwaitTimer(sessionID, w.host.arbClearSleepAll(sessionID))
}

func (w *batchRehomeRuntimeComponent) cancelAwaitTimer(sessionID, id string) {
	if id == "" {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	sched, err := w.host.wakeSchedulerFor()
	if err == nil {
		// Internal cleanup is idempotent, unlike the model-facing tool.
		err = sched.CancelSchedule(ctx, sessionID, id)
		if err == nil {
			sched.Notify()
		}
	}
	if err != nil {
		slog.Warn("await_tasks: failed to cancel the max_wait schedule",
			"session_id", sessionID, "schedule_id", id, "err", err)
	}
}

func (w *batchRehomeRuntimeComponent) bgShellRowCancelled(sessionID, shellID string) bool {
	if w.host.bgShellManager() == nil {
		return false
	}
	_, ok := w.host.bgShellManager().GetOwned(sessionID, shellID)
	return !ok
}

func (c *coordinator) arbClearSleepAll(sessionID string) string {
	return c.arb.clearSleepAll(sessionID)
}

func (c *coordinator) bgShellManager() *shellbg.BackgroundShellManager {
	return c.background
}

// batchRootLeaseStore is the root adapter preserving the historic unexported
// acquire name over the batchflow.BatchRootLeaseStore ledger (DM5c moved the
// ledger state and behavior to runtime/batch/batchflow/lease.go). The private
// diskOwnerKey construction stays here — it is mechanically tied to the
// unexported diskCapabilityManager — exposed to batchflow only through the
// acquire/release owner callbacks taken at construction.
type batchRootLeaseStore struct {
	caps *diskCapabilityManager

	*batchflow.BatchRootLeaseStore
}

func newBatchRootLeaseStore(caps *diskCapabilityManager) *batchRootLeaseStore {
	s := &batchRootLeaseStore{caps: caps}
	s.BatchRootLeaseStore = batchflow.NewBatchRootLeaseStore(
		ErrDiskProviderUnavailable,
		// A nil manager returns the bare sentinel, as the historic acquire
		// did before ever calling the manager.
		func(binding, registry, instance string) error {
			if s.caps == nil {
				return ErrDiskProviderUnavailable
			}
			return s.caps.acquire(rootOwnerKey(binding, registry, instance))
		},
		// A nil manager is a no-op, as the historic release was.
		func(binding, registry, instance string) {
			if s.caps == nil {
				return
			}
			s.caps.release(rootOwnerKey(binding, registry, instance))
		},
	)
	return s
}

// rootOwnerKey builds the one Root capability owner key. Keep the key
// construction in exactly one place: the ledger and the capability manager
// must never disagree on the key shape.
func rootOwnerKey(binding, registry, instance string) diskOwnerKey {
	return diskOwnerKey{kind: diskOwnerRoot, instance: instance, bindingID: binding, registryID: registry}
}

// BatchRootLeaseStore is the exported alias of the per-root identity
// store. The app package only constructs and passes the store through
// (install → model control); it never calls its methods.
type BatchRootLeaseStore = batchRootLeaseStore

// NewBatchRootLeaseStore is the exported constructor for hosts that wire
// the store by hand (tests, dormant installs); newBatchInstall builds its
// own.
func NewBatchRootLeaseStore(caps *DiskProviderRegistry) *BatchRootLeaseStore {
	return newBatchRootLeaseStore(caps)
}

// acquire takes the Root owner — BEFORE Submit, using the already-minted
// root ID — and records the root's capability identity and owning session.
// A nil store is the historic nil-sentinel contract; every other method is
// the promoted batchflow surface (Acquire/Adopt/Release/Matches/
// OwnerForRoot/Roots), whose lower implementations carry their own nil
// guards.
func (s *batchRootLeaseStore) acquire(root jobbatch.ID, owner, binding, registry string) error {
	if s == nil {
		return ErrDiskProviderUnavailable
	}
	return s.Acquire(root, owner, binding, registry)
}

type batchBoundRunnerHandle interface {
	SubmitBound(ctx context.Context, req jobbatch.SubmitRequest) (jobbatch.ID, *jobbatch.Batch, error)
}

type batchReleaseHandle interface {
	Release(context.Context, jobbatch.ID) error
}

type batchModelRunner interface {
	Submit(ctx context.Context, req jobbatch.SubmitRequest) (jobbatch.ID, error)
	Add(ctx context.Context, req jobbatch.AddRequest) ([]jobbatch.ID, error)
	Snapshot(ctx context.Context, id jobbatch.ID) (jobbatch.BatchView, error)
	View(ctx context.Context, id jobbatch.ID) (jobbatch.NodeView, error)
	Output(ctx context.Context, id jobbatch.ID, cursor uint64) (jobbatch.OutputChunk, error)
	Stop(ctx context.Context, id jobbatch.ID) error
}

// batchDelivery is the narrow R11D seam: deliver this started root per the
// invocation's mode/window and return its tool response. A nil seam is a
// typed not-wired error, never a silent fallback to the old path.
type batchDelivery interface {
	DeliverBatch(ctx context.Context, root jobbatch.ID, inv BatchInvocation) (fantasy.ToolResponse, error)
}

// batchRuntimeConfig specializes the core wiring for root compatibility types.
type batchRuntimeConfig = batchcore.RuntimeConfig[*BatchProfile, batchDelivery, BatchWorkIngress, *DiskProviderRegistry, *batchRootLeaseStore]

// batchRuntime is the dormant R11C ingress core. Construction has NO side
// effects: no goroutine, no executor, no global registration; R11F owns every
// wiring. It is inert until something calls Invoke.
type batchRuntime struct {
	cfg batchRuntimeConfig

	mu   sync.Mutex
	core *batchcore.Ingress
}

func newBatchRuntime(cfg batchRuntimeConfig) (*batchRuntime, error) {
	core, err := cfg.InitIngress(func(profile *BatchProfile) bool {
		return profile != nil && profile.caps != nil
	}, errBatchAttachNotWired, errBatchProfileNotWired, errBatchLeavesNotWired)
	if err != nil {
		return nil, err
	}
	return &batchRuntime{cfg: cfg, core: core}, nil
}

// bindRunner attaches the engine handle (R11F). Safe to call once.
func (r *batchRuntime) bindRunner(runner batchcore.RunnerHandle) error {
	return r.cfg.BindRunner(&r.mu, runner, errBatchRunnerNotWired)
}

// setIngress binds the App-owned ingress (R12-H2 P1-4). Mutex-guarded, so
// it is safe even after Start, though production wiring calls it during
// install, before the runtime ever Invokes.
func (r *batchRuntime) setIngress(ingress BatchWorkIngress) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.cfg.Ingress = ingress
}

// ingress reads the App-owned ingress under the same mutex as setIngress,
// so a LeafEnv built before BindRuntimeIngress still sees the binding
// lazily at invocation time.
func (r *batchRuntime) ingress() BatchWorkIngress {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.cfg.Ingress
}

// LeafEnv builds the executor environment whose leaf resolution is keyed by
// the payload's pinned registry identity (batchdispatch.LeafLookupID), never by
// session, closing the section-3 collision gap.
func (r *batchRuntime) LeafEnv() batchLeafEnv {
	return batchLeafEnv{Leaves: r.cfg.Leaves, DiskBindings: r.cfg.DiskBindings, Ingress: r.ingress}
}

func (r *batchRuntime) RegistryCount() int {
	if r == nil {
		return 0
	}
	if r.cfg.RegistryCount == nil {
		return -1
	}
	return r.cfg.RegistryCount()
}

func batchRootID() jobbatch.ID { return batchcore.RootID() }

func (r *batchRuntime) inFlightCalls() int { return r.core.InFlightCalls() }

// Test compatibility: the channel remains owned by the in-flight core.
type batchCallEntryView struct{ joined <-chan struct{} }

func (r *batchRuntime) entryFor(sessionID, callID string) *batchCallEntryView {
	joined := r.core.Joined(sessionID, callID)
	if joined == nil {
		return nil
	}
	return &batchCallEntryView{joined: joined}
}

// batchAgentScope preserves the root call surface; childagent owns all state.
type batchAgentScope struct {
	state *childagent.Scope
}

var (
	ErrBatchAgentNotHeld         = batchinst.ErrBatchAgentNotHeld
	ErrBatchAgentStopped         = batchinst.ErrBatchAgentStopped
	ErrBatchAgentStaleGeneration = batchinst.ErrBatchAgentStaleGeneration

	ErrBatchQuestionCapabilityNotDurable = batchinst.ErrBatchQuestionCapabilityNotDurable
)

func newBatchAgentScope() *batchAgentScope {
	return &batchAgentScope{state: childagent.NewScope()}
}

func (s *batchAgentScope) HeldDepth() (int, bool) { return s.state.HeldDepth() }
func (s *batchAgentScope) PublishQuestion(publish func(sequence uint64) error) error {
	return s.state.PublishQuestion(publish)
}

func (s *batchAgentScope) beginStop() { s.state.BeginStop() }
func (s *batchAgentScope) stop() bool { return s.state.Stop() }
func (s *batchAgentScope) hold(depth int) (*childagent.HeldQuestion, error) {
	return s.state.Hold(depth)
}
func (s *batchAgentScope) discardHold(h *childagent.HeldQuestion) { s.state.DiscardHold(h) }
func (s *batchAgentScope) deliverWithCommit(msg batchinst.AgentInjectMessage, commit func() error) error {
	return s.state.DeliverWithCommit(msg, commit)
}

// Scope-time batch recovery budgets (H4 W2, review item 1c): ONE total
// recovery budget and at most ONE candidate root attempt per scope call.
// On budget exhaustion or any failure the caller's answer stays
// conservatively live; the App-owned background maintenance pass
// (internal/app/app_batch_maintenance.go) completes the rest later.
// batchScopeRecoveryBudget: a 500ms best-effort watchdog around at most ONE
// candidate root attempt per scope call (raised from 10ms in the store-v1
// composition, where CI showed the recovery write not completing inside the
// old bound; a failure is now logged, see recoverDeadCandidates). Var only so
// tests can zero it for a deterministic budget-exhaustion run; production
// never writes it.
var (
	batchScopeRecoveryBudget   = 500 * time.Millisecond
	batchScopeRecoveryMaxRoots = 1
)

// batchScopeRecoverySource is the store-and-probe pair extracted from the
// coordinator's bound node-backed fact source. The zero source recovers
// nothing: no binding, a conservative unknown source, a nil store or a nil
// live reader all mean no ON recovery is possible, and the caller keeps
// its pre-hook answer unchanged.
type batchScopeRecoverySource struct {
	facts BatchCutoverFacts
	store *session.BatchStore
	probe session.BatchHostProbe
}

// scopeRecoverySource extracts the recovery source from c's bound
// facts. Only the node-backed source (nodeBatchCutoverFacts) carries a
// store; the live reader's own probe and data dir are reused so the
// recovery probe is exactly the predicate the scope read used.
func (c *coordinator) scopeRecoverySource() batchScopeRecoverySource {
	facts := coordinatorBatchCutoverFacts(c)
	nf, ok := facts.(nodeBatchCutoverFacts)
	if !ok || nf.store == nil || nf.live == nil || nf.live.DataDir == "" || nf.live.ProbeHost == nil {
		return batchScopeRecoverySource{}
	}
	probeHost, dataDir := nf.live.ProbeHost, nf.live.DataDir
	return batchScopeRecoverySource{
		facts: facts,
		store: nf.store,
		probe: func(host string) (sessionhosts.HostLockStatus, error) {
			return probeHost(dataDir, host)
		},
	}
}

// recoverDeadCandidates recovers the dead hosts this owner's live scope
// just observed, outside any read transaction: dedupe the hosts whose
// observed verdict is Dead, then ONE bounded RecoverHostsOnce with a
// single-root budget. Dead hosts stay live until recovery commits, so a
// failed, cancelled or budget-exhausted attempt is simply retried by the
// background maintenance pass; it never changes the caller's answer by
// itself. Best-effort: errors are dropped, never propagated.
func (s batchScopeRecoverySource) recoverDeadCandidates(ctx context.Context, owner string) {
	if s.store == nil || s.probe == nil || owner == "" {
		return
	}
	nodes, err := s.facts.LiveNodes(ctx, owner)
	if err != nil {
		// An unreadable scope read must not select recovery targets.
		return
	}
	seen := make(map[string]bool)
	var hosts []string
	for _, n := range nodes {
		if n.HostID == "" || seen[n.HostID] || n.HostStatus != session.HostStatusDead {
			continue
		}
		seen[n.HostID] = true
		hosts = append(hosts, n.HostID)
	}
	if len(hosts) == 0 {
		return
	}
	recCtx, cancel := context.WithTimeout(ctx, batchScopeRecoveryBudget)
	defer cancel()
	if _, _, err := s.store.RecoverHostsOnce(recCtx, s.probe, hosts, batchScopeRecoveryMaxRoots); err != nil {
		// Best-effort: the caller's answer is unchanged and the background
		// maintenance pass retries; the failure is only made visible.
		slog.Warn("scope-time batch recovery failed", "owner", owner, "hosts", hosts, "err", err)
	}
}

// recoverBatchScopeDeadCandidates is the ONE scope-time recovery hook for
// the flag-ON paths (H4 W2): the turn preamble BEFORE the notice pull,
// runningWorkOpen, CLIActiveWork's batch branch, and nothing else.
// Ordinary output/status/live-work rendering must not trigger recovery.
func recoverBatchScopeDeadCandidates(ctx context.Context, c *coordinator, owner string) {
	c.scopeRecoverySource().recoverDeadCandidates(ctx, owner)
}
