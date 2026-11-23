package api

import (
	"bytes"
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/PHPCraftdream/rush/internal/agent"
	"github.com/PHPCraftdream/rush/internal/config"
	"github.com/PHPCraftdream/rush/internal/serve/server/wire"
	session "github.com/PHPCraftdream/rush/internal/session/service"
)

const (
	ApiRunMaxFieldBytes = 100_000
	ApiRunMaxBody       = ApiRunMaxFieldBytes*6 + 4096
)

type apiRunCreateRequest struct {
	Prompt        string `json:"prompt"`
	Role          string `json:"role,omitempty"`
	Model         string `json:"model,omitempty"`
	MaxAgentDepth *int   `json:"max_agent_depth,omitempty"`
}

type apiRunInjectRequest struct {
	Message string `json:"message"`
	Mode    string `json:"mode,omitempty"`
}

func ApiDecodeStrictBody(r *http.Request, target any) error {
	body, err := io.ReadAll(io.LimitReader(r.Body, ApiRunMaxBody+1))
	if err != nil {
		return err
	}
	if len(body) > ApiRunMaxBody {
		return &http.MaxBytesError{Limit: ApiRunMaxBody}
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	if decoder.Decode(new(any)) != io.EOF {
		return errors.New("invalid request body")
	}
	return nil
}

func writeAPIRunGateUnavailable(w http.ResponseWriter, r *http.Request) {
	SetAPIResponseHeaders(w, r)
	w.Header().Set("Content-Type", "application/problem+json")
	w.WriteHeader(http.StatusServiceUnavailable)
	_ = json.NewEncoder(w).Encode(APIProblem{
		Type:   "about:blank",
		Title:  "Service Unavailable",
		Status: http.StatusServiceUnavailable,
		Detail: "permission gate unavailable",
	})
}

func apiCreateRunHandler(store APIWriteStore, backend APIRunBackend, registry *APIRunRegistry) http.HandlerFunc {
	var snapshotFn func() (*config.Config, error)
	if appBackend, ok := backend.(*AppAPIRunBackend); ok && appBackend.app != nil {
		snapshotFn = func() (*config.Config, error) { cfg, _ := appBackend.app.Store().Snapshot(); return cfg, nil }
	}
	return APIRunCreateHandlerWithSnapshot(store, backend, registry, snapshotFn)
}

func APIRunCreateHandlerWithSnapshot(store APIWriteStore, backend APIRunBackend, registry *APIRunRegistry, snapshotFn func() (*config.Config, error)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var payload apiRunCreateRequest
		if err := ApiDecodeStrictBody(r, &payload); err != nil || strings.TrimSpace(payload.Prompt) == "" || len(payload.Prompt) > ApiRunMaxFieldBytes {
			WriteAPIProblem(w, r, http.StatusBadRequest, "Bad Request", "invalid request body", "", err)
			return
		}
		var selection *APIRunSelection
		if snapshotFn != nil {
			cfg, snapshotErr := snapshotFn()
			if snapshotErr != nil || cfg == nil {
				WriteAPIProblem(w, r, http.StatusServiceUnavailable, "Service Unavailable", "role configuration unavailable", "", snapshotErr)
				return
			}
			if _, err := ValidateAPIRunSelection(cfg, payload.Role, payload.Model); err != nil {
				WriteAPIProblem(w, r, http.StatusBadRequest, "Bad Request", err.Error(), "", nil)
				return
			}
			selection = &APIRunSelection{Config: cfg}
			if payload.Role != "" {
				if parsed, roleErr := apiRunRole(payload.Role); roleErr == nil && parsed == "" {
					selection.Contract, roleErr = agent.ResolveRoleContract(cfg, payload.Role, nil)
					if roleErr != nil {
						WriteAPIProblem(w, r, http.StatusBadRequest, "Bad Request", roleErr.Error(), "", nil)
						return
					}
				}
			}
		} else {
			parsed, parseErr := apiRunRole(payload.Role)
			if parseErr != nil || (payload.Role != "" && parsed == "") {
				WriteAPIProblem(w, r, http.StatusBadRequest, "Bad Request", "invalid role", "", nil)
				return
			}
		}
		maxAgentDepth := 2
		if payload.MaxAgentDepth != nil {
			maxAgentDepth = *payload.MaxAgentDepth
		}
		if maxAgentDepth < 0 || maxAgentDepth > 32 {
			WriteAPIProblem(w, r, http.StatusBadRequest, "Bad Request", "max_agent_depth must be between 0 and 32", "", nil)
			return
		}
		item, err := store.GetSession(r, r.PathValue("id"))
		if err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				ApiNotFound(w, r, "session")
				return
			}
			ApiReadFailure(w, r, err)
			return
		}
		if !store.OwnsSession(item) {
			ApiNotFound(w, r, "session")
			return
		}
		if backend.Busy(item.ID) {
			WriteAPIProblem(w, r, http.StatusConflict, "Conflict", "session is busy", "", nil)
			return
		}
		principal, _ := PrincipalFrom(r.Context())
		run, err := registry.Create(item.ID, principal.TokenID)
		if err != nil {
			ApiReadFailure(w, r, err)
			return
		}
		done, err := startAPIRunWithDepth(backend, context.WithoutCancel(r.Context()), run.ID, item.ID, payload.Prompt, payload.Role, payload.Model, maxAgentDepth, selection)
		if err != nil {
			_, _ = registry.Transition(run.ID, APIRunFailed, "start_failed", "run could not be started", "")
			switch {
			case errors.Is(err, ErrAPIRunBackendBusy):
				WriteAPIProblem(w, r, http.StatusConflict, "Conflict", "session is busy", "", nil)
			case errors.Is(err, ErrAPIRunBackendGateUnavailable):
				writeAPIRunGateUnavailable(w, r)
			case errors.Is(err, ErrAPIRunAutoApproveDenied):
				WriteAPIProblem(w, r, http.StatusForbidden, "Forbidden", "auto-approve not permitted on this listener", "", nil)
			case errors.Is(err, ErrAPIRunBackendInvalidModel), errors.Is(err, ErrAPIRunBackendInvalidRole):
				WriteAPIProblem(w, r, http.StatusBadRequest, "Bad Request", err.Error(), "", nil)
			default:
				WriteAPIProblem(w, r, http.StatusServiceUnavailable, "Service Unavailable", "run service unavailable", "", err)
			}
			return
		}
		if _, err := registry.Transition(run.ID, APIRunRunning, "", "", ""); err != nil {
			ApiReadFailure(w, r, err)
			return
		}
		go func() {
			outcome, ok := <-done
			if !ok {
				_, _ = registry.Transition(run.ID, APIRunFailed, "run_failed", "run ended without an outcome", "")
				return
			}
			_, _ = registry.Transition(run.ID, outcome.State, outcome.ErrorCode, outcome.ErrorSummary, outcome.ResultMessageID)
		}()
		w.Header().Set("Location", ApiBasePath+"/runs/"+run.ID)
		WriteAPIResponse(w, r, http.StatusAccepted, struct {
			RunID     string      `json:"run_id"`
			SessionID string      `json:"session_id"`
			State     APIRunState `json:"state"`
		}{run.ID, item.ID, APIRunRunning})
	}
}

type APIRunSelection struct {
	Config   *config.Config
	Contract *session.RoleContract
}
type apiRunSelectionBackend interface {
	StartWithSelection(context.Context, string, string, string, string, string, int, *APIRunSelection) (<-chan RunOutcome, error)
}
type apiRunDepthBackend interface {
	StartWithDepth(context.Context, string, string, string, string, string, int) (<-chan RunOutcome, error)
}

func startAPIRunWithDepth(backend APIRunBackend, ctx context.Context, runID, sessionID, prompt, role, model string, depth int, selection *APIRunSelection) (<-chan RunOutcome, error) {
	if selected, ok := backend.(apiRunSelectionBackend); ok {
		return selected.StartWithSelection(ctx, runID, sessionID, prompt, role, model, depth, selection)
	}
	if depthBackend, ok := backend.(apiRunDepthBackend); ok {
		return depthBackend.StartWithDepth(ctx, runID, sessionID, prompt, role, model, depth)
	}
	return backend.Start(ctx, runID, sessionID, prompt, role, model)
}

func apiGetRunHandler(registry *APIRunRegistry) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		run, ok := apiOwnedRun(r, registry, r.PathValue("id"))
		if !ok {
			ApiNotFound(w, r, "run")
			return
		}
		waitValues, present := r.URL.Query()["wait"]
		if present {
			if len(waitValues) != 1 {
				WriteAPIProblem(w, r, http.StatusBadRequest, "Bad Request", "wait must be an integer from 1 to 30", "", nil)
				return
			}
			seconds, err := strconv.Atoi(waitValues[0])
			if err != nil || seconds < 1 || seconds > 30 {
				WriteAPIProblem(w, r, http.StatusBadRequest, "Bad Request", "wait must be an integer from 1 to 30", "", nil)
				return
			}
			deadline := time.NewTimer(time.Duration(seconds) * time.Second)
			defer deadline.Stop()
			for !wire.TerminalAPIRunState(run.State) && run.State != APIRunAwaitingAnswer {
				snapshot, notify, exists := registry.SnapshotAndWait(run.ID)
				if !exists {
					ApiNotFound(w, r, "run")
					return
				}
				run = snapshot
				if wire.TerminalAPIRunState(run.State) || run.State == APIRunAwaitingAnswer {
					break
				}
				select {
				case <-r.Context().Done():
					run, _ = apiOwnedRun(r, registry, run.ID)
					goto respond
				case <-deadline.C:
					run, _ = apiOwnedRun(r, registry, run.ID)
					goto respond
				case <-notify:
				}
			}
		}
	respond:
		WriteAPIResponse(w, r, http.StatusOK, run)
	}
}

func apiOwnedRun(r *http.Request, registry *APIRunRegistry, id string) (APIRun, bool) {
	p, ok := PrincipalFrom(r.Context())
	if !ok {
		return APIRun{}, false
	}
	if config.APITokenScopeAllows(p.Scopes, "admin") {
		return registry.Get(id)
	}
	return registry.GetOwned(id, p.TokenID)
}

func apiCancelRunHandler(registry *APIRunRegistry, backend APIRunBackend) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		run, ok := apiOwnedRun(r, registry, r.PathValue("id"))
		if !ok {
			ApiNotFound(w, r, "run")
			return
		}
		registry.Mu.Lock()
		_, exists := registry.Runs[run.ID]
		activeID := ""
		var activeSequence uint64
		for id, candidate := range registry.Runs {
			if candidate.run.SessionID == run.SessionID && (candidate.run.State == APIRunQueued || candidate.run.State == APIRunRunning) && candidate.Sequence > activeSequence {
				activeID, activeSequence = id, candidate.Sequence
			}
		}
		if !exists || activeID != run.ID {
			registry.Mu.Unlock()
			current, found := apiOwnedRun(r, registry, run.ID)
			if !found {
				ApiNotFound(w, r, "run")
				return
			}
			WriteAPIResponse(w, r, http.StatusOK, current)
			return
		}
		cancelled := backend.Cancel(run.SessionID, run.ID)
		registry.Mu.Unlock()
		if !cancelled {
			current, exists := registry.Get(run.ID)
			if !exists {
				ApiNotFound(w, r, "run")
				return
			}
			WriteAPIResponse(w, r, http.StatusOK, current)
			return
		}
		WriteAPIResponse(w, r, http.StatusAccepted, struct {
			State APIRunState `json:"state"`
		}{run.State})
	}
}

func apiInjectHandler(store APIWriteStore, backend APIRunBackend, registry *APIRunRegistry) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var payload apiRunInjectRequest
		if err := ApiDecodeStrictBody(r, &payload); err != nil || strings.TrimSpace(payload.Message) == "" || len(payload.Message) > ApiRunMaxFieldBytes {
			WriteAPIProblem(w, r, http.StatusBadRequest, "Bad Request", "invalid request body", "", err)
			return
		}
		if payload.Mode == "" {
			payload.Mode = "merge"
		}
		if payload.Mode != "merge" && payload.Mode != "interrupt" {
			WriteAPIProblem(w, r, http.StatusBadRequest, "Bad Request", "invalid mode", "", nil)
			return
		}
		item, err := store.GetSession(r, r.PathValue("id"))
		if err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				ApiNotFound(w, r, "session")
				return
			}
			ApiReadFailure(w, r, err)
			return
		}
		if !store.OwnsSession(item) {
			ApiNotFound(w, r, "session")
			return
		}
		if !registry.IsSessionActive(item.ID) {
			WriteAPIProblem(w, r, http.StatusConflict, "Conflict", "no active run", "", nil)
			return
		}
		if !backend.Inject(item.ID, payload.Message, payload.Mode) {
			WriteAPIProblem(w, r, http.StatusConflict, "Conflict", "no active run", "", nil)
			return
		}
		WriteAPIResponse(w, r, http.StatusAccepted, map[string]string{"state": "accepted"})
	}
}

const (
	APIRunMaxRetained = 1000
	APIRunTTL         = 24 * time.Hour
)

var (
	ErrAPIRunNotFound     = errors.New("API run not found")
	ErrAPIRunInvalidState = errors.New("invalid API run state transition")
	ErrAPIRunRegistryFull = errors.New("API run registry is full")
	ErrAPIRunIDGeneration = errors.New("could not generate API run ID")
)

type APIRunError struct {
	Code    string `json:"code"`
	Summary string `json:"summary"`
}

type APIRun struct {
	ID         string       `json:"id"`
	SessionID  string       `json:"session_id"`
	TokenID    string       `json:"-"`
	State      APIRunState  `json:"state"`
	CreatedAt  time.Time    `json:"created_at"`
	StartedAt  *time.Time   `json:"started_at,omitempty"`
	FinishedAt *time.Time   `json:"finished_at,omitempty"`
	Error      *APIRunError `json:"error,omitempty"`
	MessageID  string       `json:"message_id,omitempty"`
}

type apiRunEntry struct {
	run      APIRun
	notify   chan struct{}
	Sequence uint64
}

// APIRunRegistry is process-local and should be created once per API route set.
type APIRunRegistry struct {
	Mu       sync.Mutex
	Runs     map[string]*apiRunEntry
	now      func() time.Time
	sequence uint64
	observer func(APIRun)
}

func NewAPIRunRegistry(now func() time.Time) *APIRunRegistry {
	if now == nil {
		now = time.Now
	}
	return &APIRunRegistry{Runs: make(map[string]*apiRunEntry), now: now}
}

func (r *APIRunRegistry) Create(sessionID, tokenID string) (APIRun, error) {
	r.Mu.Lock()
	defer r.Mu.Unlock()
	now := r.now().UTC()
	r.evictExpiredLocked(now)
	if len(r.Runs) >= APIRunMaxRetained {
		r.evictOldestFinishedLocked()
	}
	if len(r.Runs) >= APIRunMaxRetained {
		return APIRun{}, ErrAPIRunRegistryFull
	}
	id, err := r.newIDLocked()
	if err != nil {
		return APIRun{}, err
	}
	r.sequence++
	run := APIRun{ID: id, SessionID: sessionID, TokenID: tokenID, State: APIRunQueued, CreatedAt: now}
	r.Runs[id] = &apiRunEntry{run: run, notify: make(chan struct{}), Sequence: r.sequence}
	return cloneAPIRun(run), nil
}

func (r *APIRunRegistry) newIDLocked() (string, error) {
	for range APIRunMaxRetained + 1 {
		id, err := newAPIRunID()
		if err != nil {
			return "", err
		}
		if _, exists := r.Runs[id]; !exists {
			return id, nil
		}
	}
	return "", ErrAPIRunIDGeneration
}

func newAPIRunID() (string, error) {
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", errors.Join(ErrAPIRunIDGeneration, err)
	}
	return "run_" + hex.EncodeToString(raw[:]), nil
}

func (r *APIRunRegistry) Get(id string) (APIRun, bool) {
	r.Mu.Lock()
	defer r.Mu.Unlock()
	r.evictExpiredLocked(r.now())
	entry, ok := r.Runs[id]
	if !ok {
		return APIRun{}, false
	}
	return cloneAPIRun(entry.run), true
}

func (r *APIRunRegistry) GetOwned(id, tokenID string) (APIRun, bool) {
	run, ok := r.Get(id)
	return run, ok && run.TokenID == tokenID
}

// Transition updates a run only when the requested state is a legal successor.
func (r *APIRunRegistry) Transition(id string, state APIRunState, errorCode, errorSummary, resultMessageID string) (APIRun, error) {
	r.Mu.Lock()
	defer r.Mu.Unlock()
	r.evictExpiredLocked(r.now())
	entry, ok := r.Runs[id]
	if !ok {
		return APIRun{}, ErrAPIRunNotFound
	}
	if !ValidAPIRunTransition(entry.run.State, state) {
		return cloneAPIRun(entry.run), ErrAPIRunInvalidState
	}
	now := r.now().UTC()
	entry.run.State = state
	if state == APIRunRunning && entry.run.StartedAt == nil {
		entry.run.StartedAt = timePointer(now.UTC())
	}
	if wire.TerminalAPIRunState(state) {
		entry.run.FinishedAt = timePointer(now.UTC())
	}
	if errorCode != "" || errorSummary != "" {
		entry.run.Error = &APIRunError{Code: errorCode, Summary: errorSummary}
	} else {
		entry.run.Error = nil
	}
	entry.run.MessageID = resultMessageID
	close(entry.notify)
	entry.notify = make(chan struct{})
	updated := cloneAPIRun(entry.run)
	if r.observer != nil {
		r.observer(updated)
	}
	return updated, nil
}

func (r *APIRunRegistry) SetObserver(observer func(APIRun)) {
	r.Mu.Lock()
	r.observer = observer
	r.Mu.Unlock()
}

func ValidAPIRunTransition(from, to APIRunState) bool {
	if wire.TerminalAPIRunState(from) {
		return false
	}
	switch from {
	case APIRunQueued:
		return to == APIRunRunning || to == APIRunCancelled || to == APIRunFailed
	case APIRunRunning:
		return to == APIRunAwaitingAnswer || to == APIRunSucceeded || to == APIRunFailed || to == APIRunCancelled
	case APIRunAwaitingAnswer:
		return to == APIRunRunning || to == APIRunFailed || to == APIRunCancelled
	default:
		return false
	}
}

func (r *APIRunRegistry) SnapshotAndWait(id string) (APIRun, <-chan struct{}, bool) {
	r.Mu.Lock()
	defer r.Mu.Unlock()
	r.evictExpiredLocked(r.now().UTC())
	entry, ok := r.Runs[id]
	if !ok {
		return APIRun{}, nil, false
	}
	return cloneAPIRun(entry.run), entry.notify, true
}

func (r *APIRunRegistry) WaitChannel(id string) (<-chan struct{}, bool) {
	_, wait, ok := r.SnapshotAndWait(id)
	return wait, ok
}

func (r *APIRunRegistry) IsSessionActive(id string) bool {
	r.Mu.Lock()
	defer r.Mu.Unlock()
	r.evictExpiredLocked(r.now())
	for _, entry := range r.Runs {
		if entry.run.SessionID == id && (entry.run.State == APIRunQueued || entry.run.State == APIRunRunning) {
			return true
		}
	}
	return false
}

func (r *APIRunRegistry) ActiveSessionIDs() []string {
	r.Mu.Lock()
	defer r.Mu.Unlock()
	r.evictExpiredLocked(r.now())
	active := make(map[string]struct{})
	for _, entry := range r.Runs {
		if entry.run.State == APIRunQueued || entry.run.State == APIRunRunning {
			active[entry.run.SessionID] = struct{}{}
		}
	}
	ids := make([]string, 0, len(active))
	for id := range active {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}

func (r *APIRunRegistry) evictExpiredLocked(now time.Time) {
	for id, entry := range r.Runs {
		if wire.TerminalAPIRunState(entry.run.State) && entry.run.FinishedAt != nil && now.Sub(*entry.run.FinishedAt) >= APIRunTTL {
			delete(r.Runs, id)
		}
	}
}

func (r *APIRunRegistry) evictOldestFinishedLocked() {
	var oldestID string
	var oldestFinished time.Time
	for id, entry := range r.Runs {
		if !wire.TerminalAPIRunState(entry.run.State) || entry.run.FinishedAt == nil {
			continue
		}
		if oldestID == "" || entry.run.FinishedAt.Before(oldestFinished) {
			oldestID, oldestFinished = id, *entry.run.FinishedAt
		}
	}
	if oldestID != "" {
		delete(r.Runs, oldestID)
	}
}

func timePointer(value time.Time) *time.Time { return &value }

func cloneAPIRun(run APIRun) APIRun {
	run.CreatedAt = run.CreatedAt.UTC()
	if run.StartedAt != nil {
		run.StartedAt = timePointer(run.StartedAt.UTC())
	}
	if run.FinishedAt != nil {
		run.FinishedAt = timePointer(run.FinishedAt.UTC())
	}
	if run.Error != nil {
		errorCopy := *run.Error
		run.Error = &errorCopy
	}
	return run
}
