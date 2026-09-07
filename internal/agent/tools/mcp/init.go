// Package mcp provides functionality for managing Model Context Protocol (MCP)
// clients within the Rush application.
package mcp

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptrace"
	"os"
	"os/exec"
	"reflect"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/PHPCraftdream/rush/internal/config"
	"github.com/PHPCraftdream/rush/internal/csync"
	"github.com/PHPCraftdream/rush/internal/home"
	"github.com/PHPCraftdream/rush/internal/permission"
	"github.com/PHPCraftdream/rush/internal/platform"
	"github.com/PHPCraftdream/rush/internal/pubsub"
	"github.com/PHPCraftdream/rush/internal/version"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func parseLevel(level mcp.LoggingLevel) slog.Level {
	switch level {
	case "info":
		return slog.LevelInfo
	case "notice":
		return slog.LevelInfo
	case "warning":
		return slog.LevelWarn
	default:
		return slog.LevelDebug
	}
}

// ClientSession wraps an mcp.ClientSession with a context cancel function so
// that the context created during session establishment is properly cleaned up
// on close.
type ClientSession struct {
	*mcp.ClientSession
	cancel         context.CancelFunc
	promote        func() bool
	terminal       func()
	cleanup        func()
	cancelOnce     sync.Once
	closeOnce      sync.Once
	closeDoneOnce  sync.Once
	closeDone      chan struct{}
	closeErr       error
	closeStateMu   sync.Mutex
	closeFinished  bool
	afterClose     []func()
	closeOwnerOnce sync.Once
	closeQueueOnce sync.Once
	closeStarted   bool
	closeName      string
	owner          *Owner

	operationMu   sync.Mutex
	operationRefs int
	retired       bool
	retireCtx     context.Context
	retireCancel  context.CancelFunc
}

// Close cancels the session context and then closes the underlying session.
func (s *ClientSession) Close() error {
	closeDone := s.closedDone()
	s.closeOnce.Do(func() {
		s.closeStateMu.Lock()
		s.closeStarted = true
		closeOwner := s.owner
		s.closeStateMu.Unlock()
		s.cancelContext()
		if s.terminal != nil {
			s.terminal()
		}
		s.closeErr = s.closeTransport()
		if s.cleanup != nil {
			s.cleanup()
		}
		s.closeStateMu.Lock()
		s.closeFinished = true
		callbacks := s.afterClose
		s.afterClose = nil
		s.closeStateMu.Unlock()
		close(closeDone)
		for _, callback := range callbacks {
			callback()
		}
		s.closeOwnerOnce.Do(func() {
			if closeOwner != nil {
				closeOwner.untrackSession(s)
			}
		})
	})
	return s.closeErr
}

func (s *ClientSession) afterCloseContext(ctx context.Context, callback func()) {
	if ctx == nil {
		ctx = context.Background()
	}
	if ctx.Err() != nil {
		return
	}
	wrapped := func() {
		if ctx.Err() == nil {
			callback()
		}
	}
	s.closeStateMu.Lock()
	if s.closeFinished {
		s.closeStateMu.Unlock()
		wrapped()
		return
	}
	s.afterClose = append(s.afterClose, wrapped)
	s.closeStateMu.Unlock()
}

func (s *ClientSession) closedDone() chan struct{} {
	s.closeDoneOnce.Do(func() {
		s.closeDone = make(chan struct{})
	})
	return s.closeDone
}

func (s *ClientSession) cancelContext() {
	s.cancelOnce.Do(func() {
		if s.cancel != nil {
			s.cancel()
		}
	})
}

func (s *ClientSession) closeTransport() error {
	if s.ClientSession == nil {
		return nil
	}
	return s.ClientSession.Close()
}

func (s *ClientSession) promoteContext() bool {
	if s.promote == nil {
		return true
	}
	return s.promote()
}

// acquireOperation pins this session generation without holding the server
// mutation lock. Retirement cancels only the operation context; the transport
// is closed after the last pinned operation releases its reference.
func (s *ClientSession) acquireOperation(ctx context.Context) (context.Context, func(), bool) {
	if ctx == nil {
		ctx = context.Background()
	}
	s.operationMu.Lock()
	defer s.operationMu.Unlock()
	if s.retired {
		return nil, nil, false
	}
	if s.retireCtx == nil {
		s.retireCtx, s.retireCancel = context.WithCancel(context.Background())
	}
	operationCtx, cancel := context.WithCancel(ctx)
	stop := context.AfterFunc(s.retireCtx, cancel)
	s.operationRefs++
	var once sync.Once
	release := func() {
		once.Do(func() {
			stop()
			cancel()
			s.releaseOperation()
		})
	}
	return operationCtx, release, true
}

func (s *ClientSession) releaseOperation() {
	s.operationMu.Lock()
	if s.operationRefs > 0 {
		s.operationRefs--
	}
	closeNow := s.retired && s.operationRefs == 0
	s.operationMu.Unlock()
	if closeNow {
		s.queueClose()
	}
}

// retire detaches a session generation from publication and queues its close
// when no operation still pins the generation.
func (s *ClientSession) retire() bool {
	s.operationMu.Lock()
	if !s.retired {
		s.retired = true
		if s.retireCancel != nil {
			s.retireCancel()
		}
	}
	closeNow := s.operationRefs == 0
	s.operationMu.Unlock()
	if closeNow {
		s.queueClose()
	}
	return closeNow
}

type closeRequest struct {
	session  *ClientSession
	name     string
	shutdown bool
}

// sessionCloser owns one close worker and a deduplicated pending set. The set
// is bounded by tracked sessions, while a blocked SDK Close cannot create more
// workers.
type sessionCloser struct {
	mu          sync.Mutex
	pending     map[*ClientSession]closeRequest
	wake        chan struct{}
	stop        chan struct{}
	beforeClose func(closeRequest)
	stopped     bool
	work        atomic.Int32
	stopOnce    sync.Once
	wg          sync.WaitGroup
	done        chan struct{}
}

func newSessionCloser() *sessionCloser {
	return &sessionCloser{
		pending: make(map[*ClientSession]closeRequest),
		wake:    make(chan struct{}, 1),
		stop:    make(chan struct{}),
		done:    make(chan struct{}),
	}
}

func (c *sessionCloser) start() {
	c.wg.Add(1)
	go c.run()
}

func (c *sessionCloser) enqueue(request closeRequest) {
	if request.session == nil {
		return
	}
	c.mu.Lock()
	if c.stopped {
		c.mu.Unlock()
		return
	}
	if _, ok := c.pending[request.session]; !ok {
		c.pending[request.session] = request
		c.work.Add(1)
	}
	c.mu.Unlock()
	select {
	case c.wake <- struct{}{}:
	default:
	}
}

func (c *sessionCloser) take() (closeRequest, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for session, request := range c.pending {
		delete(c.pending, session)
		return request, true
	}
	return closeRequest{}, false
}

func (c *sessionCloser) hasPending() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.pending) != 0
}

func (c *sessionCloser) run() {
	defer c.wg.Done()
	defer close(c.done)
	for {
		if request, ok := c.take(); ok {
			func() {
				defer c.work.Add(-1)
				c.mu.Lock()
				hook := c.beforeClose
				c.mu.Unlock()
				if hook != nil {
					hook(request)
				}
				err := request.session.Close()
				if request.shutdown {
					logMCPShutdownError(request.name, err)
				} else {
					logMCPCloseError(request.name, err)
				}
			}()
			continue
		}
		select {
		case <-c.wake:
		case <-c.stop:
			if !c.hasPending() {
				return
			}
		}
	}
}

func (c *sessionCloser) stopAndWait() {
	c.stopOnce.Do(func() {
		c.mu.Lock()
		c.stopped = true
		c.mu.Unlock()
		close(c.stop)
	})
	select {
	case c.wake <- struct{}{}:
	default:
	}
	c.wg.Wait()
}

func (s *ClientSession) queueClose() {
	s.queueCloseWithMode(false)
}

// The first queue mode wins. A retirement already queued for normal close is
// not upgraded during owner shutdown, avoiding duplicate close requests.
func (s *ClientSession) queueCloseWithMode(shutdown bool) {
	s.closeQueueOnce.Do(func() {
		s.closeStateMu.Lock()
		closeOwner := s.owner
		s.closeStateMu.Unlock()
		if closeOwner != nil {
			closeOwner.enqueueSessionClose(s, shutdown)
			return
		}
		_ = s.Close()
	})
}

var (
	sessions    = csync.NewMap[string, *ClientSession]()
	states      = csync.NewMap[string, ClientInfo]()
	stateOwners = csync.NewMap[string, *serverAdmission]()
	broker      = pubsub.NewBroker[Event]()
	leases      = newLeaseRegistry()

	lifecycleMu contextRWMutex
	owner       *Owner
	initDone    = closedChannel()
	generation  uint64
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

type leaseWaiter struct {
	write   bool
	ready   chan struct{}
	granted bool
}

// contextRWMutex is a fair, FIFO reader/writer lock with cancellation.
// Readers are granted as one batch, but never bypass an earlier writer.
type contextRWMutex struct {
	mu              sync.Mutex
	readers         int
	writer          bool
	waiters         []*leaseWaiter
	waitHook        func(bool)
	waiterAllocHook func(bool)
}

func (m *contextRWMutex) Lock() {
	_ = m.lock(context.Background(), true)
}

func (m *contextRWMutex) Unlock() {
	m.mu.Lock()
	if !m.writer {
		m.mu.Unlock()
		panic("sync: unlock of unlocked contextRWMutex")
	}
	m.writer = false
	m.grantLocked()
	m.mu.Unlock()
}

func (m *contextRWMutex) RLock() {
	_ = m.lock(context.Background(), false)
}

func (m *contextRWMutex) RUnlock() {
	m.mu.Lock()
	if m.readers == 0 {
		m.mu.Unlock()
		panic("sync: RUnlock of unlocked contextRWMutex")
	}
	m.readers--
	if m.readers == 0 {
		m.grantLocked()
	}
	m.mu.Unlock()
}

func (m *contextRWMutex) lockContext(ctx context.Context, write bool) bool {
	return m.lock(ctx, write)
}

func (m *contextRWMutex) lock(ctx context.Context, write bool) bool {
	if ctx == nil {
		ctx = context.Background()
	}
	if ctx.Err() != nil {
		return false
	}
	m.mu.Lock()
	if !m.writer && len(m.waiters) == 0 && (!write || m.readers == 0) {
		if write {
			m.writer = true
		} else {
			m.readers++
		}
		m.mu.Unlock()
		return true
	}
	waiter := &leaseWaiter{write: write, ready: make(chan struct{})}
	if m.waiterAllocHook != nil {
		m.waiterAllocHook(write)
	}
	if m.waitHook != nil {
		m.waitHook(write)
	}
	m.waiters = append(m.waiters, waiter)
	m.mu.Unlock()

	select {
	case <-waiter.ready:
		return true
	case <-ctx.Done():
		m.mu.Lock()
		if waiter.granted {
			if waiter.write {
				m.writer = false
				m.grantLocked()
			} else {
				m.readers--
				if m.readers == 0 {
					m.grantLocked()
				}
			}
			m.mu.Unlock()
			return false
		}
		for i, queued := range m.waiters {
			if queued == waiter {
				m.waiters = append(m.waiters[:i], m.waiters[i+1:]...)
				break
			}
		}
		m.grantLocked()
		m.mu.Unlock()
		return false
	}
}

func (m *contextRWMutex) grantLocked() {
	if m.writer || m.readers != 0 || len(m.waiters) == 0 {
		return
	}
	if m.waiters[0].write {
		waiter := m.waiters[0]
		m.waiters = m.waiters[1:]
		m.grantLockedWaiter(waiter)
		return
	}
	for len(m.waiters) > 0 && !m.waiters[0].write {
		waiter := m.waiters[0]
		m.waiters = m.waiters[1:]
		m.grantLockedWaiter(waiter)
	}
}

func (m *contextRWMutex) grantLockedWaiter(waiter *leaseWaiter) {
	waiter.granted = true
	if waiter.write {
		m.writer = true
	} else {
		m.readers++
	}
	close(waiter.ready)
}

// serverLease serializes replacement and closing of one server session while
// allowing concurrent callers to use the current session.
type serverLease struct {
	mu                            contextRWMutex
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
	if !l.mu.lockContext(ctx, write) {
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

// ErrOwnerBusy reports that another application currently owns the process
// wide MCP registry. The SDK deliberately permits only one application-mode
// owner; library-mode Apps do not acquire this owner.
var ErrOwnerBusy = errors.New("mcp: application owner is already active")

// ErrMCPConfigStoreBusy reports that an active owner is already bound to a
// different config store.
var ErrMCPConfigStoreBusy = errors.New("mcp: application owner is bound to a different config store")

// ErrMCPConfigUncertain reports that a committed config mutation has not yet
// been reconciled with a successful disk reload.
var ErrMCPConfigUncertain = errors.New("mcp: config mutation outcome is uncertain")

// Owner is the lifetime token for the process-wide MCP registry. The MCP
// package predates multiple App instances and its tool/state maps remain
// process-wide, so ownership is explicit rather than silently shared.
type Owner struct {
	implicit            bool
	closing             bool
	generation          uint64
	lifecycleCtx        context.Context
	lifecycleCancel     context.CancelFunc
	initCount           int
	initWG              sync.WaitGroup
	initStarted         bool
	fullInitCount       int
	initDone            chan struct{}
	serverEpochs        map[string]uint64
	serverCancels       map[string]map[uint64]serverCancel
	committedAdmissions map[string]*serverAdmission
	pendingGlobalAdds   map[string]*addTransaction
	nextCancelToken     uint64
	refreshCh           chan struct{}
	refreshPending      map[refreshKey]refreshRequest
	refreshRunning      map[refreshKey]struct{}
	config              *config.ConfigStore
	refreshWG           sync.WaitGroup
	closeOnce           sync.Once
	closeDoneOnce       sync.Once
	closeDone           chan struct{}
	refreshDone         chan struct{}
	closeErr            error
	trackedSessions     map[*ClientSession]struct{}
	trackedEmpty        chan struct{}
	closer              *sessionCloser
	fallbackWorker      *fallbackWorker
}

type serverCancel struct {
	cancel context.CancelFunc
	token  uint64
}

// trackSessionLocked keeps the owner fence alive until a published generation
// has completed its one transport close, including after detachment.
func (o *Owner) trackSessionLocked(session *ClientSession, name string) {
	if o == nil || session == nil {
		return
	}
	session.closeStateMu.Lock()
	if session.closeFinished || session.closeStarted {
		session.closeStateMu.Unlock()
		return
	}
	if o.trackedSessions == nil {
		o.trackedSessions = make(map[*ClientSession]struct{})
	}
	if _, ok := o.trackedSessions[session]; ok {
		session.closeStateMu.Unlock()
		return
	}
	if len(o.trackedSessions) == 0 {
		o.trackedEmpty = make(chan struct{})
	}
	o.trackedSessions[session] = struct{}{}
	session.owner = o
	session.closeName = name
	session.closeStateMu.Unlock()
}

func (o *Owner) untrackSession(session *ClientSession) {
	lifecycleMu.Lock()
	if _, ok := o.trackedSessions[session]; ok {
		delete(o.trackedSessions, session)
		if len(o.trackedSessions) == 0 {
			close(o.trackedEmpty)
		}
	}
	lifecycleMu.Unlock()
}

func (o *Owner) waitTrackedSessions() {
	for {
		lifecycleMu.Lock()
		if len(o.trackedSessions) == 0 {
			lifecycleMu.Unlock()
			return
		}
		done := o.trackedEmpty
		lifecycleMu.Unlock()
		<-done
	}
}

type fallbackRequest struct {
	ctx    context.Context
	cfg    *config.ConfigStore
	result config.MCPMutationResult
	owner  *Owner
}

type fallbackWorker struct {
	mu       sync.Mutex
	pending  []fallbackRequest
	wake     chan struct{}
	stop     chan struct{}
	stopped  bool
	work     atomic.Int32
	stopOnce sync.Once
	wg       sync.WaitGroup
	done     chan struct{}
}

func newFallbackWorker() *fallbackWorker {
	return &fallbackWorker{
		wake: make(chan struct{}, 1),
		stop: make(chan struct{}),
		done: make(chan struct{}),
	}
}

func (w *fallbackWorker) start() {
	w.wg.Add(1)
	go w.run()
}

func (w *fallbackWorker) enqueue(request fallbackRequest) {
	if request.owner == nil || request.ctx == nil || request.ctx.Err() != nil {
		return
	}
	w.mu.Lock()
	if w.stopped {
		w.mu.Unlock()
		return
	}
	w.pending = append(w.pending, request)
	w.work.Add(1)
	w.mu.Unlock()
	select {
	case w.wake <- struct{}{}:
	default:
	}
}

func (w *fallbackWorker) take() (fallbackRequest, bool) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if len(w.pending) == 0 {
		return fallbackRequest{}, false
	}
	request := w.pending[0]
	w.pending = w.pending[1:]
	return request, true
}

func (w *fallbackWorker) run() {
	defer w.wg.Done()
	defer close(w.done)
	for {
		if request, ok := w.take(); ok {
			func() {
				defer w.work.Add(-1)
				if request.ctx.Err() == nil {
					startFallbackMutation(request.ctx, request.cfg, request.result, request.owner)
				}
			}()
			continue
		}
		select {
		case <-w.wake:
		case <-w.stop:
			return
		}
	}
}

func (w *fallbackWorker) stopAndWait() {
	w.stopOnce.Do(func() {
		w.mu.Lock()
		w.stopped = true
		w.mu.Unlock()
		close(w.stop)
	})
	select {
	case w.wake <- struct{}{}:
	default:
	}
	w.wg.Wait()
}

func (o *Owner) enqueueSessionClose(session *ClientSession, shutdown bool) {
	if o == nil || o.closer == nil {
		return
	}
	lifecycleMu.Lock()
	if owner != o {
		lifecycleMu.Unlock()
		return
	}
	session.closeStateMu.Lock()
	name := session.closeName
	session.closeStateMu.Unlock()
	o.closer.enqueue(closeRequest{session: session, name: name, shutdown: shutdown})
	lifecycleMu.Unlock()
}

func (o *Owner) enqueueFallback(cfg *config.ConfigStore, result config.MCPMutationResult) {
	if o == nil || o.fallbackWorker == nil {
		return
	}
	lifecycleMu.Lock()
	if owner != o || o.closing {
		lifecycleMu.Unlock()
		return
	}
	o.fallbackWorker.enqueue(fallbackRequest{
		ctx:    o.lifecycleCtx,
		cfg:    cfg,
		result: result,
		owner:  o,
	})
	lifecycleMu.Unlock()
}

func adoptSessionForRetirement(name string, session *ClientSession) {
	if session == nil {
		return
	}
	lifecycleMu.Lock()
	session.closeStateMu.Lock()
	unowned := session.owner == nil
	session.closeStateMu.Unlock()
	if unowned && owner != nil && !owner.closing {
		owner.trackSessionLocked(session, name)
	}
	lifecycleMu.Unlock()
}

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

type refreshKind uint8

const (
	refreshToolsKind refreshKind = iota
	refreshPromptsKind
	refreshResourcesKind
)

type refreshKey struct {
	name           string
	kind           refreshKind
	epoch          uint64
	candidateToken uint64
}

type refreshRequest struct {
	kind      refreshKind
	name      string
	cfg       *config.ConfigStore
	admission serverAdmission
	// deferUntilCommit retains a notification received before its candidate
	// session is published.
	deferUntilCommit bool
	// candidateToken keeps a deferred notification attached to the exact
	// candidate admission. It must not be inferred from the server name.
	candidateToken uint64
	// rawEvents counts notifications deferred with this refresh. They become
	// observable only after the candidate commits successfully.
	rawEvents uint
}

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
	if owner != a.owner || a.owner.closing || a.owner.generation != a.generation ||
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
	return (a.promoted || a.ctx == nil || a.ctx.Err() == nil) && owner == a.owner &&
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
	valid := owner == a.owner && !a.owner.closing &&
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

func closedChannel() chan struct{} {
	ch := make(chan struct{})
	close(ch)
	return ch
}

// Acquire reserves the process-wide MCP registry for an application.
func Acquire() (*Owner, error) {
	return acquire(false)
}

// acquireImplicit supports the legacy package-level entry points. Unlike an
// App-owned token, an idle implicit owner may be reclaimed after its registry
// has been emptied by its caller.
func acquireImplicit() (*Owner, error) {
	return acquire(true)
}

// canReclaimLocked verifies that an implicit owner has no lifecycle work that
// could outlive the registry reset. lifecycleMu must be held by the caller.
func (o *Owner) canReclaimLocked() bool {
	if o == nil || !o.implicit || o.closing || o.initCount != 0 || o.fullInitCount != 0 {
		return false
	}
	if len(o.trackedSessions) != 0 || sessions.Len() != 0 || states.Len() != 0 ||
		allTools.Len() != 0 || allPrompts.Len() != 0 || allResources.Len() != 0 {
		return false
	}
	if len(o.serverCancels) != 0 || len(o.committedAdmissions) != 0 ||
		len(o.pendingGlobalAdds) != 0 || len(o.refreshPending) != 0 || len(o.refreshRunning) != 0 {
		return false
	}
	if o.closer != nil && o.closer.work.Load() != 0 {
		return false
	}
	return o.fallbackWorker == nil || o.fallbackWorker.work.Load() == 0
}

func acquire(implicit bool) (*Owner, error) {
	lifecycleMu.Lock()
	if owner != nil {
		oldOwner := owner
		if !oldOwner.canReclaimLocked() {
			lifecycleMu.Unlock()
			return nil, ErrOwnerBusy
		}
		oldOwner.closing = true
		oldOwner.lifecycleCancel()
		lifecycleMu.Unlock()

		oldOwner.closeOnce.Do(func() {
			oldOwner.finishClose(true)
		})
		<-oldOwner.closeDone

		lifecycleMu.Lock()
		if owner != oldOwner {
			lifecycleMu.Unlock()
			return nil, ErrOwnerBusy
		}
	}

	generation++
	lifecycleCtx, lifecycleCancel := context.WithCancel(context.Background())
	o := &Owner{
		implicit:            implicit,
		generation:          generation,
		lifecycleCtx:        lifecycleCtx,
		lifecycleCancel:     lifecycleCancel,
		closeDone:           make(chan struct{}),
		initDone:            closedChannel(),
		serverEpochs:        make(map[string]uint64),
		serverCancels:       make(map[string]map[uint64]serverCancel),
		committedAdmissions: make(map[string]*serverAdmission),
		pendingGlobalAdds:   make(map[string]*addTransaction),
		refreshCh:           make(chan struct{}, 1),
		refreshPending:      make(map[refreshKey]refreshRequest),
		refreshRunning:      make(map[refreshKey]struct{}),
		trackedSessions:     make(map[*ClientSession]struct{}),
		trackedEmpty:        closedChannel(),
		closer:              newSessionCloser(),
		fallbackWorker:      newFallbackWorker(),
		refreshDone:         make(chan struct{}),
	}
	o.closer.start()
	o.fallbackWorker.start()
	owner = o
	initDone = o.initDone
	o.refreshWG.Add(1)
	go o.refreshLoop()
	lifecycleMu.Unlock()
	return o, nil
}

func currentOwner() *Owner {
	lifecycleMu.Lock()
	defer lifecycleMu.Unlock()
	return owner
}

func (o *Owner) refreshLoop() {
	defer o.refreshWG.Done()
	defer close(o.refreshDone)
	for {
		select {
		case <-o.lifecycleCtx.Done():
			return
		case <-o.refreshCh:
			for {
				request, ok := o.nextRefresh()
				if !ok {
					break
				}
				o.runRefresh(request)
				select {
				case <-o.lifecycleCtx.Done():
					return
				default:
				}
			}
		}
	}
}

func (o *Owner) nextRefresh() (refreshRequest, bool) {
	lifecycleMu.Lock()
	defer lifecycleMu.Unlock()
	for key, request := range o.refreshPending {
		if request.deferUntilCommit {
			if !request.admission.notificationsValidLocked() {
				delete(o.refreshPending, key)
				continue
			}
			// A deferred request is activated only by the exact candidate
			// admission that owns it. Seeing a session under the same name is
			// insufficient: during same-name replacement it is the old session.
			continue
		}
		if !request.admission.validLocked() {
			delete(o.refreshPending, key)
			continue
		}
		delete(o.refreshPending, key)
		o.refreshRunning[key] = struct{}{}
		return request, true
	}
	return refreshRequest{}, false
}

func (o *Owner) enqueueRefresh(request refreshRequest) {
	lifecycleMu.Lock()
	wake := o.enqueueRefreshLocked(request)
	lifecycleMu.Unlock()
	if wake {
		o.signalRefresh()
	}
}

// enqueueRefreshLocked admits one generation of refresh work. lifecycleMu
// must be held by the caller.
func (o *Owner) enqueueRefreshLocked(request refreshRequest) bool {
	if owner != o || o.closing || request.admission.owner != o ||
		!request.admission.notificationsValidLocked() {
		return false
	}
	key := refreshKeyForRequest(request)
	if existing, exists := o.refreshPending[key]; exists {
		if request.deferUntilCommit && existing.deferUntilCommit &&
			existing.candidateToken == request.candidateToken {
			existing.rawEvents += request.rawEvents
			o.refreshPending[key] = existing
		}
		return false
	}
	// If the same key is already running, retaining one pending request marks
	// it dirty. The worker will run it once more after the in-flight snapshot
	// returns, coalescing any further notifications into that rerun.
	o.refreshPending[key] = request
	return true
}

func (o *Owner) signalRefresh() {
	select {
	case o.refreshCh <- struct{}{}:
	default:
		// The pending map is the authoritative queue. The channel is only a
		// wake-up edge, so a saturated channel does not drop a refresh.
	}
}

// activateRefreshesLocked atomically transitions deferred refreshes for one
// exact committed candidate and returns raw notifications that may now be
// published. lifecycleMu must be held by the caller.
func (o *Owner) activateRefreshesLocked(admission *serverAdmission) ([]Event, bool) {
	if admission == nil || admission.serverCancelToken == 0 {
		return nil, false
	}
	wake := false
	var events []Event
	for key, request := range o.refreshPending {
		if !request.deferUntilCommit || request.candidateToken != admission.serverCancelToken {
			continue
		}
		delete(o.refreshPending, key)
		request.deferUntilCommit = false
		request.candidateToken = 0
		request.admission.committed = admission.committed
		request.admission.committedName = admission.committedName
		request.admission.committedEpoch = admission.committedEpoch
		request.admission.mcpRevision = admission.mcpRevision
		request.admission.resolverRevision = admission.resolverRevision
		request.admission.mcpAdmission = admission.mcpAdmission
		request.admission.configIdentity = admission.configIdentity
		request.admission.hasConfigIdentity = admission.hasConfigIdentity
		request.admission.publishedSession = admission.publishedSession
		if request.admission.committedName == "" {
			request.admission.committedName = admission.name
			request.admission.committedEpoch = admission.epoch
		}
		request.admission.committed = true
		requestKey := refreshKeyForRequest(request)
		if _, exists := o.refreshPending[requestKey]; !exists {
			o.refreshPending[requestKey] = request
			wake = true
		}
		for range request.rawEvents {
			events = append(events, Event{
				Type: listChangedEventType(request.kind),
				Name: request.name,
			})
		}
	}
	return events, wake
}

func publishListChangedEvents(events []Event) {
	for _, event := range events {
		broker.Publish(pubsub.UpdatedEvent, event)
	}
}

func publishListChangedEventsOn(brokerForEvent *pubsub.Broker[Event], events []Event) {
	for _, event := range events {
		brokerForEvent.Publish(pubsub.UpdatedEvent, event)
	}
}

func (o *Owner) discardDeferredRefreshesLocked(candidateToken uint64) {
	for key, request := range o.refreshPending {
		if request.deferUntilCommit && request.candidateToken == candidateToken {
			delete(o.refreshPending, key)
		}
	}
}

func refreshKeyForRequest(request refreshRequest) refreshKey {
	epoch := request.admission.epoch
	if request.admission.committedName != "" {
		epoch = request.admission.committedEpoch
	}
	return refreshKey{
		name:           request.name,
		kind:           request.kind,
		epoch:          epoch,
		candidateToken: request.candidateToken,
	}
}

func (o *Owner) runRefresh(request refreshRequest) {
	epoch := request.admission.epoch
	if request.admission.committedName != "" {
		epoch = request.admission.committedEpoch
	}
	key := refreshKeyForRequest(request)
	defer func() {
		lifecycleMu.Lock()
		delete(o.refreshRunning, key)
		lifecycleMu.Unlock()
	}()
	if !request.admission.valid() {
		return
	}
	// Refresh owns a temporary lifecycle reference while it holds the server
	// read lease and performs the SDK list call.
	refreshAdmission, err := o.admitServer(o.lifecycleCtx, request.cfg, request.name, false)
	if err != nil || refreshAdmission.epoch != epoch {
		if err == nil {
			refreshAdmission.done()
		}
		return
	}
	defer refreshAdmission.done()
	refreshAdmission.committed = true
	refreshAdmission.committedName = request.admission.committedName
	refreshAdmission.committedEpoch = epoch
	refreshAdmission.publishedSession = request.admission.publishedSession
	refreshAdmission.configIdentity = request.admission.configIdentity
	refreshAdmission.hasConfigIdentity = request.admission.hasConfigIdentity
	refreshAdmission.epoch = epoch
	switch request.kind {
	case refreshToolsKind:
		refreshTools(o.lifecycleCtx, request.cfg, request.name, &refreshAdmission)
	case refreshPromptsKind:
		refreshPrompts(request.name, &refreshAdmission)
	case refreshResourcesKind:
		refreshResources(request.name, &refreshAdmission)
	}
}

func (o *Owner) isCurrentLocked() bool {
	return owner == o && !o.closing
}

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

// mcpReloadAfterSuccessHook is a deterministic test seam for the boundary
// between a successful disk reload and uncertainty finalization.
var mcpReloadAfterSuccessHook func()

var mcpInitTestHooks struct {
	sync.Mutex
	afterWaitGroupDone           func()
	beforeSkippedAdmission       func(string, config.MCPAdmissionSnapshot)
	beforeAdmissionTurn          func(string)
	beforeAdmissionFinalValidate func(string)
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

// withMCPAdmissionFinalTurn takes one cancelable lifecycle turn, then performs
// the final source validation immediately before the guarded mutation.
func withMCPAdmissionFinalTurn(ctx context.Context, name string, guard config.MCPAdmissionGuard, mutate func() error) error {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	runBeforeAdmissionTurn(name)
	if !lifecycleMu.lockContext(ctx, true) {
		if err := ctx.Err(); err != nil {
			return err
		}
		return context.Canceled
	}
	defer lifecycleMu.Unlock()
	runBeforeAdmissionFinalValidate(name)
	if err := guard.ValidateCurrent(); err != nil {
		return err
	}
	return mutate()
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
	if owner != o || o.closing {
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
		if owner != o || o.closing || o.committedAdmissions[candidate.name] != candidate.admission {
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

func (o *Owner) rememberConfig(cfg *config.ConfigStore) error {
	if o == nil || cfg == nil {
		return nil
	}
	lifecycleMu.Lock()
	defer lifecycleMu.Unlock()
	if o.config != nil && o.config != cfg {
		return ErrMCPConfigStoreBusy
	}
	if o.isCurrentLocked() {
		o.config = cfg
	}
	return nil
}

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
		initDone = o.initDone
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

func (o *Owner) acceptsSession() bool {
	lifecycleMu.Lock()
	defer lifecycleMu.Unlock()
	return o.isCurrentLocked()
}

// commitRenewal consumes session on every path: it either publishes the
// session or closes it before returning an error. The lease-aware helper
// below detaches the old session while the server lease is held and leaves
// retirement and transport closes to the caller after that lease is released.
func (o *Owner) commitRenewal(admission *serverAdmission, name string, session *ClientSession, counts Counts) error {
	retired, err := o.commitRenewalForLease(admission, name, session, counts)
	if err != nil && retired != nil {
		retired.cancelContext()
	}
	retireMCPClient(name, retired)
	return err
}

func (o *Owner) commitRenewalForLease(admission *serverAdmission, name string, session *ClientSession, counts Counts) (*ClientSession, error) {
	if admission == nil || admission.cfg == nil {
		return session, ErrOwnerBusy
	}
	snapshot := admission.mcpAdmission
	if snapshot.Config == nil {
		snapshot = admission.cfg.SnapshotMCPAdmission(name)
	}
	var (
		oldSession    *ClientSession
		hadOldSession bool
		pendingEvents []Event
		wakeRefresh   bool
	)
	err := admission.cfg.WithCurrentMCPAdmission(snapshot, name, func(guard config.MCPAdmissionGuard) error {
		return withMCPAdmissionFinalTurn(admission.ctx, name, guard, func() error {
			if admission.owner != o || admission.name != name || !admission.validLocked() {
				if admission.candidate && admission.configSnapshotStaleLocked() {
					return config.ErrMCPMutationStale
				}
				return ErrOwnerBusy
			}
			if !session.promoteContext() {
				return ErrOwnerBusy
			}
			oldSession, hadOldSession = sessions.Get(name)
			o.trackSessionLocked(session, name)
			sessions.Set(name, session)
			admission.committed = true
			admission.committedName = name
			admission.committedEpoch = o.serverEpochs[name]
			admission.publishedSession = session
			if current, ok := admission.cfg.MCPConfig(name); ok {
				admission.configIdentity = current
				admission.hasConfigIdentity = true
			}
			if current := admission.cfg.SnapshotMCPAdmission(name); current.Exists {
				admission.mcpRevision = current.MCPRevision
				admission.resolverRevision = current.ResolverRevision
				admission.mcpAdmission = current
			}
			o.committedAdmissions[name] = admission
			setState(name, StateConnected, nil, session, counts)
			pendingEvents, wakeRefresh = o.activateRefreshesLocked(admission)
			return nil
		})
	})
	if err != nil {
		return session, err
	}
	if wakeRefresh {
		o.signalRefresh()
	}
	if !hadOldSession || oldSession == session {
		oldSession = nil
	}
	// The old session is detached by sessions.Set above. Its retirement and
	// transport close are performed by the caller after the server lease is
	// released.
	publishStateEvent(name, StateConnected, nil, counts)
	publishListChangedEvents(pendingEvents)
	return oldSession, nil
}

func (o *Owner) acceptsGeneration(generation uint64) bool {
	lifecycleMu.Lock()
	defer lifecycleMu.Unlock()
	return owner == o && !o.closing && o.generation == generation
}

func (o *Owner) operationContext(ctx context.Context) (context.Context, func()) {
	operationCtx, cancel := context.WithCancel(ctx)
	stop := context.AfterFunc(o.lifecycleCtx, cancel)
	return operationCtx, func() {
		stop()
		cancel()
	}
}

// Close starts shutdown and waits until it finishes or ctx expires. If ctx
// expires, the process-wide owner remains fenced in closing state and rejects
// Acquire until its single cleanup goroutine has joined every admitted
// initialization or renewal and closed every session. A non-cooperative
// session close can therefore retain the fence indefinitely; releasing it
// would allow callbacks from that old session to mutate the next owner's
// process-wide registry.
func (o *Owner) Close(ctx context.Context) error {
	o.closeOnce.Do(func() {
		lifecycleMu.Lock()
		if owner != o {
			lifecycleMu.Unlock()
			o.closeDoneOnce.Do(func() { close(o.closeDone) })
			return
		}
		o.closing = true
		o.lifecycleCancel()
		lifecycleMu.Unlock()

		go o.finishClose(false)
	})

	select {
	case <-o.closeDone:
		return o.closeErr
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (o *Owner) finishClose(reclaim bool) {
	if o.fallbackWorker != nil {
		o.fallbackWorker.stopAndWait()
	}
	// Do not abandon this wait when a caller's cleanup context expires. The
	// owner remains the lifecycle fence until every admitted operation exits.
	o.initWG.Wait()
	// The notification worker is independently joined so no callback from
	// this owner can publish into a later owner after the registry is reset.
	o.refreshWG.Wait()

	type namedSession struct {
		name    string
		session *ClientSession
	}
	snapshotBySession := make(map[*ClientSession]string)
	lifecycleMu.Lock()
	for name, session := range sessions.Seq2() {
		session.closeStateMu.Lock()
		unowned := session.owner == nil
		session.closeStateMu.Unlock()
		if unowned {
			o.trackSessionLocked(session, name)
		}
		snapshotBySession[session] = name
	}
	for session := range o.trackedSessions {
		if _, ok := snapshotBySession[session]; !ok {
			snapshotBySession[session] = ""
		}
	}
	lifecycleMu.Unlock()
	snapshot := make([]namedSession, 0, len(snapshotBySession))
	for session, name := range snapshotBySession {
		snapshot = append(snapshot, namedSession{name: name, session: session})
	}

	// Cancel every transport before entering any potentially non-cooperative
	// SDK Close. The owner closer then runs those closes sequentially.
	for _, item := range snapshot {
		item.session.cancelContext()
	}
	for _, item := range snapshot {
		item.session.queueCloseWithMode(true)
	}
	o.waitTrackedSessions()
	if o.closer != nil {
		o.closer.stopAndWait()
	}

	lifecycleMu.Lock()
	if owner == o {
		clear(o.committedAdmissions)
		resetRegistryLocked()
		if !reclaim {
			owner = nil
			initDone = closedChannel()
		}
		o.closeInitBarrierLocked()
	}
	o.closeDoneOnce.Do(func() { close(o.closeDone) })
	lifecycleMu.Unlock()
}

func resetRegistryLocked() {
	for name := range sessions.Seq2() {
		sessions.Del(name)
	}
	for name := range states.Seq2() {
		states.Del(name)
	}
	for name := range stateOwners.Seq2() {
		stateOwners.Del(name)
	}
	for name := range allTools.Seq2() {
		allTools.Del(name)
	}
	for name := range allPrompts.Seq2() {
		allPrompts.Del(name)
	}
	for name := range allResources.Seq2() {
		allResources.Del(name)
	}
	leases.reset()
	broker.Shutdown()
	broker = pubsub.NewBroker[Event]()
}

// State represents the current state of an MCP client
type State int

const (
	StateDisabled State = iota
	StateStarting
	StateConnected
	StateError
)

func (s State) String() string {
	switch s {
	case StateDisabled:
		return "disabled"
	case StateStarting:
		return "starting"
	case StateConnected:
		return "connected"
	case StateError:
		return "error"
	default:
		return "unknown"
	}
}

// EventType represents the type of MCP event
type EventType uint

const (
	EventStateChanged EventType = iota
	EventToolsListChanged
	EventPromptsListChanged
	EventResourcesListChanged
)

// Event represents an event in the MCP system
type Event struct {
	Type   EventType
	Name   string
	State  State
	Error  error
	Counts Counts
}

// Counts number of available tools, prompts, etc.
type Counts struct {
	Tools     int
	Prompts   int
	Resources int
}

// ClientInfo holds information about an MCP client's state
type ClientInfo struct {
	Name        string
	State       State
	Error       error
	Client      *ClientSession
	Counts      Counts
	ConnectedAt time.Time
}

// SubscribeEvents returns a channel for MCP events
func SubscribeEvents(ctx context.Context) <-chan pubsub.Event[Event] {
	return currentBroker().Subscribe(ctx)
}

func currentBroker() *pubsub.Broker[Event] {
	lifecycleMu.Lock()
	defer lifecycleMu.Unlock()
	return broker
}

func publishEvent(t pubsub.EventType, event Event) {
	currentBroker().Publish(t, event)
}

// GetStates returns the current state of all MCP clients
func GetStates() map[string]ClientInfo {
	return states.Copy()
}

// IsConfigured reports whether name belongs to the consuming config store.
// MCP runtime state is process-wide, so callers that serve more than one
// ConfigStore must apply this ownership check before exposing registry data.
// A nil store preserves the legacy package-level behavior used by internal
// test fixtures that do not have a consuming configuration.
func IsConfigured(cfg *config.ConfigStore, name string) bool {
	if cfg == nil {
		return true
	}
	_, ok := cfg.MCPConfig(name)
	return ok
}

// GetState returns the state of a specific MCP client
func GetState(name string) (ClientInfo, bool) {
	return states.Get(name)
}

// Close closes all MCP clients. This should be called during application shutdown.
func Close(ctx context.Context) error {
	o := currentOwner()
	if o == nil {
		return nil
	}
	return o.Close(ctx)
}

// Initialize initializes MCP clients based on the provided configuration.
//
// restrictToCLIEnabled, when true, additionally skips every server whose
// config does not set EnabledInCLI — set by internal/app.New's
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
	slog.Info("Initializing MCP clients")
	// The permission service is consumed later while tools are called. Keep it
	// in the signature for compatibility with the existing startup contract.
	_ = permissions
	var wg sync.WaitGroup
	initCtx, cancel := context.WithCancel(ctx)
	lifecycleMu.Lock()
	if owner != o || o.closing {
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
	finish := func() {}
	if admission != nil {
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
		err = cfg.WithCurrentMCPAdmission(snapshot, name, func(guard config.MCPAdmissionGuard) error {
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

// DisableSingle disables and closes a single MCP client by name.
func DisableSingle(cfg *config.ConfigStore, name string) error {
	o, err := ensureOwner()
	if err != nil {
		return err
	}
	if err := o.rememberConfig(cfg); err != nil {
		return err
	}
	if err := o.reconcileUncertainty(context.Background(), cfg, name); err != nil {
		return err
	}
	if !o.beginInit() {
		return ErrOwnerBusy
	}
	defer o.endInit()
	lease := serverLeaseFor(name)
	lease.Lock()
	o.invalidateServer(name)
	oldSession := detachSessionLocked(name)
	clearAdvertised(name)
	updateState(name, StateDisabled, nil, nil, Counts{})
	lease.Unlock()
	retireMCPClient(name, oldSession)

	slog.Info("Disabled mcp client", "name", name)
	return nil
}

// DisableServer disables an MCP server: closes its session, removes its tools,
// and persists the disabled flag to config.
func DisableServer(ctx context.Context, cfg *config.ConfigStore, name string) error {
	return disableServerWithResultPersistence(ctx, cfg, name,
		func(cfg *config.ConfigStore, scope config.Scope, name string, pending *config.MCPConfig) (config.MCPMutationResult, error) {
			if pending != nil {
				return cfg.PersistMCPConfigResult(scope, name, *pending)
			}
			return cfg.PersistMCPDisabledOverrideResult(scope, name, true)
		})
}

// disableServerPersister performs exactly one durable mutation. pending is the
// complete disabled definition for an in-flight Add, or nil when an existing
// durable definition only needs a disabled field overlay.
type disableServerPersister func(*config.ConfigStore, config.Scope, string, *config.MCPConfig) error

type disableServerResultPersister func(*config.ConfigStore, config.Scope, string, *config.MCPConfig) (config.MCPMutationResult, error)

func disableServerWithPersistence(
	ctx context.Context,
	cfg *config.ConfigStore,
	name string,
	persist disableServerPersister,
) error {
	return disableServerWithResultPersistence(ctx, cfg, name, func(cfg *config.ConfigStore, scope config.Scope, name string, pending *config.MCPConfig) (config.MCPMutationResult, error) {
		if err := persist(cfg, scope, name, pending); err != nil {
			if outcome, ok := config.CommitOutcomeFromError(err); ok && commitOutcomeIsReconciled(outcome) {
				return currentMCPMutationResult(cfg, "disable", name, name), err
			}
			return config.MCPMutationResult{}, err
		}
		return currentMCPMutationResult(cfg, "disable", name, name), nil
	})
}

func disableServerWithResultPersistence(
	ctx context.Context,
	cfg *config.ConfigStore,
	name string,
	persist disableServerResultPersister,
) error {
	o, err := ensureOwner()
	if err != nil {
		return err
	}
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
	mcpCfg, ok := cfg.MCPConfig(name)
	if !ok {
		return fmt.Errorf("MCP server %q not found: %w", name, config.ErrMCPNotFound)
	}
	lease := serverLeaseFor(name)
	if !lease.lockContext(ctx, true) {
		return ctx.Err()
	}
	var detached *ClientSession
	unlock := func() {
		lease.Unlock()
		retireMCPClient(name, detached)
	}
	mcpCfg, ok = cfg.MCPConfig(name)
	if !ok {
		unlock()
		return fmt.Errorf("MCP server %q disappeared while disabling: %w", name, config.ErrMCPNotFound)
	}
	if o.isUncertain(cfg, name) {
		unlock()
		return ErrMCPConfigUncertain
	}
	if !o.acceptsSession() {
		unlock()
		return ErrOwnerBusy
	}
	// Resolve the origin while holding the ordered server lease. A user
	// workspace definition must stay in the workspace file, while project,
	// system, external, and otherwise unrepresentable definitions fail closed.
	scope, err := resolveMCPMutationScope(cfg, name, mcpCfg)
	if err != nil {
		unlock()
		return fmt.Errorf("cannot determine writable scope for MCP server %q: %w", name, err)
	}
	// Persist first. If disk persistence fails, the session and in-memory
	// snapshot remain enabled, so the operation has no half-applied result.
	transaction := o.pendingGlobalAdd(name, cfg)
	var pending *config.MCPConfig
	if transaction != nil {
		fullConfig := mcpCfg
		fullConfig.Disabled = true
		pending = &fullConfig
	}
	if !o.acceptsSession() {
		unlock()
		return ErrOwnerBusy
	}
	result, err := persist(cfg, scope, name, pending)
	if err != nil {
		outcome, hasOutcome := config.CommitOutcomeFromError(err)
		if hasOutcome && commitOutcomeIsReconciled(outcome) {
			if !result.NewExists {
				unlock()
				return fmt.Errorf("failed to persist MCP disabled state for %q: %w", name, err)
			}
			if transaction != nil {
				transaction.markUserMutation()
			}
			o.invalidateServer(name)
			detached = detachSessionLocked(name)
			clearAdvertised(name)
			updateState(name, StateDisabled, nil, nil, Counts{})
			unlock()
			return fmt.Errorf("failed to persist MCP disabled state for %q: %w", name, err)
		}
		if hasOutcome && commitOutcomeNeedsRuntimeFence(outcome) {
			if transaction != nil {
				transaction.markUserMutation()
			}
			detached = fenceMCPRuntimeLocked(o, cfg, name)
			unlock()
			return fmt.Errorf("failed to persist MCP disabled state for %q: %w", name, err)
		}
		unlock()
		return fmt.Errorf("failed to persist MCP disabled state for %q: %w", name, err)
	}
	if !result.NewExists {
		unlock()
		return fmt.Errorf("MCP server %q disappeared while disabling: %w", name, config.ErrMCPNotFound)
	}
	if transaction != nil {
		transaction.markUserMutation()
	}
	// Persistence is the fallible part of this transaction. Only after it
	// succeeds may this operation invalidate candidates; the write lease keeps
	// a candidate from publishing between these steps and the runtime update.
	o.invalidateServer(name)
	detached = detachSessionLocked(name)
	clearAdvertised(name)
	updateState(name, StateDisabled, nil, nil, Counts{})
	unlock()
	return nil
}

// EnableServer re-enables a disabled MCP server and starts a new session.
func EnableServer(ctx context.Context, cfg *config.ConfigStore, name string) error {
	return enableServerWithPersistence(ctx, cfg, name, func(cfg *config.ConfigStore, scope config.Scope, name string, pending *config.MCPConfig) (config.MCPMutationResult, error) {
		return enableMCPConfig(cfg, scope, name, pending)
	})
}

type enableServerResultPersister func(*config.ConfigStore, config.Scope, string, *config.MCPConfig) (config.MCPMutationResult, error)

func enableServerWithPersistence(ctx context.Context, cfg *config.ConfigStore, name string, persist enableServerResultPersister) error {
	return enableServerWithPersistenceAndInitializer(ctx, cfg, name, persist, initClientAdmitted)
}

func enableServerWithPersistenceAndInitializer(
	ctx context.Context,
	cfg *config.ConfigStore,
	name string,
	persist enableServerResultPersister,
	initialize admittedClientInitializer,
) error {
	return enableServerWithPersistenceAndInitializerAndRollback(ctx, cfg, name, persist, initialize, nil)
}

func enableServerWithPersistenceAndInitializerAndRollback(
	ctx context.Context,
	cfg *config.ConfigStore,
	name string,
	persist enableServerResultPersister,
	initialize admittedClientInitializer,
	rollbackPersist enableServerResultPersister,
) error {
	o, err := ensureOwner()
	if err != nil {
		return err
	}
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
	mcpCfg, ok := cfg.MCPConfig(name)
	if !ok {
		return fmt.Errorf("MCP server %q not found: %w", name, config.ErrMCPNotFound)
	}
	lease := serverLeaseFor(name)
	if !lease.lockContext(ctx, true) {
		return ctx.Err()
	}
	mcpCfg, ok = cfg.MCPConfig(name)
	if !ok {
		lease.Unlock()
		return fmt.Errorf("MCP server %q disappeared while enabling", name)
	}
	if o.isUncertain(cfg, name) {
		lease.Unlock()
		return ErrMCPConfigUncertain
	}
	scope, err := resolveMCPMutationScope(cfg, name, mcpCfg)
	if err != nil {
		lease.Unlock()
		return fmt.Errorf("cannot determine writable scope for MCP server %q: %w", name, err)
	}
	transaction := o.pendingGlobalAdd(name, cfg)
	var pending *config.MCPConfig
	if transaction != nil {
		fullConfig := mcpCfg
		fullConfig.Disabled = false
		pending = &fullConfig
	}
	result, err := persist(cfg, scope, name, pending)
	var commitUncertainty error
	if err != nil {
		outcome, ok := config.CommitOutcomeFromError(err)
		if !ok || (!outcome.Committed && !outcome.MaybeCommitted) {
			lease.Unlock()
			return fmt.Errorf("failed to persist MCP enabled state for %q: %w", name, err)
		}
		if transaction != nil {
			transaction.markUserMutation()
		}
		if commitOutcomeNeedsRuntimeFence(outcome) {
			detached := fenceMCPRuntimeLocked(o, cfg, name)
			lease.Unlock()
			retireMCPClient(name, detached)
			return fmt.Errorf("failed to persist MCP enabled state for %q: %w", name, err)
		}
		commitUncertainty = err
	}
	if transaction != nil {
		transaction.markUserMutation()
	}
	type rollbackResult struct {
		err        error
		needsFence bool
	}
	rollbackPersistence := func() rollbackResult {
		var rollbackErr error
		if rollbackPersist != nil {
			var rollback *config.MCPConfig
			if transaction != nil {
				rollbackConfig := mcpCfg
				rollbackConfig.Disabled = true
				rollback = &rollbackConfig
			}
			_, rollbackErr = rollbackPersist(cfg, scope, name, rollback)
		} else {
			_, rollbackErr = cfg.PersistMCPEnableRollbackResult(scope, name, result)
		}
		if rollbackErr == nil {
			return rollbackResult{}
		}
		outcome, hasOutcome := config.CommitOutcomeFromError(rollbackErr)
		knownMismatch := errors.Is(rollbackErr, config.ErrMCPMutationStale)
		return rollbackResult{err: rollbackErr, needsFence: knownMismatch || hasOutcome && commitOutcomeNeedsRuntimeFence(outcome)}
	}
	rollbackAndReport := func(cause error) (*ClientSession, error) {
		rollback := rollbackPersistence()
		if rollback.err == nil {
			return nil, cause
		}
		if rollback.needsFence {
			detached := fenceMCPRuntimeLocked(o, cfg, name)
			return detached, errors.Join(cause, ErrMCPConfigUncertain,
				fmt.Errorf("failed to roll back MCP enabled state for %q: %w", name, rollback.err))
		}
		return nil, errors.Join(cause,
			fmt.Errorf("failed to roll back MCP enabled state for %q: %w", name, rollback.err))
	}
	if !o.acceptsSession() {
		if commitUncertainty == nil {
			detached, rollbackErr := rollbackAndReport(ErrOwnerBusy)
			lease.Unlock()
			retireMCPClient(name, detached)
			return rollbackErr
		} else {
			detached := fenceMCPRuntimeLocked(o, cfg, name)
			lease.Unlock()
			retireMCPClient(name, detached)
			if commitUncertainty != nil {
				return fmt.Errorf("failed to persist MCP enabled state for %q: %w", name, commitUncertainty)
			}
			return ErrOwnerBusy
		}
	}
	// The new admission below is the only initializer allowed to publish this
	// epoch. A failed persistence round-trip therefore leaves the old epoch
	// untouched and the operation has no half-applied lifecycle result.
	admissionSnapshot := cfg.SnapshotMCPAdmission(name)
	resolver := admissionSnapshot.Resolver
	if result.Generation != 0 && admissionSnapshot.Generation != result.Generation {
		detached, rollbackErr := rollbackAndReport(config.ErrMCPMutationStale)
		lease.Unlock()
		retireMCPClient(name, detached)
		return rollbackErr
	}
	o.invalidateServer(name)
	if !result.NewExists {
		if commitUncertainty != nil {
			detached := fenceMCPRuntimeLocked(o, cfg, name)
			lease.Unlock()
			retireMCPClient(name, detached)
			return fmt.Errorf("failed to persist MCP enabled state for %q: %w", name, commitUncertainty)
		}
		detached, rollbackErr := rollbackAndReport(fmt.Errorf("MCP server %q disappeared while enabling: %w", name, config.ErrMCPNotFound))
		lease.Unlock()
		retireMCPClient(name, detached)
		return rollbackErr
	}
	admission, err := o.admitServerForConfig(ctx, cfg, name, result.NewConfig, true)
	if err != nil {
		if commitUncertainty != nil {
			detached := fenceMCPRuntimeLocked(o, cfg, name)
			lease.Unlock()
			retireMCPClient(name, detached)
			return fmt.Errorf("failed to persist MCP enabled state for %q: %w", name, commitUncertainty)
		}
		detached, rollbackErr := rollbackAndReport(err)
		lease.Unlock()
		retireMCPClient(name, detached)
		return rollbackErr
	}
	admission.candidate = true
	admission.mcpRevision = admissionSnapshot.MCPRevision
	admission.resolverRevision = admissionSnapshot.ResolverRevision
	admission.mcpAdmission = admissionSnapshot
	admission.mutationResult = &result
	if !admission.valid() {
		detached, rollbackErr := rollbackAndReport(config.ErrMCPMutationStale)
		lease.Unlock()
		retireMCPClient(name, detached)
		return rollbackErr
	}
	updateAdmissionState(&admission, StateStarting, nil, nil, Counts{})
	lease.Unlock()
	go func() {
		if err := initialize(ctx, cfg, name, result.NewConfig, resolver, &admission); err != nil {
			cleanupFailedAdmission(&admission, err)
			slog.Error("Failed to enable MCP server", "name", name, "err", err)
		}
	}()
	if commitUncertainty != nil {
		return fmt.Errorf("failed to persist MCP enabled state for %q: %w", name, commitUncertainty)
	}
	return nil
}

func enableMCPConfig(cfg *config.ConfigStore, scope config.Scope, name string, pending *config.MCPConfig) (config.MCPMutationResult, error) {
	if pending != nil {
		return cfg.PersistMCPConfigResult(scope, name, *pending)
	}
	return cfg.PersistMCPDisabledOverrideResult(scope, name, false)
}

// AddServer validates and adds a new MCP server. It attempts to connect; if
// successful the server is added to the in-memory config and persisted to disk.
func AddServer(ctx context.Context, cfg *config.ConfigStore, name string, mcpCfg config.MCPConfig) error {
	return addServerWithPreparationAndPersistence(ctx, cfg, name, mcpCfg, prepareClient,
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
	return replaceServerWithResultPersistenceAndPreparation(ctx, cfg, oldName, newName, mcpCfg, persist, prepareClient)
}

type preparedClientFunc func(context.Context, *config.ConfigStore, string, config.MCPConfig, config.VariableResolver, *serverAdmission) (*preparedClient, error)

func addServerWithPreparationAndPersistence(
	ctx context.Context,
	cfg *config.ConfigStore,
	name string,
	mcpCfg config.MCPConfig,
	prepare preparedClientFunc,
	persist addServerResultPersister,
) error {
	return addServerWithInitializerAndPersistence(ctx, cfg, name, mcpCfg,
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
	if mcpCfg.Source == config.MCPSourceExternal {
		return fmt.Errorf("MCP server %q is from .mcp.json and cannot be replaced: %w", oldName, config.ErrMCPExternal)
	}
	o, err := ensureOwner()
	if err != nil {
		return err
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
			owner == o && !o.closing && o.generation == admission.generation &&
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

// resolveMCPMutationScope resolves the writable origin for a disable/enable
// operation. External definitions are intentionally represented by a
// workspace-only disabled overlay; all other definitions must be owned by an
// exact writable config scope.
func resolveMCPMutationScope(cfg *config.ConfigStore, name string, mcpCfg config.MCPConfig) (config.Scope, error) {
	if mcpCfg.Source == config.MCPSourceExternal {
		if !cfg.HasWorkspaceConfig() {
			return config.ScopeWorkspace, config.ErrNoWorkspaceConfig
		}
		return config.ScopeWorkspace, nil
	}
	scope, err := cfg.ResolveMCPWritableScope(name)
	if err == nil {
		return scope, nil
	}
	// AddServer has a deliberate global-scope contract, but its definition is
	// not on disk until initialization finishes. Preserve the cancellation
	// path for a concurrent disable/remove of that in-flight add; an ordinary
	// unowned in-memory definition remains fail-closed below.
	if (errors.Is(err, config.ErrMCPUnwritableOrigin) || errors.Is(err, config.ErrMCPNotFound)) && hasPendingGlobalAddFor(cfg, name) {
		return config.ScopeGlobal, nil
	}
	return config.ScopeGlobal, err
}

func hasPendingGlobalAdd(name string) bool {
	return hasPendingGlobalAddFor(nil, name)
}

func hasPendingGlobalAddFor(cfg *config.ConfigStore, name string) bool {
	lifecycleMu.Lock()
	defer lifecycleMu.Unlock()
	currentOwner := owner
	if currentOwner == nil {
		return false
	}
	transaction := currentOwner.pendingGlobalAdds[name]
	return transaction != nil && transaction.token != 0 && (cfg == nil || transaction.cfg == cfg)
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
	if owner != o || o.generation != admission.generation ||
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

// RemoveServer removes an MCP server, closes its session, and removes it from config.
// External servers (from .mcp.json) cannot be removed — only disabled.
func RemoveServer(cfg *config.ConfigStore, name string) error {
	return removeServerWithResultPersistence(cfg, name,
		func(cfg *config.ConfigStore, scope config.Scope, name string) (config.MCPMutationResult, error) {
			if pending := pendingGlobalAddFor(cfg, name); pending != nil {
				return cfg.PersistRemovePendingMCPConfigResult(scope, name)
			}
			return cfg.PersistRemoveMCPConfigResult(scope, name)
		})
}

type removeServerPersister func(*config.ConfigStore, string) error
type scopedRemoveServerPersister func(*config.ConfigStore, config.Scope, string) error
type removeServerResultPersister func(*config.ConfigStore, config.Scope, string) (config.MCPMutationResult, error)

func removeServerWithPersistence(
	cfg *config.ConfigStore,
	name string,
	persist removeServerPersister,
) error {
	return removeServerWithScopedPersistence(cfg, name,
		func(cfg *config.ConfigStore, _ config.Scope, name string) error {
			return persist(cfg, name)
		})
}

func removeServerWithScopedPersistence(
	cfg *config.ConfigStore,
	name string,
	persist scopedRemoveServerPersister,
) error {
	return removeServerWithResultPersistence(cfg, name, func(cfg *config.ConfigStore, scope config.Scope, name string) (config.MCPMutationResult, error) {
		if err := persist(cfg, scope, name); err != nil {
			if outcome, ok := config.CommitOutcomeFromError(err); ok && commitOutcomeIsReconciled(outcome) {
				return currentMCPMutationResult(cfg, "remove", name, name), err
			}
			return config.MCPMutationResult{}, err
		}
		return currentMCPMutationResult(cfg, "remove", name, name), nil
	})
}

func removeServerWithResultPersistence(
	cfg *config.ConfigStore,
	name string,
	persist removeServerResultPersister,
) error {
	o, err := ensureOwner()
	if err != nil {
		return err
	}
	if err := o.rememberConfig(cfg); err != nil {
		return err
	}
	if err := o.reconcileUncertainty(context.Background(), cfg, name); err != nil {
		return err
	}
	if !o.beginInit() {
		return ErrOwnerBusy
	}
	defer o.endInit()
	mcpCfg, exists := cfg.MCPConfig(name)
	if !exists {
		return fmt.Errorf("MCP server %q not found: %w", name, config.ErrMCPNotFound)
	}
	if mcpCfg.Source == config.MCPSourceExternal {
		return fmt.Errorf("MCP server %q is from .mcp.json and cannot be removed (disable it instead): %w", name, config.ErrMCPExternal)
	}

	lease := serverLeaseFor(name)
	lease.Lock()
	var detached *ClientSession
	unlock := func() {
		lease.Unlock()
		retireMCPClient(name, detached)
	}
	mcpCfg, exists = cfg.MCPConfig(name)
	if o.isUncertain(cfg, name) {
		unlock()
		return ErrMCPConfigUncertain
	}
	if !o.acceptsSession() {
		unlock()
		return ErrOwnerBusy
	}
	if !exists {
		unlock()
		return fmt.Errorf("MCP server %q disappeared while removing: %w", name, config.ErrMCPNotFound)
	}
	if mcpCfg.Source == config.MCPSourceExternal {
		unlock()
		return fmt.Errorf("MCP server %q is from .mcp.json and cannot be removed (disable it instead): %w", name, config.ErrMCPExternal)
	}
	if transaction := o.pendingGlobalAdd(name, cfg); transaction != nil {
		if !o.acceptsSession() {
			unlock()
			return ErrOwnerBusy
		}
		result, persistErr := persist(cfg, config.ScopeGlobal, name)
		var commitUncertainty error
		if persistErr != nil {
			outcome, ok := config.CommitOutcomeFromError(persistErr)
			if !ok || (!outcome.Committed && !outcome.MaybeCommitted) {
				unlock()
				return fmt.Errorf("failed to remove pending MCP server %q from config: %w", name, persistErr)
			}
			transaction.markUserMutation()
			if commitOutcomeNeedsRuntimeFence(outcome) {
				detached = fenceMCPRuntimeLocked(o, cfg, name)
				_, _ = cfg.RemoveMCP(name)
				unlock()
				return fmt.Errorf("failed to remove pending MCP server %q from config: %w", name, persistErr)
			}
			commitUncertainty = persistErr
		}
		if result.NewExists {
			unlock()
			return fmt.Errorf("MCP server %q remained configured after removal: %w", name, config.ErrMCPTargetExists)
		}
		transaction.markUserMutation()
		o.invalidateServer(name)
		_, _ = cfg.RemoveMCP(name)
		detached = detachSessionLocked(name)
		clearAdvertised(name)
		states.Del(name)
		// The delete is part of this server's lease linearization point. It
		// must reach subscribers before the lease is released to a same-name
		// Add, and before the detached session can block in Close.
		publishEvent(pubsub.DeletedEvent, Event{Type: EventStateChanged, Name: name, State: StateDisabled})
		unlock()
		if commitUncertainty != nil {
			return fmt.Errorf("failed to remove pending MCP server %q from config: %w", name, commitUncertainty)
		}
		return nil
	}
	scope, err := resolveMCPMutationScope(cfg, name, mcpCfg)
	if err != nil {
		unlock()
		return fmt.Errorf("cannot determine writable scope for MCP server %q: %w", name, err)
	}
	if !o.acceptsSession() {
		unlock()
		return ErrOwnerBusy
	}
	result, err := persist(cfg, scope, name)
	var commitUncertainty error
	if err != nil {
		outcome, ok := config.CommitOutcomeFromError(err)
		if !ok || (!outcome.Committed && !outcome.MaybeCommitted) {
			unlock()
			return fmt.Errorf("failed to remove MCP server %q from config: %w", name, err)
		}
		if commitOutcomeNeedsRuntimeFence(outcome) {
			detached = fenceMCPRuntimeLocked(o, cfg, name)
			unlock()
			return fmt.Errorf("failed to remove MCP server %q from config: %w", name, err)
		}
		commitUncertainty = err
	}
	// Persistence is the fallible part of this transaction. Only after it
	// succeeds may this operation invalidate candidates; the write lease keeps
	// a candidate from publishing until runtime state is removed.
	o.invalidateServer(name)
	detached = detachSessionLocked(name)
	clearAdvertised(name)
	states.Del(name)
	if result.NewExists {
		if result.NewConfig.Disabled {
			updateState(name, StateDisabled, nil, nil, Counts{})
			unlock()
			if commitUncertainty != nil {
				return fmt.Errorf("failed to remove MCP server %q from config: %w", name, commitUncertainty)
			}
			return nil
		}
		publishEvent(pubsub.DeletedEvent, Event{Type: EventStateChanged, Name: name, State: StateDisabled})
		lease.Unlock()
		startFallbackAfterRetirement(name, detached, cfg, result, o)
		if commitUncertainty != nil {
			return fmt.Errorf("failed to remove MCP server %q from config: %w", name, commitUncertainty)
		}
		return nil
	}
	publishEvent(pubsub.DeletedEvent, Event{Type: EventStateChanged, Name: name, State: StateDisabled})
	unlock()
	if commitUncertainty != nil {
		return fmt.Errorf("failed to remove MCP server %q from config: %w", name, commitUncertainty)
	}
	return nil
}

func ensureOwner() (*Owner, error) {
	if o := currentOwner(); o != nil {
		return o, nil
	}
	return acquireImplicit()
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

// detachSessionLocked removes the published session while the caller holds
// the server write lease. It takes lifecycleMu before touching owner state.
func detachSessionLocked(name string) *ClientSession {
	lifecycleMu.Lock()
	defer lifecycleMu.Unlock()
	return detachSessionLifecycleLocked(name)
}

// detachSessionLifecycleLocked is the lifecycle-locked detach primitive. The
// caller holds the server lease and lifecycleMu, in that order.
func detachSessionLifecycleLocked(name string) *ClientSession {
	if o := owner; o != nil {
		delete(o.committedAdmissions, name)
	}
	stateOwners.Del(name)
	if session, ok := sessions.Get(name); ok {
		sessions.Del(name)
		return session
	}
	return nil
}

func closeMCPClient(name string, session *ClientSession) {
	if session == nil {
		return
	}
	if err := session.Close(); err != nil {
		logMCPCloseError(name, err)
	}
}

func logMCPCloseError(name string, err error) {
	if err != nil && !errors.Is(err, io.EOF) &&
		!errors.Is(err, context.Canceled) && err.Error() != "signal: killed" {
		slog.Warn("Error closing MCP session", "name", name, "error", err)
	}
}

func logMCPShutdownError(name string, err error) {
	if err != nil && !errors.Is(err, io.EOF) &&
		!errors.Is(err, context.Canceled) && err.Error() != "signal: killed" {
		slog.Warn("Failed to shutdown MCP client", "name", name, "error", err)
	}
}

func retireMCPClient(name string, session *ClientSession) {
	if session != nil {
		adoptSessionForRetirement(name, session)
		session.retire()
	}
}

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

func commitOutcomeNeedsRuntimeFence(outcome *config.CommitOutcome) bool {
	return outcome != nil && (outcome.MaybeCommitted || (outcome.Committed && !outcome.Reconciled))
}

func commitOutcomeIsReconciled(outcome *config.CommitOutcome) bool {
	return outcome != nil && !outcome.MaybeCommitted && outcome.Committed && outcome.Reconciled
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

func failClosedMCP(ctx context.Context, name string) error {
	return failClosedMCPWithState(ctx, name, StateDisabled, nil)
}

func failClosedInitializeMCP(ctx context.Context, name string, cause error) {
	if cause == nil || ctx == nil || ctx.Err() != nil {
		return
	}
	_ = failClosedMCPWithState(ctx, name, StateError, cause)
}

func failClosedMCPWithState(ctx context.Context, name string, state State, stateErr error) error {
	if ctx == nil {
		return errors.New("mcp: nil context")
	}
	lease := serverLeaseFor(name)
	if !lease.lockContext(ctx, true) {
		if err := ctx.Err(); err != nil {
			return err
		}
		return ErrOwnerBusy
	}
	var cancels []context.CancelFunc
	var detached *ClientSession
	lifecycleMu.Lock()
	if current := owner; current != nil {
		cancels = current.invalidateServerLocked(name)
	}
	detached = detachSessionLifecycleLocked(name)
	clearAdvertised(name)
	setState(name, state, stateErr, nil, Counts{})
	lifecycleMu.Unlock()
	lease.Unlock()
	for _, cancel := range cancels {
		cancel()
	}
	retireMCPClient(name, detached)
	publishStateEvent(name, state, stateErr, Counts{})
	return nil
}

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

type skippedMCPFinalization struct {
	detached *ClientSession
	canceled []context.CancelFunc
	event    Event
	accepted bool
}

func (f skippedMCPFinalization) finish() {
	if !f.accepted {
		return
	}
	for _, cancel := range f.canceled {
		cancel()
	}
	retireMCPClient(f.event.Name, f.detached)
	publishEvent(pubsub.UpdatedEvent, f.event)
}

func transitionSkippedMCP(
	ctx context.Context,
	o *Owner,
	cfg *config.ConfigStore,
	name string,
	snapshot config.MCPAdmissionSnapshot,
) (skippedMCPFinalization, error) {
	finalization := skippedMCPFinalization{}
	var canceled []context.CancelFunc
	var detached *ClientSession
	mcpInitTestHooks.Lock()
	hook := mcpInitTestHooks.beforeSkippedAdmission
	mcpInitTestHooks.Unlock()
	if hook != nil {
		hook(name, snapshot)
	}
	err := cfg.WithCurrentMCPAdmission(snapshot, name, func(guard config.MCPAdmissionGuard) error {
		return withMCPAdmissionFinalTurn(ctx, name, guard, func() error {
			if !o.isCurrentLocked() {
				return ErrOwnerBusy
			}
			if _, uncertain := cfg.MCPUncertaintyVersion(name); uncertain {
				return config.ErrMCPMutationStale
			}
			canceled = o.invalidateServerLocked(name)
			detached = detachSessionLifecycleLocked(name)
			clearAdvertised(name)
			setState(name, StateDisabled, nil, nil, Counts{})
			return nil
		})
	})
	if err != nil {
		return finalization, err
	}
	finalization.detached = detached
	finalization.canceled = canceled
	finalization.event = Event{Type: EventStateChanged, Name: name, State: StateDisabled}
	finalization.accepted = true
	return finalization, nil
}

func createSession(ctx context.Context, name string, m config.MCPConfig, resolver config.VariableResolver) (*ClientSession, error) {
	return createSessionWithAdmission(ctx, name, m, resolver, nil)
}

// sessionContext is a handoff context for a candidate session. Admission and
// owner cancellation can abort initialization, but once the candidate is
// published admission completion must not cancel the SDK connection context.
type sessionContext struct {
	owner           context.Context
	candidate       context.Context
	caller          context.Context
	done            chan struct{}
	workerDone      chan struct{}
	candidateStop   func() bool
	candidateCancel context.CancelFunc

	mu       sync.Mutex
	promoted bool
	err      error
	closed   bool
}

func newSessionContext(owner, candidate context.Context) *sessionContext {
	return newSessionContextWithCleanup(owner, candidate, nil, nil)
}

func newSessionContextWithCleanup(owner, candidate context.Context, candidateCancel context.CancelFunc, candidateStop func() bool) *sessionContext {
	return newSessionContextWithCaller(owner, candidate, nil, candidateCancel, candidateStop)
}

func newSessionContextWithCaller(
	owner, candidate, caller context.Context,
	candidateCancel context.CancelFunc,
	candidateStop func() bool,
) *sessionContext {
	s := &sessionContext{
		owner:           owner,
		candidate:       candidate,
		caller:          caller,
		done:            make(chan struct{}),
		workerDone:      make(chan struct{}),
		candidateCancel: candidateCancel,
		candidateStop:   candidateStop,
	}
	go func() {
		defer close(s.workerDone)
		select {
		case <-owner.Done():
			s.finish(owner, false)
		case <-candidate.Done():
			if s.finish(candidate, true) {
				return
			}
			select {
			case <-owner.Done():
				s.finish(owner, false)
			case <-s.done:
			}
		case <-s.done:
		}
	}()
	return s
}

func (s *sessionContext) finish(source context.Context, candidate bool) bool {
	s.mu.Lock()
	if s.closed || (candidate && s.promoted) {
		s.mu.Unlock()
		return false
	}
	s.err = source.Err()
	s.closed = true
	close(s.done)
	cancel := s.candidateCancel
	stop := s.candidateStop
	s.candidateCancel = nil
	s.candidateStop = nil
	s.mu.Unlock()
	if stop != nil {
		stop()
	}
	if cancel != nil {
		cancel()
	}
	return true
}

func (s *sessionContext) promote() bool {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return false
	}
	// Promotion is the handoff linearization point. Check every context that
	// can cancel the candidate while holding the same mutex that selects the
	// winner over the cancellation worker. The caller check is essential:
	// canceling a caller schedules candidate cancellation asynchronously, so
	// checking only candidate.Err() permits a late publish.
	for _, ctx := range []context.Context{s.owner, s.candidate, s.caller} {
		if ctx == nil || ctx.Err() == nil {
			continue
		}
		stop, cancel := s.rejectLocked(ctx.Err())
		s.mu.Unlock()
		stopCandidate(stop, cancel)
		return false
	}
	s.promoted = true
	stop := s.candidateStop
	s.candidateStop = nil
	s.candidateCancel = nil
	s.mu.Unlock()
	if stop != nil {
		stop()
	}
	return true
}

func (s *sessionContext) rejectLocked(err error) (func() bool, context.CancelFunc) {
	s.err = err
	s.closed = true
	close(s.done)
	stop := s.candidateStop
	cancel := s.candidateCancel
	s.candidateStop = nil
	s.candidateCancel = nil
	return stop, cancel
}

func stopCandidate(stop func() bool, cancel context.CancelFunc) {
	if stop != nil {
		stop()
	}
	if cancel != nil {
		cancel()
	}
}

func (s *sessionContext) abort() {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return
	}
	stop, cancel := s.rejectLocked(context.Canceled)
	s.mu.Unlock()
	stopCandidate(stop, cancel)
}

func (s *sessionContext) Deadline() (time.Time, bool) { return s.owner.Deadline() }

func (s *sessionContext) Done() <-chan struct{} { return s.done }

func (s *sessionContext) Err() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.err
}

func (s *sessionContext) Value(key any) any { return s.owner.Value(key) }

func createSessionWithAdmission(ctx context.Context, name string, m config.MCPConfig, resolver config.VariableResolver, admission *serverAdmission) (*ClientSession, error) {
	timeout := mcpTimeout(m)
	var handoff *sessionContext
	sessionCtx := ctx
	if admission != nil && admission.owner != nil {
		candidateCtx := admission.ctx
		var candidateCancel context.CancelFunc
		var candidateStop func() bool
		if ctx != nil {
			candidateCtx, candidateCancel = context.WithCancel(admission.ctx)
			candidateStop = context.AfterFunc(ctx, candidateCancel)
		}
		handoff = newSessionContextWithCaller(
			admission.owner.lifecycleCtx,
			candidateCtx,
			ctx,
			candidateCancel,
			candidateStop,
		)
		sessionCtx = handoff
	}
	lifetimeCtx, cancelSession := context.WithCancelCause(sessionCtx)
	timeoutCause := errors.New("mcp initialization timeout")
	timerDone := make(chan struct{})
	cancelTimer := time.AfterFunc(timeout, func() {
		cancelSession(timeoutCause)
		close(timerDone)
	})
	var stopTimerOnce sync.Once
	stopInitTimer := func() {
		stopTimerOnce.Do(func() {
			if !cancelTimer.Stop() {
				<-timerDone
			}
		})
	}
	cancelFailedSession := func() {
		stopInitTimer()
		cancelSession(context.Canceled)
	}

	transport, err := createTransport(lifetimeCtx, m, resolver)
	if err != nil {
		if admission == nil || (!admission.suppressState && admission.valid()) {
			if admission == nil {
				updateState(name, StateError, err, nil, Counts{})
			} else {
				updateAdmissionState(admission, StateError, err, nil, Counts{})
			}
		}
		slog.Error("Error creating MCP client", "error", err, "name", name)
		cancelFailedSession()
		return nil, err
	}

	client := mcp.NewClient(
		&mcp.Implementation{
			Name:    "rush",
			Version: version.Version,
			Title:   "Rush",
		},
		&mcp.ClientOptions{
			ToolListChangedHandler: func(context.Context, *mcp.ToolListChangedRequest) {
				notifyListChanged(admission, name, refreshToolsKind)
			},
			PromptListChangedHandler: func(context.Context, *mcp.PromptListChangedRequest) {
				notifyListChanged(admission, name, refreshPromptsKind)
			},
			ResourceListChangedHandler: func(context.Context, *mcp.ResourceListChangedRequest) {
				notifyListChanged(admission, name, refreshResourcesKind)
			},
			LoggingMessageHandler: func(ctx context.Context, req *mcp.LoggingMessageRequest) {
				level := parseLevel(req.Params.Level)
				slog.Log(ctx, level, "MCP log", "name", name, "logger", req.Params.Logger, "data", req.Params.Data)
			},
		},
	)

	session, err := client.Connect(lifetimeCtx, transport, nil)
	stopInitTimer()
	timeoutErrCause := context.Cause(lifetimeCtx)
	if err == nil && lifetimeCtx.Err() != nil {
		err = lifetimeCtx.Err()
		_ = session.Close()
	}
	if err != nil {
		err = maybeStdioErr(err, transport)
		err = maybeTimeoutErr(err, timeout, timeoutErrCause, timeoutCause)
		if admission == nil || (!admission.suppressState && admission.valid()) {
			if admission == nil {
				updateState(name, StateError, err, nil, Counts{})
			} else {
				updateAdmissionState(admission, StateError, err, nil, Counts{})
			}
		}
		slog.Error("MCP client failed to initialize", "error", err, "name", name)
		cancelSession(context.Canceled)
		return nil, err
	}

	// Keep the context live for the published SDK connection. The caller's
	// operation context is intentionally not its lifetime context.
	slog.Debug("MCP client initialized", "name", name)
	cleanup := transportCleanup(transport)
	return &ClientSession{
		ClientSession: session,
		cancel: func() {
			stopInitTimer()
			cancelSession(context.Canceled)
		},
		promote: func() bool {
			if handoff != nil {
				return handoff.promote()
			}
			return true
		},
		terminal: func() {
			if handoff != nil {
				handoff.abort()
			}
		},
		cleanup: cleanup,
	}, nil
}

func transportCleanup(transport mcp.Transport) func() {
	var roundTripper http.RoundTripper
	switch transport := transport.(type) {
	case *mcp.StreamableClientTransport:
		if transport.HTTPClient != nil {
			roundTripper = transport.HTTPClient.Transport
		}
	case *mcp.SSEClientTransport:
		if transport.HTTPClient != nil {
			roundTripper = transport.HTTPClient.Transport
		}
	}
	owned, ok := roundTripper.(interface{ CloseIdleConnections() })
	if !ok {
		return nil
	}
	return owned.CloseIdleConnections
}

func notifyListChanged(admission *serverAdmission, name string, kind refreshKind) {
	eventType := listChangedEventType(kind)
	if admission == nil || admission.owner == nil {
		lifecycleMu.Lock()
		broker.Publish(pubsub.UpdatedEvent, Event{Type: eventType, Name: name})
		lifecycleMu.Unlock()
		return
	}

	// Admission, queueing, and the raw notification form one lifecycle
	// transition. A delete or disable that wins the lifecycle lock first will
	// invalidate the generation and suppress both the refresh and its event.
	lifecycleMu.Lock()
	if !admission.notificationsValidLocked() {
		committed := admission.committed
		lifecycleMu.Unlock()
		if committed {
			fenceInvalidCommittedNotification(admission)
		}
		return
	}
	request := refreshRequest{
		kind:      kind,
		name:      name,
		cfg:       admission.cfg,
		admission: *admission,
	}
	if !admission.committed && admission.candidate && admission.serverCancelToken != 0 {
		// Any uncommitted admission is a candidate, including InitializeSingle
		// when it is replacing an already published same-name session. Its
		// notification must remain attached to this exact token until commit.
		request.deferUntilCommit = true
		request.candidateToken = admission.serverCancelToken
		request.rawEvents = 1
	}
	wake := admission.owner.enqueueRefreshLocked(request)
	if request.deferUntilCommit {
		lifecycleMu.Unlock()
		if wake {
			admission.owner.signalRefresh()
		}
		return
	}
	if !wake {
		// A duplicate pending/running request is still a valid notification;
		// only generation admission decides whether its raw event is published.
		if owner != admission.owner || admission.owner.closing ||
			!admission.notificationsValidLocked() {
			lifecycleMu.Unlock()
			return
		}
	}
	broker.Publish(pubsub.UpdatedEvent, Event{Type: eventType, Name: name})
	lifecycleMu.Unlock()
	if wake {
		admission.owner.signalRefresh()
	}
}

func fenceInvalidCommittedNotification(admission *serverAdmission) {
	if admission == nil || admission.owner == nil {
		return
	}
	name := admission.committedName
	if name == "" {
		name = admission.name
	}
	lease := serverLeaseFor(name)
	lease.Lock()
	lifecycleMu.Lock()
	current, exists := sessions.Get(name)
	registered := admission.owner.committedAdmissions[name] == admission ||
		(admission.owner.committedAdmissions[name] == nil && current == admission.publishedSession)
	if registered && exists && current == admission.publishedSession && !admission.committedValidLocked() {
		cancels := admission.owner.invalidateServerLocked(name)
		detached := detachSessionLifecycleLocked(name)
		clearAdvertised(name)
		setState(name, StateDisabled, nil, nil, Counts{})
		lifecycleMu.Unlock()
		lease.Unlock()
		for _, cancel := range cancels {
			cancel()
		}
		retireMCPClient(name, detached)
		publishStateEvent(name, StateDisabled, nil, Counts{})
		return
	}
	lifecycleMu.Unlock()
	lease.Unlock()
}

// detachInvalidCommittedSessionLocked removes only the exact published
// session represented by admission. The caller holds the server lease and
// lifecycleMu, in that order.
func (o *Owner) detachInvalidCommittedSessionLocked(name string, session *ClientSession, cfg *config.ConfigStore) ([]context.CancelFunc, *ClientSession, bool) {
	if o == nil || session == nil || cfg == nil {
		return nil, nil, false
	}
	admission := o.committedAdmissions[name]
	if admission == nil || admission.cfg != cfg || admission.publishedSession != session || admission.committedValidLocked() {
		return nil, nil, false
	}
	cancels := o.invalidateServerLocked(name)
	detached := detachSessionLifecycleLocked(name)
	clearAdvertised(name)
	setState(name, StateDisabled, nil, nil, Counts{})
	return cancels, detached, true
}

func listChangedEventType(kind refreshKind) EventType {
	switch kind {
	case refreshPromptsKind:
		return EventPromptsListChanged
	case refreshResourcesKind:
		return EventResourcesListChanged
	default:
		return EventToolsListChanged
	}
}

// maybeStdioErr if a stdio mcp prints an error in non-json format, it'll fail
// to parse, and the cli will then close it, causing the EOF error.
// so, if we got an EOF err, and the transport is STDIO, we try to exec it
// again with a timeout and collect the output so we can add details to the
// error.
// this happens particularly when starting things with npx, e.g. if node can't
// be found or some other error like that.
func maybeStdioErr(err error, transport mcp.Transport) error {
	if !errors.Is(err, io.EOF) {
		return err
	}
	ct, ok := transport.(*mcp.CommandTransport)
	if !ok {
		return err
	}
	if err2 := stdioCheck(ct.Command); err2 != nil {
		err = errors.Join(err, err2)
	}
	return err
}

func maybeTimeoutErr(err error, timeout time.Duration, cause, timeoutCause error) error {
	if cause == timeoutCause {
		return fmt.Errorf("timed out after %s", timeout)
	}
	return err
}

func createTransport(ctx context.Context, m config.MCPConfig, resolver config.VariableResolver) (mcp.Transport, error) {
	switch m.Type {
	case config.MCPStdio:
		command, err := resolver.ResolveValue(m.Command)
		if err != nil {
			return nil, fmt.Errorf("invalid mcp command: %w", err)
		}
		if strings.TrimSpace(command) == "" {
			return nil, fmt.Errorf("mcp stdio config requires a non-empty 'command' field")
		}
		args, err := m.ResolvedArgs(resolver)
		if err != nil {
			return nil, err
		}
		envs, err := m.ResolvedEnv(resolver)
		if err != nil {
			return nil, err
		}
		cmd := platform.Command(ctx, home.Long(command), args...)
		cmd.Env = append(os.Environ(), envs...)
		// Run the child in its own process group and kill the whole group when
		// the session context is cancelled. A stdio server often spawns its own
		// children (signal-mcp launches signal-cli); os/exec's default
		// cancellation kills only the direct child, orphaning the rest with
		// PPID 1 — production accumulated 15+ such zombies over two days.
		configureStdioProcess(cmd)
		return &mcp.CommandTransport{
			Command: cmd,
		}, nil
	case config.MCPHttp:
		url, err := m.ResolvedURL(resolver)
		if err != nil {
			return nil, err
		}
		if strings.TrimSpace(url) == "" {
			return nil, fmt.Errorf("mcp http config requires a non-empty 'url' field")
		}
		headers, err := m.ResolvedHeaders(resolver)
		if err != nil {
			return nil, err
		}
		transport, err := cloneHTTPTransport()
		if err != nil {
			return nil, err
		}
		client := &http.Client{
			Transport: &headerRoundTripper{
				headers:   headers,
				ctx:       ctx,
				transport: transport,
			},
		}
		return &mcp.StreamableClientTransport{
			Endpoint:   url,
			HTTPClient: client,
		}, nil
	case config.MCPSSE:
		url, err := m.ResolvedURL(resolver)
		if err != nil {
			return nil, err
		}
		if strings.TrimSpace(url) == "" {
			return nil, fmt.Errorf("mcp sse config requires a non-empty 'url' field")
		}
		headers, err := m.ResolvedHeaders(resolver)
		if err != nil {
			return nil, err
		}
		transport, err := cloneHTTPTransport()
		if err != nil {
			return nil, err
		}
		client := &http.Client{
			Transport: &headerRoundTripper{
				headers:   headers,
				ctx:       ctx,
				transport: transport,
			},
		}
		return &mcp.SSEClientTransport{
			Endpoint:   url,
			HTTPClient: client,
		}, nil
	default:
		return nil, fmt.Errorf("unsupported mcp type: %s", m.Type)
	}
}

type headerRoundTripper struct {
	headers   map[string]string
	ctx       context.Context
	transport *http.Transport
}

func cloneHTTPTransport() (*http.Transport, error) {
	if base, ok := http.DefaultTransport.(*http.Transport); ok {
		return base.Clone(), nil
	}
	dialer := &net.Dialer{
		Timeout:   30 * time.Second,
		KeepAlive: 30 * time.Second,
	}
	return &http.Transport{
		Proxy:                 http.ProxyFromEnvironment,
		DialContext:           dialer.DialContext,
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          100,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ExpectContinueTimeout: time.Second,
	}, nil
}

func (rt *headerRoundTripper) CloseIdleConnections() {
	if rt.transport != nil {
		rt.transport.CloseIdleConnections()
	}
}

type ownerResponseBody struct {
	io.ReadCloser
	stop   func() bool
	cancel context.CancelFunc
	once   sync.Once
}

func (b *ownerResponseBody) release() {
	b.once.Do(func() {
		b.stop()
		b.cancel()
	})
}

func (b *ownerResponseBody) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	if err != nil {
		b.release()
	}
	return n, err
}

func (b *ownerResponseBody) Close() error {
	b.release()
	return b.ReadCloser.Close()
}

func (rt *headerRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	if rt.transport == nil {
		return nil, errors.New("mcp http transport is not initialized")
	}
	for k, v := range rt.headers {
		req.Header.Set(k, v)
	}
	if rt.ctx != nil {
		var connMu sync.Mutex
		var requestConn net.Conn
		canceled := false
		ctx := httptrace.WithClientTrace(req.Context(), &httptrace.ClientTrace{
			GotConn: func(info httptrace.GotConnInfo) {
				connMu.Lock()
				defer connMu.Unlock()
				requestConn = info.Conn
				if canceled {
					_ = info.Conn.Close()
				}
			},
		})
		ctx, cancel := context.WithCancel(ctx)
		req = req.WithContext(ctx)
		legacyCancel := make(chan struct{})
		req.Cancel = legacyCancel
		var cancelOnce sync.Once
		cancelRequest := func() {
			cancelOnce.Do(func() {
				connMu.Lock()
				canceled = true
				if requestConn != nil {
					_ = requestConn.Close()
				}
				connMu.Unlock()
				close(legacyCancel)
				cancel()
				// The MCP SDK deliberately detaches its connection context from
				// Connect's context. CancelRequest is retained here as an explicit
				// transport fence for an HTTP request that is blocked before it has
				// produced a response body; context cancellation alone is not
				// sufficient for every Windows net/http transport path.
				rt.transport.CancelRequest(req)
			})
		}
		stop := context.AfterFunc(rt.ctx, cancelRequest)
		resp, err := rt.transport.RoundTrip(req)
		if err != nil {
			stop()
			cancelRequest()
			return nil, err
		}
		if resp.Body == nil {
			stop()
			cancelRequest()
			return resp, nil
		}
		resp.Body = &ownerResponseBody{
			ReadCloser: resp.Body,
			stop:       stop,
			cancel:     cancel,
		}
		return resp, nil
	}
	return rt.transport.RoundTrip(req)
}

func mcpTimeout(m config.MCPConfig) time.Duration {
	return time.Duration(cmp.Or(m.Timeout, 15)) * time.Second
}

func stdioCheck(old *exec.Cmd) error {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second*5)
	defer cancel()
	cmd := platform.Command(ctx, old.Path, old.Args...)
	cmd.Env = old.Env
	out, err := cmd.CombinedOutput()
	if err == nil || errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return nil
	}
	return fmt.Errorf("%w: %s", err, string(out))
}
