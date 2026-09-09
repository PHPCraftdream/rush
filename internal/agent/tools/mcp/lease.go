package mcp

import (
	"context"
	"errors"
	"fmt"
	"github.com/PHPCraftdream/rush/internal/agent/tools/mcp/internal/contextlock"
	"github.com/PHPCraftdream/rush/internal/config"
	"slices"
	"sync"
	"time"
)

// leaseRegistry owns the per-server locks. References are held by lock
// holders and waiters, so an entry can be reclaimed as soon as its last
// operation leaves. In particular, a waiter keeps the old pointer alive until
// it acquires and releases the lock, preventing an ABA replacement.
type leaseRegistry struct {
	mu      sync.Mutex
	entries map[string]*serverLease
}

func newLeaseRegistry() *leaseRegistry {
	return &leaseRegistry{entries: make(map[string]*serverLease)}
}

// serverLease serializes replacement and closing of one server session while
// allowing concurrent callers to use the current session.
type serverLease struct {
	mu                            contextlock.RWMutex
	registry                      *leaseRegistry
	name                          string
	refs                          int
	renewing                      bool
	renewDone                     chan struct{}
	renewalBeginHook              func()
	renewalWaitHook               func()
	renewalPublishHook            func()
	renewalAfterPublishUnlockHook func()
	renewalEndHook                func()
}

// serverLeaseHookSet provides deterministic test seams for lock ordering. It
// is intentionally kept outside the lease state so production lock ownership
// is unchanged.
type serverLeaseHookSet struct {
	sync.Mutex
	beforeLockFn    func(*serverLease)
	beforeTryLockFn func(*serverLease)
	afterLockFn     func(*serverLease)
	afterUnlockFn   func(*serverLease)
}

var serverLeaseHooks serverLeaseHookSet

type addAdmissionHookSet struct {
	sync.Mutex
	afterRetainFn func(*serverAdmission)
}

var addAdmissionHooks addAdmissionHookSet

func callAfterAddRetain(admission *serverAdmission) {
	addAdmissionHooks.Lock()
	fn := addAdmissionHooks.afterRetainFn
	addAdmissionHooks.Unlock()
	if fn != nil {
		fn(admission)
	}
}

func (h *serverLeaseHookSet) callBeforeLock(lease *serverLease) {
	h.Lock()
	fn := h.beforeLockFn
	h.Unlock()
	if fn != nil {
		fn(lease)
	}
}

func (h *serverLeaseHookSet) callBeforeTryLock(lease *serverLease) {
	h.Lock()
	fn := h.beforeTryLockFn
	h.Unlock()
	if fn != nil {
		fn(lease)
	}
}

func (h *serverLeaseHookSet) callAfterLock(lease *serverLease) {
	h.Lock()
	fn := h.afterLockFn
	h.Unlock()
	if fn != nil {
		fn(lease)
	}
}

func (h *serverLeaseHookSet) callAfterUnlock(lease *serverLease) {
	h.Lock()
	fn := h.afterUnlockFn
	h.Unlock()
	if fn != nil {
		fn(lease)
	}
}

// getRetained atomically looks up (or creates) a lease and reserves one
// reference for the caller before another goroutine can reclaim the entry.
func (r *leaseRegistry) getRetained(name string) *serverLease {
	r.mu.Lock()
	defer r.mu.Unlock()
	lease, ok := r.entries[name]
	if !ok {
		lease = &serverLease{registry: r, name: name}
		r.entries[name] = lease
	}
	lease.refs++
	return lease
}

// retain keeps a lease entry alive while a caller transitions from a read
// lock to a write lock or reacquires a read lock after renewal.
func (r *leaseRegistry) retain(lease *serverLease) bool {
	r.mu.Lock()
	if r.entries[lease.name] != lease || lease.refs == 0 {
		r.mu.Unlock()
		return false
	}
	lease.refs++
	r.mu.Unlock()
	return true
}

func (r *leaseRegistry) release(lease *serverLease) {
	r.mu.Lock()
	if lease.refs == 0 {
		r.mu.Unlock()
		return
	}
	lease.refs--
	if lease.refs == 0 && r.entries[lease.name] == lease {
		delete(r.entries, lease.name)
	}
	r.mu.Unlock()
}

func (r *leaseRegistry) Len() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.entries)
}

func (r *leaseRegistry) reset() {
	r.mu.Lock()
	r.entries = make(map[string]*serverLease)
	r.mu.Unlock()
}

func (l *serverLease) Lock() {
	serverLeaseHooks.callBeforeLock(l)
	l.mu.Lock()
	serverLeaseHooks.callAfterLock(l)
}

func (l *serverLease) Unlock() {
	l.mu.Unlock()
	serverLeaseHooks.callAfterUnlock(l)
	l.registry.release(l)
}

func (l *serverLease) RLock() {
	l.mu.RLock()
}

func (l *serverLease) RUnlock() {
	l.mu.RUnlock()
	l.registry.release(l)
}

func (l *serverLease) beginRenewal() bool {
	if l.renewing {
		return false
	}
	l.renewing = true
	l.renewDone = make(chan struct{})
	return l.registry.retain(l)
}

func (l *serverLease) endRenewal() {
	l.mu.Lock()
	if !l.renewing {
		l.mu.Unlock()
		return
	}
	done := l.renewDone
	l.renewing = false
	l.renewDone = nil
	close(done)
	l.mu.Unlock()
	l.registry.release(l)
}

func (l *serverLease) callRenewalEndHook() {
	l.mu.Lock()
	hook := l.renewalEndHook
	l.mu.Unlock()
	if hook != nil {
		hook()
	}
}

func (l *serverLease) callRenewalAfterPublishUnlockHook() {
	l.mu.Lock()
	hook := l.renewalAfterPublishUnlockHook
	l.mu.Unlock()
	if hook != nil {
		hook()
	}
}

// lockContext waits for the short mutation critical section while honoring
// cancellation. Network calls must never run while this lock is held.
func (l *serverLease) lockContext(ctx context.Context, write bool) bool {
	if ctx == nil {
		ctx = context.Background()
	}
	// A canceled context must never acquire a lock, even when the lock is
	// immediately available. Apart from making cancellation deterministic,
	// this keeps the caller's retained identity reference balanced.
	if ctx.Err() != nil {
		l.registry.release(l)
		return false
	}
	serverLeaseHooks.callBeforeTryLock(l)
	if !l.mu.LockContext(ctx, write) {
		l.registry.release(l)
		return false
	}
	if ctx.Err() != nil {
		if write {
			l.mu.Unlock()
		} else {
			l.mu.RUnlock()
		}
		l.registry.release(l)
		return false
	}
	return true
}

// reacquireContext transitions an existing retained identity reference into a
// new lock reference. The retained reference remains owned by the caller when
// lock acquisition is canceled.
func (l *serverLease) reacquireContext(ctx context.Context, write bool) bool {
	if !l.registry.retain(l) {
		return false
	}
	return l.lockContext(ctx, write)
}

func acquireServerLease(name string, write bool) *serverLease {
	lease := leases.getRetained(name)
	if write {
		lease.Lock()
	} else {
		lease.RLock()
	}
	return lease
}

func lockServerLeases(names ...string) []*serverLease {
	locked, _ := lockServerLeasesContext(context.Background(), names...)
	return locked
}

func lockServerLeasesContext(ctx context.Context, names ...string) ([]*serverLease, bool) {
	ordered := slices.Clone(names)
	slices.Sort(ordered)
	locked := make([]*serverLease, 0, len(ordered))
	for _, name := range ordered {
		if len(locked) > 0 && locked[len(locked)-1].name == name {
			continue
		}
		lease := serverLeaseFor(name)
		if !lease.lockContext(ctx, true) {
			unlockServerLeases(locked)
			return nil, false
		}
		locked = append(locked, lease)
	}
	return locked, true
}

func unlockServerLeases(leases []*serverLease) {
	for i := len(leases) - 1; i >= 0; i-- {
		leases[i].Unlock()
	}
}

// clientLease keeps the server read lock and owner initialization fence until
// the caller has finished its actual MCP operation.
type clientLease struct {
	session    *ClientSession
	ctx        context.Context
	release    func()
	once       sync.Once
	owner      *Owner
	name       string
	generation uint64
	epoch      uint64
	cfg        *config.ConfigStore
}

func (l *clientLease) close() {
	l.once.Do(l.release)
}

// lockForPublish acquires the short server mutation lock and validates that
// this operation still owns the current session generation. Callers must hold
// the returned lock while updating advertised data and publishing its event.
// No MCP/network operation may be performed while the lock is held.
func (l *clientLease) lockForPublish(ctx context.Context) (*serverLease, bool) {
	if l == nil {
		return nil, false
	}
	lease := serverLeaseFor(l.name)
	if !lease.lockContext(ctx, true) {
		return nil, false
	}
	lifecycleMu.Lock()
	valid := l.validForPublishLocked()
	lifecycleMu.Unlock()
	if !valid {
		lease.Unlock()
		return nil, false
	}
	return lease, true
}

func (l *clientLease) validForPublishLocked() bool {
	if l.owner != nil && (owner != l.owner || l.owner.closing ||
		l.owner.generation != l.generation || l.owner.serverEpochs[l.name] != l.epoch) {
		return false
	}
	current, ok := sessions.Get(l.name)
	if !ok || current != l.session {
		return false
	}
	if l.cfg != nil {
		mcpConfig, exists := l.cfg.MCPConfig(l.name)
		if !exists || mcpConfig.Disabled {
			return false
		}
	}
	return true
}

// publishIfCurrent runs a no-network state publication while the server lease
// is held. The validation and mutation are one short critical section, so a
// retired session cannot publish after a replacement or disable wins.
func (l *clientLease) publishIfCurrent(ctx context.Context, update, publish func()) bool {
	lease, ok := l.lockForPublish(ctx)
	if !ok {
		return false
	}
	lifecycleMu.Lock()
	if !l.validForPublishLocked() {
		lifecycleMu.Unlock()
		lease.Unlock()
		return false
	}
	update()
	lifecycleMu.Unlock()
	if publish != nil {
		publish()
	}
	lease.Unlock()
	return true
}

func serverLeaseFor(name string) *serverLease {
	return leases.getRetained(name)
}

func getOrRenewClient(ctx context.Context, cfg *config.ConfigStore, name string) (*clientLease, error) {
	lease, err := getOrRenewClientOnce(ctx, cfg, name)
	for attempt := 0; errors.Is(err, config.ErrMCPMutationStale) && attempt < maxRenewalRecoveryAttempts; attempt++ {
		lease, err = recoverStaleRenewal(ctx, cfg, name)
	}
	return lease, err
}

func recoverStaleRenewal(ctx context.Context, cfg *config.ConfigStore, name string) (*clientLease, error) {
	if refreshErr := refreshAdmissionStore(ctx, cfg); refreshErr != nil {
		return nil, refreshErr
	}
	snapshot := cfg.SnapshotMCPAdmission(name)
	if !snapshot.Exists || snapshot.MCPConfig.Disabled {
		if err := failClosedMCP(ctx, name); err != nil {
			return nil, err
		}
		return nil, fmt.Errorf("mcp '%s' not available", name)
	}
	if err := InitializeSingle(ctx, name, cfg); err != nil {
		latest := cfg.SnapshotMCPAdmission(name)
		if !latest.Exists || latest.MCPConfig.Disabled {
			if closeErr := failClosedMCP(ctx, name); closeErr != nil {
				return nil, closeErr
			}
		}
		return nil, err
	}
	return getOrRenewClientOnce(ctx, cfg, name)
}

// maxRenewalRecoveryAttempts excludes the initial renewal attempt.
const maxRenewalRecoveryAttempts = 2

func getOrRenewClientOnce(ctx context.Context, cfg *config.ConfigStore, name string) (*clientLease, error) {
	o := currentOwner()
	var admission *serverAdmission
	operationCtx := ctx
	finish := func() {}
	if o != nil {
		if err := o.rememberConfig(cfg); err != nil {
			return nil, err
		}
		if err := o.reconcileUncertainty(ctx, cfg, name); err != nil {
			return nil, err
		}
		if !o.beginInit() {
			return nil, ErrOwnerBusy
		}
		admitted, err := o.snapshotServerAdmission(ctx, cfg, name)
		if errors.Is(err, ErrMCPConfigUncertain) {
			o.endInit()
			if reconcileErr := o.reconcileUncertainty(ctx, cfg, name); reconcileErr != nil {
				return nil, reconcileErr
			}
			if !o.beginInit() {
				return nil, ErrOwnerBusy
			}
			admitted, err = o.snapshotServerAdmission(ctx, cfg, name)
		}
		if err != nil {
			o.endInit()
			return nil, err
		}
		admission = &admitted
		// The caller's context fences this operation and its health probe.
		// The admission context is independently fenced by owner shutdown and
		// the server epoch so a renewal cannot strand followers after disable
		// or remove.
		var operationFinish func()
		operationCtx, operationFinish = o.operationContext(ctx)
		finish = func() {
			admission.done()
			operationFinish()
		}
	}

	var lease *serverLease
	var sess *ClientSession
	var m config.MCPConfig
	var ok, exists bool
	var retired bool
	var sessionCtx context.Context
	var releaseSession func()
	for {
		lease = serverLeaseFor(name)
		if !lease.lockContext(operationCtx, true) {
			finish()
			if o != nil {
				o.endInit()
			}
			return nil, operationCtx.Err()
		}
		sess, ok = sessions.Get(name)
		retired = false
		if ok {
			sess.operationMu.Lock()
			retired = sess.retired
			sess.operationMu.Unlock()
		}
		m, exists = cfg.MCPConfig(name)
		if o != nil && ok {
			lifecycleMu.Lock()
			cancels, detached, invalidated := o.detachInvalidCommittedSessionLocked(name, sess, cfg)
			lifecycleMu.Unlock()
			if invalidated {
				lease.Unlock()
				for _, cancel := range cancels {
					cancel()
				}
				retireMCPClient(name, detached)
				publishStateEvent(name, StateDisabled, nil, Counts{})
				finish()
				o.endInit()
				return nil, fmt.Errorf("MCP session %q no longer matches its current configuration", name)
			}
		}
		if !exists || m.Disabled {
			lease.Unlock()
			finish()
			if o != nil {
				o.endInit()
			}
			return nil, fmt.Errorf("mcp '%s' not available", name)
		}
		if (!ok || retired) && lease.renewing {
			if lease.renewalWaitHook != nil {
				lease.renewalWaitHook()
			}
			renewDone := lease.renewDone
			lease.Unlock()
			select {
			case <-renewDone:
				continue
			case <-operationCtx.Done():
				finish()
				if o != nil {
					o.endInit()
				}
				return nil, operationCtx.Err()
			}
		}
		if !ok {
			lease.Unlock()
			finish()
			if o != nil {
				o.endInit()
			}
			return nil, fmt.Errorf("mcp '%s' not available", name)
		}
		var usable bool
		sessionCtx, releaseSession, usable = sess.acquireOperation(operationCtx)
		if !usable || !lease.registry.retain(lease) {
			lease.Unlock()
			if usable {
				releaseSession()
			}
			finish()
			if o != nil {
				o.endInit()
			}
			return nil, fmt.Errorf("mcp '%s' not available", name)
		}
		lease.Unlock()
		break
	}
	leaseRefTransferred := false
	defer func() {
		if leaseRefTransferred {
			return
		}
		lease.registry.release(lease)
	}()
	state, _ := states.Get(name)
	timeout := mcpTimeout(m)
	err := pingWithTimeout(sessionCtx, sess, timeout)
	if err == nil {
		if admission != nil && !admission.valid() {
			releaseSession()
			finish()
			o.endInit()
			return nil, ErrOwnerBusy
		}
		leaseRefTransferred = true
		return newClientLease(sess, sessionCtx, releaseSession, func() { lease.registry.release(lease) }, finish, o, name, cfg, admissionEpoch(admission, o, name)), nil
	}
	// The health probe no longer owns a server lock. Release its generation
	// reference before renewal so a writer can detach and replace it now.
	releaseSession()
	if !lease.reacquireContext(operationCtx, true) {
		finish()
		if o != nil {
			o.endInit()
		}
		return nil, operationCtx.Err()
	}
	if admission != nil && !admission.valid() {
		lease.Unlock()
		finish()
		o.endInit()
		return nil, ErrOwnerBusy
	}

	current, currentOK := sessions.Get(name)
	if currentOK && current != sess {
		currentCtx, releaseCurrent, usable := current.acquireOperation(operationCtx)
		lease.Unlock()
		if !usable {
			finish()
			if o != nil {
				o.endInit()
			}
			return nil, fmt.Errorf("mcp '%s' not available", name)
		}
		if admission != nil && !admission.valid() {
			releaseCurrent()
			finish()
			o.endInit()
			return nil, ErrOwnerBusy
		}
		leaseRefTransferred = true
		return newClientLease(current, currentCtx, releaseCurrent, func() { lease.registry.release(lease) }, finish, o, name, cfg, admissionEpoch(admission, o, name)), nil
	}
	if lease.renewing {
		renewDone := lease.renewDone
		lease.Unlock()
		select {
		case <-renewDone:
		case <-operationCtx.Done():
			finish()
			if o != nil {
				o.endInit()
			}
			return nil, operationCtx.Err()
		}
		if !lease.reacquireContext(operationCtx, true) {
			finish()
			if o != nil {
				o.endInit()
			}
			return nil, operationCtx.Err()
		}
		if admission != nil && !admission.valid() {
			lease.Unlock()
			finish()
			o.endInit()
			return nil, ErrOwnerBusy
		}
		current, currentOK = sessions.Get(name)
		if !currentOK {
			lease.Unlock()
			finish()
			if o != nil {
				o.endInit()
			}
			return nil, fmt.Errorf("mcp '%s' not available", name)
		}
		currentCtx, releaseCurrent, usable := current.acquireOperation(operationCtx)
		lease.Unlock()
		if !usable {
			finish()
			if o != nil {
				o.endInit()
			}
			return nil, fmt.Errorf("mcp '%s' not available", name)
		}
		leaseRefTransferred = true
		return newClientLease(current, currentCtx, releaseCurrent, func() { lease.registry.release(lease) }, finish, o, name, cfg, admissionEpoch(admission, o, name)), nil
	}
	if !lease.beginRenewal() {
		lease.Unlock()
		finish()
		if o != nil {
			o.endInit()
		}
		return nil, ErrOwnerBusy
	}
	if lease.renewalBeginHook != nil {
		lease.renewalBeginHook()
	}
	defer func() {
		lease.endRenewal()
		lease.callRenewalEndHook()
	}()

	setState(name, StateError, err, nil, state.Counts)
	publishStateEvent(name, StateError, err, state.Counts)
	var detachedCurrent *ClientSession
	if currentOK {
		// The ping failure identifies the session that must be retired. Remove
		// it explicitly; state transitions must never infer session ownership
		// from the previous state's Client field.
		if published, ok := sessions.Get(name); ok && published == current {
			lifecycleMu.Lock()
			if published, stillCurrent := sessions.Get(name); stillCurrent && published == current {
				detachSessionLifecycleLocked(name)
			}
			lifecycleMu.Unlock()
		}
		detachedCurrent = current
	}
	lease.Unlock()
	retireMCPClient(name, detachedCurrent)

	// The candidate connection remains caller-cancellable until promotion.
	// createSessionWithAdmission then hands its lifetime context over to the
	// owner when the candidate is committed, so a promoted session survives
	// the caller's cancellation.
	sessionCtx = operationCtx
	newSession, err := createSessionWithAdmission(sessionCtx, name, m, cfg.Resolver(), admission)
	if err != nil {
		finish()
		if o != nil {
			o.endInit()
		}
		return nil, err
	}

	if !lease.reacquireContext(operationCtx, true) {
		_ = newSession.Close()
		finish()
		if o != nil {
			o.endInit()
		}
		return nil, operationCtx.Err()
	}
	if admission != nil && !admission.valid() {
		lease.Unlock()
		_ = newSession.Close()
		finish()
		o.endInit()
		return nil, ErrOwnerBusy
	}
	if o != nil {
		retiredSession, err := o.commitRenewalForLease(admission, name, newSession, state.Counts)
		if err != nil {
			lease.Unlock()
			retireMCPClient(name, retiredSession)
			finish()
			o.endInit()
			return nil, err
		}
		defer func() { retireMCPClient(name, retiredSession) }()
	} else {
		sessions.Set(name, newSession)
		setState(name, StateConnected, nil, newSession, state.Counts)
		publishStateEvent(name, StateConnected, nil, state.Counts)
	}
	if lease.renewalPublishHook != nil {
		lease.renewalPublishHook()
	}
	newCtx, releaseNew, usable := newSession.acquireOperation(operationCtx)
	lease.Unlock()
	lease.callRenewalAfterPublishUnlockHook()
	if !usable {
		finish()
		if o != nil {
			o.endInit()
		}
		return nil, ErrOwnerBusy
	}
	if admission != nil && !admission.valid() {
		releaseNew()
		finish()
		o.endInit()
		return nil, ErrOwnerBusy
	}
	leaseRefTransferred = true
	return newClientLease(newSession, newCtx, releaseNew, func() { lease.registry.release(lease) }, finish, o, name, cfg, admissionEpoch(admission, o, name)), nil
}

func pingWithTimeout(ctx context.Context, session *ClientSession, timeout time.Duration) error {
	timeoutCause := errors.New("mcp ping timeout")
	pingCtx, cancel := context.WithTimeoutCause(ctx, timeout, timeoutCause)
	err := session.Ping(pingCtx, nil)
	errCause := context.Cause(pingCtx)
	cancel()
	return maybeTimeoutErr(err, timeout, errCause, timeoutCause)
}

func newClientLease(session *ClientSession, ctx context.Context, releaseSession, releaseLease func(), finish func(), o *Owner, name string, cfg *config.ConfigStore, epoch uint64) *clientLease {
	return &clientLease{
		session: session,
		ctx:     ctx,
		release: func() {
			releaseSession()
			if releaseLease != nil {
				releaseLease()
			}
			finish()
			if o != nil {
				o.endInit()
			}
		},
		owner:      o,
		name:       name,
		generation: ownerGeneration(o),
		epoch:      epoch,
		cfg:        cfg,
	}
}

// currentClientLease admits an operation on the current session without a
// health check. It is used by notification refreshers, which already receive
// a session selected by the MCP transport.
func currentClientLease(ctx context.Context, name string, configs ...*config.ConfigStore) (*clientLease, error) {
	o := currentOwner()
	operationCtx := ctx
	finish := func() {}
	if o != nil {
		if !o.beginInit() {
			return nil, ErrOwnerBusy
		}
		operationCtx, finish = o.operationContext(ctx)
	}

	lease := serverLeaseFor(name)
	if !lease.lockContext(operationCtx, true) {
		finish()
		if o != nil {
			o.endInit()
		}
		return nil, operationCtx.Err()
	}
	session, ok := sessions.Get(name)
	if !ok {
		lease.Unlock()
		finish()
		if o != nil {
			o.endInit()
		}
		return nil, fmt.Errorf("mcp '%s' not available", name)
	}
	sessionCtx, releaseSession, usable := session.acquireOperation(operationCtx)
	lease.Unlock()
	if !usable {
		finish()
		if o != nil {
			o.endInit()
		}
		return nil, fmt.Errorf("mcp '%s' not available", name)
	}
	var cfg *config.ConfigStore
	var epoch uint64
	if o != nil {
		lifecycleMu.Lock()
		cfg = o.config
		epoch = o.serverEpochs[name]
		lifecycleMu.Unlock()
	}
	if len(configs) > 0 && configs[0] != nil {
		cfg = configs[0]
	}
	return newClientLease(session, sessionCtx, releaseSession, nil, finish, o, name, cfg, epoch), nil
}

func currentClientLeaseFor(ctx context.Context, name string, admission *serverAdmission) (*clientLease, error) {
	if admission == nil || !admission.valid() {
		return nil, ErrOwnerBusy
	}
	lease := serverLeaseFor(name)
	if !lease.lockContext(ctx, true) {
		return nil, ctx.Err()
	}
	if !admission.valid() {
		lease.Unlock()
		return nil, ErrOwnerBusy
	}
	session, ok := sessions.Get(name)
	if !ok {
		lease.Unlock()
		return nil, fmt.Errorf("mcp '%s' not available", name)
	}
	sessionCtx, releaseSession, usable := session.acquireOperation(ctx)
	lease.Unlock()
	if !usable {
		return nil, fmt.Errorf("mcp '%s' not available", name)
	}
	return &clientLease{
		session:    session,
		ctx:        sessionCtx,
		release:    releaseSession,
		owner:      admission.owner,
		name:       name,
		generation: admission.generation,
		epoch:      admissionEpoch(admission, admission.owner, name),
		cfg:        admission.cfg,
	}, nil
}
