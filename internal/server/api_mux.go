package server

import (
	"fmt"
	"log/slog"
	"net/http"
	"sort"
	"strings"

	"github.com/PHPCraftdream/rush/internal/config"
)

const apiBasePath = "/api/v1"

func NewAPIHandler(routes []APIRoute) http.Handler {
	return apiRouteMux(routes)
}

func NewAPIHandlerSplit(routes []APIRoute) (public http.Handler, protected http.Handler) {
	publicRoutes, protectedRoutes := make([]APIRoute, 0), make([]APIRoute, 0)
	for _, route := range routes {
		if route.Scope == "none" {
			publicRoutes = append(publicRoutes, route)
		} else {
			protectedRoutes = append(protectedRoutes, route)
		}
	}
	return apiRouteMux(publicRoutes), apiRouteMux(protectedRoutes)
}

func apiRouteMux(routes []APIRoute) http.Handler {
	mux := http.NewServeMux()
	methods := http.NewServeMux()
	methodSets := make(map[string]map[string]struct{})
	for _, route := range routes {
		path := apiBasePath + route.Pattern
		wrapped := route
		mux.Handle(route.Method+" "+path, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			protectAPIRoute(wrapped).ServeHTTP(w, r)
		}))
		if methodSets[path] == nil {
			methods.Handle(path, http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
			methodSets[path] = make(map[string]struct{})
		}
		methodSets[path][route.Method] = struct{}{}
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		idResponse := &apiTrackingWriter{ResponseWriter: w}
		setAPIResponseHeaders(idResponse, r)
		idRequest := r.Clone(r.Context())
		idRequest.Header = r.Header.Clone()
		idRequest.Header.Set("X-Request-Id", idResponse.Header().Get("X-Request-Id"))
		defer func() {
			if value := recover(); value != nil {
				slog.Error("API handler panic", "panic", Redact(fmt.Sprint(value)))
				if !idResponse.wrote {
					WriteAPIProblem(idResponse, idRequest, http.StatusInternalServerError, "Internal Server Error", "", "", fmt.Errorf("handler panic"))
				}
			}
		}()

		_, pattern := mux.Handler(idRequest)
		if pattern == "" {
			_, pathPattern := methods.Handler(idRequest)
			available := methodSets[pathPattern]
			if len(available) == 0 {
				WriteAPIProblem(idResponse, idRequest, http.StatusNotFound, "Not Found", "", "", nil)
				return
			}
			allowed := make([]string, 0, len(available))
			for method := range available {
				allowed = append(allowed, method)
			}
			sort.Strings(allowed)
			idResponse.Header().Set("Allow", strings.Join(allowed, ", "))
			WriteAPIProblem(idResponse, idRequest, http.StatusMethodNotAllowed, "Method Not Allowed", "", "", nil)
			return
		}
		// ServeMux.ServeHTTP, not the looked-up handler, fills r.PathValue.
		mux.ServeHTTP(idResponse, idRequest)
	})
}

// Unwrap lets http.ResponseController reach Flush (SSE) and Hijack.
func (w *apiTrackingWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

type apiTrackingWriter struct {
	http.ResponseWriter
	wrote bool
}

func (w *apiTrackingWriter) WriteHeader(status int) {
	if w.wrote {
		return
	}
	w.wrote = true
	w.ResponseWriter.WriteHeader(status)
}

func (w *apiTrackingWriter) Write(body []byte) (int, error) {
	if !w.wrote {
		w.WriteHeader(http.StatusOK)
	}
	return w.ResponseWriter.Write(body)
}

func protectAPIRoute(route APIRoute) http.Handler {
	if route.Scope == "none" {
		return route.Handler
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p, ok := PrincipalFrom(r.Context())
		if !ok {
			WriteAPIProblem(w, r, http.StatusInternalServerError, "Internal Server Error", "", "", fmt.Errorf("missing authenticated principal"))
			return
		}
		if status := resolvePinnedRoute(p.PinnedSession, route, r); status != 0 {
			if status == http.StatusNotFound {
				WriteAPIProblem(w, r, status, "Not Found", "resource not found", "", nil)
			} else {
				WriteAPIProblem(w, r, status, "Forbidden", "pinned-token", "", nil)
			}
			return
		}
		if !config.APITokenScopeAllows(p.Scopes, route.Scope) {
			WriteAPIProblem(w, r, http.StatusForbidden, "Forbidden", "insufficient scope", "", nil)
			return
		}
		route.Handler.ServeHTTP(w, r)
	})
}
