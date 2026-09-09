package mcp

import (
	"context"
	"log/slog"

	"github.com/PHPCraftdream/rush/internal/config"
	"github.com/PHPCraftdream/rush/internal/pubsub"
)

type preparedClient struct {
	session *ClientSession
	tools   []*Tool
	prompts []*Prompt
}

// prepareClient establishes and interrogates a client without publishing any
// session, tools, prompts, resources, or state. The caller decides when the
// prepared client becomes visible.
func prepareClient(ctx context.Context, cfg *config.ConfigStore, name string, m config.MCPConfig, resolver config.VariableResolver, admission *serverAdmission) (*preparedClient, error) {
	// createSession handles its own timeout internally.
	session, err := createSessionWithAdmission(ctx, name, m, resolver, admission)
	if err != nil {
		return nil, err
	}

	tools, err := getTools(ctx, session)
	if err != nil {
		slog.Error("Error listing tools", "error", err, "name", name)
		if admission == nil || (!admission.suppressState && admission.valid()) {
			if admission == nil {
				updateState(name, StateError, err, nil, Counts{})
			} else {
				updateAdmissionState(admission, StateError, err, nil, Counts{})
			}
		}
		_ = session.Close()
		return nil, err
	}

	prompts, err := getPrompts(ctx, session)
	if err != nil {
		slog.Error("Error listing prompts", "error", err, "name", name)
		if admission == nil || (!admission.suppressState && admission.valid()) {
			if admission == nil {
				updateState(name, StateError, err, nil, Counts{})
			} else {
				updateAdmissionState(admission, StateError, err, nil, Counts{})
			}
		}
		_ = session.Close()
		return nil, err
	}

	if admission != nil {
		lifecycleMu.Lock()
		valid := admission.validLocked()
		stale := !valid && admission.candidate && admission.configSnapshotStaleLocked()
		lifecycleMu.Unlock()
		if !valid {
			_ = session.Close()
			if stale {
				return nil, config.ErrMCPMutationStale
			}
			return nil, ErrOwnerBusy
		}
	}
	return &preparedClient{session: session, tools: tools, prompts: prompts}, nil
}

func publishPreparedClient(cfg *config.ConfigStore, name string, prepared *preparedClient, admission *serverAdmission) error {
	if admission != nil && admission.mutationResult != nil {
		return publishPreparedClientWithMutation(cfg, name, prepared, admission)
	}
	session := prepared.session
	lease := serverLeaseFor(name)
	lockCtx := context.Background()
	if admission != nil && admission.ctx != nil {
		lockCtx = admission.ctx
	}
	if !lease.lockContext(lockCtx, true) {
		closeMCPClient(name, session)
		return lockCtx.Err()
	}
	var oldSession *ClientSession
	var err error
	publication := func() error {
		oldSession, err = publishPreparedClientLocked(cfg, name, prepared, admission)
		return err
	}
	if admission != nil {
		snapshot := admission.mcpAdmission
		if snapshot.Config == nil {
			snapshot = cfg.SnapshotMCPAdmission(name)
		}
		err = cfg.WithCurrentMCPAdmissionContext(lockCtx, snapshot, name, func(guard config.MCPAdmissionGuard) error {
			return withMCPAdmissionFinalTurn(lockCtx, name, guard, func() error {
				oldSession, err = publishPreparedClientUnderLifecycle(cfg, name, prepared, admission)
				return err
			})
		})
	} else {
		err = publication()
	}
	lease.Unlock()
	if err != nil {
		closeMCPClient(name, session)
		return err
	}
	if oldSession != nil && oldSession != session {
		retireMCPClient(name, oldSession)
	}
	return nil
}

func publishPreparedClientWithMutation(cfg *config.ConfigStore, name string, prepared *preparedClient, admission *serverAdmission) error {
	if admission == nil || admission.mutationResult == nil {
		return publishPreparedClient(cfg, name, prepared, admission)
	}
	session := prepared.session
	lockCtx := context.Background()
	if admission.ctx != nil {
		lockCtx = admission.ctx
	}
	lease := serverLeaseFor(name)
	if !lease.lockContext(lockCtx, true) {
		closeMCPClient(name, session)
		return lockCtx.Err()
	}
	var oldSession *ClientSession
	publicationErr := cfg.WithCurrentMCPMutation(*admission.mutationResult, func() error {
		var err error
		oldSession, err = publishPreparedClientLocked(cfg, name, prepared, admission)
		return err
	})
	lease.Unlock()
	if publicationErr != nil {
		closeMCPClient(name, session)
		return publicationErr
	}
	if oldSession != nil && oldSession != session {
		retireMCPClient(name, oldSession)
	}
	return nil
}

func publishPreparedClientLockedWithMutation(
	cfg *config.ConfigStore,
	name string,
	prepared *preparedClient,
	admission *serverAdmission,
) (*ClientSession, error) {
	if admission == nil || admission.mutationResult == nil {
		return publishPreparedClientLocked(cfg, name, prepared, admission)
	}
	var oldSession *ClientSession
	err := cfg.WithCurrentMCPMutation(*admission.mutationResult, func() error {
		var err error
		oldSession, err = publishPreparedClientLocked(cfg, name, prepared, admission)
		return err
	})
	return oldSession, err
}

// publishPreparedClientLocked publishes a prepared client while the caller
// holds the server write lease. Keeping durable commit and publication in one
// lease transition closes the post-commit race with RemoveServer. The caller
// must close a rejected candidate and retire the replaced session after it
// releases the lease; those operations may touch the network.
func publishPreparedClientLocked(cfg *config.ConfigStore, name string, prepared *preparedClient, admission *serverAdmission) (*ClientSession, error) {
	lifecycleMu.Lock()
	defer lifecycleMu.Unlock()
	return publishPreparedClientUnderLifecycle(cfg, name, prepared, admission)
}

func publishPreparedClientUnderLifecycle(cfg *config.ConfigStore, name string, prepared *preparedClient, admission *serverAdmission) (*ClientSession, error) {
	session := prepared.session
	tools := prepared.tools
	prompts := prepared.prompts
	if admission != nil && !admission.validLocked() {
		stale := admission.candidate && admission.configSnapshotStaleLocked()
		if stale {
			return nil, config.ErrMCPMutationStale
		}
		return nil, ErrOwnerBusy
	}
	if _, ok := cfg.MCPConfig(name); !ok {
		return nil, ErrOwnerBusy
	}
	if currentConfig, _ := cfg.MCPConfig(name); currentConfig.Disabled {
		return nil, ErrOwnerBusy
	}
	if admission != nil && admission.suppressUntilCommit && !admission.committed && !admission.publishingPrepared {
		if admission.prepared != nil {
			return nil, ErrOwnerBusy
		}
		admission.prepared = prepared
		return nil, nil
	}
	if !session.promoteContext() {
		return nil, ErrOwnerBusy
	}
	oldSession, _ := sessions.Get(name)
	toolCount := updateTools(cfg, name, tools)
	updatePrompts(name, prompts)
	sessionOwner := owner
	if admission != nil && admission.owner != nil {
		sessionOwner = admission.owner
	}
	if sessionOwner != nil {
		sessionOwner.trackSessionLocked(session, name)
	}
	sessions.Set(name, session)
	if admission != nil {
		admission.committed = true
		admission.committedName = name
		admission.committedEpoch = admission.owner.serverEpochs[name]
		admission.publishedSession = session
		if current, ok := cfg.MCPConfig(name); ok {
			admission.configIdentity = current
			admission.hasConfigIdentity = true
		}
		if snapshot := cfg.SnapshotMCPAdmission(name); snapshot.Exists {
			admission.mcpRevision = snapshot.MCPRevision
			admission.resolverRevision = snapshot.ResolverRevision
			admission.mcpAdmission = snapshot
		}
		admission.owner.committedAdmissions[name] = admission
	}
	counts := Counts{
		Tools:   toolCount,
		Prompts: len(prompts),
	}
	setState(name, StateConnected, nil, session, counts)
	var pendingEvents []Event
	wakeRefresh := false
	if admission != nil && admission.owner != nil {
		pendingEvents, wakeRefresh = admission.owner.activateRefreshesLocked(admission)
	}
	brokerForEvent := broker
	if wakeRefresh {
		admission.owner.signalRefresh()
	}
	brokerForEvent.Publish(pubsub.UpdatedEvent, Event{
		Type: EventStateChanged, Name: name, State: StateConnected, Counts: counts,
	})
	publishListChangedEventsOn(brokerForEvent, pendingEvents)

	return oldSession, nil
}

type preparedClientFunc func(context.Context, *config.ConfigStore, string, config.MCPConfig, config.VariableResolver, *serverAdmission) (*preparedClient, error)
