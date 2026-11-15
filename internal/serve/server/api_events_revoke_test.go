package server

import (
	"context"
	"testing"
	"time"

	"github.com/PHPCraftdream/rush/internal/config"
)

type revokeTicker struct{ ch chan time.Time }

func (t revokeTicker) C() <-chan time.Time { return t.ch }
func (revokeTicker) Stop()                 {}

type revokedTokenVerifier struct{}

func (revokedTokenVerifier) VerifyAPIToken(string, time.Time) (config.APIToken, bool, error) {
	return config.APIToken{}, false, nil
}
func (revokedTokenVerifier) Touch(string) {}
func (revokedTokenVerifier) ShowAPIToken(string) (config.APIToken, error) {
	return config.APIToken{}, config.ErrAPITokenNotFound
}

func TestAPIEventsRevocationTickerClosesOnRevokedPrincipal(t *testing.T) {
	ticker := revokeTicker{ch: make(chan time.Time, 1)}
	principal := Principal{TokenID: "deadbeef", verifier: revokedTokenVerifier{}, tickerFactory: func(time.Duration) Ticker { return ticker }}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	closed := apiSSERevocation(ctx, principal)
	ticker.ch <- time.Now()
	select {
	case <-closed:
	case <-time.After(3 * time.Second):
		t.Fatal("revocation watcher did not close for revoked principal")
	}
}
