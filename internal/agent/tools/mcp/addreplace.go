package mcp

import (
	"context"
	"errors"
	"fmt"
	"github.com/PHPCraftdream/rush/internal/config"
	"github.com/PHPCraftdream/rush/internal/pubsub"
	"reflect"
	"sync"
)

// addTransaction outlives the initializer phase of an AddServer call. The
// Add admission and its server-lease identity remain retained through the
// durable config commit, because RemoveServer must serialize against the
// complete in-memory Add transaction. The transaction is released only after
// that commit or its rollback has completed.
type addTransaction struct {
	name      string
	cfg       *config.ConfigStore
	mcpConfig config.MCPConfig
	token     uint64
	done      chan struct{}

	once         sync.Once
	mu           sync.Mutex
	userMutation bool
	durable      bool
}

func (t *addTransaction) markUserMutation() {
	if t == nil {
		return
	}
	t.mu.Lock()
	t.userMutation = true
	t.mu.Unlock()
}

func (t *addTransaction) hasUserMutation() bool {
	if t == nil {
		return false
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.userMutation
}

func (t *addTransaction) markDurable() {
	if t == nil {
		return
	}
	t.mu.Lock()
	t.durable = true
	t.mu.Unlock()
}

func (t *addTransaction) isDurable() bool {
	if t == nil {
		return false
	}
	t.mu.Lock()
	durable := t.durable
	t.mu.Unlock()
	return durable
}

// AddServer validates and adds a new MCP server. It attempts to connect; if
// successful the server is added to the in-memory config and persisted to disk.
func AddServer(ctx context.Context, cfg *config.ConfigStore, name string, mcpCfg config.MCPConfig) error {
	return addServerWithPreparationAndPersistence(ctx, cfg, name, mcpCfg, prepareClient,
		func(cfg *config.ConfigStore, scope config.Scope, name string, mcpCfg config.MCPConfig) (config.MCPMutationResult, error) {
			return cfg.PersistMCPConfigResult(scope, name, mcpCfg)
		})
}

// AddServer validates and adds a new MCP server. It attempts to connect; if
// successful the server is added to the in-memory config and persisted to disk.
// A nil receiver resolves the process-current owner exactly like the
// package-level AddServer function, so callers holding an optional owner can
// call the method unconditionally.
func (o *Owner) AddServer(ctx context.Context, cfg *config.ConfigStore, name string, mcpCfg config.MCPConfig) error {
	if o == nil {
		return AddServer(ctx, cfg, name, mcpCfg)
	}
	return addServerWithPreparationAndPersistenceForOwner(o, ctx, cfg, name, mcpCfg, prepareClient,
		func(cfg *config.ConfigStore, scope config.Scope, name string, mcpCfg config.MCPConfig) (config.MCPMutationResult, error) {
			return cfg.PersistMCPConfigResult(scope, name, mcpCfg)
		})
}

// ReplaceServer prepares a new MCP session completely before changing the
// configured server. The old session and its advertised data remain live
// until the durable remove-and-set has committed, at which point the config,
// session, tools, prompts, resources, and state switch as one lifecycle
// transition.
func ReplaceServer(ctx context.Context, cfg *config.ConfigStore, oldName, newName string, mcpCfg config.MCPConfig) error {
	return replaceServerWithResultPersistence(ctx, cfg, oldName, newName, mcpCfg,
		func(cfg *config.ConfigStore, scope config.Scope, oldName, newName string, mcpCfg config.MCPConfig) (config.MCPMutationResult, error) {
			return cfg.PersistReplaceMCPResult(scope, oldName, newName, mcpCfg)
		})
}

// ReplaceServer prepares a new MCP session completely before changing the
// configured server. The old session and its advertised data remain live
// until the durable remove-and-set has committed, at which point the config,
// session, tools, prompts, resources, and state switch as one lifecycle
// transition.
// A nil receiver resolves the process-current owner exactly like the
// package-level ReplaceServer function, so callers holding an optional owner
// can call the method unconditionally.
func (o *Owner) ReplaceServer(ctx context.Context, cfg *config.ConfigStore, oldName, newName string, mcpCfg config.MCPConfig) error {
	if o == nil {
		return ReplaceServer(ctx, cfg, oldName, newName, mcpCfg)
	}
	return replaceServerWithResultPersistenceForOwner(o, ctx, cfg, oldName, newName, mcpCfg,
		func(cfg *config.ConfigStore, scope config.Scope, oldName, newName string, mcpCfg config.MCPConfig) (config.MCPMutationResult, error) {
			return cfg.PersistReplaceMCPResult(scope, oldName, newName, mcpCfg)
		})
}

type replacementPersister func(*config.ConfigStore, string, string, config.MCPConfig) error
type scopedReplacementPersister func(*config.ConfigStore, config.Scope, string, string, config.MCPConfig) error
type replacementResultPersister func(*config.ConfigStore, config.Scope, string, string, config.MCPConfig) (config.MCPMutationResult, error)

func replaceServerWithPersistence(
	ctx context.Context,
	cfg *config.ConfigStore,
	oldName, newName string,
	mcpCfg config.MCPConfig,
	persist replacementPersister,
) error {
	return replaceServerWithResultPersistence(ctx, cfg, oldName, newName, mcpCfg,
		func(cfg *config.ConfigStore, _ config.Scope, oldName, newName string, mcpCfg config.MCPConfig) (config.MCPMutationResult, error) {
			if err := persist(cfg, oldName, newName, mcpCfg); err != nil {
				if outcome, ok := config.CommitOutcomeFromError(err); ok && commitOutcomeIsReconciled(outcome) {
					return currentMCPMutationResult(cfg, "replace", oldName, newName), err
				}
				return config.MCPMutationResult{}, err
			}
			return currentMCPMutationResult(cfg, "replace", oldName, newName), nil
		})
}

func replaceServerWithScopedPersistence(
	ctx context.Context,
	cfg *config.ConfigStore,
	oldName, newName string,
	mcpCfg config.MCPConfig,
	persist scopedReplacementPersister,
) error {
	return replaceServerWithResultPersistence(ctx, cfg, oldName, newName, mcpCfg, func(cfg *config.ConfigStore, scope config.Scope, oldName, newName string, mcpCfg config.MCPConfig) (config.MCPMutationResult, error) {
		if err := persist(cfg, scope, oldName, newName, mcpCfg); err != nil {
			return config.MCPMutationResult{}, err
		}
		return currentMCPMutationResult(cfg, "replace", oldName, newName), nil
	})
}

func replaceServerWithResultPersistence(
	ctx context.Context,
	cfg *config.ConfigStore,
	oldName, newName string,
	mcpCfg config.MCPConfig,
	persist replacementResultPersister,
) error {
	o, err := ensureOwner()
	if err != nil {
		return err
	}
	return replaceServerWithResultPersistenceForOwner(o, ctx, cfg, oldName, newName, mcpCfg, persist)
}

func replaceServerWithResultPersistenceForOwner(
	o *Owner,
	ctx context.Context,
	cfg *config.ConfigStore,
	oldName, newName string,
	mcpCfg config.MCPConfig,
	persist replacementResultPersister,
) error {
	return replaceServerWithResultPersistenceAndPreparationForOwner(o, ctx, cfg, oldName, newName, mcpCfg, persist, prepareClient)
}

func addServerWithPreparationAndPersistence(
	ctx context.Context,
	cfg *config.ConfigStore,
	name string,
	mcpCfg config.MCPConfig,
	prepare preparedClientFunc,
	persist addServerResultPersister,
) error {
	o, err := ensureOwner()
	if err != nil {
		return err
	}
	return addServerWithPreparationAndPersistenceForOwner(o, ctx, cfg, name, mcpCfg, prepare, persist)
}

func addServerWithPreparationAndPersistenceForOwner(
	o *Owner,
	ctx context.Context,
	cfg *config.ConfigStore,
	name string,
	mcpCfg config.MCPConfig,
	prepare preparedClientFunc,
	persist addServerResultPersister,
) error {
	return addServerWithInitializerAndPersistenceForOwner(o, ctx, cfg, name, mcpCfg,
		func(ctx context.Context, cfg *config.ConfigStore, name string, mcpCfg config.MCPConfig, resolver config.VariableResolver, admission *serverAdmission) error {
			admission.deferDone = true
			admission.suppressState = true
			admission.suppressUntilCommit = true
			prepared, err := prepare(ctx, cfg, name, mcpCfg, resolver, admission)
			if err == nil {
				admission.prepared = prepared
			}
			return err
		}, persist)
}

func replaceServerWithResultPersistenceAndPreparation(
	ctx context.Context,
	cfg *config.ConfigStore,
	oldName, newName string,
	mcpCfg config.MCPConfig,
	persist replacementResultPersister,
	prepare preparedClientFunc,
) error {
	o, err := ensureOwner()
	if err != nil {
		return err
	}
	return replaceServerWithResultPersistenceAndPreparationForOwner(o, ctx, cfg, oldName, newName, mcpCfg, persist, prepare)
}

func replaceServerWithResultPersistenceAndPreparationForOwner(
	o *Owner,
	ctx context.Context,
	cfg *config.ConfigStore,
	oldName, newName string,
	mcpCfg config.MCPConfig,
	persist replacementResultPersister,
	prepare preparedClientFunc,
) error {
	if mcpCfg.Source == config.MCPSourceExternal {
		return fmt.Errorf("MCP server %q is from .mcp.json and cannot be replaced: %w", oldName, config.ErrMCPExternal)
	}
	if err := o.rememberConfig(cfg); err != nil {
		return err
	}
	if err := o.reconcileUncertainty(ctx, cfg, oldName); err != nil {
		return err
	}
	if newName != oldName {
		if err := o.reconcileUncertainty(ctx, cfg, newName); err != nil {
			return err
		}
	}
	// Resolve after any required reload so the scope and existence checks use
	// the fresh config snapshot.
	scope, err := cfg.ResolveMCPWritableScope(oldName)
	if err != nil {
		return fmt.Errorf("cannot determine writable scope for MCP server %q: %w", oldName, err)
	}
	if !o.beginInit() {
		return ErrOwnerBusy
	}
	defer o.endInit()

	locked, lockedOK := lockServerLeasesContext(ctx, oldName, newName)
	if !lockedOK {
		return ctx.Err()
	}
	if o.pendingGlobalAdd(newName, cfg) != nil {
		unlockServerLeases(locked)
		return fmt.Errorf("MCP server %q has a pending add: %w", newName, config.ErrMCPTargetExists)
	}
	sourceSnapshot := cfg.SnapshotMCPAdmission(oldName)
	if !sourceSnapshot.Exists {
		unlockServerLeases(locked)
		return fmt.Errorf("MCP server %q disappeared after scope resolution: %w", oldName, config.ErrMCPNotFound)
	}
	admission, err := o.admitReplacementCandidateWithSnapshot(ctx, cfg, oldName, sourceSnapshot)
	if err != nil {
		unlockServerLeases(locked)
		return err
	}
	lifecycleMu.Lock()
	guardBound := admission.bindGuardLocked(newName)
	lifecycleMu.Unlock()
	if !guardBound {
		unlockServerLeases(locked)
		admission.done()
		return ErrMCPConfigUncertain
	}
	admission.suppressState = true
	admission.suppressUntilCommit = true
	resolver := sourceSnapshot.Resolver
	unlockServerLeases(locked)

	prepared, err := prepare(admission.ctx, cfg, newName, mcpCfg, resolver, &admission)
	if err != nil {
		admission.done()
		if errors.Is(err, ErrOwnerBusy) {
			return ErrOwnerBusy
		}
		return fmt.Errorf("failed to connect to MCP server %q: %w", newName, err)
	}

	locked, lockedOK = lockServerLeasesContext(ctx, oldName, newName)
	if !lockedOK {
		_ = prepared.session.Close()
		admission.done()
		return ctx.Err()
	}
	lifecycleMu.Lock()
	if !admission.validLocked() {
		lifecycleMu.Unlock()
		unlockServerLeases(locked)
		_ = prepared.session.Close()
		admission.done()
		return ErrOwnerBusy
	}
	// Promotion is still part of the candidate phase. If it fails, no disk or
	// registry mutation has happened and the old server remains untouched.
	if !prepared.session.promoteContext() {
		lifecycleMu.Unlock()
		unlockServerLeases(locked)
		_ = prepared.session.Close()
		admission.done()
		return ErrOwnerBusy
	}
	admission.promoted = true
	lifecycleMu.Unlock()

	// Recheck both identities immediately before the durable mutation. The
	// leases prevent a concurrent server mutation from passing this guard.
	lifecycleMu.Lock()
	validForReplacement := admission.replacementValidLocked()
	lifecycleMu.Unlock()
	if !validForReplacement {
		unlockServerLeases(locked)
		_ = prepared.session.Close()
		admission.done()
		return ErrOwnerBusy
	}

	// The durable atomic RMW may wait on an inter-process file lock for up to
	// the config write timeout. Keep the ordered server leases, but never hold
	// lifecycleMu here: Owner.Close must be able to mark the owner closing,
	// cancel its contexts, and honor the caller's deadline while this I/O is
	// stalled.
	result, err := persist(cfg, scope, oldName, newName, mcpCfg)
	var commitUncertainty error
	commitKnown := err == nil
	if err != nil {
		outcome, ok := config.CommitOutcomeFromError(err)
		if ok && commitOutcomeNeedsRuntimeFence(outcome) {
			detachedOld := fenceMCPRuntimeLocked(o, cfg, oldName)
			var detachedNew *ClientSession
			if newName != oldName {
				detachedNew = fenceMCPRuntimeLocked(o, cfg, newName)
			}
			unlockServerLeases(locked)
			retireMCPClient(oldName, detachedOld)
			retireMCPClient(newName, detachedNew)
			closeMCPClient(newName, prepared.session)
			admission.done()
			return fmt.Errorf("failed to persist MCP server replacement %q to %q: %w", oldName, newName, err)
		}
		if !ok || !outcome.Committed {
			unlockServerLeases(locked)
			_ = prepared.session.Close()
			admission.done()
			return fmt.Errorf("failed to persist MCP server replacement %q to %q: %w", oldName, newName, err)
		}
		commitKnown = true
		commitUncertainty = err
	}
	var (
		cleanup           replacementCleanup
		cleanupNeeded     bool
		canceled          []context.CancelFunc
		newExists         bool
		newDisabled       bool
		pendingEvents     []Event
		wakeRefresh       bool
		oldSession        *ClientSession
		hadOldSession     bool
		hadOldState       bool
		newSession        *ClientSession
		hadNewSession     bool
		counts            Counts
		brokerForEvent    *pubsub.Broker[Event]
		publicationResult error
	)
	publicationResult = cfg.WithCurrentMCPMutation(result, func() error {
		lifecycleMu.Lock()
		defer lifecycleMu.Unlock()
		publicationValid := admission.replacementNamesValidLocked() &&
			(o.standalone || owner == o) && !o.closing && o.generation == admission.generation &&
			o.serverEpochs[oldName] == admission.epoch
		if !publicationValid {
			if !commitKnown {
				return ErrOwnerBusy
			}
			cleanup = cleanupReplacementRuntimeLocked(o, cfg, oldName, newName)
			cleanupNeeded = true
			brokerForEvent = broker
			return nil
		}
		canceled = append(canceled, o.invalidateServerLocked(oldName)...)
		if newName != oldName {
			canceled = append(canceled, o.invalidateServerLocked(newName)...)
		}
		newExists = result.NewExists
		newDisabled = !newExists || result.NewConfig.Disabled
		admission.committed = newExists && !newDisabled
		admission.committedName = newName
		admission.committedEpoch = o.serverEpochs[newName]

		oldSession, hadOldSession = sessions.Get(oldName)
		_, hadOldState = states.Get(oldName)
		newSession, hadNewSession = sessions.Get(newName)
		if oldName != newName {
			clearAdvertised(oldName)
			sessions.Del(oldName)
			states.Del(oldName)
			delete(admission.owner.committedAdmissions, oldName)
		}
		if !newDisabled {
			toolCount := updateTools(cfg, newName, prepared.tools)
			updatePrompts(newName, prepared.prompts)
			allResources.Del(newName)
			admission.owner.trackSessionLocked(prepared.session, newName)
			sessions.Set(newName, prepared.session)
			admission.publishedSession = prepared.session
			admission.configIdentity = result.NewConfig
			admission.hasConfigIdentity = true
			if snapshot := cfg.SnapshotMCPAdmission(newName); snapshot.Exists {
				admission.mcpRevision = snapshot.MCPRevision
				admission.resolverRevision = snapshot.ResolverRevision
				admission.mcpAdmission = snapshot
			}
			admission.owner.committedAdmissions[newName] = &admission
			counts = Counts{Tools: toolCount, Prompts: len(prepared.prompts)}
			setState(newName, StateConnected, nil, prepared.session, counts)
		} else {
			clearAdvertised(newName)
			sessions.Del(newName)
			states.Del(newName)
			delete(admission.owner.committedAdmissions, newName)
			if newExists {
				setState(newName, StateDisabled, nil, nil, Counts{})
			}
		}
		if admission.committed {
			pendingEvents, wakeRefresh = o.activateRefreshesLocked(&admission)
		}
		brokerForEvent = broker
		return nil
	})
	if errors.Is(publicationResult, config.ErrMCPMutationStale) {
		if !commitKnown {
			unlockServerLeases(locked)
			_ = prepared.session.Close()
			admission.done()
			return ErrOwnerBusy
		}
		lifecycleMu.Lock()
		cleanup = cleanupReplacementRuntimeLocked(o, cfg, oldName, newName)
		cleanupNeeded = true
		brokerForEvent = broker
		lifecycleMu.Unlock()
	} else if publicationResult != nil {
		unlockServerLeases(locked)
		_ = prepared.session.Close()
		admission.done()
		return publicationResult
	}
	if cleanupNeeded {
		if brokerForEvent != nil {
			for _, event := range cleanup.events {
				brokerForEvent.Publish(event.kind, event.event)
			}
		}
		unlockServerLeases(locked)
		for _, cancel := range cleanup.canceled {
			cancel()
		}
		retireMCPClient(oldName, cleanup.detachedOld)
		retireMCPClient(newName, cleanup.detachedNew)
		closeMCPClient(newName, prepared.session)
		admission.done()
		if commitUncertainty != nil {
			return fmt.Errorf("failed to persist MCP server replacement %q to %q: %w", oldName, newName, commitUncertainty)
		}
		return nil
	}
	if wakeRefresh {
		o.signalRefresh()
	}
	if oldName != newName && !result.FallbackExists && hadOldState {
		brokerForEvent.Publish(pubsub.DeletedEvent, Event{
			Type: EventStateChanged, Name: oldName, State: StateDisabled,
		})
	}
	if newExists {
		if newDisabled {
			brokerForEvent.Publish(pubsub.UpdatedEvent, Event{
				Type: EventStateChanged, Name: newName, State: StateDisabled,
			})
		} else {
			brokerForEvent.Publish(pubsub.UpdatedEvent, Event{
				Type: EventStateChanged, Name: newName, State: StateConnected, Counts: counts,
			})
		}
	} else if newName != oldName {
		brokerForEvent.Publish(pubsub.DeletedEvent, Event{
			Type: EventStateChanged, Name: newName, State: StateDisabled,
		})
	}
	publishListChangedEventsOn(brokerForEvent, pendingEvents)
	// Replacement events are part of its server-lease linearization point.
	// Detached sessions and candidates are retired or closed only afterward.
	unlockServerLeases(locked)

	for _, cancel := range canceled {
		cancel()
	}
	if hadOldSession && oldSession != prepared.session {
		if oldName != newName && result.FallbackExists {
			startFallbackAfterRetirement(oldName, oldSession, cfg, result, o)
		} else {
			retireMCPClient(oldName, oldSession)
		}
	}
	if newName != oldName && hadNewSession && newSession != prepared.session && newSession != oldSession {
		retireMCPClient(newName, newSession)
	}
	if oldName != newName && result.FallbackExists && !hadOldSession {
		o.enqueueFallback(cfg, result)
	}
	if newDisabled {
		closeMCPClient(newName, prepared.session)
	}
	admission.done()
	if commitUncertainty != nil {
		return fmt.Errorf("failed to persist MCP server replacement %q to %q: %w", oldName, newName, commitUncertainty)
	}
	return nil
}

type admittedClientInitializer func(
	context.Context,
	*config.ConfigStore,
	string,
	config.MCPConfig,
	config.VariableResolver,
	*serverAdmission,
) error

func addServerWithInitializer(
	ctx context.Context,
	cfg *config.ConfigStore,
	name string,
	mcpCfg config.MCPConfig,
	initialize admittedClientInitializer,
) error {
	return addServerWithInitializerAndPersistence(ctx, cfg, name, mcpCfg, initialize,
		func(cfg *config.ConfigStore, scope config.Scope, name string, mcpCfg config.MCPConfig) (config.MCPMutationResult, error) {
			return cfg.PersistMCPConfigResult(scope, name, mcpCfg)
		})
}

type addServerResultPersister func(*config.ConfigStore, config.Scope, string, config.MCPConfig) (config.MCPMutationResult, error)

func addServerWithInitializerAndPersistence(
	ctx context.Context,
	cfg *config.ConfigStore,
	name string,
	mcpCfg config.MCPConfig,
	initialize admittedClientInitializer,
	persist addServerResultPersister,
) error {
	o, err := ensureOwner()
	if err != nil {
		return err
	}
	return addServerWithInitializerAndPersistenceForOwner(o, ctx, cfg, name, mcpCfg, initialize, persist)
}

func addServerWithInitializerAndPersistenceForOwner(
	o *Owner,
	ctx context.Context,
	cfg *config.ConfigStore,
	name string,
	mcpCfg config.MCPConfig,
	initialize admittedClientInitializer,
	persist addServerResultPersister,
) error {
	if err := o.rememberConfig(cfg); err != nil {
		return err
	}
	if err := o.reconcileUncertainty(ctx, cfg, name); err != nil {
		return err
	}
	if !o.beginInit() {
		return ErrOwnerBusy
	}
	defer o.endInit()
	lease := serverLeaseFor(name)
	if !lease.lockContext(ctx, true) {
		return ctx.Err()
	}
	if o.isUncertain(cfg, name) {
		lease.Unlock()
		return ErrMCPConfigUncertain
	}
	if _, exists := cfg.MCPConfig(name); exists {
		lease.Unlock()
		return fmt.Errorf("MCP server %q already exists", name)
	}
	if !cfg.AddMCP(name, mcpCfg) {
		lease.Unlock()
		return fmt.Errorf("MCP server %q already exists", name)
	}
	admissionSnapshot := cfg.SnapshotMCPAdmission(name)
	resolver := admissionSnapshot.Resolver
	admission, err := o.admitServerForConfig(ctx, cfg, name, mcpCfg, true)
	if err != nil {
		_, _ = cfg.RemoveMCP(name)
		lease.Unlock()
		return err
	}
	admission.candidate = true
	admission.mcpRevision = admissionSnapshot.MCPRevision
	admission.resolverRevision = admissionSnapshot.ResolverRevision
	admission.mcpAdmission = admissionSnapshot
	admission.deferDone = true
	admission.suppressState = true
	admission.suppressUntilCommit = true
	transaction, ok := o.markPendingGlobalAdd(&admission, cfg, mcpCfg)
	if !ok {
		admission.done()
		_, _ = cfg.RemoveMCP(name)
		lease.Unlock()
		return ErrOwnerBusy
	}
	defer o.completePendingGlobalAdd(transaction)
	// Keep the lease entry alive while initialization runs without the write
	// lock. RemoveServer must then wait on this exact lease instance instead of
	// racing a reclaimed entry with an ABA replacement.
	if !lease.registry.retain(lease) {
		admission.done()
		o.completePendingGlobalAdd(transaction)
		_, _ = cfg.RemoveMCP(name)
		lease.Unlock()
		return ErrOwnerBusy
	}
	// This is the initialization identity reference. It must be released on
	// every path after the retain succeeds, including an admission that is
	// invalid before initialization starts.
	initLeaseRetained := true
	defer func() {
		if initLeaseRetained {
			lease.registry.release(lease)
		}
	}()
	callAfterAddRetain(&admission)
	if !admission.valid() {
		detached := rollbackAddedServer(o, cfg, name, &admission, transaction)
		prepared := discardPreparedClient(&admission)
		admission.done()
		lease.Unlock()
		retireMCPClient(name, detached)
		closeMCPClient(name, prepared)
		return ErrOwnerBusy
	}
	lease.Unlock()
	initErr := initialize(ctx, cfg, name, mcpCfg, resolver, &admission)
	if initErr != nil {
		lease.Lock()
		detached := rollbackAddedServer(o, cfg, name, &admission, transaction)
		prepared := discardPreparedClient(&admission)
		admission.done()
		initLeaseRetained = false
		lease.Unlock()
		retireMCPClient(name, detached)
		closeMCPClient(name, prepared)
		if errors.Is(initErr, ErrOwnerBusy) {
			return ErrOwnerBusy
		}
		return fmt.Errorf("failed to connect to MCP server %q: %w", name, initErr)
	}

	// Hold the write lease while persisting so RemoveServer cannot remove the
	// in-memory entry and then lose the race by being followed by this write.
	if !lease.reacquireContext(ctx, true) {
		lease.Lock()
		detached := rollbackAddedServer(o, cfg, name, &admission, transaction)
		prepared := discardPreparedClient(&admission)
		admission.done()
		initLeaseRetained = false
		lease.Unlock()
		retireMCPClient(name, detached)
		closeMCPClient(name, prepared)
		return ctx.Err()
	}
	if !admission.valid() {
		detached := rollbackAddedServer(o, cfg, name, &admission, transaction)
		prepared := discardPreparedClient(&admission)
		admission.done()
		lease.Unlock()
		retireMCPClient(name, detached)
		closeMCPClient(name, prepared)
		return ErrOwnerBusy
	}
	if admission.prepared != nil {
		lifecycleMu.Lock()
		promoted := admission.validLocked() && admission.prepared.session.promoteContext()
		if promoted {
			admission.promoted = true
		}
		lifecycleMu.Unlock()
		if !promoted {
			detached := rollbackAddedServer(o, cfg, name, &admission, transaction)
			prepared := discardPreparedClient(&admission)
			admission.done()
			lease.Unlock()
			retireMCPClient(name, detached)
			closeMCPClient(name, prepared)
			return ErrOwnerBusy
		}
	}
	result, err := persist(cfg, config.ScopeGlobal, name, mcpCfg)
	var commitUncertainty error
	if err != nil {
		outcome, ok := config.CommitOutcomeFromError(err)
		if ok && (outcome.Committed || outcome.MaybeCommitted) {
			if commitOutcomeNeedsRuntimeFence(outcome) {
				// The durable state is unknown. Retire the candidate and remove
				// the runtime candidate; a later reload owns config recovery.
				detached := fenceMCPRuntimeLocked(o, cfg, name)
				prepared := discardPreparedClient(&admission)
				admission.done()
				o.completePendingGlobalAdd(transaction)
				lease.Unlock()
				retireMCPClient(name, detached)
				closeMCPClient(name, prepared)
				return fmt.Errorf("failed to persist MCP server %q: %w", name, err)
			}
			if !admission.deferDone {
				o.completePendingGlobalAdd(transaction)
				lease.Unlock()
				return fmt.Errorf("failed to persist MCP server %q: %w", name, err)
			}
			commitUncertainty = err
		} else {
			detached := rollbackAddedServer(o, cfg, name, &admission, transaction)
			prepared := discardPreparedClient(&admission)
			admission.done()
			lease.Unlock()
			retireMCPClient(name, detached)
			closeMCPClient(name, prepared)
			return fmt.Errorf("failed to persist MCP server %q: %w", name, err)
		}
	}
	if !result.NewExists {
		detached := rollbackAddedServer(o, cfg, name, &admission, transaction)
		prepared := discardPreparedClient(&admission)
		admission.done()
		lease.Unlock()
		retireMCPClient(name, detached)
		closeMCPClient(name, prepared)
		return fmt.Errorf("MCP server %q disappeared while persisting: %w", name, config.ErrMCPNotFound)
	}
	transaction.markDurable()
	resultSnapshot := cfg.SnapshotMCPAdmission(name)
	admission.mcpRevision = resultSnapshot.MCPRevision
	admission.resolverRevision = resultSnapshot.ResolverRevision
	admission.mcpAdmission = resultSnapshot
	admission.mutationResult = &result
	// The durable Add outcome is complete before releasing the lease. A later
	// RemoveServer must then use the ordinary conditional remove path rather
	// than treating an already-persisted Add as still pending.
	o.completePendingGlobalAdd(transaction)
	var oldSession *ClientSession
	var publishErr error
	var prepared *preparedClient
	if admission.prepared != nil {
		prepared = admission.prepared
		lifecycleMu.Lock()
		admission.publishingPrepared = true
		admission.prepared = nil
		lifecycleMu.Unlock()
		oldSession, publishErr = publishPreparedClientLockedWithMutation(cfg, name, prepared, &admission)
	}
	lease.Unlock()
	if oldSession != nil && (prepared == nil || oldSession != prepared.session) {
		retireMCPClient(name, oldSession)
	}
	if prepared != nil {
		if publishErr != nil {
			closeMCPClient(name, prepared.session)
		}
		admission.done()
		if publishErr != nil {
			if errors.Is(publishErr, ErrOwnerBusy) {
				if commitUncertainty != nil {
					return fmt.Errorf("failed to persist MCP server %q: %w", name, commitUncertainty)
				}
				return nil
			}
			return fmt.Errorf("failed to publish MCP server %q: %w", name, publishErr)
		}
	}
	if commitUncertainty != nil {
		return fmt.Errorf("failed to persist MCP server %q: %w", name, commitUncertainty)
	}
	return nil
}

func discardPreparedClient(admission *serverAdmission) *ClientSession {
	if admission == nil || admission.prepared == nil {
		return nil
	}
	prepared := admission.prepared
	admission.prepared = nil
	return prepared.session
}

// rollbackAddedServer removes only the server instance represented by the
// exact add transaction. Callers hold the per-server write lease, so a newer
// same-name transaction cannot be removed by a stale rollback. Its detached
// session is retired and closed after that lease is released. Owner.Close may
// be closing; the exact pending transaction is still safe to clean up.
func rollbackAddedServer(o *Owner, cfg *config.ConfigStore, name string, admission *serverAdmission, transaction *addTransaction) *ClientSession {
	if admission == nil || admission.owner != o || admission.name != name || transaction == nil ||
		transaction.hasUserMutation() || transaction.isDurable() {
		return nil
	}
	lifecycleMu.Lock()
	valid := rollbackAdmissionValidLocked(o, cfg, name, admission, transaction, true)
	expectedRevision := admission.mcpRevision
	if expectedRevision == 0 {
		expectedRevision = cfg.SnapshotMCPAdmission(name).MCPRevision
	}
	if !valid {
		lifecycleMu.Unlock()
		return nil
	}
	lifecycleMu.Unlock()
	if _, removed := cfg.RemoveMCPIfCurrent(name, transaction.mcpConfig, expectedRevision); !removed {
		return nil
	}
	lifecycleMu.Lock()
	if !rollbackAdmissionValidLocked(o, cfg, name, admission, transaction, false) {
		lifecycleMu.Unlock()
		return nil
	}
	detached := detachSessionLifecycleLocked(name)
	canceled := o.invalidateServerLocked(name)
	clearAdvertised(name)
	states.Del(name)
	lifecycleMu.Unlock()
	for _, cancel := range canceled {
		cancel()
	}
	return detached
}

func rollbackAdmissionValidLocked(
	o *Owner,
	cfg *config.ConfigStore,
	name string,
	admission *serverAdmission,
	transaction *addTransaction,
	configPresent bool,
) bool {
	if !o.standalone && (owner != o || o.generation != admission.generation) ||
		o.serverEpochs[name] != admission.epoch || o.pendingGlobalAdds[name] != transaction {
		return false
	}
	currentSession, sessionExists := sessions.Get(name)
	if admission.publishedSession != nil {
		if !sessionExists || currentSession != admission.publishedSession {
			return false
		}
	} else if sessionExists {
		return false
	}
	if !configPresent {
		_, exists := cfg.MCPConfig(name)
		return !exists
	}
	if admission.mcpRevision != 0 && cfg.SnapshotMCPAdmission(name).MCPRevision != admission.mcpRevision {
		return false
	}
	currentConfig, exists := cfg.MCPConfig(name)
	return exists && reflect.DeepEqual(currentConfig, transaction.mcpConfig)
}
