// Package mcp provides functionality for managing Model Context Protocol (MCP)
// clients within the Rush application.
package mcp

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"

	"github.com/PHPCraftdream/rush/internal/agent/tools/mcp/internal/contextlock"
	"github.com/PHPCraftdream/rush/internal/config"
	"github.com/PHPCraftdream/rush/internal/csync"
	"github.com/PHPCraftdream/rush/internal/permission"
	"github.com/PHPCraftdream/rush/internal/pubsub"
)

var (
	sessions    = csync.NewMap[string, *ClientSession]()
	states      = csync.NewMap[string, ClientInfo]()
	stateOwners = csync.NewMap[string, *serverAdmission]()
	broker      = pubsub.NewBroker[Event]()
	leases      = newLeaseRegistry()

	lifecycleMu contextlock.RWMutex
	owner       *Owner
	initDone    = closedChannel()
	generation  uint64
)

func (o *Owner) beginInit() bool {
	lifecycleMu.Lock()
	defer lifecycleMu.Unlock()
	if !o.isCurrentLocked() {
		return false
	}
	o.initCount++
	o.initWG.Add(1)
	return true
}

// beginInitialize starts one full initialization barrier for this owner. The
// barrier is opened lazily so an owner used only for single-server operations
// does not make WaitForInit wait forever.
func (o *Owner) beginInitialize() bool {
	lifecycleMu.Lock()
	defer lifecycleMu.Unlock()
	if !o.isCurrentLocked() {
		return false
	}
	if o.fullInitCount == 0 {
		o.initStarted = true
		o.initDone = make(chan struct{})
		// A standalone owner must never clobber the installed owner's
		// initDone mirror.
		if !o.standalone {
			initDone = o.initDone
		}
	}
	o.fullInitCount++
	o.initCount++
	o.initWG.Add(1)
	return true
}

func (o *Owner) endInit() {
	lifecycleMu.Lock()
	o.initCount--
	lifecycleMu.Unlock()
	o.initWG.Done()
	mcpInitTestHooks.Lock()
	hook := mcpInitTestHooks.afterWaitGroupDone
	mcpInitTestHooks.Unlock()
	if hook != nil {
		hook()
	}
}

func (o *Owner) finishInitialize() {
	lifecycleMu.Lock()
	if o.fullInitCount > 0 {
		o.fullInitCount--
	}
	if o.fullInitCount == 0 && o.initStarted {
		o.closeInitBarrierLocked()
	}
	lifecycleMu.Unlock()
}

func (o *Owner) closeInitBarrierLocked() {
	if o.initStarted {
		select {
		case <-o.initDone:
		default:
			close(o.initDone)
		}
	}
}

// RestrictMCPToCLI option for non-interactive invocations (rush run and
// every other CLI subcommand except the bare `rush` that starts the web
// UI). The interactive web/TUI path always passes false here, so it
// keeps starting every non-disabled server exactly as before this field
// existed.
func Initialize(ctx context.Context, permissions permission.Service, cfg *config.ConfigStore, restrictToCLIEnabled bool) {
	o := currentOwner()
	if o == nil {
		var err error
		o, err = acquireImplicit()
		if err != nil {
			slog.Error("Failed to acquire MCP application owner", "error", err)
			return
		}
	}
	o.Initialize(ctx, permissions, cfg, restrictToCLIEnabled)
}

// Initialize initializes MCP clients using this owner's lifecycle barrier.
func (o *Owner) Initialize(ctx context.Context, permissions permission.Service, cfg *config.ConfigStore, restrictToCLIEnabled bool) {
	if o == nil {
		Initialize(ctx, permissions, cfg, restrictToCLIEnabled)
		return
	}
	slog.Info("Initializing MCP clients")
	// The permission service is consumed later while tools are called. Keep it
	// in the signature for compatibility with the existing startup contract.
	_ = permissions
	var wg sync.WaitGroup
	initCtx, cancel := context.WithCancel(ctx)
	lifecycleMu.Lock()
	if owner != o && !o.standalone || o.closing {
		lifecycleMu.Unlock()
		cancel()
		return
	}
	if o.config != nil && o.config != cfg {
		lifecycleMu.Unlock()
		cancel()
		slog.Warn("Rejected MCP initialization for a different config store")
		return
	}
	o.config = cfg
	lifecycleMu.Unlock()
	if err := o.reconcileAllUncertainty(ctx, cfg); err != nil {
		slog.Warn("Failed to reconcile uncertain MCP config", "err", err)
	}
	if !o.beginInitialize() {
		cancel()
		return
	}
	defer func() {
		o.finishInitialize()
		o.endInit()
	}()
	stopOwner := context.AfterFunc(o.lifecycleCtx, cancel)
	defer stopOwner()
	defer cancel()
	// Capture the server names from one immutable snapshot. Each admission then
	// captures its own config and resolver under its server lease.
	configured, _, _, _ := cfg.SnapshotWithResolverAndMCPRevisions()
	if configured == nil {
		return
	}
	for name := range configured.MCP {
		if !o.acceptsSession() {
			break
		}
		for attempt := 0; attempt < maxAdmissionRetries; attempt++ {
			lease := serverLeaseFor(name)
			if !lease.lockContext(initCtx, true) {
				break
			}
			admissionSnapshot := cfg.SnapshotMCPAdmission(name)
			if !admissionSnapshot.Exists {
				lease.Unlock()
				_ = failClosedMCP(initCtx, name)
				break
			}
			if o.isUncertain(cfg, name) {
				lease.Unlock()
				_ = failClosedMCP(initCtx, name)
				break
			}
			if admissionSnapshot.MCPConfig.Disabled || restrictToCLIEnabled && !admissionSnapshot.MCPConfig.EnabledInCLI {
				skipped, skippedErr := transitionSkippedMCP(initCtx, o, cfg, name, admissionSnapshot)
				lease.Unlock()
				skipped.finish()
				if errors.Is(skippedErr, config.ErrMCPMutationStale) {
					if admissionRetriesExhausted(attempt) {
						failClosedInitializeMCP(initCtx, name, skippedErr)
						break
					}
					if refreshErr := refreshAdmissionStore(initCtx, cfg); refreshErr != nil {
						failClosedInitializeMCP(initCtx, name, refreshErr)
						break
					}
					continue
				}
				if skippedErr != nil {
					break
				}
				if admissionSnapshot.MCPConfig.Disabled {
					slog.Debug("Skipping disabled MCP", "name", name)
				} else {
					slog.Debug("Skipping MCP not enabled for CLI mode (set enabled_in_cli or pass --all-mcp)", "name", name)
				}
				break
			}
			admission, err := o.admitServerForConfig(initCtx, cfg, name, admissionSnapshot.MCPConfig, true)
			if err != nil {
				lease.Unlock()
				break
			}
			admission.mcpRevision = admissionSnapshot.MCPRevision
			admission.resolverRevision = admissionSnapshot.ResolverRevision
			admission.mcpAdmission = admissionSnapshot
			admission.configIdentity = admissionSnapshot.MCPConfig
			admission.hasConfigIdentity = true
			resolver := admissionSnapshot.Resolver
			lease.Unlock()
			admission.candidate = true
			wg.Add(1)
			go func(name string, m config.MCPConfig, resolver config.VariableResolver, admission serverAdmission) {
				defer func() {
					wg.Done()
					admission.done()
					if r := recover(); r != nil {
						var err error
						switch v := r.(type) {
						case error:
							err = v
						case string:
							err = fmt.Errorf("panic: %s", v)
						default:
							err = fmt.Errorf("panic: %v", v)
						}
						cleanupFailedAdmission(&admission, err)
						slog.Error("Panic in MCP client initialization", "error", err, "name", name)
					}
				}()

				if err := initClientAdmittedWithRetry(initCtx, cfg, name, m, resolver, &admission); err != nil {
					slog.Debug("Failed to initialize MCP client", "name", name, "error", err)
				}
			}(name, admissionSnapshot.MCPConfig, resolver, admission)
			break
		}
	}
	wg.Wait()
}

// WaitForInit blocks until MCP initialization is complete.
// If Initialize was never called, this returns immediately.
func WaitForInit(ctx context.Context) error {
	lifecycleMu.Lock()
	done := initDone
	lifecycleMu.Unlock()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// WaitForInit blocks until MCP initialization is complete.
// If Initialize was never called, this returns immediately.
// A nil receiver resolves the process-current owner exactly like the
// package-level WaitForInit function, so callers holding an optional owner
// can call the method unconditionally.
func (o *Owner) WaitForInit(ctx context.Context) error {
	if o == nil {
		return WaitForInit(ctx)
	}
	lifecycleMu.Lock()
	done := o.initDone
	lifecycleMu.Unlock()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// InitializeSingle initializes a single MCP client by name.
func InitializeSingle(ctx context.Context, name string, cfg *config.ConfigStore) error {
	o := currentOwner()
	if o == nil {
		var err error
		o, err = acquireImplicit()
		if err != nil {
			return err
		}
	}
	return initializeSingleForOwner(o, ctx, name, cfg)
}

// InitializeSingle initializes a single MCP client by name.
// A nil receiver resolves the process-current owner exactly like the
// package-level InitializeSingle function, so callers holding an optional
// owner can call the method unconditionally.
func (o *Owner) InitializeSingle(ctx context.Context, name string, cfg *config.ConfigStore) error {
	if o == nil {
		return InitializeSingle(ctx, name, cfg)
	}
	return initializeSingleForOwner(o, ctx, name, cfg)
}

func initializeSingleForOwner(o *Owner, ctx context.Context, name string, cfg *config.ConfigStore) error {
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

	for attempt := 0; attempt < maxAdmissionRetries; attempt++ {
		lease := serverLeaseFor(name)
		if !lease.lockContext(ctx, true) {
			return ctx.Err()
		}

		admissionSnapshot := cfg.SnapshotMCPAdmission(name)
		resolver := admissionSnapshot.Resolver
		m, exists := admissionSnapshot.MCPConfig, admissionSnapshot.Exists
		if !exists {
			lease.Unlock()
			return fmt.Errorf("mcp '%s' not found in configuration", name)
		}
		lifecycleMu.Lock()
		uncertain := false
		if o.isCurrentLocked() {
			_, uncertain = cfg.MCPUncertaintyVersion(name)
		}
		lifecycleMu.Unlock()
		if uncertain {
			lease.Unlock()
			return ErrMCPConfigUncertain
		}

		if m.Disabled {
			skipped, transitionErr := transitionSkippedMCP(ctx, o, cfg, name, admissionSnapshot)
			lease.Unlock()
			skipped.finish()
			if errors.Is(transitionErr, config.ErrMCPMutationStale) {
				if admissionRetriesExhausted(attempt) {
					failClosedInitializeMCP(ctx, name, transitionErr)
					return transitionErr
				}
				if refreshErr := refreshAdmissionStore(ctx, cfg); refreshErr != nil {
					failClosedInitializeMCP(ctx, name, refreshErr)
					return refreshErr
				}
				continue
			}
			if transitionErr != nil {
				return transitionErr
			}
			slog.Debug("Skipping disabled MCP", "name", name)
			return nil
		}

		admitted, err := o.admitServerForConfig(ctx, cfg, name, m, true)
		if err != nil {
			lease.Unlock()
			return err
		}
		admitted.mcpRevision = admissionSnapshot.MCPRevision
		admitted.resolverRevision = admissionSnapshot.ResolverRevision
		admitted.mcpAdmission = admissionSnapshot
		lease.Unlock()
		err = initClientAdmitted(admitted.ctx, cfg, name, m, resolver, &admitted)
		if !errors.Is(err, config.ErrMCPMutationStale) || ctx.Err() != nil || admissionRetriesExhausted(attempt) {
			if errors.Is(err, config.ErrMCPMutationStale) {
				cleanupFailedAdmission(&admitted, err)
			}
			return err
		}
		if refreshErr := refreshAdmissionStore(ctx, cfg); refreshErr != nil {
			cleanupFailedAdmission(&admitted, refreshErr)
			return refreshErr
		}
	}
	return config.ErrMCPMutationStale
}

func initClientAdmitted(ctx context.Context, cfg *config.ConfigStore, name string, m config.MCPConfig, resolver config.VariableResolver, admission *serverAdmission) error {
	return initClientAdmittedWithState(ctx, cfg, name, m, resolver, admission, true)
}

func initClientAdmittedWithState(ctx context.Context, cfg *config.ConfigStore, name string, m config.MCPConfig, resolver config.VariableResolver, admission *serverAdmission, announceStarting bool) error {
	if admission != nil {
		defer admission.done()
		lifecycleMu.Lock()
		valid := admission.validLocked()
		stale := !valid && admission.candidate && admission.configSnapshotStaleLocked()
		lifecycleMu.Unlock()
		if !valid {
			err := ErrOwnerBusy
			if stale {
				err = config.ErrMCPMutationStale
			} else {
				cleanupFailedAdmission(admission, err)
			}
			return err
		}
	}
	if announceStarting {
		if admission == nil {
			updateState(name, StateStarting, nil, nil, Counts{})
		} else {
			updateAdmissionState(admission, StateStarting, nil, nil, Counts{})
		}
	}

	operationCtx := ctx
	if admission != nil {
		var finish func()
		operationCtx, finish = admission.owner.operationContext(ctx)
		defer finish()
	}
	prepared, err := prepareClient(operationCtx, cfg, name, m, resolver, admission)
	if err != nil {
		if !errors.Is(err, config.ErrMCPMutationStale) {
			cleanupFailedAdmission(admission, err)
		}
		return err
	}
	err = publishPreparedClient(cfg, name, prepared, admission)
	if err != nil {
		if !errors.Is(err, config.ErrMCPMutationStale) {
			cleanupFailedAdmission(admission, err)
		}
	}
	return err
}
