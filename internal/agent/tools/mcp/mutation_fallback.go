package mcp

import (
	"context"
	"log/slog"
	"reflect"

	"github.com/PHPCraftdream/rush/internal/config"
	"github.com/PHPCraftdream/rush/internal/kit/sync/pubsub"
)

func startFallbackAfterRetirement(
	name string,
	session *ClientSession,
	cfg *config.ConfigStore,
	result config.MCPMutationResult,
	o *Owner,
) {
	if session == nil {
		o.enqueueFallback(cfg, result)
		return
	}
	adoptSessionForRetirement(name, session)
	session.afterCloseContext(o.lifecycleCtx, func() {
		o.enqueueFallback(cfg, result)
	})
	session.retire()
}

func currentMCPMutationResult(cfg *config.ConfigStore, operation, oldName, newName string) config.MCPMutationResult {
	result := config.MCPMutationResult{
		Operation: operation,
		OldName:   oldName,
		NewName:   newName,
	}
	if current, ok := cfg.MCPConfig(newName); ok {
		result.NewExists = true
		result.NewConfig = current
	}
	if current, ok := cfg.MCPConfig(oldName); ok {
		result.OldExists = true
		result.OldConfig = current
		if operation == "remove" || (operation == "replace" && oldName != newName) {
			result.FallbackExists = true
			result.FallbackConfig = current
		}
	}
	return result
}

type replacementCleanup struct {
	detachedOld *ClientSession
	detachedNew *ClientSession
	canceled    []context.CancelFunc
	events      []replacementEvent
}

type replacementEvent struct {
	kind  pubsub.EventType
	event Event
}

// cleanupReplacementRuntimeLocked leaves both names inactive after a durable
// replacement that can no longer publish its prepared candidate.
func cleanupReplacementRuntimeLocked(o *Owner, cfg *config.ConfigStore, oldName, newName string) replacementCleanup {
	cleanup := replacementCleanup{}
	configured, _ := cfg.Snapshot()
	seen := make(map[string]struct{}, 2)
	for _, name := range []string{oldName, newName} {
		if _, ok := seen[name]; ok {
			continue
		}
		seen[name] = struct{}{}
		cleanup.canceled = append(cleanup.canceled, o.invalidateServerLocked(name)...)
		var hadRuntime bool
		if name == oldName {
			cleanup.detachedOld = detachSessionLifecycleLocked(name)
		} else {
			cleanup.detachedNew = detachSessionLifecycleLocked(name)
		}
		_, hadState := states.Get(name)
		if name == oldName && cleanup.detachedOld != nil {
			hadRuntime = true
		}
		if name == newName && cleanup.detachedNew != nil {
			hadRuntime = true
		}
		clearAdvertised(name)
		_, exists := configured.MCP[name]
		if exists {
			setState(name, StateDisabled, nil, nil, Counts{})
			if name == newName || hadRuntime || hadState {
				cleanup.events = append(cleanup.events, replacementEvent{
					kind:  pubsub.UpdatedEvent,
					event: Event{Type: EventStateChanged, Name: name, State: StateDisabled},
				})
			}
		} else {
			states.Del(name)
			emitDeleted := (name == newName && oldName != newName) ||
				(name != newName && (hadRuntime || hadState))
			if emitDeleted {
				cleanup.events = append(cleanup.events, replacementEvent{
					kind:  pubsub.DeletedEvent,
					event: Event{Type: EventStateChanged, Name: name, State: StateDisabled},
				})
			}
		}
	}
	return cleanup
}

// startFallback keeps the old test/helper shape while production callers pass
// the exact durable mutation through startFallbackMutation.
func startFallback(ctx context.Context, cfg *config.ConfigStore, name string, mcpCfg config.MCPConfig, o *Owner) {
	snapshot := cfg.SnapshotMCPAdmission(name)
	result := config.MCPMutationResult{
		Operation:  "fallback",
		OldName:    name,
		NewName:    name,
		Generation: snapshot.Generation,
		NewExists:  snapshot.Exists,
		NewConfig:  mcpCfg,
	}
	startFallbackMutation(ctx, cfg, result, o)
}

func startFallbackMutation(ctx context.Context, cfg *config.ConfigStore, result config.MCPMutationResult, o *Owner) {
	if o == nil {
		return
	}
	name, mcpCfg, selectedExists := fallbackDefinition(result)
	if !selectedExists {
		return
	}
	if result.Operation == "fallback" && mcpCfg.Disabled {
		lease := serverLeaseFor(name)
		if lease.lockContext(ctx, true) {
			setState(name, StateDisabled, nil, nil, Counts{})
			publishStateEvent(name, StateDisabled, nil, Counts{})
			lease.Unlock()
		}
		return
	}
	if !selectedExists || mcpCfg.Disabled {
		return
	}
	if err := o.reconcileUncertainty(ctx, cfg, name); err != nil {
		slog.Debug("Failed to reconcile MCP fallback config", "name", name, "err", err)
		return
	}
	lease := serverLeaseFor(name)
	if !lease.lockContext(ctx, true) {
		return
	}
	var admission serverAdmission
	var resolver config.VariableResolver
	var startErr error
	var startingEvent Event
	var publishStarting bool
	publicationErr := cfg.WithCurrentMCPMutation(result, func() error {
		snapshot := cfg.SnapshotMCPAdmission(name)
		if !snapshot.Exists || snapshot.MCPConfig.Disabled || !reflect.DeepEqual(snapshot.MCPConfig, mcpCfg) {
			return config.ErrMCPMutationStale
		}
		resolver = snapshot.Resolver
		admission, startErr = o.admitServerForConfig(ctx, cfg, name, snapshot.MCPConfig, true)
		if startErr != nil {
			return startErr
		}
		admission.mcpRevision = snapshot.MCPRevision
		admission.resolverRevision = snapshot.ResolverRevision
		admission.mcpAdmission = snapshot
		admission.mutationResult = &result
		startingEvent, publishStarting = admissionStateEventUnpinned(&admission, StateStarting, nil, nil, Counts{})
		return nil
	})
	lease.Unlock()
	if publicationErr == nil && publishStarting {
		publishEvent(pubsub.UpdatedEvent, startingEvent)
	}
	if publicationErr != nil {
		if startErr != nil {
			cleanupFailedAdmission(&admission, startErr)
		}
		return
	}
	go func() {
		if err := initClientAdmittedWithState(ctx, cfg, name, mcpCfg, resolver, &admission, false); err != nil {
			cleanupFailedAdmission(&admission, err)
			slog.Error("Failed to initialize revealed MCP server", "name", name, "err", err)
		}
	}()
}

func fallbackDefinition(result config.MCPMutationResult) (string, config.MCPConfig, bool) {
	if result.Operation == "replace" && result.OldName != result.NewName {
		return result.OldName, result.FallbackConfig, result.FallbackExists
	}
	return result.NewName, result.NewConfig, result.NewExists
}

func clearAdvertised(name string) {
	allTools.Del(name)
	allPrompts.Del(name)
	allResources.Del(name)
}
