package api

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"sort"

	appPkg "github.com/PHPCraftdream/rush/internal/app"
	"github.com/PHPCraftdream/rush/internal/serve/server/wire"
	"github.com/PHPCraftdream/rush/internal/serve/server/ws/wsapi"
	sessionlock "github.com/PHPCraftdream/rush/internal/session/proc/sessionlock"
	core "github.com/PHPCraftdream/rush/internal/session/service"
	"github.com/PHPCraftdream/rush/internal/store/message"
)

// APIReadStore is the small read seam used by the session and message endpoints.
type APIReadStore interface {
	ListSessions(context.Context) ([]core.Session, error)
	GetSession(context.Context, string) (core.Session, error)
	ListMessages(context.Context, string) ([]message.Message, error)
	ListModels(context.Context) ([]APIModel, error)
}

type apiAppReadStore struct {
	app *appPkg.App
}

func (s apiAppReadStore) ListSessions(ctx context.Context) ([]core.Session, error) {
	items, err := s.app.Sessions.List(ctx)
	if err != nil {
		return nil, err
	}
	return withoutServiceSessions(items), nil
}

func (s apiAppReadStore) GetSession(ctx context.Context, id string) (core.Session, error) {
	return hideServiceSession(s.app.Sessions.Get(ctx, id))
}

// withoutServiceSessions drops the API service sessions that own batch roots:
// they are not conversations and are never listed.
func withoutServiceSessions(items []core.Session) []core.Session {
	out := make([]core.Session, 0, len(items))
	for _, item := range items {
		if item.Origin != message.OriginAPIService {
			out = append(out, item)
		}
	}
	return out
}

// hideServiceSession makes a service session look missing to the session routes:
// it never accepts runs, injections, reads or event streams.
func hideServiceSession(item core.Session, err error) (core.Session, error) {
	if err == nil && item.Origin == message.OriginAPIService {
		return core.Session{}, sql.ErrNoRows
	}
	return item, err
}

func (s apiAppReadStore) ListMessages(ctx context.Context, id string) ([]message.Message, error) {
	return s.app.Messages.List(ctx, id)
}

func (s apiAppReadStore) ListModels(context.Context) ([]APIModel, error) {
	return apiModelsFromApp(s.app), nil
}

func APIRoutesWithDeps(deps APIReadStore) []APIRoute {
	routes := apiReadRoutes()
	for i := range routes {
		switch routes[i].Pattern {
		case "/sessions":
			routes[i].Handler = apiListSessionsHandler(deps)
		case "/sessions/{id}":
			routes[i].Handler = apiGetSessionHandler(deps)
		case "/sessions/{id}/messages":
			routes[i].Handler = apiListMessagesHandler(deps)
		case "/models":
			routes[i].Handler = apiListModelsHandler(deps)
		}
	}
	return append(append(apiCoreRoutes(), routes...), append(apiSessionRoutes(), append(apiRunRoutesWithPlaceholders(), append(apiPermissionRouteDeclarations(), BatchRoutePlaceholders()...)...)...)...)
}

// APIRoutesWithBroker also returns the event log and run registry the routes share.
func APIRoutesWithBroker(a *appPkg.App, broker *APIPermissionBroker, allowAutoApprove bool, wiredBatchRoutes func(*appPkg.App) []APIRoute) ([]APIRoute, *APIEventLog, *APIRunRegistry) {
	log := NewAPIEventLog(nil)
	registry := NewAPIRunRegistry(nil)
	routes := APIRoutesWithDeps(apiAppReadStore{app: a})
	writeRoutes := APISessionWriteRoutes(APIAppWriteStore{App: a})
	runRoutes := apiRunWriteRoutesWithRegistry(APIAppWriteStore{App: a}, NewAPIRunBackendWithPolicy(a, broker, allowAutoApprove), registry)
	permissionRoutes := APIPermissionRoutes(broker)
	batchRoutes := BatchRoutePlaceholders()
	if a != nil && wiredBatchRoutes != nil {
		batchRoutes = wiredBatchRoutes(a)
	}
	routes = append(routes[:len(routes)-len(apiSessionRoutes())-len(apiRunRoutes())-len(apiPermissionRouteDeclarations())-len(batchRoutes)], append(append(append(writeRoutes, runRoutes...), permissionRoutes...), batchRoutes...)...)
	events := apiEventsRoute()
	events.Handler = NewAPIEventsHandler(APIAppWriteStore{App: a}, log, nil, nil)
	return append(routes, events), log, registry
}

func apiListSessionsHandler(store APIReadStore) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		page, err := ParsePagination(r)
		if err != nil {
			WriteAPIProblem(w, r, http.StatusBadRequest, "Bad Request", err.Error(), "", nil)
			return
		}
		sessions, err := store.ListSessions(r.Context())
		if err != nil {
			ApiReadFailure(w, r, err)
			return
		}
		if principal, ok := PrincipalFrom(r.Context()); ok && principal.PinnedSession != "" {
			sessions = pinnedSessionsOnly(sessions, principal.PinnedSession)
		}
		sort.Slice(sessions, func(i, j int) bool {
			if sessions[i].UpdatedAt != sessions[j].UpdatedAt {
				return sessions[i].UpdatedAt > sessions[j].UpdatedAt
			}
			return sessions[i].ID < sessions[j].ID
		})
		start, ok := CursorStart(sessions, page.After, func(item core.Session) string { return item.ID })
		if !ok {
			apiInvalidCursor(w, r)
			return
		}
		end := start + page.Limit
		hasMore := end < len(sessions)
		if end > len(sessions) {
			end = len(sessions)
		}
		items := make([]APISession, 0, end-start)
		ids := make([]string, 0, end-start)
		for _, item := range sessions[start:end] {
			items = append(items, wire.APISessionFromModel(item))
			ids = append(ids, item.ID)
		}
		WritePaginatedResponse(w, r, http.StatusOK, items, ids, hasMore)
	}
}

func apiGetSessionHandler(store APIReadStore) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		item, err := store.GetSession(r.Context(), r.PathValue("id"))
		if err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				ApiNotFound(w, r, "session")
				return
			}
			ApiReadFailure(w, r, err)
			return
		}
		WriteAPIResponse(w, r, http.StatusOK, wire.APISessionFromModel(item))
	}
}

func apiListMessagesHandler(store APIReadStore) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		page, err := ParsePagination(r)
		if err != nil {
			WriteAPIProblem(w, r, http.StatusBadRequest, "Bad Request", err.Error(), "", nil)
			return
		}
		sessionID := r.PathValue("id")
		if _, err := store.GetSession(r.Context(), sessionID); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				ApiNotFound(w, r, "session")
				return
			}
			ApiReadFailure(w, r, err)
			return
		}
		items, err := store.ListMessages(r.Context(), sessionID)
		if err != nil {
			ApiReadFailure(w, r, err)
			return
		}
		start, ok := CursorStart(items, page.After, func(item message.Message) string { return item.ID })
		if !ok {
			apiInvalidCursor(w, r)
			return
		}
		end := start + page.Limit
		hasMore := end < len(items)
		if end > len(items) {
			end = len(items)
		}
		projected := make([]APIMessage, 0, end-start)
		ids := make([]string, 0, end-start)
		for _, item := range items[start:end] {
			projected = append(projected, wire.APIMessageFromModel(item))
			ids = append(ids, item.ID)
		}
		WritePaginatedResponse(w, r, http.StatusOK, projected, ids, hasMore)
	}
}

func apiListModelsHandler(store APIReadStore) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		items, err := store.ListModels(r.Context())
		if err != nil {
			ApiReadFailure(w, r, err)
			return
		}
		if items == nil {
			items = []APIModel{}
		}
		WriteAPIResponse(w, r, http.StatusOK, items)
	}
}

func apiModelsFromApp(a *appPkg.App) []APIModel {
	wire, ok := buildConfigWire(a)
	if !ok {
		return []APIModel{}
	}
	models := make([]APIModel, 0)
	for providerID, provider := range wire.Providers {
		for _, model := range provider.Models {
			models = append(models, APIModel{ID: model.ID, Name: model.Name, Provider: providerID})
		}
	}
	sort.Slice(models, func(i, j int) bool {
		if models[i].Provider != models[j].Provider {
			return models[i].Provider < models[j].Provider
		}
		return models[i].ID < models[j].ID
	})
	return models
}

func CursorStart[T any](items []T, after string, id func(T) string) (int, bool) {
	if after == "" {
		return 0, true
	}
	for i := range items {
		if id(items[i]) == after {
			return i + 1, true
		}
	}
	return 0, false
}

func apiInvalidCursor(w http.ResponseWriter, r *http.Request) {
	// Unknown cursors (including IDs from another session) are rejected rather
	// than silently restarting or returning an empty page.
	WriteAPIProblem(w, r, http.StatusBadRequest, "Bad Request", "after cursor not found", "", nil)
}

func ApiNotFound(w http.ResponseWriter, r *http.Request, kind string) {
	WriteAPIProblem(w, r, http.StatusNotFound, "Not Found", kind+" not found", "", nil)
}

func ApiReadFailure(w http.ResponseWriter, r *http.Request, err error) {
	WriteAPIProblem(w, r, http.StatusInternalServerError, "Internal Server Error", "", "", err)
}

type APIWriteStore interface {
	CreateSession(*http.Request, string, message.Origin) (core.Session, error)
	ForkSession(*http.Request, string, string) (core.Session, error)
	DeleteSession(*http.Request, string) error
	GetSession(*http.Request, string) (core.Session, error)
	SessionBusy(string) bool
	OwnsSession(core.Session) bool
}

type APIAppWriteStore struct{ App *appPkg.App }

func (s APIAppWriteStore) CreateSession(r *http.Request, title string, origin message.Origin) (core.Session, error) {
	if creator, ok := s.App.Sessions.(core.OriginCreator); ok {
		return creator.CreateWithOrigin(r.Context(), title, origin)
	}
	return s.App.Sessions.Create(r.Context(), title)
}

func (s APIAppWriteStore) ForkSession(r *http.Request, id, title string) (core.Session, error) {
	return s.App.Sessions.ForkSession(r.Context(), id, title)
}

func (s APIAppWriteStore) DeleteSession(r *http.Request, id string) error {
	cfg := s.App.Config()
	if cfg == nil || cfg.Options == nil || cfg.Options.DataDirectory == "" {
		return errors.New("cannot verify session lock")
	}
	probe, err := sessionlock.TryHoldSessionLockShared(cfg.Options.DataDirectory, id)
	if err != nil {
		return err
	}
	defer probe.Release()
	return s.App.Sessions.Delete(r.Context(), id)
}

func (s APIAppWriteStore) GetSession(r *http.Request, id string) (core.Session, error) {
	return hideServiceSession(s.App.Sessions.Get(r.Context(), id))
}

func (s APIAppWriteStore) SessionBusy(id string) bool {
	if s.App.AgentCoordinator != nil && s.App.AgentCoordinator.IsSessionBusy(id) {
		return true
	}
	cfg := s.App.Config()
	if cfg == nil || cfg.Options == nil || cfg.Options.DataDirectory == "" {
		return true
	}
	probe, err := sessionlock.TryHoldSessionLockShared(cfg.Options.DataDirectory, id)
	if err != nil {
		return true
	}
	probe.Release()
	return false
}

func (s APIAppWriteStore) OwnsSession(item core.Session) bool {
	return wsapi.AppOwnsSession(s.App, item)
}

func APISessionWriteRoutes(store APIWriteStore) []APIRoute {
	// One store per route set (one per serve listener), not per process.
	idempotency := NewIdempotencyStore(nil)
	routes := apiSessionRoutes()
	routes[0].Handler = WithIdempotency(idempotency, http.HandlerFunc(apiCreateSessionHandler(store))).ServeHTTP
	routes[1].Handler = WithIdempotency(idempotency, http.HandlerFunc(apiForkSessionHandler(store))).ServeHTTP
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
			ApiReadFailure(w, r, err)
			return
		}
		w.Header().Set("Location", ApiBasePath+"/sessions/"+item.ID)
		WriteAPIResponse(w, r, http.StatusCreated, wire.APISessionFromModel(item))
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
				ApiNotFound(w, r, "session")
				return
			}
			ApiReadFailure(w, r, err)
			return
		}
		w.Header().Set("Location", ApiBasePath+"/sessions/"+item.ID)
		WriteAPIResponse(w, r, http.StatusCreated, wire.APISessionFromModel(item))
	}
}

func apiDeleteSessionHandler(store APIWriteStore) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		item, err := store.GetSession(r, id)
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
		if store.SessionBusy(id) {
			WriteAPIProblem(w, r, 409, "Conflict", "session is busy", "", nil)
			return
		}
		if err := store.DeleteSession(r, id); err != nil {
			var busyErr *sessionlock.SessionLockBusyError
			if errors.As(err, &busyErr) {
				WriteAPIProblem(w, r, http.StatusConflict, "Conflict", "session is busy", "", nil)
				return
			}
			if errors.Is(err, sql.ErrNoRows) {
				ApiNotFound(w, r, "session")
				return
			}
			ApiReadFailure(w, r, err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}
}
