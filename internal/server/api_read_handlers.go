package server

import (
	"context"
	"database/sql"
	"errors"
	"net/http"
	"sort"

	appPkg "github.com/PHPCraftdream/rush/internal/app"
	"github.com/PHPCraftdream/rush/internal/message"
	"github.com/PHPCraftdream/rush/internal/session"
)

// APIReadStore is the small read seam used by the session and message endpoints.
type APIReadStore interface {
	ListSessions(context.Context) ([]session.Session, error)
	GetSession(context.Context, string) (session.Session, error)
	ListMessages(context.Context, string) ([]message.Message, error)
	ListModels(context.Context) ([]APIModel, error)
}

type apiAppReadStore struct {
	app *appPkg.App
}

func (s apiAppReadStore) ListSessions(ctx context.Context) ([]session.Session, error) {
	return s.app.Sessions.List(ctx)
}

func (s apiAppReadStore) GetSession(ctx context.Context, id string) (session.Session, error) {
	return s.app.Sessions.Get(ctx, id)
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
	return append(append(apiCoreRoutes(), routes...), append(apiSessionRoutes(), append(apiRunRoutesWithPlaceholders(), apiPermissionRouteDeclarations()...)...)...)
}

// APIRoutesWithApp binds the read routes to the production App services.
func APIRoutesWithApp(a *appPkg.App) []APIRoute {
	routes, _, _ := apiRoutesWithBroker(a, nil, false)
	return routes
}

// apiRoutesWithBroker also returns the event log and run registry the routes share.
func apiRoutesWithBroker(a *appPkg.App, broker *APIPermissionBroker, allowAutoApprove bool) ([]APIRoute, *APIEventLog, *APIRunRegistry) {
	log := NewAPIEventLog(nil)
	registry := NewAPIRunRegistry(nil)
	routes := APIRoutesWithDeps(apiAppReadStore{app: a})
	writeRoutes := apiSessionWriteRoutes(apiAppWriteStore{app: a})
	runRoutes := apiRunWriteRoutesWithRegistry(apiAppWriteStore{app: a}, newAPIRunBackendWithPolicy(a, broker, allowAutoApprove), registry)
	permissionRoutes := apiPermissionRoutes(broker)
	routes = append(routes[:len(routes)-len(apiSessionRoutes())-len(apiRunRoutes())-len(apiPermissionRouteDeclarations())], append(append(writeRoutes, runRoutes...), permissionRoutes...)...)
	events := apiEventsRoute()
	events.Handler = newAPIEventsHandler(apiAppWriteStore{app: a}, log, nil, nil)
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
			apiReadFailure(w, r, err)
			return
		}
		sort.Slice(sessions, func(i, j int) bool {
			if sessions[i].UpdatedAt != sessions[j].UpdatedAt {
				return sessions[i].UpdatedAt > sessions[j].UpdatedAt
			}
			return sessions[i].ID < sessions[j].ID
		})
		start, ok := cursorStart(sessions, page.After, func(item session.Session) string { return item.ID })
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
			items = append(items, apiSessionFromModel(item))
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
				apiNotFound(w, r, "session")
				return
			}
			apiReadFailure(w, r, err)
			return
		}
		WriteAPIResponse(w, r, http.StatusOK, apiSessionFromModel(item))
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
				apiNotFound(w, r, "session")
				return
			}
			apiReadFailure(w, r, err)
			return
		}
		items, err := store.ListMessages(r.Context(), sessionID)
		if err != nil {
			apiReadFailure(w, r, err)
			return
		}
		start, ok := cursorStart(items, page.After, func(item message.Message) string { return item.ID })
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
			projected = append(projected, apiMessageFromModel(item))
			ids = append(ids, item.ID)
		}
		WritePaginatedResponse(w, r, http.StatusOK, projected, ids, hasMore)
	}
}

func apiListModelsHandler(store APIReadStore) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		items, err := store.ListModels(r.Context())
		if err != nil {
			apiReadFailure(w, r, err)
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

func cursorStart[T any](items []T, after string, id func(T) string) (int, bool) {
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

func apiNotFound(w http.ResponseWriter, r *http.Request, kind string) {
	WriteAPIProblem(w, r, http.StatusNotFound, "Not Found", kind+" not found", "", nil)
}

func apiReadFailure(w http.ResponseWriter, r *http.Request, err error) {
	WriteAPIProblem(w, r, http.StatusInternalServerError, "Internal Server Error", "", "", err)
}
