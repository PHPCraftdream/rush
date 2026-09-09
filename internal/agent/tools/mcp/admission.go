package mcp

import (
	"context"
	"errors"
	"fmt"
	"github.com/PHPCraftdream/rush/internal/config"
	"github.com/PHPCraftdream/rush/internal/pubsub"
	"reflect"
	"sync"
	"time"
)

// serverAdmission pins one owner generation and one server epoch. Its init
// reference is held until the candidate or operation has completely exited,
// allowing Owner.Close to fence all late work before registry reset.
type serverAdmission struct {
	owner               *Owner
	generation          uint64
	epoch               uint64
	cfg                 *config.ConfigStore
	mcpAdmission        config.MCPAdmissionSnapshot
	configIdentity      config.MCPConfig
	hasConfigIdentity   bool
	mcpRevision         uint64
	resolverRevision    uint64
	name                string
	guardName           string
	guardEpoch          uint64
	ctx                 context.Context
	cancel              context.CancelFunc
	stop                func()
	once                *sync.Once
	serverCancelToken   uint64
	stateToken          uint64
	committed           bool
	candidate           bool
	promoted            bool
	suppressState       bool
	suppressUntilCommit bool
	committedName       string
	committedEpoch      uint64
	prepared            *preparedClient
	publishedSession    *ClientSession
	mutationResult      *config.MCPMutationResult
	deferDone           bool
	publishingPrepared  bool
}

func (a *serverAdmission) done() {
	if a == nil || a.owner == nil {
		return
	}
	lifecycleMu.Lock()
	// A prepared Add candidate owns its admission through the durable commit.
	// Initializer adapters are allowed to call done themselves, so keep that
	// call harmless while the candidate is still staged for publication.
	if a.deferDone && a.prepared != nil && !a.committed {
		lifecycleMu.Unlock()
		return
	}
	lifecycleMu.Unlock()
	if a.once != nil {
		a.once.Do(a.owner.endInit)
	}
	if a.stop != nil {
		a.stop()
	}
	lifecycleMu.Lock()
	if a.serverCancelToken != 0 {
		a.owner.discardDeferredRefreshesLocked(a.serverCancelToken)
	}
	if a.serverCancelToken != 0 {
		if current, ok := a.owner.serverCancels[a.name]; ok {
			if registered, ok := current[a.serverCancelToken]; ok && registered.token == a.serverCancelToken {
				delete(current, a.serverCancelToken)
				if len(current) == 0 {
					delete(a.owner.serverCancels, a.name)
				}
			}
		}
	}
	lifecycleMu.Unlock()
	if a.cancel != nil {
		a.cancel()
	}
}

func (a *serverAdmission) valid() bool {
	if a == nil || a.owner == nil || a.cfg == nil {
		return true
	}
	lifecycleMu.Lock()
	valid := a.validLocked()
	lifecycleMu.Unlock()
	return valid
}

func (a *serverAdmission) notificationsValid() bool {
	if a == nil || a.owner == nil || a.cfg == nil {
		return true
	}
	lifecycleMu.Lock()
	valid := a.notificationsValidLocked()
	lifecycleMu.Unlock()
	return valid
}

func (a *serverAdmission) notificationsValidLocked() bool {
	if a == nil || a.owner == nil || a.cfg == nil {
		return true
	}
	if a.suppressUntilCommit && !a.committed {
		return a.candidateValidLocked()
	}
	return a.validLocked()
}

// bindGuardLocked adds the destination identity to a replacement candidate.
// lifecycleMu must be held by the caller.
func (a *serverAdmission) bindGuardLocked(name string) bool {
	if a == nil || a.owner == nil || a.cfg == nil || name == "" || name == a.name {
		return true
	}
	if _, uncertain := a.cfg.MCPUncertaintyVersion(name); uncertain {
		return false
	}
	a.guardName = name
	a.guardEpoch = a.owner.serverEpochs[name]
	return true
}

// replacementValidLocked checks both identities before the durable replacement
// is published. It does not require the old config to remain present after the
// durable rename has committed.
func (a *serverAdmission) replacementValidLocked() bool {
	if a == nil || a.owner == nil || a.cfg == nil {
		return true
	}
	if owner != a.owner && !a.owner.standalone || a.owner.closing || a.owner.generation != a.generation ||
		!a.replacementNamesValidLocked() || !a.sourceConfigValidLocked() {
		return false
	}
	return true
}

func (a *serverAdmission) sourceConfigValidLocked() bool {
	if a == nil || a.cfg == nil || !a.hasConfigIdentity {
		return true
	}
	snapshot := a.cfg.SnapshotMCPAdmission(a.name)
	return snapshot.Exists &&
		(a.mcpRevision == 0 || snapshot.MCPRevision == a.mcpRevision) &&
		(a.resolverRevision == 0 || snapshot.ResolverRevision == a.resolverRevision) &&
		reflect.DeepEqual(snapshot.MCPConfig, a.configIdentity)
}

func (a *serverAdmission) replacementNamesValidLocked() bool {
	if a == nil || a.owner == nil || a.cfg == nil {
		return true
	}
	if a.owner.serverEpochs[a.name] != a.epoch {
		return false
	}
	if _, uncertain := a.cfg.MCPUncertaintyVersion(a.name); uncertain {
		return false
	}
	if a.guardName != "" && (a.owner.serverEpochs[a.guardName] != a.guardEpoch) {
		return false
	}
	if a.guardName != "" {
		if _, uncertain := a.cfg.MCPUncertaintyVersion(a.guardName); uncertain {
			return false
		}
	}
	return true
}

// candidateValidLocked validates a replacement candidate without consulting
// the durable config. The durable replacement may already have committed and
// removed the old name while runtime publication is still waiting on the
// lifecycle lock. lifecycleMu must be held by the caller.
func (a *serverAdmission) candidateValidLocked() bool {
	_, uncertain := a.cfg.MCPUncertaintyVersion(a.name)
	if a.guardName != "" {
		if a.owner.serverEpochs[a.guardName] != a.guardEpoch {
			return false
		}
		if _, guardUncertain := a.cfg.MCPUncertaintyVersion(a.guardName); guardUncertain {
			return false
		}
	}
	if a.hasConfigIdentity {
		snapshot := a.cfg.SnapshotMCPAdmission(a.name)
		if a.mcpRevision != 0 && snapshot.MCPRevision != a.mcpRevision ||
			a.resolverRevision != 0 && snapshot.ResolverRevision != a.resolverRevision {
			return false
		}
		current, exists := snapshot.MCPConfig, snapshot.Exists
		if !exists || current.Disabled || !reflect.DeepEqual(current, a.configIdentity) {
			return false
		}
	}
	return (a.promoted || a.ctx == nil || a.ctx.Err() == nil) && (a.owner.standalone || owner == a.owner) &&
		!a.owner.closing && a.owner.generation == a.generation &&
		a.owner.serverEpochs[a.name] == a.epoch && !uncertain
}

func (a *serverAdmission) validLocked() bool {
	if a == nil || a.owner == nil || a.cfg == nil {
		return true
	}
	if !a.committed {
		return a.candidateValidLocked()
	}
	return a.committedValidLocked()
}

func (a *serverAdmission) configSnapshotStaleLocked() bool {
	if a == nil || a.cfg == nil || a.mcpAdmission.Config == nil {
		return false
	}
	current := a.cfg.SnapshotMCPAdmission(a.name)
	return current.Exists != a.mcpAdmission.Exists ||
		current.MCPRevision != a.mcpAdmission.MCPRevision ||
		current.ResolverRevision != a.mcpAdmission.ResolverRevision ||
		current.HasMCPInput != a.mcpAdmission.HasMCPInput ||
		current.HasMCPInput && current.MCPInput != a.mcpAdmission.MCPInput ||
		current.Exists && !reflect.DeepEqual(current.MCPConfig, a.mcpAdmission.MCPConfig)
}

func (a *serverAdmission) committedValidLocked() bool {
	if a == nil || a.owner == nil || a.cfg == nil {
		return true
	}
	admissionName := a.name
	admissionEpoch := a.epoch
	if a.committedName != "" {
		admissionName = a.committedName
		admissionEpoch = a.committedEpoch
	}
	valid := (a.owner.standalone || owner == a.owner) && !a.owner.closing &&
		a.owner.generation == a.generation &&
		a.owner.serverEpochs[admissionName] == admissionEpoch
	if !valid {
		return false
	}
	if _, uncertain := a.cfg.MCPUncertaintyVersion(admissionName); uncertain {
		return false
	}
	mcpConfig, exists := a.cfg.MCPConfig(admissionName)
	if !exists {
		return false
	}
	if a.publishedSession != nil {
		currentSession, sessionExists := sessions.Get(admissionName)
		if !sessionExists || currentSession != a.publishedSession {
			return false
		}
	}
	if a.hasConfigIdentity && !mcpConnectionConfigEqual(a.configIdentity, mcpConfig) {
		return false
	}
	return !mcpConfig.Disabled
}

func mcpConnectionConfigEqual(a, b config.MCPConfig) bool {
	return a.Type == b.Type && a.Command == b.Command &&
		reflect.DeepEqual(a.Env, b.Env) && reflect.DeepEqual(a.Args, b.Args) &&
		a.URL == b.URL && reflect.DeepEqual(a.Headers, b.Headers)
}

func (o *Owner) admitServer(ctx context.Context, cfg *config.ConfigStore, name string, bump bool) (serverAdmission, error) {
	return o.admitServerWithConfig(ctx, cfg, name, config.MCPConfig{}, false, bump)
}

func (o *Owner) admitServerForConfig(ctx context.Context, cfg *config.ConfigStore, name string, mcpConfig config.MCPConfig, bump bool) (serverAdmission, error) {
	admission, err := o.admitServerWithConfig(ctx, cfg, name, mcpConfig, true, bump)
	if err == nil {
		admission.candidate = true
	}
	return admission, err
}

func (o *Owner) admitServerWithConfig(ctx context.Context, cfg *config.ConfigStore, name string, mcpConfig config.MCPConfig, pinConfig, bump bool) (serverAdmission, error) {
	lifecycleMu.Lock()
	defer lifecycleMu.Unlock()
	if !o.isCurrentLocked() {
		return serverAdmission{}, ErrOwnerBusy
	}
	if o.config != nil && o.config != cfg {
		return serverAdmission{}, ErrMCPConfigStoreBusy
	}
	if _, uncertain := cfg.MCPUncertaintyVersion(name); uncertain {
		return serverAdmission{}, ErrMCPConfigUncertain
	}
	if bump {
		for _, registered := range o.serverCancels[name] {
			registered.cancel()
		}
		delete(o.serverCancels, name)
		o.serverEpochs[name]++
	}
	var operationCtx context.Context
	var cancel context.CancelFunc
	var stop func()
	var serverCancelToken uint64
	if bump {
		operationCtx, cancel = context.WithCancel(ctx)
		stopFunc := context.AfterFunc(o.lifecycleCtx, cancel)
		stop = func() { _ = stopFunc() }
		if o.serverCancels[name] == nil {
			o.serverCancels[name] = make(map[uint64]serverCancel)
		}
		o.nextCancelToken++
		serverCancelToken = o.nextCancelToken
		o.serverCancels[name][serverCancelToken] = serverCancel{cancel: cancel, token: serverCancelToken}
	} else {
		operationCtx = ctx
	}
	admissionSnapshot := cfg.SnapshotMCPAdmission(name)
	admission := serverAdmission{
		owner:             o,
		generation:        o.generation,
		epoch:             o.serverEpochs[name],
		cfg:               cfg,
		mcpAdmission:      admissionSnapshot,
		configIdentity:    mcpConfig,
		hasConfigIdentity: pinConfig,
		mcpRevision:       admissionSnapshot.MCPRevision,
		resolverRevision:  admissionSnapshot.ResolverRevision,
		name:              name,
		once:              new(sync.Once),
		ctx:               operationCtx,
		cancel:            cancel,
		stop:              stop,
		serverCancelToken: serverCancelToken,
		stateToken:        serverCancelToken,
	}
	o.initCount++
	o.initWG.Add(1)
	return admission, nil
}

// admitReplacementCandidate registers a cancellable candidate against the
// source server without changing its epoch. A failed replacement can then
// remove only this token, leaving the published session's admission and
// notification callbacks fully valid.
func (o *Owner) admitReplacementCandidate(ctx context.Context, cfg *config.ConfigStore, name string) (serverAdmission, error) {
	admission, err := o.admitReplacementCandidateWithSnapshot(ctx, cfg, name, cfg.SnapshotMCPAdmission(name))
	if err == nil {
		admission.hasConfigIdentity = false
	}
	return admission, err
}

func (o *Owner) admitReplacementCandidateWithSnapshot(
	ctx context.Context,
	cfg *config.ConfigStore,
	name string,
	snapshot config.MCPAdmissionSnapshot,
) (serverAdmission, error) {
	lifecycleMu.Lock()
	defer lifecycleMu.Unlock()
	if !o.isCurrentLocked() {
		return serverAdmission{}, ErrOwnerBusy
	}
	if o.config != nil && o.config != cfg {
		return serverAdmission{}, ErrMCPConfigStoreBusy
	}
	if _, uncertain := cfg.MCPUncertaintyVersion(name); uncertain {
		return serverAdmission{}, ErrMCPConfigUncertain
	}
	operationCtx, cancel := context.WithCancel(ctx)
	stopFunc := context.AfterFunc(o.lifecycleCtx, cancel)
	if o.serverCancels[name] == nil {
		o.serverCancels[name] = make(map[uint64]serverCancel)
	}
	o.nextCancelToken++
	token := o.nextCancelToken
	o.serverCancels[name][token] = serverCancel{cancel: cancel, token: token}
	admission := serverAdmission{
		owner:             o,
		generation:        o.generation,
		epoch:             o.serverEpochs[name],
		cfg:               cfg,
		mcpAdmission:      snapshot,
		configIdentity:    snapshot.MCPConfig,
		hasConfigIdentity: snapshot.Exists,
		mcpRevision:       snapshot.MCPRevision,
		resolverRevision:  snapshot.ResolverRevision,
		name:              name,
		once:              new(sync.Once),
		ctx:               operationCtx,
		cancel:            cancel,
		stop:              func() { _ = stopFunc() },
		serverCancelToken: token,
		stateToken:        token,
		candidate:         true,
	}
	o.initCount++
	o.initWG.Add(1)
	return admission, nil
}

// markPendingGlobalAdd records the one AddServer transaction whose in-memory
// definition is intentionally destined for the global config but has not
// reached disk yet. Full initialization, replacement, renewal, and refresh
// admissions never call this method and therefore can never authorize a global
// fallback.
func (o *Owner) markPendingGlobalAdd(admission *serverAdmission, cfg *config.ConfigStore, mcpCfg config.MCPConfig) (*addTransaction, bool) {
	if admission == nil || admission.serverCancelToken == 0 {
		return nil, false
	}
	lifecycleMu.Lock()
	defer lifecycleMu.Unlock()
	if admission.owner != o || admission.name == "" || !o.isCurrentLocked() {
		return nil, false
	}
	registered, ok := o.serverCancels[admission.name][admission.serverCancelToken]
	if !ok || registered.token != admission.serverCancelToken {
		return nil, false
	}
	transaction := &addTransaction{
		name:      admission.name,
		cfg:       cfg,
		mcpConfig: mcpCfg,
		token:     admission.serverCancelToken,
		done:      make(chan struct{}),
	}
	o.pendingGlobalAdds[admission.name] = transaction
	return transaction, true
}

// completePendingGlobalAdd removes only the exact transaction and closes its
// completion signal. A newer same-name Add may already own the map entry.
func (o *Owner) completePendingGlobalAdd(transaction *addTransaction) {
	if transaction == nil {
		return
	}
	transaction.once.Do(func() {
		lifecycleMu.Lock()
		if current := o.pendingGlobalAdds[transaction.name]; current == transaction && current.token == transaction.token {
			delete(o.pendingGlobalAdds, transaction.name)
		}
		lifecycleMu.Unlock()
		close(transaction.done)
	})
}

func (o *Owner) pendingGlobalAdd(name string, cfg *config.ConfigStore) *addTransaction {
	lifecycleMu.Lock()
	defer lifecycleMu.Unlock()
	transaction := o.pendingGlobalAdds[name]
	if transaction == nil || transaction.cfg != cfg || transaction.token == 0 {
		return nil
	}
	return transaction
}

func pendingGlobalAddFor(cfg *config.ConfigStore, name string) *addTransaction {
	lifecycleMu.Lock()
	defer lifecycleMu.Unlock()
	if owner == nil {
		return nil
	}
	transaction := owner.pendingGlobalAdds[name]
	if transaction == nil || transaction.cfg != cfg || transaction.token == 0 {
		return nil
	}
	return transaction
}

// snapshotServerAdmission captures the lifecycle and configuration fence for
// an operation. The caller owns the separate init reference used to keep the
// owner alive while the operation runs.
func (o *Owner) snapshotServerAdmission(_ context.Context, cfg *config.ConfigStore, name string) (serverAdmission, error) {
	lifecycleMu.Lock()
	defer lifecycleMu.Unlock()
	if !o.isCurrentLocked() {
		return serverAdmission{}, ErrOwnerBusy
	}
	if o.config != nil && o.config != cfg {
		return serverAdmission{}, ErrMCPConfigStoreBusy
	}
	if _, uncertain := cfg.MCPUncertaintyVersion(name); uncertain {
		return serverAdmission{}, ErrMCPConfigUncertain
	}
	operationCtx, cancel := context.WithCancel(o.lifecycleCtx)
	if o.serverCancels[name] == nil {
		o.serverCancels[name] = make(map[uint64]serverCancel)
	}
	o.nextCancelToken++
	token := o.nextCancelToken
	o.serverCancels[name][token] = serverCancel{cancel: cancel, token: token}
	admissionSnapshot := cfg.SnapshotMCPAdmission(name)
	return serverAdmission{
		owner:             o,
		generation:        o.generation,
		epoch:             o.serverEpochs[name],
		cfg:               cfg,
		mcpAdmission:      admissionSnapshot,
		mcpRevision:       admissionSnapshot.MCPRevision,
		resolverRevision:  admissionSnapshot.ResolverRevision,
		name:              name,
		ctx:               operationCtx,
		cancel:            cancel,
		serverCancelToken: token,
		stateToken:        token,
	}, nil
}

func (o *Owner) invalidateServer(name string) {
	lifecycleMu.Lock()
	cancels := o.invalidateServerLocked(name)
	lifecycleMu.Unlock()
	for _, cancel := range cancels {
		cancel()
	}
}

func (o *Owner) invalidateServerLocked(name string) []context.CancelFunc {
	cancels := make([]context.CancelFunc, 0, len(o.serverCancels[name]))
	for _, registered := range o.serverCancels[name] {
		cancels = append(cancels, registered.cancel)
	}
	delete(o.serverCancels, name)
	o.serverEpochs[name]++
	return cancels
}

// cancelServerCandidates promptly aborts admitted initialization and
// replacement work without changing the published server epoch. The caller
// performs the epoch bump only after its own durable mutation succeeds.
func (o *Owner) cancelServerCandidates(name string) {
	lifecycleMu.Lock()
	cancels := make([]context.CancelFunc, 0, len(o.serverCancels[name]))
	for _, registered := range o.serverCancels[name] {
		cancels = append(cancels, registered.cancel)
	}
	lifecycleMu.Unlock()
	for _, cancel := range cancels {
		cancel()
	}
}

// mcpReloadAfterSuccessHook is a deterministic test seam for the boundary
// between a successful disk reload and uncertainty finalization.
var mcpReloadAfterSuccessHook func()

var mcpInitTestHooks struct {
	sync.Mutex
	afterWaitGroupDone            func()
	beforeSkippedAdmission        func(string, config.MCPAdmissionSnapshot)
	beforeAdmissionTurn           func(string)
	beforeAdmissionFinalValidate  func(string)
	afterAdmissionFinalRevalidate func(string)
	beforeCloseRequest            func(*Owner)
}

func runBeforeAdmissionTurn(name string) {
	mcpInitTestHooks.Lock()
	hook := mcpInitTestHooks.beforeAdmissionTurn
	mcpInitTestHooks.Unlock()
	if hook != nil {
		hook(name)
	}
}

func runBeforeAdmissionFinalValidate(name string) {
	mcpInitTestHooks.Lock()
	hook := mcpInitTestHooks.beforeAdmissionFinalValidate
	mcpInitTestHooks.Unlock()
	if hook != nil {
		hook(name)
	}
}

func runAfterAdmissionFinalRevalidate(name string) {
	mcpInitTestHooks.Lock()
	hook := mcpInitTestHooks.afterAdmissionFinalRevalidate
	mcpInitTestHooks.Unlock()
	if hook != nil {
		hook(name)
	}
}

func runBeforeCloseRequest(owner *Owner) {
	mcpInitTestHooks.Lock()
	hook := mcpInitTestHooks.beforeCloseRequest
	mcpInitTestHooks.Unlock()
	if hook != nil {
		hook(owner)
	}
}

// withMCPAdmissionFinalTurn revalidates the pinned source token before taking
// lifecycleMu, then verifies its bytes at the final publication boundary.
func withMCPAdmissionFinalTurn(ctx context.Context, name string, guard config.MCPAdmissionGuard, mutate func() error) error {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	runBeforeAdmissionTurn(name)
	runBeforeAdmissionFinalValidate(name)
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := guard.RevalidateCurrentContext(ctx); err != nil {
		return err
	}
	runAfterAdmissionFinalRevalidate(name)
	if !lifecycleMu.LockContext(ctx, true) {
		if err := ctx.Err(); err != nil {
			return err
		}
		return context.Canceled
	}
	defer lifecycleMu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := guard.FinalValidateCurrentContext(ctx); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	return mutate()
}

const maxAdmissionRetries = 2

func admissionRetriesExhausted(attempt int) bool {
	return attempt+1 >= maxAdmissionRetries
}

func initClientAdmittedWithRetry(
	ctx context.Context,
	cfg *config.ConfigStore,
	name string,
	m config.MCPConfig,
	resolver config.VariableResolver,
	admission *serverAdmission,
) error {
	for attempt := 0; attempt < maxAdmissionRetries; attempt++ {
		err := initClientAdmitted(admission.ctx, cfg, name, m, resolver, admission)
		if !errors.Is(err, config.ErrMCPMutationStale) {
			return err
		}
		if ctx.Err() != nil || admissionRetriesExhausted(attempt) {
			cleanupFailedAdmission(admission, err)
			return err
		}
		if refreshErr := refreshAdmissionStore(ctx, cfg); refreshErr != nil {
			cleanupFailedAdmission(admission, refreshErr)
			return refreshErr
		}

		next, nextConfig, nextResolver, retryErr := retryAdmission(ctx, cfg, name, admission.owner)
		if retryErr != nil {
			cleanupFailedAdmission(admission, retryErr)
			return retryErr
		}
		if next == nil {
			return nil
		}
		admission = next
		m = nextConfig
		resolver = nextResolver
	}
	return config.ErrMCPMutationStale
}

func refreshAdmissionStore(ctx context.Context, cfg *config.ConfigStore) error {
	if cfg == nil || cfg.WorkingDir() == "" {
		return nil
	}
	return cfg.ReloadFromDisk(ctx)
}

func retryAdmission(
	ctx context.Context,
	cfg *config.ConfigStore,
	name string,
	o *Owner,
) (*serverAdmission, config.MCPConfig, config.VariableResolver, error) {
	lease := serverLeaseFor(name)
	if !lease.lockContext(ctx, true) {
		return nil, config.MCPConfig{}, nil, ctx.Err()
	}
	snapshot := cfg.SnapshotMCPAdmission(name)
	if !snapshot.Exists {
		lease.Unlock()
		return nil, config.MCPConfig{}, nil, fmt.Errorf("mcp '%s' not found in configuration", name)
	}
	if snapshot.MCPConfig.Disabled {
		skipped, err := transitionSkippedMCP(ctx, o, cfg, name, snapshot)
		lease.Unlock()
		skipped.finish()
		return nil, config.MCPConfig{}, nil, err
	}
	next, err := o.admitServerForConfig(ctx, cfg, name, snapshot.MCPConfig, true)
	if err == nil {
		next.mcpAdmission = snapshot
		next.mcpRevision = snapshot.MCPRevision
		next.resolverRevision = snapshot.ResolverRevision
	}
	lease.Unlock()
	if err != nil {
		return nil, config.MCPConfig{}, nil, err
	}
	return &next, snapshot.MCPConfig, snapshot.Resolver, nil
}

func ownerGeneration(o *Owner) uint64 {
	if o == nil {
		return 0
	}
	return o.generation
}

func admissionEpoch(admission *serverAdmission, o *Owner, name string) uint64 {
	if admission != nil {
		if admission.committedName != "" {
			return admission.committedEpoch
		}
		return admission.epoch
	}
	if o == nil {
		return 0
	}
	lifecycleMu.Lock()
	defer lifecycleMu.Unlock()
	return o.serverEpochs[name]
}

func setState(name string, state State, err error, client *ClientSession, counts Counts) {
	info := ClientInfo{
		Name:   name,
		State:  state,
		Error:  err,
		Client: client,
		Counts: counts,
	}
	if state == StateConnected {
		info.ConnectedAt = time.Now()
	}
	states.Set(name, info)
	stateOwners.Del(name)
}

func setAdmissionState(admission *serverAdmission, state State, err error, client *ClientSession, counts Counts) {
	if admission == nil {
		return
	}
	setState(admission.name, state, err, client, counts)
	stateOwners.Set(admission.name, admission)
}

// updateState updates the state of an MCP client and publishes an event.
func updateState(name string, state State, err error, client *ClientSession, counts Counts) {
	setState(name, state, err, client, counts)
	publishStateEvent(name, state, err, counts)
}

func updateAdmissionState(admission *serverAdmission, state State, err error, client *ClientSession, counts Counts) {
	if admission == nil {
		updateState("", state, err, client, counts)
		return
	}
	if admission.mutationResult != nil {
		mutation := admission.mutationResult
		var event Event
		var publish bool
		publicationErr := admission.cfg.WithCurrentMCPMutation(*mutation, func() error {
			event, publish = admissionStateEventUnpinned(admission, state, err, client, counts)
			return nil
		})
		if publicationErr == nil && publish {
			publishEvent(pubsub.UpdatedEvent, event)
		}
		return
	}
	updateAdmissionStateUnpinned(admission, state, err, client, counts)
}

func updateAdmissionStateUnpinned(admission *serverAdmission, state State, err error, client *ClientSession, counts Counts) {
	event, publish := admissionStateEventUnpinned(admission, state, err, client, counts)
	if publish {
		publishEvent(pubsub.UpdatedEvent, event)
	}
}

func admissionStateEventUnpinned(admission *serverAdmission, state State, err error, client *ClientSession, counts Counts) (Event, bool) {
	lifecycleMu.Lock()
	if (admission.suppressState && !admission.committed) || !admission.validLocked() {
		lifecycleMu.Unlock()
		return Event{}, false
	}
	setAdmissionState(admission, state, err, client, counts)
	event := Event{
		Type:   EventStateChanged,
		Name:   admission.name,
		State:  state,
		Error:  err,
		Counts: counts,
	}
	lifecycleMu.Unlock()
	return event, true
}

// cleanupFailedAdmission resolves a candidate-owned Starting state after a
// stale, canceled, or failed preparation without touching a newer winner.
func cleanupFailedAdmission(admission *serverAdmission, cause error) {
	if admission == nil || admission.owner == nil || admission.cfg == nil {
		return
	}
	lease := serverLeaseFor(admission.name)
	if !lease.lockContext(context.Background(), true) {
		return
	}
	var state State
	var publish bool
	lifecycleMu.Lock()
	info, exists := states.Get(admission.name)
	stateOwner, hasOwner := stateOwners.Get(admission.name)
	owned := hasOwner && stateOwner == admission && admission.stateToken != 0 &&
		admission.stateToken == admission.serverCancelToken
	if owned && exists && info.State == StateStarting && admission.owner.isCurrentLocked() &&
		admission.owner.serverEpochs[admission.name] == admission.epoch {
		current, configured := admission.cfg.MCPConfig(admission.name)
		if !configured || current.Disabled {
			state = StateDisabled
		} else {
			state = StateError
		}
		stateErr := cause
		if state == StateDisabled {
			stateErr = nil
		}
		setAdmissionState(admission, state, stateErr, nil, Counts{})
		publish = true
		cause = stateErr
	}
	lifecycleMu.Unlock()
	lease.Unlock()
	if publish {
		publishStateEvent(admission.name, state, cause, Counts{})
	}
}

func publishStateEvent(name string, state State, err error, counts Counts) {
	publishEvent(pubsub.UpdatedEvent, Event{
		Type:   EventStateChanged,
		Name:   name,
		State:  state,
		Error:  err,
		Counts: counts,
	})
}
