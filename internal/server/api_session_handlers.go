package server

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"net/http"

	appPkg "github.com/PHPCraftdream/rush/internal/app"
	"github.com/PHPCraftdream/rush/internal/message"
	"github.com/PHPCraftdream/rush/internal/session"
)

type APIWriteStore interface {
	CreateSession(*http.Request, string, message.Origin) (session.Session, error)
	ForkSession(*http.Request, string, string) (session.Session, error)
	DeleteSession(*http.Request, string) error
	GetSession(*http.Request, string) (session.Session, error)
	SessionBusy(string) bool
	OwnsSession(session.Session) bool
}

type apiAppWriteStore struct{ app *appPkg.App }

func (s apiAppWriteStore) CreateSession(r *http.Request, title string, origin message.Origin) (session.Session, error) {
	if creator, ok := s.app.Sessions.(session.OriginCreator); ok {
		return creator.CreateWithOrigin(r.Context(), title, origin)
	}
	return s.app.Sessions.Create(r.Context(), title)
}

func (s apiAppWriteStore) ForkSession(r *http.Request, id, title string) (session.Session, error) {
	return s.app.Sessions.ForkSession(r.Context(), id, title)
}

func (s apiAppWriteStore) DeleteSession(r *http.Request, id string) error {
	cfg := s.app.Config()
	if cfg == nil || cfg.Options == nil || cfg.Options.DataDirectory == "" {
		return errors.New("cannot verify session lock")
	}
	probe, err := session.TryHoldSessionLockShared(cfg.Options.DataDirectory, id)
	if err != nil {
		return err
	}
	defer probe.Release()
	return s.app.Sessions.Delete(r.Context(), id)
}

func (s apiAppWriteStore) GetSession(r *http.Request, id string) (session.Session, error) {
	return hideServiceSession(s.app.Sessions.Get(r.Context(), id))
}

func (s apiAppWriteStore) SessionBusy(id string) bool {
	if s.app.AgentCoordinator != nil && s.app.AgentCoordinator.IsSessionBusy(id) {
		return true
	}
	cfg := s.app.Config()
	if cfg == nil || cfg.Options == nil || cfg.Options.DataDirectory == "" {
		return true
	}
	probe, err := session.TryHoldSessionLockShared(cfg.Options.DataDirectory, id)
	if err != nil {
		return true
	}
	probe.Release()
	return false
}

func (s apiAppWriteStore) OwnsSession(item session.Session) bool { return appOwnsSession(s.app, item) }

func apiSessionWriteRoutes(store APIWriteStore) []APIRoute {
	// One store per route set (one per serve listener), not per process.
	idempotency := newIdempotencyStore(nil)
	routes := apiSessionRoutes()
	routes[0].Handler = withIdempotency(idempotency, http.HandlerFunc(apiCreateSessionHandler(store))).ServeHTTP
	routes[1].Handler = withIdempotency(idempotency, http.HandlerFunc(apiForkSessionHandler(store))).ServeHTTP
	routes[2].Handler = apiDeleteSessionHandler(store)
	return routes
}

func apiDecodeSessionTitle(r *http.Request) (string, error) {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		return "", err
	}
	if len(body) == 0 {
		return "", nil
	}
	var payload struct {
		Title string `json:"title"`
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&payload); err != nil {
		return "", err
	}
	if decoder.Decode(new(any)) != io.EOF {
		return "", errors.New("invalid JSON body")
	}
	return payload.Title, nil
}

func apiCreateSessionHandler(store APIWriteStore) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		title, err := apiDecodeSessionTitle(r)
		if err != nil {
			var maxBytesError *http.MaxBytesError
			if errors.As(err, &maxBytesError) {
				WriteAPIProblem(w, r, http.StatusRequestEntityTooLarge, "Request Entity Too Large", "request body too large", "", nil)
				return
			}
			WriteAPIProblem(w, r, 400, "Bad Request", "invalid request body", "", err)
			return
		}
		if title == "" {
			title = "New Session"
		}
		item, err := store.CreateSession(r, title, message.OriginAPI)
		if err != nil {
			apiReadFailure(w, r, err)
			return
		}
		w.Header().Set("Location", apiBasePath+"/sessions/"+item.ID)
		WriteAPIResponse(w, r, http.StatusCreated, apiSessionFromModel(item))
	}
}

func apiForkSessionHandler(store APIWriteStore) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		title, err := apiDecodeSessionTitle(r)
		if err != nil {
			var maxBytesError *http.MaxBytesError
			if errors.As(err, &maxBytesError) {
				WriteAPIProblem(w, r, http.StatusRequestEntityTooLarge, "Request Entity Too Large", "request body too large", "", nil)
				return
			}
			WriteAPIProblem(w, r, 400, "Bad Request", "invalid request body", "", err)
			return
		}
		item, err := store.ForkSession(r, r.PathValue("id"), title)
		if err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				apiNotFound(w, r, "session")
				return
			}
			apiReadFailure(w, r, err)
			return
		}
		w.Header().Set("Location", apiBasePath+"/sessions/"+item.ID)
		WriteAPIResponse(w, r, http.StatusCreated, apiSessionFromModel(item))
	}
}

func apiDeleteSessionHandler(store APIWriteStore) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		item, err := store.GetSession(r, id)
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
		if store.SessionBusy(id) {
			WriteAPIProblem(w, r, 409, "Conflict", "session is busy", "", nil)
			return
		}
		if err := store.DeleteSession(r, id); err != nil {
			var busyErr *session.SessionLockBusyError
			if errors.As(err, &busyErr) {
				WriteAPIProblem(w, r, http.StatusConflict, "Conflict", "session is busy", "", nil)
				return
			}
			if errors.Is(err, sql.ErrNoRows) {
				apiNotFound(w, r, "session")
				return
			}
			apiReadFailure(w, r, err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}
}
