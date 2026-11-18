// batch_glue.go holds the coordinator-binding glue functions that contain
// *coordinator type assertions (lane SA2 of the 2026-11-15 agent split,
// design §3.2 step 3); the DM7 lane moves this file verbatim to
// runtime/coord/batch_glue.go.
package agent

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"

	"github.com/PHPCraftdream/rush/internal/agent/provider/cliprovider"
	"github.com/PHPCraftdream/rush/internal/session"
	"github.com/PHPCraftdream/rush/internal/session/jobbatch"
	coresession "github.com/PHPCraftdream/rush/internal/session/service"
)

// NewBatchCLIBuilderWithQuestionRoute builds like NewBatchCLIBuilder and
// stamps the answer-route capability carried by the calling intake surface.
func NewBatchCLIBuilderWithQuestionRoute(c Coordinator, in BatchWorkIngress, questionRoute bool) (*BatchCLIBuilder, error) {
	cc, ok := c.(*coordinator)
	if !ok || in == nil {
		return nil, errors.New("batch CLI: installed coordinator and runtime are required")
	}
	return &BatchCLIBuilder{coord: cc, ingress: in, questionRoute: questionRoute}, nil
}

// SetBatchDriverClaimer wires the driver-marker store onto a coordinator
// (internal/app composition root). Fails closed on nil receiver or store.
func SetBatchDriverClaimer(c Coordinator, s BatchDriverClaimer) error {
	if c == nil {
		return errors.New("batch driver claimer: nil coordinator")
	}
	if s == nil {
		return errors.New("batch driver claimer: nil store")
	}
	co, ok := c.(*coordinator)
	if !ok {
		return errors.New("batch driver claimer: unsupported coordinator type")
	}
	co.batchDriverClaimer = s
	return nil
}

// PscCoordinatorChildReactionFactsLate builds the production child
// reaction facts for an App-level wiring site whose coordinator is
// assigned AFTER the batch install is built: get is re-read on every
// call, so a not-yet-bound coordinator reports the conservative answers
// (scope open/incomplete; initial outcome kept) until it is. The outcome
// half reuses coordinator.refreshSubAgentCompletion — the legacy release
// path's refresh — so reaction-failure markers, the paused-question
// delivery and the empty-final-text notice keep their existing
// semantics.
func PscCoordinatorChildReactionFactsLate(get func() Coordinator) BatchChildReactionFacts {
	if get == nil {
		return nil
	}
	return pscChildReactionFacts{
		scope: func(ctx context.Context, childSessionID string) (CLIScopeState, error) {
			c := get()
			if c == nil {
				return CLIScopeState{}, errPscChildScopeUnavailable
			}
			src, ok := c.(ReactionDebtSource)
			if !ok {
				return CLIScopeState{}, errPscChildScopeUnavailable
			}
			return src.CLIScope(ctx, childSessionID)
		},
		outcome: func(ctx context.Context, childSessionID string, initial AsyncCompletion) (AsyncCompletion, bool) {
			cc, ok := get().(*coordinator)
			if !ok || cc == nil {
				return initial, false
			}
			return cc.refreshSubAgentCompletion(childSessionID, initial), true
		},
	}
}

// BindCoordinatorBatchRuntimeBindings binds one coordinator to its specific
// runtime and provider-binding registry; no process-global provider lookup
// is used, so separate App instances remain isolated.
func BindCoordinatorBatchRuntimeBindings(c Coordinator, ingress BatchWorkIngress, controls BatchModelControl, diskBindings *DiskProviderRegistry, live ...*session.BatchLiveReader) error {
	if c == nil || ingress == nil || diskBindings == nil {
		return errors.New("batch coordinator binding: coordinator, ingress and disk registry are required")
	}
	bind := &batchCoordinatorBinding{ingress: ingress, controls: controls, diskBindings: diskBindings}
	if len(live) > 0 {
		bind.live = live[0]
	}
	// Continuations resolve through the SAME App-owned manager, never a
	// process global. The coordinator fields are written BEFORE the binding
	// is published and only after the asynchronous tool builds started at
	// construction have finished: those builds read diskCaps and bind.coord
	// without a lock, so writing them while a build runs is a data race.
	if cc, ok := c.(*coordinator); ok {
		_ = cc.readyWg.Wait() // first build error is reported by its own path
		cc.diskCaps = diskBindings
		bind.coord = cc
	}
	coordinatorBatchBindings.Store(c, bind)
	// The root agent is built before the batch install binds the manager,
	// so propagate the registry onto it here too.
	if cc, ok := c.(*coordinator); ok {
		if cc.currentAgent != nil {
			if sa, ok := cc.currentAgent.(*sessionAgent); ok {
				sa.bindDiskCaps(diskBindings)
			}
		}
	}
	processBatchBinding.CompareAndSwap(nil, bind)
	// MCP executor routing (R7a): names registered by the MCP family resolve
	// through the MCP kind; every other name keeps the built-in mapping.
	setBatchLeafKindOverride(func(tool string) (string, bool) {
		if strings.HasPrefix(tool, "mcp_") {
			return batchKindMCP, true
		}
		return "", false
	})
	cliprovider.SetBridgeFallback(coordinatorBridgeFallback)
	return nil
}

// SetCoordinatorSessionNoticeStore binds the retained SessionNoticeStore
// onto a coordinator (internal/app composition root). The completion-notice
// writers (supervision tick, reaction-chain marker, bg-shell done, held
// child question) persist through it; nil store or coordinator is refused.
func SetCoordinatorSessionNoticeStore(c Coordinator, s *session.SessionNoticeStore) error {
	if c == nil {
		return errors.New("session notice store: nil coordinator")
	}
	if s == nil {
		return errors.New("session notice store: nil store")
	}
	co, ok := c.(*coordinator)
	if !ok {
		return errors.New("session notice store: unsupported coordinator type")
	}
	co.noticeStore = s
	return nil
}

// AdmitBatchRoleContracts resolves every explicitly selected custom role in a
// structural batch before the caller can reserve capacity or construct tools.
// The returned payloads contain immutable role snapshots used by later queue
// execution; config is read only from the coordinator's pinned snapshot.
func AdmitBatchRoleContracts(ctx context.Context, c Coordinator, spec jobbatch.Spec) (jobbatch.Spec, error) {
	if err := validateBatchSpecShapeForRoleAdmission(spec); err != nil {
		return jobbatch.Spec{}, err
	}
	if batchRoleContractsWereAdmitted(ctx) {
		if err := validateBatchRolePayloads(spec); err != nil {
			return jobbatch.Spec{}, err
		}
		return spec, nil
	}
	if cfg, ok := parentRoleConfigFrom(ctx); ok {
		admitted, err := admitBatchRoleContracts(cfg, parentRoleContractFrom(ctx), spec)
		if err != nil {
			return jobbatch.Spec{}, err
		}
		return admitted, nil
	}
	if cc, ok := c.(*coordinator); ok && cc.cfg != nil {
		cfg, _ := cc.cfg.Snapshot()
		return admitBatchRoleContracts(cfg, parentRoleContractFrom(ctx), spec)
	}
	if batchSpecHasCustomRole(spec) {
		return jobbatch.Spec{}, fmt.Errorf("%w: custom role configuration unavailable", ErrAgentRoleUnconfigured)
	}
	return spec, nil
}

// PsvBatchSupervisionArmingLate builds the production batch delivery
// arming func for an app-level wiring site whose coordinator is assigned
// AFTER the batch install is built (get is re-read on every call). The
// returned func calls the late-bound coordinator's noteWorkStarted, which
// already: exempts delegated children
// (subAgentDrivers), resolves config + per-call CallOptions
// (--no-supervision / --supervision-interval) from the PASSED ctx, is
// idempotent for an already-armed session, and resumes a paused session
// with fresh backoff (legacy new-work resume semantics). A not-yet-bound
// or non-concrete coordinator is a silent no-op, matching
// PscCoordinatorChildReactionFactsLate's late-binding discipline.
func PsvBatchSupervisionArmingLate(get func() Coordinator) func(context.Context, string) {
	return func(ctx context.Context, owner string) {
		if get == nil || owner == "" {
			return
		}
		cc, ok := get().(*coordinator)
		if !ok || cc == nil {
			return
		}
		cc.noteWorkStarted(ctx, owner)
	}
}

// SetCoordinatorSessionCredentialRequirementStore binds the optional durable,
// secret-free marker store used to preserve credential boundaries across restarts.
func SetCoordinatorSessionCredentialRequirementStore(c Coordinator, store coresession.SessionCredentialRequirementStore) error {
	if c == nil {
		return errors.New("session credential requirement store: nil coordinator")
	}
	if store == nil {
		return errors.New("session credential requirement store: nil store")
	}
	co, ok := c.(*coordinator)
	if !ok {
		return errors.New("session credential requirement store: unsupported coordinator type")
	}
	co.credentialReqs = store
	return nil
}

// SetCoordinatorExecutionRequirementStore wires the durable
// session-execution-fence store onto a coordinator (internal/app
// composition root), mirroring the SetBatchDriverClaimer setter pattern.
func SetCoordinatorExecutionRequirementStore(c Coordinator, store coresession.SessionExecutionRequirementStore) error {
	if c == nil {
		return errors.New("execution requirement store: nil coordinator")
	}
	if store == nil {
		return errors.New("execution requirement store: nil store")
	}
	co, ok := c.(*coordinator)
	if !ok {
		return errors.New("execution requirement store: unsupported coordinator type")
	}
	co.execReqs = store
	return nil
}

// SetCoordinatorDiskCapabilityManager binds an App-owned capability
// manager onto an already-constructed coordinator, mirroring what
// BindCoordinatorBatchRuntimeBindings does for a batch-runtime install:
// the coordinator field AND the already-built root agent both get the
// registry (the root agent is constructed inside NewCoordinator, before
// this bind, so its SessionAgentOptions.diskCaps was nil). internal/app's
// InitCoderAgent calls this for an ephemeral flag-ON App (dataDir == "",
// no batch runtime install) so continuations resolve bindings through the
// SAME registry that app_run_setup.go's per-call BindCallDiskProvider
// bound against. Fails closed on nil arguments or a non-standard
// coordinator.
func SetCoordinatorDiskCapabilityManager(c Coordinator, caps *DiskProviderRegistry) error {
	if c == nil {
		return errors.New("disk capability manager: nil coordinator")
	}
	if caps == nil {
		return errors.New("disk capability manager: nil registry")
	}
	co, ok := c.(*coordinator)
	if !ok {
		return errors.New("disk capability manager: unsupported coordinator type")
	}
	_ = co.readyWg.Wait() // in-flight asynchronous tool builds read diskCaps unlocked
	co.diskCaps = caps
	if sa, ok := co.currentAgent.(*sessionAgent); ok {
		sa.bindDiskCaps(caps)
	}
	return nil
}

// SetCoordinatorFinalizationWiring builds the finalization wiring from deps
// and binds it to the concrete coordinator. A disabled flag builds nothing
// and binds nothing; a partially installable configuration fails closed.
func SetCoordinatorFinalizationWiring(c Coordinator, deps FinalizationWiringDeps) error {
	if !deps.Enabled {
		return nil
	}
	if c == nil {
		return fmt.Errorf("finalization wiring: nil coordinator")
	}
	co, ok := c.(*coordinator)
	if !ok {
		return fmt.Errorf("finalization wiring: unsupported coordinator type %T", c)
	}
	w, err := newFinalizationWiring(deps, co)
	if err != nil {
		return err
	}
	co.finalization = w
	slog.Info("Finalization wiring installed", "session_poll", w.poll.String())
	return nil
}

type batchCutoverGlueHost interface {
	batchGlueNil() bool
	batchGlueDriverClaimer() BatchDriverClaimer
	batchGlueBGArrival() *ctxMutex
	batchGlueForeignLiveDriver(ctx context.Context, sessionID string) (bool, error)
	batchGlueSessions() coresession.Service
	batchGlueDurableChild(ctx context.Context, sessionID string) (bool, error)
	batchGlueAutonomyConfigured() bool
	batchGlueAutonomyEnabled() bool
	batchGlueArbSnapshot(sessionID string) arbiterSnapshot
	batchGlueDriverHints() *batchDriverHints
	batchGlueStopTree(sessionID string)
	batchGlueCutoverFacts() BatchCutoverFacts
	batchGlueRerunRuntime() (batchRunnerHandle, string, *session.BatchStore)
}

type coordinatorBatchGlueHost struct{ coordinator *coordinator }

func newBatchCutoverGlueComponent(c *coordinator) *batchCutoverGlueComponent {
	return &batchCutoverGlueComponent{host: coordinatorBatchGlueHost{coordinator: c}}
}

func (h coordinatorBatchGlueHost) batchGlueNil() bool { return h.coordinator == nil }

func (h coordinatorBatchGlueHost) batchGlueDriverClaimer() BatchDriverClaimer {
	return h.coordinator.batchDriverClaimer
}

func (h coordinatorBatchGlueHost) batchGlueBGArrival() *ctxMutex {
	return &h.coordinator.bgArrival
}

func (h coordinatorBatchGlueHost) batchGlueForeignLiveDriver(ctx context.Context, sessionID string) (bool, error) {
	return h.coordinator.foreignLiveDriver(ctx, sessionID)
}

func (h coordinatorBatchGlueHost) batchGlueSessions() coresession.Service {
	return h.coordinator.sessions
}

func (h coordinatorBatchGlueHost) batchGlueDurableChild(ctx context.Context, sessionID string) (bool, error) {
	return h.coordinator.isDurableDelegationChild(ctx, sessionID)
}

func (h coordinatorBatchGlueHost) batchGlueAutonomyConfigured() bool { return h.coordinator.cfg != nil }

func (h coordinatorBatchGlueHost) batchGlueAutonomyEnabled() bool {
	return h.coordinator.autonomyEnabled()
}

func (h coordinatorBatchGlueHost) batchGlueArbSnapshot(sessionID string) arbiterSnapshot {
	return h.coordinator.arb.snapshot(sessionID)
}

func (h coordinatorBatchGlueHost) batchGlueDriverHints() *batchDriverHints {
	return h.coordinator.driverHints
}

func (h coordinatorBatchGlueHost) batchGlueStopTree(sessionID string) {
	h.coordinator.stopTree(sessionID)
}

func (h coordinatorBatchGlueHost) batchGlueCutoverFacts() BatchCutoverFacts {
	return coordinatorBatchCutoverFacts(h.coordinator)
}

func (h coordinatorBatchGlueHost) batchGlueRerunRuntime() (batchRunnerHandle, string, *session.BatchStore) {
	return batchRerunRuntime(h.coordinator)
}

type batchCutoverGlueComponent struct{ host batchCutoverGlueHost }

func (g *batchCutoverGlueComponent) driverMarkerStore() BatchDriverClaimer {
	if g.host.batchGlueNil() || g.host.batchGlueDriverClaimer() == nil {
		return nil
	}
	return g.host.batchGlueDriverClaimer()
}

func (g *batchCutoverGlueComponent) readTurnFactsBatch(ctx context.Context, facts BatchCutoverFacts, sessionID string, f TurnFacts) (TurnFacts, error) {
	h := g.host
	// Gate order (ASYNC-02/09): the seam fires right
	// before the arrival gate and the rows read under bgArrival, so the debt
	// rows and the over-cap set are ONE consistent pair
	// (persistBGShellCompletion holds the same gate around insert + slot
	// decision).
	if seam := readTurnFactsGateSeam.Load(); seam != nil {
		(*seam)()
	}
	if err := h.batchGlueBGArrival().lock(ctx); err != nil {
		return f, fmt.Errorf("turn facts: bg arrival: %w", err)
	}
	defer h.batchGlueBGArrival().unlock()
	rows, err := facts.PendingInclusiveDebt(ctx, sessionID)
	if err != nil {
		return f, fmt.Errorf("turn facts: batch debt rows: %w", err)
	}
	f.Debt.BGShellOnly = len(rows.Nodes) == 0 && len(rows.Notices) > 0
	for _, n := range rows.Notices {
		if n.Kind != session.NoticeKindBGShellDone {
			f.Debt.BGShellOnly = false
		}
	}
	f.Debt.BGShellNotices = len(rows.Notices)
	// CompletionOnly (#1270 rule 4b): notice rows count only as completions
	// for bg_shell_done and supervision kinds; batch node debt rows are
	// completion-class unconditionally (a finished node's selected notice
	// IS a completion), like job debt was on the ledger path.
	f.Debt.CompletionOnly = true
	for _, n := range rows.Notices {
		switch n.Kind {
		case session.NoticeKindBGShellDone, session.NoticeKindSupervision:
		default:
			f.Debt.CompletionOnly = false
		}
	}
	// Reaction-independent held questions: a child question whose answer was
	// never ACCEPTED stays non-completion debt even after the owner's
	// reaction flow marked the notice reacted, so rule 4b's armed sleep
	// cannot hold across a held child (the 45-minute hang). An unreadable
	// read is conservative: not completion-only.
	if src, ok := facts.(BatchHeldQuestionSource); ok {
		if held, herr := src.ActionableHeldQuestions(ctx, sessionID); herr != nil || len(held) > 0 {
			f.Debt.CompletionOnly = false
		}
	}
	f.Debt.PendingIncl = len(rows.Nodes) > 0 || len(rows.Notices) > 0
	// The session driver marker is retained host infrastructure
	// (session_drivers, not batch rows).
	f.Session.ForeignDriverLive, err = h.batchGlueForeignLiveDriver(ctx, sessionID)
	if err != nil {
		return f, fmt.Errorf("turn facts: driver marker: %w", err)
	}
	// The child's parent is the composition key: the parent's live tree scan
	// answers "a live node names this child" (= running delegation).
	parent := ""
	if h.batchGlueSessions() != nil {
		if sess, gerr := h.batchGlueSessions().Get(ctx, sessionID); gerr == nil {
			parent = sess.ParentSessionID
		}
	}
	if parent != "" {
		f.Session.RunningDelegation, err = facts.LiveDelegationChildOf(ctx, parent, sessionID)
		if err != nil {
			return f, fmt.Errorf("turn facts: delegation state: %w", err)
		}
	}
	if !f.Session.ExternallyDrivenSelf {
		f.Session.DurableChild, err = h.batchGlueDurableChild(ctx, sessionID)
		if err != nil {
			return f, fmt.Errorf("turn facts: delegation identity: %w", err)
		}
	}
	if h.batchGlueAutonomyConfigured() {
		f.Session.AutonomyEnabled = h.batchGlueAutonomyEnabled()
	}
	// ---- in-process half ----
	s := h.batchGlueArbSnapshot(sessionID)
	owed := make(map[int64]struct{}, len(rows.Notices))
	for _, n := range rows.Notices {
		owed[n.ID] = struct{}{}
	}
	if s.AutoResumes >= turnAutoResumeCap { // over-cap rows only count with every slot spent
		for id := range s.OverCap {
			if _, ok := owed[id]; ok {
				f.Debt.OverCapRows++
			}
		}
	}
	f.Session.AutoResumes = s.AutoResumes
	f.Session.Held = s.Held
	f.Session.Suspended = s.Suspended
	f.Session.SleepAll = s.SleepAll
	f.Session.ChainLinks = s.ChainLinks
	f.Session.ChainNoticed = s.ChainNoticed
	if len(s.ChainBatchRoots) > 0 {
		f.Session.ChainBatchRoots = make([]string, 0, len(s.ChainBatchRoots))
		for root := range s.ChainBatchRoots {
			f.Session.ChainBatchRoots = append(f.Session.ChainBatchRoots, root)
		}
	}
	if s.SleepAll {
		own, descendants, liveErr := facts.LiveWorkSummary(ctx, sessionID)
		if liveErr != nil {
			return f, fmt.Errorf("turn facts: batch live work: %w", liveErr)
		}
		// Own roots plus live delegation descendants count (#1270).
		f.Session.RunningOwnJobs = own + descendants
	}
	// Rule 5: the chain guard owns the debt by IDENTITY,
	// not by shape. Batch launches carry their launched root's tool_call_id
	// (the started tool response's own identity, recorded per leg by
	// recordChainEvidence into chainBatchRoots), and every node debt row is
	// keyed by its root id -- so the ON predicate is ownership: the chain
	// defers only when EVERY pending node debt row is a completion of a
	// root the chain itself launched, and no notice row intervenes. An
	// unrelated completion (a root the chain never launched) is NOT owned,
	// so it neither defers the chain nor gets deferred by it. Rule 5 defers
	// WITHOUT pacing: there is no "waits one pace" step anywhere on this
	// path -- the deferred launch is simply refused until the ownership
	// predicate changes.
	if f.Session.ChainLinks >= turnReactionChainLimit && len(rows.Nodes) > 0 && len(rows.Notices) == 0 {
		if len(f.Session.ChainBatchRoots) > 0 {
			ids := facts.DebtToolCallIDs(ctx, session.BatchDebtSnapshot{Owner: sessionID, Nodes: rows.Nodes})
			// No resolvable identity: fail closed (no defer) rather than
			// pretend ownership over rows we cannot name.
			owns := len(ids) > 0
			for _, id := range facts.DebtToolCallIDs(ctx, session.BatchDebtSnapshot{Owner: sessionID, Nodes: rows.Nodes}) {
				found := false
				for _, root := range f.Session.ChainBatchRoots {
					if root == id {
						found = true
						break
					}
				}
				if !found {
					owns = false
					break
				}
			}
			f.Session.ChainOwnsAllDebt = owns
		}
	}
	f.Gate = s.Gate
	f.Gate.HintNow = h.batchGlueDriverHints().hintSeqOf(sessionID)
	f.Session.ExternallyDrivenSelf = h.batchGlueDriverHints().isExternalDriver(sessionID)
	return f, nil
}

func (g *batchCutoverGlueComponent) batchRerunStopVoided(ctx context.Context, sessionID string, voided []session.BatchRerunVoidedNode) {
	h := g.host
	if h.batchGlueNil() || len(voided) == 0 {
		return
	}
	runner, host, store := h.batchGlueRerunRuntime()
	if runner == nil || host == "" {
		slog.Warn("coordinator.BatchRerunStopVoided: batch runtime not bound; voided incarnations retire by delivery fencing only",
			"session_id", sessionID, "voided", len(voided))
	}
	for i := range voided {
		v := voided[i]
		// Stop every voided delegation's child tree first, regardless of the
		// root's own state (the retained transitive-stop shape).
		if v.ChildSessionID != "" {
			h.batchGlueStopTree(v.ChildSessionID)
		}
		if !batchRerunStopWorthy(v.State) {
			continue
		}
		if runner == nil {
			continue
		}
		if v.HostID != host {
			slog.Warn("coordinator.BatchRerunStopVoided: voided running incarnation lives on another host; its executor is not stopped, its completion commits void",
				"session_id", sessionID, "root_id", v.RootID, "tool_call_id", v.ToolCallID, "host_id", v.HostID)
			continue
		}
		// Exact-incarnation guard AND repeated-stop suppression: re-read the
		// row and stop only while THIS root is still live. The durable void
		// already took delivery out of the pullable world; once the executor
		// confirms the stop the root settles terminal, so a second pass over
		// the same Voided set never cancels again — and a provider-reused
		// tool_call_id on a NEW live root can never widen this stop onto the
		// new incarnation.
		if store != nil {
			snap, err := store.Read(ctx, sessionID, v.RootID)
			if err != nil {
				slog.Warn("coordinator.BatchRerunStopVoided: voided incarnation row unreadable; skipping the stop",
					"session_id", sessionID, "root_id", v.RootID, "err", err)
				continue
			}
			if !batchRerunStopWorthy(snap.Root.State) {
				continue
			}
		}
		out := cancelBatchCall(ctx, runner, jobbatch.ID(v.RootID))
		if out.Err != nil {
			slog.Warn("coordinator.BatchRerunStopVoided: stop request failed; the completion commits void",
				"session_id", sessionID, "root_id", v.RootID, "err", out.Err)
		}
	}
}

func (g *batchCutoverGlueComponent) heldQuestionAwaitRefusal(ctx context.Context, owner string) error {
	src, ok := g.host.batchGlueCutoverFacts().(BatchHeldQuestionSource)
	if !ok {
		return nil
	}
	routes, err := src.ActionableHeldQuestions(ctx, owner)
	if err != nil {
		return batchHeldQuestionAwaitError(routes)
	}
	if len(routes) == 0 {
		return nil
	}
	return batchHeldQuestionAwaitError(routes)
}

func (g *batchCutoverGlueComponent) batchHeldQuestionResumeRefusal(ctx context.Context, owner, child string) string {
	src, ok := g.host.batchGlueCutoverFacts().(BatchHeldQuestionSource)
	if !ok {
		return ""
	}
	routes, err := src.ActionableHeldQuestions(ctx, owner)
	if err != nil {
		return fmt.Sprintf("resume_session_id %q cannot be resumed while held-question state is unavailable; inspect_agent or inject_agent must address the existing child, not create another delegation", child)
	}
	if len(routes) == 0 {
		return ""
	}
	var childRoutes []BatchHeldQuestionRoute
	for _, route := range routes {
		if route.ChildSessionID == child {
			childRoutes = append(childRoutes, route)
		}
	}
	if len(childRoutes) == 0 {
		return ""
	}
	return fmt.Sprintf(
		"resume_session_id %q would queue a NEW run behind this child's HELD batch question; the held ask wakes only on inject_agent, so the queued run could not start. Answer the held question instead: %s",
		child, batchHeldQuestionHintText(childRoutes))
}

func (g *batchCutoverGlueComponent) batchHeldQuestionResumeHint(ctx context.Context, owner string) string {
	src, ok := g.host.batchGlueCutoverFacts().(BatchHeldQuestionSource)
	if !ok {
		return ""
	}
	routes, err := src.ActionableHeldQuestions(ctx, owner)
	if err != nil || len(routes) == 0 {
		return ""
	}
	return "\n" + batchHeldQuestionHintText(routes)
}
