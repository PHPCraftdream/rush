package server

import (
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/url"
	"regexp"
	"strings"
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
