// port.go: designed port added by lane L3 (sprint 0 split, design
// docs/plans/2026-11-15-sprint0-split-server.md §2.2 + hard-rule escape
// "small local copy or designed port"). The TLS certificate-store machinery
// moved here unexported, but the server composer (server.go) and the TLS
// white tests that stay in package server until lanes L5/L7 still reach it.
// Thin one-line delegates only — no state, no mutable var exports. Lanes
// L5/L7 (consumers) delete the matching server-side wrappers; the final
// export set is settled at L7.
package listener

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"log"
	"time"
)

// NewTLSCertificateStore wraps newTLSCertificateStore for server.go:264 and
// the staying server TLS tests.
func NewTLSCertificateStore(policy ListenerTLS) (*TLSCertificateStore, error) {
	return newTLSCertificateStore(policy)
}

// TLSConfig wraps tlsConfig for server.go:269 and the staying server TLS tests.
func TLSConfig(policy ListenerTLS, store *TLSCertificateStore) (*tls.Config, error) {
	return tlsConfig(policy, store)
}

// LoadClientCAPool wraps loadClientCAPool for the staying server TLS tests.
func LoadClientCAPool(path string) (*x509.CertPool, error) {
	return loadClientCAPool(path)
}

// TLSReload wraps runTLSReload with the zero options the composer passes
// (server.go:287: runTLSReload(ctx, store, tlsReloadOptions{})); zero options
// select the production defaults inside runTLSReload.
func TLSReload(ctx context.Context, store *TLSCertificateStore) {
	runTLSReload(ctx, store, tlsReloadOptions{})
}

// TLSErrorLogger wraps tlsErrorLogger for server.go:374.
func TLSErrorLogger() *log.Logger { return tlsErrorLogger() }

// Reload wraps reload for the staying server TLS tests.
func (s *TLSCertificateStore) Reload() error { return s.reload() }

// ReloadAt wraps reloadAt for the staying server TLS tests.
func (s *TLSCertificateStore) ReloadAt(now time.Time) error { return s.reloadAt(now) }

// Pins wraps pins for Server.TLSPins (server.go:184) and the staying server
// TLS tests.
func (s *TLSCertificateStore) Pins() (TLSPins, error) { return s.pins() }

// CurrentSnapshot exposes the current certificate snapshot pointer for the
// identity comparisons in the staying server TLS tests (was store.current.Load()).
func (s *TLSCertificateStore) CurrentSnapshot() *certificateSnapshot { return s.current.Load() }

// CurrentCertificate exposes the leaf certificate of the current snapshot for
// the staying server TLS tests (was store.current.Load().cert).
func (s *TLSCertificateStore) CurrentCertificate() *tls.Certificate {
	snap := s.current.Load()
	if snap == nil {
		return nil
	}
	return snap.cert
}

// Configured wraps ListenerTLS.configured for server.go:262.
func (t ListenerTLS) Configured() bool { return t.configured() }

// HasCertificatePair wraps ListenerTLS.hasCertificatePair for
// server.go:240,250.
func (t ListenerTLS) HasCertificatePair() bool { return t.hasCertificatePair() }
