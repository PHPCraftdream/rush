package mcp

import (
	"context"
	"errors"
	"fmt"
	"github.com/PHPCraftdream/rush/internal/config"
	"github.com/PHPCraftdream/rush/internal/pubsub"
	"github.com/PHPCraftdream/rush/internal/version"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"
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
		// Cancel before enqueueing so a blocked earlier Close cannot keep this
		// retired generation's lifetime resources alive.
		s.cancelContext()
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
		// Cancellation is intentionally outside operationMu and before queueing:
		// cancel must remain nonblocking and cannot run while lifecycle locks are
		// held.
		s.cancelContext()
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
	err := admission.cfg.WithCurrentMCPAdmissionContext(admission.ctx, snapshot, name, func(guard config.MCPAdmissionGuard) error {
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
	err := cfg.WithCurrentMCPAdmissionContext(ctx, snapshot, name, func(guard config.MCPAdmissionGuard) error {
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

func maybeTimeoutErr(err error, timeout time.Duration, cause, timeoutCause error) error {
	if cause == timeoutCause {
		return fmt.Errorf("timed out after %s", timeout)
	}
	return err
}
