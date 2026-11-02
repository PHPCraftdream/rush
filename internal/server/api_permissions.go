package server

import (
	"context"
	"errors"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/PHPCraftdream/rush/internal/permission"
)

const (
	defaultPermissionTimeout        = 5 * time.Minute
	maxPendingPermissionsPerSession = 100
	permissionDescriptionLimit      = 200
)

var (
	ErrAPIPermissionUnknown  = errors.New("API permission not found")
	ErrAPIPermissionDecided  = errors.New("API permission already decided")
	ErrAPIPermissionDecision = errors.New("invalid API permission decision")
)

type APIPermissionRequest struct {
	ID          string    `json:"id"`
	SessionID   string    `json:"session_id"`
	RunID       string    `json:"run_id"`
	Tool        string    `json:"tool"`
	Action      string    `json:"action"`
	Description string    `json:"description"`
	Path        string    `json:"path,omitempty"`
	CreatedAt   time.Time `json:"created_at"`
}

type apiPermissionEvent struct {
	Type    string               `json:"type"`
	Request APIPermissionRequest `json:"request"`
}

type apiPermissionTimer interface {
	C() <-chan time.Time
	Stop() bool
}

type realAPIPermissionTimer struct{ timer *time.Timer }

func (t realAPIPermissionTimer) C() <-chan time.Time { return t.timer.C }
func (t realAPIPermissionTimer) Stop() bool          { return t.timer.Stop() }

type (
	apiPermissionRegistration struct{ runID, sessionID, tokenID string }
	apiPermissionPending      struct {
		request      permission.PermissionRequest
		public       APIPermissionRequest
		registration apiPermissionRegistration
		state        string
		timer        apiPermissionTimer
		done         chan struct{}
	}
)

type APIPermissionBroker struct {
	mu            sync.Mutex
	service       permission.Service
	workspace     string
	timeout       time.Duration
	newTimer      func(time.Duration) apiPermissionTimer
	now           func() time.Time
	emit          func(apiPermissionEvent)
	ctx           context.Context
	cancel        context.CancelFunc
	done          chan struct{}
	workers       sync.WaitGroup
	started       bool
	stopped       bool
	healthy       bool
	registrations map[string]apiPermissionRegistration
	bySession     map[string]string
	pending       map[string]*apiPermissionPending
	decided       map[string]apiPermissionRegistration
	decidedOrder  []string
	stopDone      chan struct{}
	waitStarted   func()
}

// NewAPIPermissionBroker constructs a broker. A missing optional timeout uses five minutes.
func NewAPIPermissionBroker(service permission.Service, workspace string, emit func(apiPermissionEvent), timeout ...time.Duration) *APIPermissionBroker {
	d := defaultPermissionTimeout
	if len(timeout) > 0 && timeout[0] > 0 {
		d = timeout[0]
	}
	return &APIPermissionBroker{
		service: service, workspace: workspace, timeout: d, newTimer: func(d time.Duration) apiPermissionTimer { return realAPIPermissionTimer{time.NewTimer(d)} },
		now: time.Now, emit: emit, registrations: make(map[string]apiPermissionRegistration), bySession: make(map[string]string), pending: make(map[string]*apiPermissionPending), decided: make(map[string]apiPermissionRegistration),
	}
}

func (b *APIPermissionBroker) SetEmitter(emit func(apiPermissionEvent)) {
	b.mu.Lock()
	b.emit = emit
	b.mu.Unlock()
}

// Start subscribes once. Pubsub delivery can be lossy; permission.Service currently has no pending-request listing API,
// so a dropped publication cannot be reconciled. The run's permission gate must remain fail-closed when this broker is absent.
func (b *APIPermissionBroker) Start(ctx context.Context) error {
	if ctx == nil || ctx.Err() != nil {
		return ErrAPIPermissionUnknown
	}
	b.mu.Lock()
	if b.started || b.stopped || b.service == nil {
		b.mu.Unlock()
		return ErrAPIPermissionUnknown
	}
	b.started = true
	b.healthy = true
	b.ctx, b.cancel = context.WithCancel(ctx)
	b.done = make(chan struct{})
	b.workers.Add(1)
	events := b.service.Subscribe(b.ctx)
	b.mu.Unlock()
	go func() {
		defer b.workers.Done()
		defer close(b.done)
		for {
			select {
			case <-b.ctx.Done():
				return
			case event, ok := <-events:
				if !ok {
					b.mu.Lock()
					b.healthy = false
					pending := make([]*apiPermissionPending, 0, len(b.pending))
					for _, p := range b.pending {
						if p.state == "pending" {
							p.state = "expired"
							p.timer.Stop()
							close(p.done)
							delete(b.pending, p.public.ID)
							pending = append(pending, p)
						}
					}
					b.mu.Unlock()
					for _, p := range pending {
						b.service.Deny(p.request)
					}
					return
				}
				b.accept(event.Payload)
			}
		}
	}()
	return nil
}

// Register marks a session as owned by an active API run. Only registered sessions are observed.
func (b *APIPermissionBroker) Register(runID, sessionID, tokenID string) error {
	if runID == "" || sessionID == "" || tokenID == "" {
		return ErrAPIPermissionUnknown
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.stopped || !b.started || !b.healthy || b.ctx == nil || b.ctx.Err() != nil {
		return ErrAPIPermissionUnknown
	}
	if prior, ok := b.bySession[sessionID]; ok && prior != runID {
		return ErrAPIPermissionUnknown
	}
	r := apiPermissionRegistration{runID: runID, sessionID: sessionID, tokenID: tokenID}
	b.registrations[runID] = r
	b.bySession[sessionID] = runID
	return nil
}

// Release ends a run's registration and cancels its outstanding request timers.
func (b *APIPermissionBroker) Release(runID string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	r, ok := b.registrations[runID]
	if !ok {
		return
	}
	delete(b.registrations, runID)
	delete(b.bySession, r.sessionID)
	for id, p := range b.pending {
		if p.public.RunID == runID {
			p.state = "expired"
			p.timer.Stop()
			close(p.done)
			delete(b.pending, id)
		}
	}
	for id, r := range b.decided {
		if r.runID == runID {
			delete(b.decided, id)
		}
	}
	kept := b.decidedOrder[:0]
	for _, id := range b.decidedOrder {
		if r, ok := b.decided[id]; ok && r.runID != runID {
			kept = append(kept, id)
		}
	}
	b.decidedOrder = kept
}

func (b *APIPermissionBroker) accept(req permission.PermissionRequest) {
	b.mu.Lock()
	runID, registered := b.bySession[req.SessionID]
	r, active := b.registrations[runID]
	if !registered || !active || b.stopped {
		b.mu.Unlock()
		return
	}
	if _, exists := b.pending[req.ID]; exists {
		b.mu.Unlock()
		return
	}
	p := &apiPermissionPending{request: req, registration: r, state: "pending", timer: b.newTimer(b.timeout), done: make(chan struct{})}
	p.public = APIPermissionRequest{ID: req.ID, SessionID: req.SessionID, RunID: runID, Tool: req.ToolName, Action: req.Action, Description: truncatePermissionDescription(req.Description), Path: redactPermissionPath(req.Path, b.workspace), CreatedAt: b.now().UTC()}
	b.pending[req.ID] = p
	b.workers.Add(1)
	denied := b.trimSessionLocked(req.SessionID)
	b.mu.Unlock()
	for _, dropped := range denied {
		b.service.Deny(dropped)
	}
	b.mu.Lock()
	emit := b.emit
	b.mu.Unlock()
	if emit != nil {
		emit(apiPermissionEvent{Type: "permission_request", Request: p.public})
	}
	go b.watch(p)
}

func (b *APIPermissionBroker) watch(p *apiPermissionPending) {
	defer b.workers.Done()
	select {
	case <-b.ctx.Done():
		return
	case <-p.done:
		return
	case <-p.timer.C():
		b.mu.Lock()
		current := b.pending[p.public.ID]
		if current != p || p.state != "pending" {
			b.mu.Unlock()
			return
		}
		p.state = "timeout"
		delete(b.pending, p.public.ID)
		close(p.done)
		b.mu.Unlock()
		b.service.Deny(p.request)
	}
}

func (b *APIPermissionBroker) trimSessionLocked(sessionID string) []permission.PermissionRequest {
	var denied []permission.PermissionRequest
	for {
		count := 0
		var oldestID string
		var oldest time.Time
		for id, p := range b.pending {
			if p.public.SessionID != sessionID {
				continue
			}
			count++
			if oldestID == "" || p.public.CreatedAt.Before(oldest) {
				oldestID, oldest = id, p.public.CreatedAt
			}
		}
		if count <= maxPendingPermissionsPerSession {
			return denied
		}
		p := b.pending[oldestID]
		p.state = "overflow"
		p.timer.Stop()
		close(p.done)
		b.rememberDecidedLocked(oldestID, p.registration)
		delete(b.pending, oldestID)
		denied = append(denied, p.request)
	}
}

// Answer applies one non-persistent decision. Admin bypasses token ownership.
func (b *APIPermissionBroker) Answer(id, tokenID, decision string, admin bool) error {
	if decision != "allow" && decision != "deny" {
		return ErrAPIPermissionDecision
	}
	b.mu.Lock()
	p, ok := b.pending[id]
	if !ok {
		owner, decided := b.decided[id]
		b.mu.Unlock()
		if decided {
			if !admin && owner.tokenID != tokenID {
				return ErrAPIPermissionUnknown
			}
			return ErrAPIPermissionDecided
		}
		return ErrAPIPermissionUnknown
	}
	if p.state != "pending" {
		b.mu.Unlock()
		return ErrAPIPermissionDecided
	}
	if !admin && p.registration.tokenID != tokenID {
		b.mu.Unlock()
		return ErrAPIPermissionUnknown
	}
	if reg, active := b.registrations[p.registration.runID]; !active || reg.sessionID != p.public.SessionID {
		b.mu.Unlock()
		return ErrAPIPermissionUnknown
	}
	p.state = decision
	delete(b.pending, id)
	b.rememberDecidedLocked(id, p.registration)
	close(p.done)
	p.timer.Stop()
	b.mu.Unlock()
	if decision == "allow" {
		b.service.Grant(p.request)
	} else {
		b.service.Deny(p.request)
	}
	return nil
}

// Pending returns visible pending requests ordered by creation time, optionally restricted to a session.
func (b *APIPermissionBroker) Pending(tokenID string, admin bool, sessionID string) []APIPermissionRequest {
	b.mu.Lock()
	defer b.mu.Unlock()
	out := make([]APIPermissionRequest, 0)
	for _, p := range b.pending {
		if p.state != "pending" || (!admin && p.registration.tokenID != tokenID) || (sessionID != "" && p.public.SessionID != sessionID) {
			continue
		}
		out = append(out, p.public)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.Before(out[j].CreatedAt) })
	return out
}

// rememberDecidedLocked bounds duplicate-answer tombstones as well as pending requests.
func (b *APIPermissionBroker) rememberDecidedLocked(id string, registration apiPermissionRegistration) {
	if _, exists := b.decided[id]; exists {
		return
	}
	b.decided[id] = registration
	b.decidedOrder = append(b.decidedOrder, id)
	if len(b.decidedOrder) > maxPendingPermissionsPerSession {
		delete(b.decided, b.decidedOrder[0])
		b.decidedOrder = b.decidedOrder[1:]
	}
}

// Stop cancels subscription and timers, then joins every broker worker.
func (b *APIPermissionBroker) Stop() {
	b.mu.Lock()
	if b.stopped {
		done := b.stopDone
		b.mu.Unlock()
		if done != nil {
			<-done
		}
		return
	}
	b.stopped = true
	b.stopDone = make(chan struct{})
	if b.cancel != nil {
		b.cancel()
	}
	pending := make([]permission.PermissionRequest, 0, len(b.pending))
	for _, p := range b.pending {
		if p.state == "pending" {
			p.state = "expired"
			p.timer.Stop()
			close(p.done)
			pending = append(pending, p.request)
		}
	}
	b.pending = make(map[string]*apiPermissionPending)
	b.mu.Unlock()
	for _, request := range pending {
		b.service.Deny(request)
	}
	if b.waitStarted != nil {
		b.waitStarted()
	}
	b.workers.Wait()
	close(b.stopDone)
}

func truncatePermissionDescription(s string) string {
	if len([]rune(s)) <= permissionDescriptionLimit {
		return s
	}
	return string([]rune(s)[:permissionDescriptionLimit])
}

func redactPermissionPath(path, workspace string) string {
	if path == "" {
		return ""
	}
	normalize := func(value string) string { return strings.ReplaceAll(value, "\\", "/") }
	portableAbs := func(value string) bool {
		value = normalize(value)
		return filepath.IsAbs(value) || strings.HasPrefix(value, "//") || (len(value) >= 3 && value[1] == ':' && value[2] == '/')
	}
	if portableAbs(path) {
		cleanPath, cleanWorkspace := filepath.ToSlash(filepath.Clean(normalize(path))), filepath.ToSlash(filepath.Clean(normalize(workspace)))
		if workspace != "" && portableAbs(workspace) {
			if len(cleanPath) >= 3 && len(cleanWorkspace) >= 3 && cleanPath[1] == ':' && cleanWorkspace[1] == ':' && strings.EqualFold(cleanPath[:2], cleanWorkspace[:2]) {
				base := strings.TrimSuffix(cleanWorkspace, "/") + "/"
				if strings.HasPrefix(strings.ToLower(cleanPath), strings.ToLower(base)) {
					return strings.TrimPrefix(cleanPath[len(base):], "/")
				}
			}
			if filepath.IsAbs(cleanPath) && filepath.IsAbs(cleanWorkspace) {
				rel, err := filepath.Rel(cleanWorkspace, cleanPath)
				if err == nil && rel != ".." && !strings.HasPrefix(rel, "../") && !filepath.IsAbs(rel) {
					return filepath.ToSlash(rel)
				}
			}
		}
		return filepath.Base(cleanPath)
	}
	return filepath.ToSlash(path)
}
