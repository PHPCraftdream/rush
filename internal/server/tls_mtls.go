package server

import (
	"crypto/x509"
	"errors"
	"fmt"
	"net/http"
	"os"
)

func loadClientCAPool(path string) (*x509.CertPool, error) {
	pem, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read client CA: %w", err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(pem) || len(pool.Subjects()) == 0 {
		return nil, errors.New("invalid or empty client CA PEM")
	}
	return pool, nil
}

// mtlsFingerprintAuth decorates an existing bearer-token principal only after
// TLS has verified a client certificate. Certificate identity never grants API scopes.
func mtlsFingerprintAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.TLS != nil && len(r.TLS.VerifiedChains) > 0 && len(r.TLS.PeerCertificates) > 0 {
			fingerprint := certificateFingerprint(r)
			if principal, ok := PrincipalFrom(r.Context()); ok {
				principal.CertFingerprint = fingerprint
				r = r.WithContext(WithPrincipal(r.Context(), principal))
				setAuditPrincipal(r, principal)
			}
		}
		next.ServeHTTP(w, r)
	})
}
