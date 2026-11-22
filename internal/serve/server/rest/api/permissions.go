package api

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net/http"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/PHPCraftdream/rush/internal/agent/safety/permission"
	"github.com/PHPCraftdream/rush/internal/config"
)

const (
	defaultPermissionTimeout        = 5 * time.Minute
	MaxPendingPermissionsPerSession = 100
	PermissionDescriptionLimit      = 200
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

type APIPermissionEvent struct {
	Type    string               `json:"type"`
	Request APIPermissionRequest `json:"request"`
}

type APIPermissionTimer interface {
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
		timer        APIPermissionTimer
		done         chan struct{}
	}
)

type APIPermissionBroker struct {
	Mu            sync.Mutex
	service       permission.Service
	workspace     string
	timeout       time.Duration
	NewTimer      func(time.Duration) APIPermissionTimer
	Now           func() time.Time
	emit          func(APIPermissionEvent)
	ctx           context.Context
	cancel        context.CancelFunc
	Done          chan struct{}
	workers       sync.WaitGroup
	started       bool
	stopped       bool
	healthy       bool
	Registrations map[string]apiPermissionRegistration
	bySession     map[string]string
	pending       map[string]*apiPermissionPending
	decided       map[string]apiPermissionRegistration
	decidedOrder  []string
	stopDone      chan struct{}
	WaitStarted   func()
}

// NewAPIPermissionBroker constructs a broker. A missing optional timeout uses five minutes.
func NewAPIPermissionBroker(service permission.Service, workspace string, emit func(APIPermissionEvent), timeout ...time.Duration) *APIPermissionBroker {
	d := defaultPermissionTimeout
	if len(timeout) > 0 && timeout[0] > 0 {
		d = timeout[0]
	}
	return &APIPermissionBroker{
		service: service, workspace: workspace, timeout: d, NewTimer: func(d time.Duration) APIPermissionTimer { return realAPIPermissionTimer{time.NewTimer(d)} },
		Now: time.Now, emit: emit, Registrations: make(map[string]apiPermissionRegistration), bySession: make(map[string]string), pending: make(map[string]*apiPermissionPending), decided: make(map[string]apiPermissionRegistration),
	}
}

func (b *APIPermissionBroker) SetEmitter(emit func(APIPermissionEvent)) {
	b.Mu.Lock()
	b.emit = emit
	b.Mu.Unlock()
}

// Start subscribes once. Pubsub delivery can be lossy; permission.Service currently has no pending-request listing API,
// so a dropped publication cannot be reconciled. The run's permission gate must remain fail-closed when this broker is absent.
func (b *APIPermissionBroker) Start(ctx context.Context) error {
	if ctx == nil || ctx.Err() != nil {
		return ErrAPIPermissionUnknown
	}
	b.Mu.Lock()
	if b.started || b.stopped || b.service == nil {
		b.Mu.Unlock()
		return ErrAPIPermissionUnknown
	}
	b.started = true
	b.healthy = true
	b.ctx, b.cancel = context.WithCancel(ctx)
	b.Done = make(chan struct{})
	b.workers.Add(1)
	events := b.service.Subscribe(b.ctx)
	b.Mu.Unlock()
	go func() {
		defer b.workers.Done()
		defer close(b.Done)
		for {
			select {
			case <-b.ctx.Done():
				return
			case event, ok := <-events:
				if !ok {
					b.Mu.Lock()
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
					b.Mu.Unlock()
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
	b.Mu.Lock()
	defer b.Mu.Unlock()
	if b.stopped || !b.started || !b.healthy || b.ctx == nil || b.ctx.Err() != nil {
		return ErrAPIPermissionUnknown
	}
	if prior, ok := b.bySession[sessionID]; ok && prior != runID {
		return ErrAPIPermissionUnknown
	}
	r := apiPermissionRegistration{runID: runID, sessionID: sessionID, tokenID: tokenID}
	b.Registrations[runID] = r
	b.bySession[sessionID] = runID
	return nil
}

// Release ends a run's registration and cancels its outstanding request timers.
func (b *APIPermissionBroker) Release(runID string) {
	b.Mu.Lock()
	defer b.Mu.Unlock()
	r, ok := b.Registrations[runID]
	if !ok {
		return
	}
	delete(b.Registrations, runID)
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
	b.Mu.Lock()
	runID, registered := b.bySession[req.SessionID]
	r, active := b.Registrations[runID]
	if !registered || !active || b.stopped {
		b.Mu.Unlock()
		return
	}
	if _, exists := b.pending[req.ID]; exists {
		b.Mu.Unlock()
		return
	}
	p := &apiPermissionPending{request: req, registration: r, state: "pending", timer: b.NewTimer(b.timeout), done: make(chan struct{})}
	p.public = APIPermissionRequest{ID: req.ID, SessionID: req.SessionID, RunID: runID, Tool: req.ToolName, Action: req.Action, Description: truncatePermissionDescription(req.Description), Path: RedactPermissionPath(req.Path, b.workspace), CreatedAt: b.Now().UTC()}
	b.pending[req.ID] = p
	b.workers.Add(1)
	denied := b.trimSessionLocked(req.SessionID)
	b.Mu.Unlock()
	for _, dropped := range denied {
		b.service.Deny(dropped)
	}
	b.Mu.Lock()
	emit := b.emit
	b.Mu.Unlock()
	if emit != nil {
		emit(APIPermissionEvent{Type: "permission_request", Request: p.public})
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
		b.Mu.Lock()
		current := b.pending[p.public.ID]
		if current != p || p.state != "pending" {
			b.Mu.Unlock()
			return
		}
		p.state = "timeout"
		delete(b.pending, p.public.ID)
		close(p.done)
		b.Mu.Unlock()
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
		if count <= MaxPendingPermissionsPerSession {
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
	b.Mu.Lock()
	p, ok := b.pending[id]
	if !ok {
		owner, decided := b.decided[id]
		b.Mu.Unlock()
		if decided {
			if !admin && owner.tokenID != tokenID {
				return ErrAPIPermissionUnknown
			}
			return ErrAPIPermissionDecided
		}
		return ErrAPIPermissionUnknown
	}
	if p.state != "pending" {
		b.Mu.Unlock()
		return ErrAPIPermissionDecided
	}
	if !admin && p.registration.tokenID != tokenID {
		b.Mu.Unlock()
		return ErrAPIPermissionUnknown
	}
	if reg, active := b.Registrations[p.registration.runID]; !active || reg.sessionID != p.public.SessionID {
		b.Mu.Unlock()
		return ErrAPIPermissionUnknown
	}
	p.state = decision
	delete(b.pending, id)
	b.rememberDecidedLocked(id, p.registration)
	close(p.done)
	p.timer.Stop()
	b.Mu.Unlock()
	if decision == "allow" {
		b.service.Grant(p.request)
	} else {
		b.service.Deny(p.request)
	}
	return nil
}

// Pending returns visible pending requests ordered by creation time, optionally restricted to a session.
func (b *APIPermissionBroker) Pending(tokenID string, admin bool, sessionID string) []APIPermissionRequest {
	b.Mu.Lock()
	defer b.Mu.Unlock()
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
	if len(b.decidedOrder) > MaxPendingPermissionsPerSession {
		delete(b.decided, b.decidedOrder[0])
		b.decidedOrder = b.decidedOrder[1:]
	}
}

// Stop cancels subscription and timers, then joins every broker worker.
func (b *APIPermissionBroker) Stop() {
	b.Mu.Lock()
	if b.stopped {
		done := b.stopDone
		b.Mu.Unlock()
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
	b.Mu.Unlock()
	for _, request := range pending {
		b.service.Deny(request)
	}
	if b.WaitStarted != nil {
		b.WaitStarted()
	}
	b.workers.Wait()
	close(b.stopDone)
}

func truncatePermissionDescription(s string) string {
	if len([]rune(s)) <= PermissionDescriptionLimit {
		return s
	}
	return string([]rune(s)[:PermissionDescriptionLimit])
}

func RedactPermissionPath(path, workspace string) string {
	if path == "" {
		return ""
	}
	normalize := func(value string) string { return strings.ReplaceAll(value, "\\", "/") }
	portableAbs := func(value string) bool {
		value = normalize(value)
		return filepath.IsAbs(value) || strings.HasPrefix(value, "/") || (len(value) >= 3 && value[1] == ':' && value[2] == '/')
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

type apiPermissionAnswer struct {
	Decision string `json:"decision"`
}

func APIPermissionRoutes(broker *APIPermissionBroker) []APIRoute {
	routes := apiPermissionRouteDeclarations()
	routes[0].Handler = apiListPermissionsHandler(broker)
	routes[1].Handler = apiAnswerPermissionHandler(broker)
	return routes
}

func apiListPermissionsHandler(broker *APIPermissionBroker) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		principal, ok := PrincipalFrom(r.Context())
		if !ok || broker == nil {
			ApiNotFound(w, r, "permission")
			return
		}
		sessions, present := r.URL.Query()["session"]
		if present && len(sessions) != 1 {
			WriteAPIProblem(w, r, http.StatusBadRequest, "Bad Request", "session must be specified once", "", nil)
			return
		}
		admin := config.APITokenScopeAllows(principal.Scopes, "admin")
		items := broker.Pending(principal.TokenID, admin, r.URL.Query().Get("session"))
		WriteAPIResponse(w, r, http.StatusOK, items)
	}
}

func apiAnswerPermissionHandler(broker *APIPermissionBroker) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var payload apiPermissionAnswer
		if err := ApiDecodeStrictBody(r, &payload); err != nil || (payload.Decision != "allow" && payload.Decision != "deny") {
			WriteAPIProblem(w, r, http.StatusBadRequest, "Bad Request", "invalid permission decision", "", nil)
			return
		}
		principal, ok := PrincipalFrom(r.Context())
		if !ok || broker == nil {
			ApiNotFound(w, r, "permission")
			return
		}
		err := broker.Answer(r.PathValue("id"), principal.TokenID, payload.Decision, config.APITokenScopeAllows(principal.Scopes, "admin"))
		switch {
		case errors.Is(err, ErrAPIPermissionUnknown):
			ApiNotFound(w, r, "permission")
		case errors.Is(err, ErrAPIPermissionDecided):
			WriteAPIProblem(w, r, http.StatusConflict, "Conflict", "permission already decided", "", nil)
		case err != nil:
			WriteAPIProblem(w, r, http.StatusBadRequest, "Bad Request", "invalid permission decision", "", nil)
		default:
			WriteAPIResponse(w, r, http.StatusOK, struct {
				Decision string `json:"decision"`
			}{payload.Decision})
		}
	}
}

const (
	IdempotencyMaxEntries = 10_000
	IdempotencyTTL        = 24 * time.Hour
	IdempotencyStarted    = "started"
	IdempotencyReplay     = "replay"
	IdempotencyInProgress = "in_progress"
	IdempotencyConflict   = "different_request"
	IdempotencyFull       = "store_full"
)

var idempotencyKeyPattern = regexp.MustCompile(`^[A-Za-z0-9._:-]{1,128}$`)

type idempotencyResponse struct {
	Status  int
	Headers http.Header
	Body    []byte
}

type idempotencyEntry struct {
	bodyHash   string
	created    time.Time
	generation uint64
	complete   bool
	response   idempotencyResponse
}

// IdempotencyStore is process-local and non-durable; this is the v1 contract.
type IdempotencyStore struct {
	mu         sync.Mutex
	Entries    map[string]*idempotencyEntry
	now        func() time.Time
	generation uint64
}

func NewIdempotencyStore(now func() time.Time) *IdempotencyStore {
	if now == nil {
		now = time.Now
	}
	return &IdempotencyStore{Entries: make(map[string]*idempotencyEntry), now: now}
}

func ValidIdempotencyKey(key string) bool { return idempotencyKeyPattern.MatchString(key) }

func idempotencyMapKey(tokenID, key string) string { return tokenID + "\x00" + key }

func (s *IdempotencyStore) Begin(tokenID, key, bodyHash string) (string, *idempotencyResponse, uint64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	id := idempotencyMapKey(tokenID, key)
	if entry, ok := s.Entries[id]; ok {
		if entry.complete && now.Sub(entry.created) >= IdempotencyTTL {
			delete(s.Entries, id)
		} else if entry.bodyHash != bodyHash {
			return IdempotencyConflict, nil, 0
		} else if !entry.complete {
			return IdempotencyInProgress, nil, 0
		} else {
			copy := cloneIdempotencyResponse(entry.response)
			return IdempotencyReplay, &copy, 0
		}
	}
	s.evictExpiredLocked(now)
	if len(s.Entries) >= IdempotencyMaxEntries {
		s.evictOldestCompletedLocked()
	}
	if len(s.Entries) >= IdempotencyMaxEntries {
		return IdempotencyFull, nil, 0
	}
	s.generation++
	generation := s.generation
	s.Entries[id] = &idempotencyEntry{bodyHash: bodyHash, created: now, generation: generation}
	return IdempotencyStarted, nil, generation
}

func (s *IdempotencyStore) Complete(tokenID, key, bodyHash string, generation uint64, status int, headers http.Header, body []byte) {
	s.mu.Lock()
	defer s.mu.Unlock()
	id := idempotencyMapKey(tokenID, key)
	entry, ok := s.Entries[id]
	if !ok || entry.bodyHash != bodyHash || entry.generation != generation || entry.complete {
		return
	}
	if status < 200 || status >= 300 && status < 400 || status >= 500 {
		delete(s.Entries, id)
		return
	}
	entry.complete = true
	entry.response = idempotencyResponse{Status: status, Headers: selectedIdempotencyHeaders(headers), Body: bytes.Clone(body)}
}

func (s *IdempotencyStore) Release(tokenID, key string, generation uint64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	id := idempotencyMapKey(tokenID, key)
	if entry, ok := s.Entries[id]; ok && entry.generation == generation && !entry.complete {
		delete(s.Entries, id)
	}
}

func (s *IdempotencyStore) evictExpiredLocked(now time.Time) {
	for key, entry := range s.Entries {
		if entry.complete && now.Sub(entry.created) >= IdempotencyTTL {
			delete(s.Entries, key)
		}
	}
}

func (s *IdempotencyStore) evictOldestCompletedLocked() {
	var oldestKey string
	var oldest time.Time
	for key, entry := range s.Entries {
		if entry.complete && (oldestKey == "" || entry.created.Before(oldest)) {
			oldestKey, oldest = key, entry.created
		}
	}
	if oldestKey != "" {
		delete(s.Entries, oldestKey)
	}
}

func WriteIdempotencyHashPart(buffer *bytes.Buffer, part []byte) {
	var length [8]byte
	binary.BigEndian.PutUint64(length[:], uint64(len(part)))
	buffer.Write(length[:])
	buffer.Write(part)
}

func selectedIdempotencyHeaders(source http.Header) http.Header {
	result := make(http.Header)
	for _, name := range []string{"Content-Type", "Location"} {
		if values := source.Values(name); len(values) > 0 {
			result[http.CanonicalHeaderKey(name)] = append([]string(nil), values...)
		}
	}
	return result
}

func cloneIdempotencyResponse(source idempotencyResponse) idempotencyResponse {
	return idempotencyResponse{Status: source.Status, Headers: source.Headers.Clone(), Body: bytes.Clone(source.Body)}
}

func WithIdempotency(store *IdempotencyStore, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		keyValues := r.Header.Values("Idempotency-Key")
		if len(keyValues) == 0 {
			next.ServeHTTP(w, r)
			return
		}
		if len(keyValues) != 1 {
			WriteAPIProblem(w, r, http.StatusBadRequest, "Bad Request", "invalid Idempotency-Key", "", nil)
			return
		}
		value := keyValues[0]
		if !ValidIdempotencyKey(value) {
			WriteAPIProblem(w, r, http.StatusBadRequest, "Bad Request", "invalid Idempotency-Key", "", nil)
			return
		}
		principal, _ := PrincipalFrom(r.Context())
		body, err := io.ReadAll(r.Body)
		if err != nil {
			var maxBytesError *http.MaxBytesError
			if errors.As(err, &maxBytesError) {
				WriteAPIProblem(w, r, http.StatusRequestEntityTooLarge, "Request Entity Too Large", "request body too large", "", nil)
				return
			}
			WriteAPIProblem(w, r, http.StatusBadRequest, "Bad Request", "invalid request body", "", err)
			return
		}
		_ = r.Body.Close()
		r.Body = io.NopCloser(bytes.NewReader(body))
		var encoded bytes.Buffer
		WriteIdempotencyHashPart(&encoded, body)
		WriteIdempotencyHashPart(&encoded, []byte(r.Method))
		WriteIdempotencyHashPart(&encoded, []byte(r.URL.Path))
		hash := sha256.Sum256(encoded.Bytes())
		bodyHash := fmt.Sprintf("%x", hash)
		outcome, replay, generation := store.Begin(principal.TokenID, value, bodyHash)
		switch outcome {
		case IdempotencyReplay:
			for name, values := range replay.Headers {
				w.Header()[name] = append([]string(nil), values...)
			}
			w.Header().Set("Idempotent-Replayed", "true")
			w.WriteHeader(replay.Status)
			_, _ = w.Write(replay.Body)
		case IdempotencyInProgress:
			WriteAPIProblem(w, r, http.StatusConflict, "Conflict", "request in progress", "", nil)
		case IdempotencyConflict:
			WriteAPIProblem(w, r, http.StatusUnprocessableEntity, "Unprocessable Entity", "idempotency key reused with a different request", "", nil)
		case IdempotencyFull:
			WriteAPIProblem(w, r, http.StatusServiceUnavailable, "Service Unavailable", "idempotency store is full", "", nil)
		default:
			completed := false
			defer func() {
				if !completed {
					store.Release(principal.TokenID, value, generation)
				}
			}()
			capture := &idempotencyCapture{ResponseWriter: w, status: http.StatusOK}
			next.ServeHTTP(capture, r)
			store.Complete(principal.TokenID, value, bodyHash, generation, capture.status, capture.Header(), capture.body.Bytes())
			completed = true
		}
	})
}

type idempotencyCapture struct {
	http.ResponseWriter
	status int
	wrote  bool
	body   bytes.Buffer
}

func (w *idempotencyCapture) WriteHeader(status int) {
	if w.wrote {
		return
	}
	w.wrote = true
	w.status = status
	w.ResponseWriter.WriteHeader(status)
}

func (w *idempotencyCapture) Write(body []byte) (int, error) {
	if !w.wrote {
		w.WriteHeader(http.StatusOK)
	}
	w.body.Write(body)
	return w.ResponseWriter.Write(body)
}
