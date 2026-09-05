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
	"slices"
	"strings"
	"sync"
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
	cancel     context.CancelFunc
	promote    func() bool
	cleanup    func()
	cancelOnce sync.Once
	closeOnce  sync.Once
	closeErr   error
}

// Close cancels the session context and then closes the underlying session.
func (s *ClientSession) Close() error {
	s.closeOnce.Do(func() {
		s.cancelContext()
		s.closeErr = s.closeTransport()
		if s.cleanup != nil {
			s.cleanup()
		}
	})
	return s.closeErr
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

var (
	sessions = csync.NewMap[string, *ClientSession]()
	states   = csync.NewMap[string, ClientInfo]()
	broker   = pubsub.NewBroker[Event]()
	leases   = newLeaseRegistry()

	lifecycleMu sync.Mutex
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

// serverLease serializes replacement and closing of one server session while
// allowing concurrent callers to use the current session.
type serverLease struct {
	mu       sync.RWMutex
	registry *leaseRegistry
	name     string
	refs     int
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
	l.mu.Lock()
}

func (l *serverLease) Unlock() {
	l.mu.Unlock()
	l.registry.release(l)
}

func (l *serverLease) RLock() {
	l.mu.RLock()
}

func (l *serverLease) RUnlock() {
	l.mu.RUnlock()
	l.registry.release(l)
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
	ordered := slices.Clone(names)
	slices.Sort(ordered)
	locked := make([]*serverLease, 0, len(ordered))
	for _, name := range ordered {
		if len(locked) > 0 && locked[len(locked)-1].name == name {
			continue
		}
		lease := serverLeaseFor(name)
		lease.Lock()
		locked = append(locked, lease)
	}
	return locked
}

func unlockServerLeases(leases []*serverLease) {
	for i := len(leases) - 1; i >= 0; i-- {
		leases[i].Unlock()
	}
}

// clientLease keeps the server read lock and owner initialization fence until
// the caller has finished its actual MCP operation.
type clientLease struct {
	session *ClientSession
	ctx     context.Context
	release func()
	once    sync.Once
}

func (l *clientLease) close() {
	l.once.Do(l.release)
}

func serverLeaseFor(name string) *serverLease {
	return leases.getRetained(name)
}

// ErrOwnerBusy reports that another application currently owns the process
// wide MCP registry. The SDK deliberately permits only one application-mode
// owner; library-mode Apps do not acquire this owner.
var ErrOwnerBusy = errors.New("mcp: application owner is already active")

// Owner is the lifetime token for the process-wide MCP registry. The MCP
// package predates multiple App instances and its tool/state maps remain
// process-wide, so ownership is explicit rather than silently shared.
type Owner struct {
	implicit        bool
	closing         bool
	generation      uint64
	lifecycleCtx    context.Context
	lifecycleCancel context.CancelFunc
	initCount       int
	initWG          sync.WaitGroup
	initStarted     bool
	fullInitCount   int
	initDone        chan struct{}
	serverEpochs    map[string]uint64
	serverCancels   map[string]map[uint64]serverCancel
	nextCancelToken uint64
	refreshCh       chan struct{}
	refreshPending  map[refreshKey]refreshRequest
	refreshRunning  map[refreshKey]struct{}
	refreshWG       sync.WaitGroup
	closeOnce       sync.Once
	closeDone       chan struct{}
	closeErr        error
}

type serverCancel struct {
	cancel context.CancelFunc
	token  uint64
}

type refreshKind uint8

const (
	refreshToolsKind refreshKind = iota
	refreshPromptsKind
	refreshResourcesKind
)

type refreshKey struct {
	name  string
	kind  refreshKind
	epoch uint64
}

type refreshRequest struct {
	kind      refreshKind
	name      string
	cfg       *config.ConfigStore
	admission serverAdmission
}

// serverAdmission pins one owner generation and one server epoch. Its init
// reference is held until the candidate or operation has completely exited,
// allowing Owner.Close to fence all late work before registry reset.
type serverAdmission struct {
	owner               *Owner
	generation          uint64
	epoch               uint64
	cfg                 *config.ConfigStore
	name                string
	ctx                 context.Context
	cancel              context.CancelFunc
	stop                func()
	once                *sync.Once
	serverCancelToken   uint64
	committed           bool
	promoted            bool
	suppressState       bool
	suppressUntilCommit bool
	committedName       string
	committedEpoch      uint64
}

func (a *serverAdmission) done() {
	if a == nil || a.owner == nil {
		return
	}
	if a.once != nil {
		a.once.Do(a.owner.endInit)
	}
	if a.stop != nil {
		a.stop()
	}
	lifecycleMu.Lock()
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
	valid := (!a.suppressUntilCommit || a.committed) && a.validLocked()
	lifecycleMu.Unlock()
	return valid
}

func (a *serverAdmission) validLocked() bool {
	if a == nil || a.owner == nil || a.cfg == nil {
		return true
	}
	if a.ctx != nil && a.ctx.Err() != nil && !a.committed && !a.promoted {
		return false
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
	mcpConfig, exists := a.cfg.MCPConfig(admissionName)
	if !exists {
		return false
	}
	return !mcpConfig.Disabled
}

func (o *Owner) admitServer(ctx context.Context, cfg *config.ConfigStore, name string, bump bool) (serverAdmission, error) {
	lifecycleMu.Lock()
	defer lifecycleMu.Unlock()
	if !o.isCurrentLocked() {
		return serverAdmission{}, ErrOwnerBusy
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
	admission := serverAdmission{
		owner:             o,
		generation:        o.generation,
		epoch:             o.serverEpochs[name],
		cfg:               cfg,
		name:              name,
		once:              new(sync.Once),
		ctx:               operationCtx,
		cancel:            cancel,
		stop:              stop,
		serverCancelToken: serverCancelToken,
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
	lifecycleMu.Lock()
	defer lifecycleMu.Unlock()
	if !o.isCurrentLocked() {
		return serverAdmission{}, ErrOwnerBusy
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
		name:              name,
		once:              new(sync.Once),
		ctx:               operationCtx,
		cancel:            cancel,
		stop:              func() { _ = stopFunc() },
		serverCancelToken: token,
	}
	o.initCount++
	o.initWG.Add(1)
	return admission, nil
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
	return serverAdmission{
		owner:      o,
		generation: o.generation,
		epoch:      o.serverEpochs[name],
		cfg:        cfg,
		name:       name,
		ctx:        o.lifecycleCtx,
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

func acquire(implicit bool) (*Owner, error) {
	lifecycleMu.Lock()
	if owner != nil {
		if !owner.implicit || owner.closing || owner.initCount != 0 || sessions.Len() != 0 ||
			states.Len() != 0 || allTools.Len() != 0 || allPrompts.Len() != 0 ||
			allResources.Len() != 0 {
			lifecycleMu.Unlock()
			return nil, ErrOwnerBusy
		}
		owner.lifecycleCancel()
		oldOwner := owner
		lifecycleMu.Unlock()
		oldOwner.refreshWG.Wait()
		lifecycleMu.Lock()
		if owner != oldOwner {
			lifecycleMu.Unlock()
			return nil, ErrOwnerBusy
		}
		resetRegistryLocked()
	}

	generation++
	lifecycleCtx, lifecycleCancel := context.WithCancel(context.Background())
	o := &Owner{
		implicit:        implicit,
		generation:      generation,
		lifecycleCtx:    lifecycleCtx,
		lifecycleCancel: lifecycleCancel,
		closeDone:       make(chan struct{}),
		initDone:        closedChannel(),
		serverEpochs:    make(map[string]uint64),
		serverCancels:   make(map[string]map[uint64]serverCancel),
		refreshCh:       make(chan struct{}, 1),
		refreshPending:  make(map[refreshKey]refreshRequest),
		refreshRunning:  make(map[refreshKey]struct{}),
	}
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
		delete(o.refreshPending, key)
		o.refreshRunning[key] = struct{}{}
		return request, true
	}
	return refreshRequest{}, false
}

func (o *Owner) enqueueRefresh(request refreshRequest) {
	lifecycleMu.Lock()
	if owner != o || o.closing || request.admission.owner != o ||
		!request.admission.validLocked() {
		lifecycleMu.Unlock()
		return
	}
	epoch := request.admission.epoch
	if request.admission.committedName != "" {
		epoch = request.admission.committedEpoch
	}
	key := refreshKey{name: request.name, kind: request.kind, epoch: epoch}
	if _, exists := o.refreshPending[key]; exists {
		lifecycleMu.Unlock()
		return
	}
	// If the same key is already running, retaining one pending request marks
	// it dirty. The worker will run it once more after the in-flight snapshot
	// returns, coalescing any further notifications into that rerun.
	o.refreshPending[key] = request
	lifecycleMu.Unlock()

	select {
	case o.refreshCh <- struct{}{}:
	default:
		// The pending map is the authoritative queue. The channel is only a
		// wake-up edge, so a saturated channel does not drop a refresh.
	}
}

func (o *Owner) runRefresh(request refreshRequest) {
	epoch := request.admission.epoch
	if request.admission.committedName != "" {
		epoch = request.admission.committedEpoch
	}
	key := refreshKey{name: request.name, kind: request.kind, epoch: epoch}
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
// session or closes it before returning an error.
func (o *Owner) commitRenewal(admission *serverAdmission, name string, session *ClientSession, counts Counts) error {
	lifecycleMu.Lock()
	if admission == nil || admission.owner != o || admission.name != name || !admission.validLocked() {
		lifecycleMu.Unlock()
		_ = session.Close()
		return ErrOwnerBusy
	}
	if !session.promoteContext() {
		lifecycleMu.Unlock()
		_ = session.Close()
		return ErrOwnerBusy
	}
	sessions.Set(name, session)
	admission.committed = true
	setState(name, StateConnected, nil, session, counts)
	lifecycleMu.Unlock()
	publishStateEvent(name, StateConnected, nil, counts)
	return nil
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
			close(o.closeDone)
			return
		}
		o.closing = true
		o.lifecycleCancel()
		lifecycleMu.Unlock()

		go o.finishClose()
	})

	select {
	case <-o.closeDone:
		return o.closeErr
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (o *Owner) finishClose() {
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
	var snapshot []namedSession
	for name, session := range sessions.Seq2() {
		snapshot = append(snapshot, namedSession{name: name, session: session})
	}

	// Cancel every transport before entering any potentially non-cooperative
	// SDK Close. Close calls then run sequentially in this goroutine so a
	// deadline-abandoned App cleanup retains no extra waiter or close fan-out
	// goroutines beyond this one owner cleanup goroutine.
	for _, item := range snapshot {
		item.session.cancelContext()
	}
	for _, item := range snapshot {
		if err := item.session.Close(); err != nil &&
			!errors.Is(err, io.EOF) &&
			!errors.Is(err, context.Canceled) &&
			err.Error() != "signal: killed" {
			slog.Warn("Failed to shutdown MCP client", "name", item.name, "error", err)
		}
	}

	lifecycleMu.Lock()
	if owner == o {
		resetRegistryLocked()
		owner = nil
		initDone = closedChannel()
		o.closeInitBarrierLocked()
	}
	close(o.closeDone)
	lifecycleMu.Unlock()
}

func resetRegistryLocked() {
	for name := range sessions.Seq2() {
		sessions.Del(name)
	}
	for name := range states.Seq2() {
		states.Del(name)
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
	lifecycleMu.Unlock()
	if !o.beginInitialize() {
		cancel()
		return
	}
	defer o.endInit()
	stopOwner := context.AfterFunc(o.lifecycleCtx, cancel)
	defer stopOwner()
	defer cancel()
	// Capture the configuration and each server admission before starting any
	// goroutine. This prevents a later owner from being captured by a stale
	// initializer and lets disable/remove cancel the exact startup attempt.
	configured, _ := cfg.Snapshot()
	if configured == nil {
		o.finishInitialize()
		return
	}
	resolver := cfg.Resolver()
	for name, m := range configured.MCP {
		if !o.acceptsSession() {
			break
		}
		if m.Disabled {
			o.invalidateServer(name)
			updateState(name, StateDisabled, nil, nil, Counts{})
			slog.Debug("Skipping disabled MCP", "name", name)
			continue
		}
		if restrictToCLIEnabled && !m.EnabledInCLI {
			updateState(name, StateDisabled, nil, nil, Counts{})
			slog.Debug("Skipping MCP not enabled for CLI mode (set enabled_in_cli or pass --all-mcp)", "name", name)
			continue
		}

		admission, err := o.admitServer(initCtx, cfg, name, true)
		if err != nil {
			break
		}
		wg.Add(1)
		go func(name string, m config.MCPConfig, admission serverAdmission) {
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
					if admission.valid() {
						updateState(name, StateError, err, nil, Counts{})
					}
					slog.Error("Panic in MCP client initialization", "error", err, "name", name)
				}
			}()

			if err := initClientAdmitted(admission.ctx, cfg, name, m, resolver, &admission); err != nil {
				slog.Debug("Failed to initialize MCP client", "name", name, "error", err)
			}
		}(name, m, admission)
	}
	wg.Wait()
	o.finishInitialize()
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
	if !o.beginInit() {
		return ErrOwnerBusy
	}
	defer o.endInit()

	m, exists := cfg.MCPConfig(name)
	if !exists {
		return fmt.Errorf("mcp '%s' not found in configuration", name)
	}

	if m.Disabled {
		lifecycleMu.Lock()
		accepted := owner == o && !o.closing && o.generation == generation
		if accepted {
			setState(name, StateDisabled, nil, nil, Counts{})
		}
		lifecycleMu.Unlock()
		if accepted {
			publishStateEvent(name, StateDisabled, nil, Counts{})
		}
		slog.Debug("Skipping disabled MCP", "name", name)
		return nil
	}

	admitted, err := o.admitServer(ctx, cfg, name, true)
	if err != nil {
		return err
	}
	return initClientAdmitted(admitted.ctx, cfg, name, m, cfg.Resolver(), &admitted)
}

func initClientAdmitted(ctx context.Context, cfg *config.ConfigStore, name string, m config.MCPConfig, resolver config.VariableResolver, admission *serverAdmission) error {
	if admission != nil {
		defer admission.done()
		if !admission.valid() {
			return ErrOwnerBusy
		}
	}
	if admission == nil {
		updateState(name, StateStarting, nil, nil, Counts{})
	} else {
		updateAdmissionState(admission, StateStarting, nil, nil, Counts{})
	}

	operationCtx := ctx
	finish := func() {}
	if admission != nil {
		operationCtx, finish = admission.owner.operationContext(ctx)
		defer finish()
	}
	prepared, err := prepareClient(operationCtx, cfg, name, m, resolver, admission)
	if err != nil {
		return err
	}
	return publishPreparedClient(cfg, name, prepared, admission)
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

	if admission != nil && !admission.valid() {
		_ = session.Close()
		return nil, ErrOwnerBusy
	}
	return &preparedClient{session: session, tools: tools, prompts: prompts}, nil
}

func publishPreparedClient(cfg *config.ConfigStore, name string, prepared *preparedClient, admission *serverAdmission) error {
	session := prepared.session
	tools := prepared.tools
	prompts := prepared.prompts
	lease := serverLeaseFor(name)
	lease.Lock()
	defer lease.Unlock()
	lifecycleMu.Lock()
	if admission != nil && !admission.validLocked() {
		lifecycleMu.Unlock()
		_ = session.Close()
		return ErrOwnerBusy
	}
	if _, ok := cfg.MCPConfig(name); !ok {
		lifecycleMu.Unlock()
		_ = session.Close()
		return ErrOwnerBusy
	}
	if currentConfig, _ := cfg.MCPConfig(name); currentConfig.Disabled {
		lifecycleMu.Unlock()
		_ = session.Close()
		return ErrOwnerBusy
	}
	if !session.promoteContext() {
		lifecycleMu.Unlock()
		_ = session.Close()
		return ErrOwnerBusy
	}
	oldSession, hadOldSession := sessions.Get(name)
	toolCount := updateTools(cfg, name, tools)
	updatePrompts(name, prompts)
	sessions.Set(name, session)
	if admission != nil {
		admission.committed = true
	}
	counts := Counts{
		Tools:   toolCount,
		Prompts: len(prompts),
	}
	setState(name, StateConnected, nil, session, counts)
	brokerForEvent := broker
	lifecycleMu.Unlock()
	if hadOldSession && oldSession != session {
		_ = oldSession.Close()
	}
	brokerForEvent.Publish(pubsub.UpdatedEvent, Event{
		Type: EventStateChanged, Name: name, State: StateConnected, Counts: counts,
	})

	return nil
}

// DisableSingle disables and closes a single MCP client by name.
func DisableSingle(cfg *config.ConfigStore, name string) error {
	o, err := ensureOwner()
	if err != nil {
		return err
	}
	if !o.beginInit() {
		return ErrOwnerBusy
	}
	defer o.endInit()
	lease := serverLeaseFor(name)
	lease.Lock()
	defer lease.Unlock()
	o.invalidateServer(name)
	closeSessionLocked(name)
	clearAdvertised(name)
	updateState(name, StateDisabled, nil, nil, Counts{})

	slog.Info("Disabled mcp client", "name", name)
	return nil
}

// DisableServer disables an MCP server: closes its session, removes its tools,
// and persists the disabled flag to config.
func DisableServer(ctx context.Context, cfg *config.ConfigStore, name string) error {
	return disableServerWithPersistence(ctx, cfg, name,
		func(cfg *config.ConfigStore, scope config.Scope, name string) error {
			return cfg.PersistMCPDisabledOverride(scope, name, true)
		})
}

type disableServerPersister func(*config.ConfigStore, config.Scope, string) error

func disableServerWithPersistence(
	ctx context.Context,
	cfg *config.ConfigStore,
	name string,
	persist disableServerPersister,
) error {
	o, err := ensureOwner()
	if err != nil {
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
	lease.Lock()
	defer lease.Unlock()
	mcpCfg, ok = cfg.MCPConfig(name)
	if !ok {
		return fmt.Errorf("MCP server %q disappeared while disabling: %w", name, config.ErrMCPNotFound)
	}
	// Resolve the origin while holding the ordered server lease. A user
	// workspace definition must stay in the workspace file, while project,
	// system, external, and otherwise unrepresentable definitions fail closed.
	scope, err := resolveMCPMutationScope(cfg, name, mcpCfg)
	if err != nil {
		return fmt.Errorf("cannot determine writable scope for MCP server %q: %w", name, err)
	}
	// Persist first. If disk persistence fails, the session and in-memory
	// snapshot remain enabled, so the operation has no half-applied result.
	if err := persist(cfg, scope, name); err != nil {
		return fmt.Errorf("failed to persist MCP disabled state for %q: %w", name, err)
	}
	// Persistence is the fallible part of this transaction. Only after it
	// succeeds may this operation invalidate candidates; the write lease keeps
	// a candidate from publishing between these steps and the runtime update.
	o.invalidateServer(name)
	if _, ok := cfg.SetMCPDisabled(name, true); !ok {
		return fmt.Errorf("MCP server %q disappeared while disabling", name)
	}
	closeSessionLocked(name)
	clearAdvertised(name)
	updateState(name, StateDisabled, nil, nil, Counts{})
	return nil
}

// EnableServer re-enables a disabled MCP server and starts a new session.
func EnableServer(ctx context.Context, cfg *config.ConfigStore, name string) error {
	o, err := ensureOwner()
	if err != nil {
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
	lease.Lock()
	mcpCfg, ok = cfg.MCPConfig(name)
	if !ok {
		lease.Unlock()
		return fmt.Errorf("MCP server %q disappeared while enabling", name)
	}
	scope, err := resolveMCPMutationScope(cfg, name, mcpCfg)
	if err != nil {
		lease.Unlock()
		return fmt.Errorf("cannot determine writable scope for MCP server %q: %w", name, err)
	}
	if err := cfg.PersistMCPDisabledOverride(scope, name, false); err != nil {
		lease.Unlock()
		return fmt.Errorf("failed to persist MCP enabled state for %q: %w", name, err)
	}
	rollbackPersistence := func() {
		if err := cfg.PersistMCPDisabledOverride(scope, name, true); err != nil {
			slog.Error("Failed to roll back MCP enabled state", "name", name, "error", err)
		}
		_, _ = cfg.SetMCPDisabled(name, true)
	}
	if !o.acceptsSession() {
		rollbackPersistence()
		lease.Unlock()
		return ErrOwnerBusy
	}
	// The new admission below is the only initializer allowed to publish this
	// epoch. A failed persistence round-trip therefore leaves the old epoch
	// untouched and the operation has no half-applied lifecycle result.
	o.invalidateServer(name)
	updated, ok := cfg.SetMCPDisabled(name, false)
	if !ok {
		rollbackPersistence()
		lease.Unlock()
		return fmt.Errorf("MCP server %q disappeared while enabling", name)
	}
	admission, err := o.admitServer(ctx, cfg, name, true)
	if err != nil {
		rollbackPersistence()
		lease.Unlock()
		return err
	}
	updateAdmissionState(&admission, StateStarting, nil, nil, Counts{})
	resolver := cfg.Resolver()
	lease.Unlock()
	go func() {
		if err := initClientAdmitted(ctx, cfg, name, updated, resolver, &admission); err != nil {
			slog.Error("Failed to enable MCP server", "name", name, "err", err)
		}
	}()
	return nil
}

// AddServer validates and adds a new MCP server. It attempts to connect; if
// successful the server is added to the in-memory config and persisted to disk.
func AddServer(ctx context.Context, cfg *config.ConfigStore, name string, mcpCfg config.MCPConfig) error {
	return addServerWithInitializer(ctx, cfg, name, mcpCfg, initClientAdmitted)
}

// ReplaceServer prepares a new MCP session completely before changing the
// configured server. The old session and its advertised data remain live
// until the durable remove-and-set has committed, at which point the config,
// session, tools, prompts, resources, and state switch as one lifecycle
// transition.
func ReplaceServer(ctx context.Context, cfg *config.ConfigStore, oldName, newName string, mcpCfg config.MCPConfig) error {
	return replaceServerWithScopedPersistence(ctx, cfg, oldName, newName, mcpCfg,
		func(cfg *config.ConfigStore, scope config.Scope, oldName, newName string, mcpCfg config.MCPConfig) error {
			return cfg.PersistReplaceMCPInScope(scope, oldName, newName, mcpCfg)
		})
}

type replacementPersister func(*config.ConfigStore, string, string, config.MCPConfig) error
type scopedReplacementPersister func(*config.ConfigStore, config.Scope, string, string, config.MCPConfig) error

func replaceServerWithPersistence(
	ctx context.Context,
	cfg *config.ConfigStore,
	oldName, newName string,
	mcpCfg config.MCPConfig,
	persist replacementPersister,
) error {
	return replaceServerWithScopedPersistence(ctx, cfg, oldName, newName, mcpCfg,
		func(cfg *config.ConfigStore, _ config.Scope, oldName, newName string, mcpCfg config.MCPConfig) error {
			return persist(cfg, oldName, newName, mcpCfg)
		})
}

func replaceServerWithScopedPersistence(
	ctx context.Context,
	cfg *config.ConfigStore,
	oldName, newName string,
	mcpCfg config.MCPConfig,
	persist scopedReplacementPersister,
) error {
	// Resolve before acquiring an owner or admitting a candidate. This keeps
	// an external, project, system, or ambiguous origin from causing any
	// session, admission, network, or disk mutation.
	scope, err := cfg.ResolveMCPWritableScope(oldName)
	if err != nil {
		return fmt.Errorf("cannot determine writable scope for MCP server %q: %w", oldName, err)
	}
	if mcpCfg.Source == config.MCPSourceExternal {
		return fmt.Errorf("MCP server %q is from .mcp.json and cannot be replaced: %w", oldName, config.ErrMCPExternal)
	}
	o, err := ensureOwner()
	if err != nil {
		return err
	}
	if !o.beginInit() {
		return ErrOwnerBusy
	}
	defer o.endInit()

	locked := lockServerLeases(oldName, newName)
	_, exists := cfg.MCPConfig(oldName)
	if !exists {
		unlockServerLeases(locked)
		return fmt.Errorf("MCP server %q disappeared after scope resolution: %w", oldName, config.ErrMCPNotFound)
	}
	currentScope, err := cfg.ResolveMCPWritableScope(oldName)
	if err != nil {
		unlockServerLeases(locked)
		return fmt.Errorf("MCP server %q became unwritable before candidate preparation: %w", oldName, err)
	}
	if currentScope != scope {
		unlockServerLeases(locked)
		return fmt.Errorf("MCP server %q writable scope changed from %s to %s before candidate preparation", oldName, scope, currentScope)
	}
	if oldName != newName {
		if _, exists := cfg.MCPConfig(newName); exists {
			unlockServerLeases(locked)
			return fmt.Errorf("MCP server %q already exists", newName)
		}
	}

	admission, err := o.admitReplacementCandidate(ctx, cfg, oldName)
	if err != nil {
		unlockServerLeases(locked)
		return err
	}
	admission.suppressState = true
	admission.suppressUntilCommit = true
	resolver := cfg.Resolver()
	unlockServerLeases(locked)

	prepared, err := prepareClient(admission.ctx, cfg, newName, mcpCfg, resolver, &admission)
	if err != nil {
		admission.done()
		if errors.Is(err, ErrOwnerBusy) {
			return ErrOwnerBusy
		}
		return fmt.Errorf("failed to connect to MCP server %q: %w", newName, err)
	}

	locked = lockServerLeases(oldName, newName)
	lifecycleMu.Lock()
	if !admission.validLocked() || (oldName != newName && hasMCPServer(cfg, newName)) {
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

	// Scope is derived from on-disk origin, which may have changed while the
	// candidate was being prepared. Re-resolve while the ordered leases are
	// still held and immediately before the durable write; otherwise a
	// workspace replacement could silently land in the global file.
	commitScope, err := cfg.ResolveMCPWritableScope(oldName)
	if err != nil {
		unlockServerLeases(locked)
		_ = prepared.session.Close()
		admission.done()
		return fmt.Errorf("MCP server %q became unwritable before durable replacement: %w", oldName, err)
	}
	if commitScope != scope {
		unlockServerLeases(locked)
		_ = prepared.session.Close()
		admission.done()
		return fmt.Errorf("MCP server %q writable scope changed from %s to %s before durable replacement", oldName, scope, commitScope)
	}

	// The durable atomic RMW may wait on an inter-process file lock for up to
	// the config write timeout. Keep the ordered server leases, but never hold
	// lifecycleMu here: Owner.Close must be able to mark the owner closing,
	// cancel its contexts, and honor the caller's deadline while this I/O is
	// stalled.
	if err := persist(cfg, commitScope, oldName, newName, mcpCfg); err != nil {
		unlockServerLeases(locked)
		_ = prepared.session.Close()
		admission.done()
		return fmt.Errorf("failed to persist MCP server replacement %q to %q: %w", oldName, newName, err)
	}

	lifecycleMu.Lock()
	if owner != o || o.closing || o.generation != admission.generation ||
		o.serverEpochs[oldName] != admission.epoch {
		// Persistence is the transaction's durable linearization point. If
		// shutdown won after that point, report success but never publish a
		// session whose owner is already closing; the next owner will load the
		// committed config from disk.
		lifecycleMu.Unlock()
		unlockServerLeases(locked)
		_ = prepared.session.Close()
		admission.done()
		return nil
	}
	committedCfg, exists := cfg.MCPConfig(newName)
	var canceled []context.CancelFunc
	canceled = append(canceled, o.invalidateServerLocked(oldName)...)
	if newName != oldName {
		canceled = append(canceled, o.invalidateServerLocked(newName)...)
	}
	if !exists || committedCfg.Disabled {
		// The durable replacement succeeded, but its final config is not
		// runnable (either the requested replacement is disabled, or a later
		// ConfigStore mutation disabled/removed it before runtime publication).
		// Treat that later state as an inactive commit: fence both names and
		// remove every old/target runtime artifact. The operation still returns
		// success because its durable RMW linearized successfully; events below
		// describe the later effective state exactly.
		oldSession, hadOldSession := sessions.Get(oldName)
		newSession, hadNewSession := sessions.Get(newName)
		clearAdvertised(oldName)
		sessions.Del(oldName)
		states.Del(oldName)
		if newName != oldName {
			clearAdvertised(newName)
			sessions.Del(newName)
			states.Del(newName)
		}
		if exists {
			setState(newName, StateDisabled, nil, nil, Counts{})
		}
		brokerForEvent := broker
		lifecycleMu.Unlock()
		unlockServerLeases(locked)
		for _, cancel := range canceled {
			cancel()
		}
		_ = prepared.session.Close()
		if hadOldSession && oldSession != prepared.session {
			_ = oldSession.Close()
		}
		if newName != oldName && hadNewSession && newSession != prepared.session && newSession != oldSession {
			_ = newSession.Close()
		}
		admission.done()
		if newName != oldName {
			brokerForEvent.Publish(pubsub.DeletedEvent, Event{
				Type: EventStateChanged, Name: oldName, State: StateDisabled,
			})
		}
		if exists {
			brokerForEvent.Publish(pubsub.UpdatedEvent, Event{
				Type: EventStateChanged, Name: newName, State: StateDisabled,
			})
		} else {
			brokerForEvent.Publish(pubsub.DeletedEvent, Event{
				Type: EventStateChanged, Name: newName, State: StateDisabled,
			})
		}
		return nil
	}

	admission.committed = true
	admission.committedName = newName
	admission.committedEpoch = o.serverEpochs[newName]

	oldSession, hadOldSession := sessions.Get(oldName)
	_, hadOldState := states.Get(oldName)
	if oldName != newName {
		clearAdvertised(oldName)
		sessions.Del(oldName)
		states.Del(oldName)
	}
	toolCount := updateTools(cfg, newName, prepared.tools)
	updatePrompts(newName, prepared.prompts)
	// Resources are fetched lazily. A replacement must not expose resources
	// belonging to the old session under either the reused or renamed key.
	allResources.Del(newName)
	sessions.Set(newName, prepared.session)
	counts := Counts{Tools: toolCount, Prompts: len(prepared.prompts)}
	setState(newName, StateConnected, nil, prepared.session, counts)
	brokerForEvent := broker
	lifecycleMu.Unlock()
	unlockServerLeases(locked)

	for _, cancel := range canceled {
		cancel()
	}
	if hadOldSession && oldSession != prepared.session {
		_ = oldSession.Close()
	}
	admission.done()
	if oldName != newName && hadOldState {
		brokerForEvent.Publish(pubsub.DeletedEvent, Event{
			Type: EventStateChanged, Name: oldName, State: StateDisabled,
		})
	}
	brokerForEvent.Publish(pubsub.UpdatedEvent, Event{
		Type: EventStateChanged, Name: newName, State: StateConnected, Counts: counts,
	})
	return nil
}

func hasMCPServer(cfg *config.ConfigStore, name string) bool {
	_, ok := cfg.MCPConfig(name)
	return ok
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
	if errors.Is(err, config.ErrMCPUnwritableOrigin) && hasActiveServerAdmission(name) {
		return config.ScopeGlobal, nil
	}
	return config.ScopeGlobal, err
}

func hasActiveServerAdmission(name string) bool {
	lifecycleMu.Lock()
	defer lifecycleMu.Unlock()
	currentOwner := owner
	if currentOwner == nil {
		return false
	}
	return len(currentOwner.serverCancels[name]) > 0
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
	o, err := ensureOwner()
	if err != nil {
		return err
	}
	if !o.beginInit() {
		return ErrOwnerBusy
	}
	defer o.endInit()
	lease := serverLeaseFor(name)
	lease.Lock()
	if _, exists := cfg.MCPConfig(name); exists {
		lease.Unlock()
		return fmt.Errorf("MCP server %q already exists", name)
	}
	if !cfg.AddMCP(name, mcpCfg) {
		lease.Unlock()
		return fmt.Errorf("MCP server %q already exists", name)
	}
	admission, err := o.admitServer(ctx, cfg, name, true)
	if err != nil {
		_, _ = cfg.RemoveMCP(name)
		lease.Unlock()
		return err
	}
	resolver := cfg.Resolver()
	// Keep the lease entry alive while initialization runs without the write
	// lock. RemoveServer must then wait on this exact lease instance instead of
	// racing a reclaimed entry with an ABA replacement.
	if !lease.registry.retain(lease) {
		admission.done()
		_, _ = cfg.RemoveMCP(name)
		lease.Unlock()
		return ErrOwnerBusy
	}
	lease.Unlock()
	initErr := initialize(ctx, cfg, name, mcpCfg, resolver, &admission)
	if initErr != nil {
		lease.Lock()
		rollbackAddedServer(o, cfg, name, &admission)
		lease.Unlock()
		if errors.Is(initErr, ErrOwnerBusy) {
			return ErrOwnerBusy
		}
		return fmt.Errorf("failed to connect to MCP server %q: %w", name, initErr)
	}

	// Hold the write lease while persisting so RemoveServer cannot remove the
	// in-memory entry and then lose the race by being followed by this write.
	lease.Lock()
	if !admission.valid() {
		lease.Unlock()
		return ErrOwnerBusy
	}
	if err := cfg.PersistMCPConfig(config.ScopeGlobal, name, mcpCfg); err != nil {
		rollbackAddedServer(o, cfg, name, &admission)
		lease.Unlock()
		return fmt.Errorf("failed to persist MCP server %q: %w", name, err)
	}
	lease.Unlock()
	return nil
}

// rollbackAddedServer removes only the server instance represented by
// admission. Callers hold the per-server write lease, so a newer epoch cannot
// appear between the identity check and the rollback.
func rollbackAddedServer(o *Owner, cfg *config.ConfigStore, name string, admission *serverAdmission) bool {
	if admission == nil || admission.owner != o || admission.name != name || !admission.valid() {
		return false
	}
	closeSessionLocked(name)
	_, _ = cfg.RemoveMCP(name)
	o.invalidateServer(name)
	clearAdvertised(name)
	states.Del(name)
	return true
}

// RemoveServer removes an MCP server, closes its session, and removes it from config.
// External servers (from .mcp.json) cannot be removed — only disabled.
func RemoveServer(cfg *config.ConfigStore, name string) error {
	return removeServerWithScopedPersistence(cfg, name,
		func(cfg *config.ConfigStore, scope config.Scope, name string) error {
			return cfg.PersistRemoveMCPConfig(scope, name)
		})
}

type removeServerPersister func(*config.ConfigStore, string) error
type scopedRemoveServerPersister func(*config.ConfigStore, config.Scope, string) error

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
	o, err := ensureOwner()
	if err != nil {
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
	defer lease.Unlock()
	mcpCfg, exists = cfg.MCPConfig(name)
	if !exists {
		return fmt.Errorf("MCP server %q disappeared while removing: %w", name, config.ErrMCPNotFound)
	}
	if mcpCfg.Source == config.MCPSourceExternal {
		return fmt.Errorf("MCP server %q is from .mcp.json and cannot be removed (disable it instead): %w", name, config.ErrMCPExternal)
	}
	scope, err := resolveMCPMutationScope(cfg, name, mcpCfg)
	if err != nil {
		return fmt.Errorf("cannot determine writable scope for MCP server %q: %w", name, err)
	}
	if err := persist(cfg, scope, name); err != nil {
		return fmt.Errorf("failed to remove MCP server %q from config: %w", name, err)
	}
	// Persistence is the fallible part of this transaction. Only after it
	// succeeds may this operation invalidate candidates; the write lease keeps
	// a candidate from publishing until runtime state is removed.
	o.invalidateServer(name)
	// RemoveConfigField may already have published the disk reload, in which
	// case the copy-on-write removal is intentionally a no-op.
	_, _ = cfg.RemoveMCP(name)
	closeSessionLocked(name)
	clearAdvertised(name)
	states.Del(name)
	publishEvent(pubsub.DeletedEvent, Event{Type: EventStateChanged, Name: name, State: StateDisabled})
	return nil
}

func ensureOwner() (*Owner, error) {
	if o := currentOwner(); o != nil {
		return o, nil
	}
	return acquireImplicit()
}

func closeSessionLocked(name string) {
	if session, ok := sessions.Get(name); ok {
		if err := session.Close(); err != nil && !errors.Is(err, io.EOF) &&
			!errors.Is(err, context.Canceled) && err.Error() != "signal: killed" {
			slog.Warn("Error closing MCP session", "name", name, "error", err)
		}
		sessions.Del(name)
	}
}

func clearAdvertised(name string) {
	allTools.Del(name)
	allPrompts.Del(name)
	allResources.Del(name)
}

func getOrRenewClient(ctx context.Context, cfg *config.ConfigStore, name string) (*clientLease, error) {
	o := currentOwner()
	var admission *serverAdmission
	operationCtx := ctx
	finish := func() {}
	if o != nil {
		if !o.beginInit() {
			return nil, ErrOwnerBusy
		}
		admitted, err := o.snapshotServerAdmission(ctx, cfg, name)
		if err != nil {
			o.endInit()
			return nil, err
		}
		admission = &admitted
		operationCtx, finish = o.operationContext(ctx)
		admission.ctx = operationCtx
	}

	lease := serverLeaseFor(name)
	lease.RLock()
	sess, ok := sessions.Get(name)
	if !ok {
		lease.RUnlock()
		finish()
		if o != nil {
			o.endInit()
		}
		return nil, fmt.Errorf("mcp '%s' not available", name)
	}

	m, exists := cfg.MCPConfig(name)
	if !exists || m.Disabled {
		lease.RUnlock()
		finish()
		if o != nil {
			o.endInit()
		}
		return nil, fmt.Errorf("mcp '%s' not available", name)
	}
	state, _ := states.Get(name)
	timeout := mcpTimeout(m)
	err := pingWithTimeout(operationCtx, sess, timeout)
	if err == nil {
		if admission != nil && !admission.valid() {
			lease.RUnlock()
			finish()
			o.endInit()
			return nil, ErrOwnerBusy
		}
		return newClientLease(sess, operationCtx, lease, finish, o), nil
	}
	// Keep the lease object alive while upgrading from a read lock. Without
	// this extra reference, the registry could reclaim the entry between
	// RUnlock and Lock and let a new operation use an ABA-replaced lock.
	if !lease.registry.retain(lease) {
		lease.RUnlock()
		finish()
		if o != nil {
			o.endInit()
		}
		return nil, ErrOwnerBusy
	}
	lease.RUnlock()

	// Upgrade the read lease to an exclusive renewal lease. A writer waits for
	// every in-flight operation on the old session before it can close it.
	lease.Lock()
	if admission != nil && !admission.valid() {
		lease.Unlock()
		finish()
		o.endInit()
		return nil, ErrOwnerBusy
	}

	current, currentOK := sessions.Get(name)
	if currentOK && current != sess {
		// Retain before dropping the write lock so this exact lease cannot be
		// reclaimed before the read lock is reacquired.
		if !lease.registry.retain(lease) {
			lease.Unlock()
			finish()
			if o != nil {
				o.endInit()
			}
			return nil, ErrOwnerBusy
		}
		lease.Unlock()
		lease.RLock()
		if admission != nil && !admission.valid() {
			lease.RUnlock()
			finish()
			o.endInit()
			return nil, ErrOwnerBusy
		}
		current, currentOK = sessions.Get(name)
		if !currentOK {
			lease.RUnlock()
			finish()
			if o != nil {
				o.endInit()
			}
			return nil, fmt.Errorf("mcp '%s' not available", name)
		}
		return newClientLease(current, operationCtx, lease, finish, o), nil
	}

	setState(name, StateError, err, nil, state.Counts)
	publishStateEvent(name, StateError, err, state.Counts)
	if currentOK {
		// The ping failure identifies the session that must be retired. Remove
		// it explicitly; state transitions must never infer session ownership
		// from the previous state's Client field.
		if published, ok := sessions.Get(name); ok && published == current {
			sessions.Del(name)
		}
		_ = current.Close()
	}

	sessionCtx := operationCtx
	if o != nil {
		// The returned client lease owns only the caller's operation context.
		// The renewed MCP session itself must survive that lease closing and
		// remain attached to the owner until the next lifecycle transition.
		sessionCtx = o.lifecycleCtx
	}
	newSession, err := createSessionWithAdmission(sessionCtx, name, m, cfg.Resolver(), admission)
	if err != nil {
		lease.Unlock()
		finish()
		if o != nil {
			o.endInit()
		}
		return nil, err
	}

	if o != nil {
		if err := o.commitRenewal(admission, name, newSession, state.Counts); err != nil {
			lease.Unlock()
			finish()
			o.endInit()
			return nil, err
		}
	} else {
		sessions.Set(name, newSession)
		setState(name, StateConnected, nil, newSession, state.Counts)
		publishStateEvent(name, StateConnected, nil, state.Counts)
	}
	// Retain before dropping the write lock so the returned read lease remains
	// attached to this server's identity even when another goroutine is
	// waiting to replace the session.
	if !lease.registry.retain(lease) {
		lease.Unlock()
		finish()
		if o != nil {
			o.endInit()
		}
		return nil, ErrOwnerBusy
	}
	lease.Unlock()
	lease.RLock()
	if admission != nil && !admission.valid() {
		lease.RUnlock()
		finish()
		o.endInit()
		return nil, ErrOwnerBusy
	}
	current, currentOK = sessions.Get(name)
	if !currentOK {
		lease.RUnlock()
		finish()
		if o != nil {
			o.endInit()
		}
		return nil, fmt.Errorf("mcp '%s' not available", name)
	}

	return newClientLease(current, operationCtx, lease, finish, o), nil
}

func pingWithTimeout(ctx context.Context, session *ClientSession, timeout time.Duration) error {
	timeoutCause := errors.New("mcp ping timeout")
	pingCtx, cancel := context.WithTimeoutCause(ctx, timeout, timeoutCause)
	err := session.Ping(pingCtx, nil)
	errCause := context.Cause(pingCtx)
	cancel()
	return maybeTimeoutErr(err, timeout, errCause, timeoutCause)
}

func newClientLease(session *ClientSession, ctx context.Context, lease *serverLease, finish func(), o *Owner) *clientLease {
	return &clientLease{
		session: session,
		ctx:     ctx,
		release: func() {
			lease.RUnlock()
			finish()
			if o != nil {
				o.endInit()
			}
		},
	}
}

// currentClientLease admits an operation on the current session without a
// health check. It is used by notification refreshers, which already receive
// a session selected by the MCP transport.
func currentClientLease(ctx context.Context, name string) (*clientLease, error) {
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
	lease.RLock()
	session, ok := sessions.Get(name)
	if !ok {
		lease.RUnlock()
		finish()
		if o != nil {
			o.endInit()
		}
		return nil, fmt.Errorf("mcp '%s' not available", name)
	}
	return newClientLease(session, operationCtx, lease, finish, o), nil
}

func currentClientLeaseFor(ctx context.Context, name string, admission *serverAdmission) (*clientLease, error) {
	if admission == nil || !admission.valid() {
		return nil, ErrOwnerBusy
	}
	lease := serverLeaseFor(name)
	lease.RLock()
	if !admission.valid() {
		lease.RUnlock()
		return nil, ErrOwnerBusy
	}
	session, ok := sessions.Get(name)
	if !ok {
		lease.RUnlock()
		return nil, fmt.Errorf("mcp '%s' not available", name)
	}
	return &clientLease{
		session: session,
		ctx:     ctx,
		release: lease.RUnlock,
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
	lifecycleMu.Lock()
	if !admission.validLocked() {
		lifecycleMu.Unlock()
		return
	}
	setState(admission.name, state, err, client, counts)
	brokerForEvent := broker
	lifecycleMu.Unlock()
	brokerForEvent.Publish(pubsub.UpdatedEvent, Event{
		Type:   EventStateChanged,
		Name:   admission.name,
		State:  state,
		Error:  err,
		Counts: counts,
	})
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

func createSession(ctx context.Context, name string, m config.MCPConfig, resolver config.VariableResolver) (*ClientSession, error) {
	return createSessionWithAdmission(ctx, name, m, resolver, nil)
}

// sessionContext is a handoff context for a candidate session. Admission and
// owner cancellation can abort initialization, but once the candidate is
// published admission completion must not cancel the SDK connection context.
type sessionContext struct {
	owner     context.Context
	candidate context.Context
	done      chan struct{}

	mu       sync.Mutex
	promoted bool
	err      error
	closed   bool
}

func newSessionContext(owner, candidate context.Context) *sessionContext {
	s := &sessionContext{
		owner:     owner,
		candidate: candidate,
		done:      make(chan struct{}),
	}
	go func() {
		select {
		case <-owner.Done():
			s.finish(owner, false)
		case <-candidate.Done():
			if s.finish(candidate, true) {
				return
			}
			<-owner.Done()
			s.finish(owner, false)
		}
	}()
	return s
}

func (s *sessionContext) finish(source context.Context, candidate bool) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed || (candidate && s.promoted) {
		return false
	}
	s.err = source.Err()
	s.closed = true
	close(s.done)
	return true
}

func (s *sessionContext) promote() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return false
	}
	s.promoted = true
	return true
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
		handoff = newSessionContext(admission.owner.lifecycleCtx, admission.ctx)
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
		publishEvent(pubsub.UpdatedEvent, Event{Type: eventType, Name: name})
		return
	}
	if !admission.notificationsValid() {
		return
	}
	admission.owner.enqueueRefresh(refreshRequest{
		kind:      kind,
		name:      name,
		cfg:       admission.cfg,
		admission: *admission,
	})
	publishEvent(pubsub.UpdatedEvent, Event{Type: eventType, Name: name})
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
