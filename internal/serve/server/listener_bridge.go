// listener_bridge.go: temporary compatibility surface for package server after
// lane L3 moved the listener policy + TLS leaf to
// internal/serve/server/listener (sprint 0 split, design
// docs/plans/2026-11-15-sprint0-split-server.md §2.2/§2.3). Deleter lane: L7
// removes the last names (composer + cmd aliases §2.3); consumers migrate in
// lanes L4-L6. Type aliases and const re-exports only for types/constants;
// one-line delegating wrappers for functions — no mutable var re-exports.
package server

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"log"
	"log/slog"
	"net/http"
	"time"

	appPkg "github.com/PHPCraftdream/rush/internal/app"
	"github.com/PHPCraftdream/rush/internal/config"
	"github.com/PHPCraftdream/rush/internal/serve/server/listener"
)

type (
	// Core L3 surface (design §2.2 / §2.3 cmd aliases).
	ListenerPolicy      = listener.ListenerPolicy
	ListenerSurface     = listener.ListenerSurface
	ListenerTLS         = listener.ListenerTLS
	ListenerLimits      = listener.ListenerLimits
	ListenerPolicyError = listener.ListenerPolicyError
	TLSPins             = listener.TLSPins
	// tlsCertificateStore aliases the designed port type (listener/port.go);
	// the Server.tlsStore field (server.go:40) keeps its name until L7.
	tlsCertificateStore = listener.TLSCertificateStore
)

const (
	SurfaceREST = listener.SurfaceREST
	SurfaceWS   = listener.SurfaceWS
	SurfaceUI   = listener.SurfaceUI
)

func DenyAllAuth(next http.Handler) http.Handler { return listener.DenyAllAuth(next) }

func ValidateListenerSet(policies []ListenerPolicy) error {
	return listener.ValidateListenerSet(policies)
}

func EffectiveListenerPins(declared map[string]string, tokens []config.APIToken, logger *slog.Logger) (map[string]string, error) {
	return listener.EffectiveListenerPins(declared, tokens, logger)
}

func ValidatePinnedPermissionModes(tokens []config.APIToken, pins map[string]string) error {
	return listener.ValidatePinnedPermissionModes(tokens, pins)
}

func ValidatePinnedPermissionModesAcrossPolicies(tokens []config.APIToken, policies []ListenerPolicy) error {
	return listener.ValidatePinnedPermissionModesAcrossPolicies(tokens, policies)
}

func PinnedTokenIDs(pins map[string]string) []string { return listener.PinnedTokenIDs(pins) }

func ListenerDeclaresPins(pins map[string]string) bool { return listener.ListenerDeclaresPins(pins) }

func ValidateEffectivePins(ctx context.Context, app *appPkg.App, tokens []config.APIToken, policies []ListenerPolicy, logger *slog.Logger) error {
	return listener.ValidateEffectivePins(ctx, app, tokens, policies, logger)
}

func EnsureSelfSignedCertificate(dataDir, address string, sans []string, now time.Time) (string, string, error) {
	return listener.EnsureSelfSignedCertificate(dataDir, address, sans, now)
}

func EnsureSelfSignedCertificateForListener(dataDir, identityAddress, address string, sans []string, now time.Time) (string, string, error) {
	return listener.EnsureSelfSignedCertificateForListener(dataDir, identityAddress, address, sans, now)
}

func TLSPinsForCertificatePEM(data []byte) (TLSPins, error) {
	return listener.TLSPinsForCertificatePEM(data)
}

func IsLoopbackHost(host string) bool { return listener.IsLoopbackHost(host) }

func OriginsAllowed(origin string, allowed []string) bool {
	return listener.OriginsAllowed(origin, allowed)
}

func ValidateAllowedOrigins(origins []string) error { return listener.ValidateAllowedOrigins(origins) }

// TLS machinery wrappers (designed port in listener/port.go): the composer
// (server.go) and the TLS white tests that stay in package server until L5/L7
// keep their unqualified call sites.

func newTLSCertificateStore(policy ListenerTLS) (*tlsCertificateStore, error) {
	return listener.NewTLSCertificateStore(policy)
}

func tlsConfig(policy ListenerTLS, store *tlsCertificateStore) (*tls.Config, error) {
	return listener.TLSConfig(policy, store)
}

func runTLSReload(ctx context.Context, store *tlsCertificateStore) { listener.TLSReload(ctx, store) }

func tlsErrorLogger() *log.Logger { return listener.TLSErrorLogger() }

func loadClientCAPool(path string) (*x509.CertPool, error) { return listener.LoadClientCAPool(path) }
