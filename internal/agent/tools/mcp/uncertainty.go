package mcp

import (
	"context"
	"errors"
	"github.com/PHPCraftdream/rush/internal/config"
	"slices"
	"strings"
)

// ErrMCPConfigUncertain reports that a committed config mutation has not yet
// been reconciled with a successful disk reload.
var ErrMCPConfigUncertain = errors.New("mcp: config mutation outcome is uncertain")

func (o *Owner) isUncertain(cfg *config.ConfigStore, name string) bool {
	if cfg == nil {
		return false
	}
	_, uncertain := cfg.MCPUncertaintyVersion(name)
	return uncertain
}

type uncertaintyReloadToken struct {
	owner    *Owner
	cfg      *config.ConfigStore
	versions map[string]uint64
}

func (o *Owner) captureUncertainty(cfg *config.ConfigStore, names ...string) (*uncertaintyReloadToken, bool) {
	lifecycleMu.Lock()
	defer lifecycleMu.Unlock()
	if !o.isCurrentLocked() || cfg == nil {
		return nil, false
	}
	versions := cfg.MCPUncertaintyVersions(names...)
	if len(versions) == 0 {
		return nil, false
	}
	return &uncertaintyReloadToken{owner: o, cfg: cfg, versions: versions}, true
}

// reloadWithUncertaintyToken performs the disk read outside lifecycleMu and
// every server lease. MCP admission includes the resolver and source identity
// from the store-wide snapshot, so a targeted read cannot safely clear a
// fence. Reload errors therefore remain fail-closed. The version check
// prevents a new fence raised while the disk read was in flight from being
// cleared accidentally.
func reloadWithUncertaintyToken(ctx context.Context, token *uncertaintyReloadToken) error {
	if token == nil || token.owner == nil || token.cfg == nil {
		return ErrMCPConfigUncertain
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if err := token.cfg.ReloadFromDisk(ctx); err != nil {
		return err
	}
	if mcpReloadAfterSuccessHook != nil {
		mcpReloadAfterSuccessHook()
	}
	for name := range token.versions {
		if _, ok := token.cfg.MCPUncertaintyVersion(name); ok {
			return ErrMCPConfigUncertain
		}
	}
	token.owner.reconcilePublishedSessions(token.cfg)
	return nil
}

// reconcileUncertainty reloads the consuming store only when this owner has
// an uncertainty fence for name. The reload is deliberately outside
// lifecycleMu and every server lease.
func (o *Owner) reconcileUncertainty(ctx context.Context, cfg *config.ConfigStore, name string) error {
	token, uncertain := o.captureUncertainty(cfg, name)
	if !uncertain {
		return nil
	}
	return reloadWithUncertaintyToken(ctx, token)
}

// reconcileAllUncertainty performs one reload for a full initialization when
// any server is fenced. Each captured fence is cleared only if it survived
// unchanged through the successful reload.
func (o *Owner) reconcileAllUncertainty(ctx context.Context, cfg *config.ConfigStore) error {
	token, uncertain := o.captureUncertainty(cfg)
	if !uncertain {
		return nil
	}
	return reloadWithUncertaintyToken(ctx, token)
}

// ReloadAndReconcileMCPConfig owns the reload boundary. It captures the
// current owner's uncertainty versions before reading disk and clears only
// versions unchanged by the time the successful reload is finalized.
func ReloadAndReconcileMCPConfig(ctx context.Context, cfg *config.ConfigStore) error {
	if cfg == nil {
		return errors.New("mcp: nil config store")
	}
	lifecycleMu.Lock()
	current := owner
	lifecycleMu.Unlock()
	var token *uncertaintyReloadToken
	if current != nil {
		captured, _ := current.captureUncertainty(cfg)
		token = captured
	}
	if token == nil {
		if ctx == nil {
			ctx = context.Background()
		}
		if err := cfg.ReloadFromDisk(ctx); err != nil {
			return err
		}
		if current != nil {
			current.reconcilePublishedSessions(cfg)
		}
		return nil
	}
	return reloadWithUncertaintyToken(ctx, token)
}

// ReloadAndReconcileMCPConfig owns the reload boundary. It captures the
// current owner's uncertainty versions before reading disk and clears only
// versions unchanged by the time the successful reload is finalized.
// A nil receiver resolves the process-current owner exactly like the
// package-level ReloadAndReconcileMCPConfig function, so callers holding an
// optional owner can call the method unconditionally.
func (o *Owner) ReloadAndReconcileMCPConfig(ctx context.Context, cfg *config.ConfigStore) error {
	if o == nil {
		return ReloadAndReconcileMCPConfig(ctx, cfg)
	}
	if cfg == nil {
		return errors.New("mcp: nil config store")
	}
	var token *uncertaintyReloadToken
	captured, _ := o.captureUncertainty(cfg)
	token = captured
	if token == nil {
		if ctx == nil {
			ctx = context.Background()
		}
		if err := cfg.ReloadFromDisk(ctx); err != nil {
			return err
		}
		o.reconcilePublishedSessions(cfg)
		return nil
	}
	return reloadWithUncertaintyToken(ctx, token)
}

// reconcilePublishedSessions fences connections whose committed transport no
// longer matches the effective enabled MCP definition after a reload.
func (o *Owner) reconcilePublishedSessions(cfg *config.ConfigStore) {
	if o == nil || cfg == nil {
		return
	}
	type sessionCandidate struct {
		name      string
		admission *serverAdmission
		session   *ClientSession
	}
	type retiredSession struct {
		name    string
		session *ClientSession
		cancels []context.CancelFunc
	}
	var candidates []sessionCandidate
	var retired []retiredSession
	lifecycleMu.Lock()
	if owner != o && !o.standalone || o.closing {
		lifecycleMu.Unlock()
		return
	}
	for name, admission := range o.committedAdmissions {
		if admission == nil || admission.cfg != cfg || admission.committedValidLocked() || admission.publishedSession == nil {
			continue
		}
		candidates = append(candidates, sessionCandidate{
			name: name, admission: admission, session: admission.publishedSession,
		})
	}
	lifecycleMu.Unlock()
	slices.SortFunc(candidates, func(a, b sessionCandidate) int {
		return strings.Compare(a.name, b.name)
	})
	for _, candidate := range candidates {
		lease := serverLeaseFor(candidate.name)
		lease.Lock()
		lifecycleMu.Lock()
		if owner != o && !o.standalone || o.closing || o.committedAdmissions[candidate.name] != candidate.admission {
			lifecycleMu.Unlock()
			lease.Unlock()
			continue
		}
		cancels, session, invalidated := o.detachInvalidCommittedSessionLocked(candidate.name, candidate.session, cfg)
		lifecycleMu.Unlock()
		lease.Unlock()
		if !invalidated {
			continue
		}
		retired = append(retired, retiredSession{name: candidate.name, session: session, cancels: cancels})
	}
	for _, item := range retired {
		for _, cancel := range item.cancels {
			cancel()
		}
		retireMCPClient(item.name, item.session)
		publishStateEvent(item.name, StateDisabled, nil, Counts{})
	}
}

// fenceMCPRuntime closes the runtime side of a mutation whose durable result
// cannot be read back. It deliberately does not infer a config value or start
// a fallback: a later reload must reconcile the unknown disk state first.
func fenceMCPRuntime(o *Owner, name string) {
	if o == nil {
		return
	}
	lifecycleMu.Lock()
	cfg := o.config
	lifecycleMu.Unlock()
	fenceMCPRuntimeForConfig(o, cfg, name)
}

func fenceMCPRuntimeForConfig(o *Owner, cfg *config.ConfigStore, name string) {
	lease := serverLeaseFor(name)
	lease.Lock()
	detached := fenceMCPRuntimeLocked(o, cfg, name)
	lease.Unlock()
	retireMCPClient(name, detached)
}

// fenceMCPRuntimeLocked detaches all published runtime state while the
// caller holds the server write lease. The caller retires and closes the
// returned session after the lease is released.

func fenceMCPRuntimeLocked(o *Owner, cfg *config.ConfigStore, name string) *ClientSession {
	if o != nil && cfg != nil {
		lifecycleMu.Lock()
		cfg.MarkMCPUncertain(name)
		cancels := o.invalidateServerLocked(name)
		lifecycleMu.Unlock()
		for _, cancel := range cancels {
			cancel()
		}
	}
	detached := detachSessionLocked(name)
	clearAdvertised(name)
	updateState(name, StateDisabled, nil, nil, Counts{})
	return detached
}
func commitOutcomeNeedsRuntimeFence(outcome *config.CommitOutcome) bool {
	return outcome != nil && (outcome.MaybeCommitted || (outcome.Committed && !outcome.Reconciled))
}

func commitOutcomeIsReconciled(outcome *config.CommitOutcome) bool {
	return outcome != nil && !outcome.MaybeCommitted && outcome.Committed && outcome.Reconciled
}
