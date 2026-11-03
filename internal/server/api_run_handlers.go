package server

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/PHPCraftdream/rush/internal/config"
)

const (
	apiRunMaxFieldBytes = 100_000
	apiRunMaxBody       = apiRunMaxFieldBytes*6 + 4096
)

type apiRunCreateRequest struct {
	Prompt string `json:"prompt"`
	Role   string `json:"role,omitempty"`
	Model  string `json:"model,omitempty"`
}

type apiRunInjectRequest struct {
	Message string `json:"message"`
	Mode    string `json:"mode,omitempty"`
}

func apiDecodeStrictBody(r *http.Request, target any) error {
	body, err := io.ReadAll(io.LimitReader(r.Body, apiRunMaxBody+1))
	if err != nil {
		return err
	}
	if len(body) > apiRunMaxBody {
		return &http.MaxBytesError{Limit: apiRunMaxBody}
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
	setAPIResponseHeaders(w, r)
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
	return func(w http.ResponseWriter, r *http.Request) {
		var payload apiRunCreateRequest
		if err := apiDecodeStrictBody(r, &payload); err != nil || strings.TrimSpace(payload.Prompt) == "" || len(payload.Prompt) > apiRunMaxFieldBytes {
			WriteAPIProblem(w, r, http.StatusBadRequest, "Bad Request", "invalid request body", "", err)
			return
		}
		if _, err := apiRunRole(payload.Role); err != nil {
			WriteAPIProblem(w, r, http.StatusBadRequest, "Bad Request", "invalid role", "", nil)
			return
		}
		item, err := store.GetSession(r, r.PathValue("id"))
		if err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				apiNotFound(w, r, "session")
				return
			}
			apiReadFailure(w, r, err)
			return
		}
		if !store.OwnsSession(item) {
			apiNotFound(w, r, "session")
			return
		}
		if backend.Busy(item.ID) {
			WriteAPIProblem(w, r, http.StatusConflict, "Conflict", "session is busy", "", nil)
			return
		}
		principal, _ := PrincipalFrom(r.Context())
		run, err := registry.Create(item.ID, principal.TokenID)
		if err != nil {
			apiReadFailure(w, r, err)
			return
		}
		done, err := backend.Start(context.WithoutCancel(r.Context()), run.ID, item.ID, payload.Prompt, payload.Role, payload.Model)
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
			apiReadFailure(w, r, err)
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
		w.Header().Set("Location", apiBasePath+"/runs/"+run.ID)
		WriteAPIResponse(w, r, http.StatusAccepted, struct {
			RunID     string      `json:"run_id"`
			SessionID string      `json:"session_id"`
			State     APIRunState `json:"state"`
		}{run.ID, item.ID, APIRunRunning})
	}
}

func apiGetRunHandler(registry *APIRunRegistry) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		run, ok := apiOwnedRun(r, registry, r.PathValue("id"))
		if !ok {
			apiNotFound(w, r, "run")
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
			for !terminalAPIRunState(run.State) && run.State != APIRunAwaitingAnswer {
				snapshot, notify, exists := registry.SnapshotAndWait(run.ID)
				if !exists {
					apiNotFound(w, r, "run")
					return
				}
				run = snapshot
				if terminalAPIRunState(run.State) || run.State == APIRunAwaitingAnswer {
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
			apiNotFound(w, r, "run")
			return
		}
		registry.mu.Lock()
		_, exists := registry.runs[run.ID]
		activeID := ""
		var activeSequence uint64
		for id, candidate := range registry.runs {
			if candidate.run.SessionID == run.SessionID && (candidate.run.State == APIRunQueued || candidate.run.State == APIRunRunning) && candidate.sequence > activeSequence {
				activeID, activeSequence = id, candidate.sequence
			}
		}
		if !exists || activeID != run.ID {
			registry.mu.Unlock()
			current, found := apiOwnedRun(r, registry, run.ID)
			if !found {
				apiNotFound(w, r, "run")
				return
			}
			WriteAPIResponse(w, r, http.StatusOK, current)
			return
		}
		cancelled := backend.Cancel(run.SessionID, run.ID)
		registry.mu.Unlock()
		if !cancelled {
			current, exists := registry.Get(run.ID)
			if !exists {
				apiNotFound(w, r, "run")
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
		if err := apiDecodeStrictBody(r, &payload); err != nil || strings.TrimSpace(payload.Message) == "" || len(payload.Message) > apiRunMaxFieldBytes {
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
				apiNotFound(w, r, "session")
				return
			}
			apiReadFailure(w, r, err)
			return
		}
		if !store.OwnsSession(item) {
			apiNotFound(w, r, "session")
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
