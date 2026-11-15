package server

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"sync"
	"time"
)

const (
	idempotencyMaxEntries = 10_000
	idempotencyTTL        = 24 * time.Hour
	idempotencyStarted    = "started"
	idempotencyReplay     = "replay"
	idempotencyInProgress = "in_progress"
	idempotencyConflict   = "different_request"
	idempotencyFull       = "store_full"
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

// idempotencyStore is process-local and non-durable; this is the v1 contract.
type idempotencyStore struct {
	mu         sync.Mutex
	entries    map[string]*idempotencyEntry
	now        func() time.Time
	generation uint64
}

func newIdempotencyStore(now func() time.Time) *idempotencyStore {
	if now == nil {
		now = time.Now
	}
	return &idempotencyStore{entries: make(map[string]*idempotencyEntry), now: now}
}

func validIdempotencyKey(key string) bool { return idempotencyKeyPattern.MatchString(key) }

func idempotencyMapKey(tokenID, key string) string { return tokenID + "\x00" + key }

func (s *idempotencyStore) Begin(tokenID, key, bodyHash string) (string, *idempotencyResponse, uint64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	id := idempotencyMapKey(tokenID, key)
	if entry, ok := s.entries[id]; ok {
		if entry.complete && now.Sub(entry.created) >= idempotencyTTL {
			delete(s.entries, id)
		} else if entry.bodyHash != bodyHash {
			return idempotencyConflict, nil, 0
		} else if !entry.complete {
			return idempotencyInProgress, nil, 0
		} else {
			copy := cloneIdempotencyResponse(entry.response)
			return idempotencyReplay, &copy, 0
		}
	}
	s.evictExpiredLocked(now)
	if len(s.entries) >= idempotencyMaxEntries {
		s.evictOldestCompletedLocked()
	}
	if len(s.entries) >= idempotencyMaxEntries {
		return idempotencyFull, nil, 0
	}
	s.generation++
	generation := s.generation
	s.entries[id] = &idempotencyEntry{bodyHash: bodyHash, created: now, generation: generation}
	return idempotencyStarted, nil, generation
}

func (s *idempotencyStore) Complete(tokenID, key, bodyHash string, generation uint64, status int, headers http.Header, body []byte) {
	s.mu.Lock()
	defer s.mu.Unlock()
	id := idempotencyMapKey(tokenID, key)
	entry, ok := s.entries[id]
	if !ok || entry.bodyHash != bodyHash || entry.generation != generation || entry.complete {
		return
	}
	if status < 200 || status >= 300 && status < 400 || status >= 500 {
		delete(s.entries, id)
		return
	}
	entry.complete = true
	entry.response = idempotencyResponse{Status: status, Headers: selectedIdempotencyHeaders(headers), Body: bytes.Clone(body)}
}

func (s *idempotencyStore) Release(tokenID, key string, generation uint64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	id := idempotencyMapKey(tokenID, key)
	if entry, ok := s.entries[id]; ok && entry.generation == generation && !entry.complete {
		delete(s.entries, id)
	}
}

func (s *idempotencyStore) evictExpiredLocked(now time.Time) {
	for key, entry := range s.entries {
		if entry.complete && now.Sub(entry.created) >= idempotencyTTL {
			delete(s.entries, key)
		}
	}
}

func (s *idempotencyStore) evictOldestCompletedLocked() {
	var oldestKey string
	var oldest time.Time
	for key, entry := range s.entries {
		if entry.complete && (oldestKey == "" || entry.created.Before(oldest)) {
			oldestKey, oldest = key, entry.created
		}
	}
	if oldestKey != "" {
		delete(s.entries, oldestKey)
	}
}

func writeIdempotencyHashPart(buffer *bytes.Buffer, part []byte) {
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

func withIdempotency(store *idempotencyStore, next http.Handler) http.Handler {
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
		if !validIdempotencyKey(value) {
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
		writeIdempotencyHashPart(&encoded, body)
		writeIdempotencyHashPart(&encoded, []byte(r.Method))
		writeIdempotencyHashPart(&encoded, []byte(r.URL.Path))
		hash := sha256.Sum256(encoded.Bytes())
		bodyHash := fmt.Sprintf("%x", hash)
		outcome, replay, generation := store.Begin(principal.TokenID, value, bodyHash)
		switch outcome {
		case idempotencyReplay:
			for name, values := range replay.Headers {
				w.Header()[name] = append([]string(nil), values...)
			}
			w.Header().Set("Idempotent-Replayed", "true")
			w.WriteHeader(replay.Status)
			_, _ = w.Write(replay.Body)
		case idempotencyInProgress:
			WriteAPIProblem(w, r, http.StatusConflict, "Conflict", "request in progress", "", nil)
		case idempotencyConflict:
			WriteAPIProblem(w, r, http.StatusUnprocessableEntity, "Unprocessable Entity", "idempotency key reused with a different request", "", nil)
		case idempotencyFull:
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
