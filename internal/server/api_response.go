package server

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"regexp"
	"strconv"
	"strings"
)

var requestIDPattern = regexp.MustCompile(`^[A-Za-z0-9._-]{1,64}$`)

type APIProblem struct {
	Type     string `json:"type"`
	Title    string `json:"title"`
	Status   int    `json:"status"`
	Detail   string `json:"detail,omitempty"`
	Instance string `json:"instance,omitempty"`
}

// WriteAPIResponse applies the common response headers and writes a JSON payload.
func WriteAPIResponse(w http.ResponseWriter, r *http.Request, status int, payload any) {
	setAPIResponseHeaders(w, r)
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(payload); err != nil {
		slog.Error("API response encoding failed", "error", Redact(err.Error()))
	}
}

// WriteAPIProblem writes an RFC 7807 response. Internal failures always use a
// fixed body; their cause is logged only after secret redaction.
func WriteAPIProblem(w http.ResponseWriter, r *http.Request, status int, title, detail, instance string, cause error) {
	problem := APIProblem{Type: "about:blank", Title: title, Status: status, Detail: detail, Instance: instance}
	if status >= http.StatusInternalServerError {
		problem = APIProblem{Type: "about:blank", Title: "Internal Server Error", Status: status}
		if cause != nil {
			slog.Error("API request failed", "error", Redact(cause.Error()))
		}
	}
	setAPIResponseHeaders(w, r)
	w.Header().Set("Content-Type", "application/problem+json")
	w.WriteHeader(problem.Status)
	if err := json.NewEncoder(w).Encode(problem); err != nil {
		slog.Error("API problem encoding failed", "error", Redact(err.Error()))
	}
}

func setAPIResponseHeaders(w http.ResponseWriter, r *http.Request) {
	id := w.Header().Get("X-Request-Id")
	if !requestIDPattern.MatchString(id) {
		id = ""
		if r != nil {
			id = r.Header.Get("X-Request-Id")
		}
		if !requestIDPattern.MatchString(id) {
			var bytes [16]byte
			if _, err := rand.Read(bytes[:]); err != nil {
				id = strings.Repeat("0", 32)
			} else {
				id = hex.EncodeToString(bytes[:])
			}
		}
	}
	w.Header().Set("X-Request-Id", id)
	w.Header().Set("Cache-Control", "no-store")
	if w.Header().Get("Content-Type") == "" {
		w.Header().Set("Content-Type", "application/json")
	}
}

type Pagination struct {
	After string
	Limit int
}

// ParsePagination parses ?after=<id>&limit=<n>; an absent limit defaults to 50.
func ParsePagination(r *http.Request) (Pagination, error) {
	result := Pagination{After: r.URL.Query().Get("after"), Limit: 50}
	values, present := r.URL.Query()["limit"]
	if !present {
		return result, nil
	}
	if len(values) != 1 || values[0] == "" {
		return Pagination{}, errors.New("limit must be an integer from 1 to 200")
	}
	limit, err := strconv.Atoi(values[0])
	if err != nil || limit < 1 || limit > 200 {
		return Pagination{}, errors.New("limit must be an integer from 1 to 200")
	}
	result.Limit = limit
	return result, nil
}

// WritePaginatedResponse emits the common cursor envelope. The caller supplies
// non-nil items in endpoint order (sessions: updated_at descending then id;
// messages: creation order) and IDs in that same order. next_after is non-null
// only when hasMore is true and an ID is available.
func WritePaginatedResponse(w http.ResponseWriter, r *http.Request, status int, items any, ids []string, hasMore bool) {
	var next any
	if hasMore && len(ids) > 0 {
		next = ids[len(ids)-1]
	}
	WriteAPIResponse(w, r, status, struct {
		Items     any `json:"items"`
		NextAfter any `json:"next_after"`
	}{Items: items, NextAfter: next})
}
