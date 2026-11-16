package apiauth

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"time"
)

type auditKey struct{}

type auditState struct {
	mu      sync.Mutex
	token   string
	session string
	scope   string
	cert    string
}

func auditFrom(ctx context.Context) *auditState {
	s, _ := ctx.Value(auditKey{}).(*auditState)
	return s
}

func setAuditToken(r *http.Request, id string) {
	if s := auditFrom(r.Context()); s != nil {
		s.mu.Lock()
		s.token = id
		s.mu.Unlock()
	}
}

func setAuditPrincipal(r *http.Request, p Principal) {
	if s := auditFrom(r.Context()); s != nil {
		s.mu.Lock()
		s.token = p.TokenID
		s.cert = p.CertFingerprint
		s.scope = strings.Join(p.Scopes, ",")
		s.mu.Unlock()
	}
}

func RecordScope(ctx context.Context, scope string) {
	if s := auditFrom(ctx); s != nil {
		s.mu.Lock()
		s.scope = scope
		s.mu.Unlock()
	}
}

func RecordSession(ctx context.Context, id string) {
	if s := auditFrom(ctx); s != nil {
		s.mu.Lock()
		s.session = id
		s.mu.Unlock()
	}
}

func auditMiddleware(log *slog.Logger, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		s := &auditState{scope: "none"}
		r = r.WithContext(context.WithValue(r.Context(), auditKey{}, s))
		sw := &statusWriter{ResponseWriter: w, status: http.StatusOK}
		defer func() {
			recovered := recover()
			status := sw.status
			if recovered != nil {
				status = http.StatusInternalServerError
			}
			auditLine(log, r, s, status, start, sw.bytes)
			if recovered != nil {
				panic(recovered)
			}
		}()
		next.ServeHTTP(sw, r)
	})
}

func auditLine(log *slog.Logger, r *http.Request, s *auditState, status int, start time.Time, bytes int64) {
	s.mu.Lock()
	token, scope, session, cert := s.token, s.scope, s.session, s.cert
	s.mu.Unlock()
	route := r.Pattern
	if route == "" {
		route = "unmatched"
	}
	log.Info(Redact("API request"), "token_id", Redact(token), "scope", Redact(scope), "method", Redact(r.Method), "route", Redact(route), "status", status, "session_id", Redact(session), "duration", time.Since(start), "remote_addr", Redact(r.RemoteAddr), "certificate_fingerprint", Redact(cert), "bytes", bytes)
}

type statusWriter struct {
	http.ResponseWriter
	status      int
	bytes       int64
	wroteHeader bool
}

func (w *statusWriter) WriteHeader(code int) {
	if !w.wroteHeader {
		w.wroteHeader = true
		w.status = code
		w.ResponseWriter.WriteHeader(code)
	}
}

func (w *statusWriter) Write(b []byte) (int, error) {
	if !w.wroteHeader {
		w.WriteHeader(http.StatusOK)
	}
	n, err := w.ResponseWriter.Write(b)
	w.bytes += int64(n)
	return n, err
}

func (w *statusWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

func (w *statusWriter) Flush() {
	if !w.wroteHeader {
		w.WriteHeader(http.StatusOK)
	}
	// ResponseController walks Unwrap chains; a direct http.Flusher assertion would
	// silently drop the flush when a wrapper below does not implement Flusher itself.
	_ = http.NewResponseController(w.ResponseWriter).Flush()
}

func (w *statusWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	h, ok := w.ResponseWriter.(http.Hijacker)
	if !ok {
		return nil, nil, http.ErrNotSupported
	}
	return h.Hijack()
}

var (
	_ http.Flusher  = (*statusWriter)(nil)
	_ http.Hijacker = (*statusWriter)(nil)
)

var (
	secretPattern      = regexp.MustCompile(`rush_[0-9a-f]{8}\.[A-Za-z0-9_-]{43}`)
	authValuePattern   = regexp.MustCompile(`(?i)(authorization\s*:\s*)([^\r\n]+)`)
	querySecretPattern = regexp.MustCompile(`(?i)(\b(?:token|access_token)=)[^&\s"']*`)
)

func Redact(s string) string {
	s = secretPattern.ReplaceAllStringFunc(s, func(token string) string { return token[:13] + ".[REDACTED]" })
	s = authValuePattern.ReplaceAllString(s, "${1}[REDACTED]")
	return querySecretPattern.ReplaceAllString(s, "${1}[REDACTED]")
}

func RedactURL(u *url.URL) *url.URL {
	if u == nil {
		return nil
	}
	c := *u
	q := c.Query()
	for _, k := range []string{"token", "access_token"} {
		if q.Has(k) {
			q.Set(k, "[REDACTED]")
		}
	}
	c.RawQuery = q.Encode()
	c.User = nil
	c.Path = Redact(c.Path)
	return &c
}

func RedactHeader(h http.Header) http.Header {
	out := h.Clone()
	for k := range out {
		if strings.EqualFold(k, "Authorization") {
			out.Set(k, "[REDACTED]")
			continue
		}
		for i, v := range out[k] {
			out[k][i] = Redact(v)
		}
	}
	return out
}

func certificateFingerprint(r *http.Request) string {
	if r.TLS == nil || len(r.TLS.PeerCertificates) == 0 {
		return ""
	}
	sum := sha256.Sum256(r.TLS.PeerCertificates[0].Raw)
	return hex.EncodeToString(sum[:])
}
