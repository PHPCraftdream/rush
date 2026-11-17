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
	core "github.com/PHPCraftdream/rush/internal/session/service"
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
func SetCoordinatorSessionCredentialRequirementStore(c Coordinator, store core.SessionCredentialRequirementStore) error {
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
func SetCoordinatorExecutionRequirementStore(c Coordinator, store core.SessionExecutionRequirementStore) error {
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
