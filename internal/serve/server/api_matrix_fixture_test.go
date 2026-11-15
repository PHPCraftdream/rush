package server

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"net/http"
	"strings"
	"testing"
	"time"

	appPkg "github.com/PHPCraftdream/rush/internal/app"
	"github.com/PHPCraftdream/rush/internal/config"
)

type channel struct {
	name string
	tls  bool
	ws   bool
}

var apiMatrixChannels = []channel{
	{name: "plain-http"},
	{name: "plain-ws", ws: true},
	{name: "tls-http", tls: true},
	{name: "tls-ws", tls: true, ws: true},
}

type apiMatrixFixture struct {
	channel channel
	base    string
	wsURL   string
	token   string
	app     *appPkg.App
	cert    *x509.Certificate
}

func newAPIMatrixFixture(t *testing.T, ch channel) *apiMatrixFixture {
	t.Helper()
	isolateAPIGlobals(t)
	workingDir := t.TempDir()
	app := newAttachmentsTestApp(t, workingDir, t.TempDir())
	store := config.GlobalAPITokenStore()
	token, _, err := store.CreateAPIToken("api matrix", []string{"read", "run"}, "ask", nil)
	if err != nil {
		t.Fatal(err)
	}
	policy := ListenerPolicy{
		Address: "127.0.0.1:0", Workspace: workingDir,
		Surfaces: []ListenerSurface{SurfaceREST, SurfaceWS},
		Limits:   ListenerLimits{MaxConnsPerToken: 20, RPS: 100000, Burst: 100000, MaxBody: 64},
	}
	var fixtureCert *x509.Certificate
	if ch.tls {
		cert, key, certificate := writeTLSTestPair(t, t.TempDir(), "api-matrix", time.Now(), []string{"localhost", "127.0.0.1"})
		policy.TLS = ListenerTLS{Cert: cert, Key: key}
		fixtureCert = certificate
	}
	api, err := NewServeAPI(policy, app, store, nil)
	if err != nil {
		t.Fatal(err)
	}
	api.Configure(&policy)
	srv := NewWithPolicy(app, policy, nil)
	base := startPolicyServer(t, srv)
	if ch.tls {
		base = "https" + strings.TrimPrefix(base, "http")
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := api.Stop(ctx); err != nil {
			t.Errorf("stop serve API: %v", err)
		}
	})
	return &apiMatrixFixture{channel: ch, base: base, wsURL: "ws" + strings.TrimPrefix(base, "http") + "/ws", token: token, app: app, cert: fixtureCert}
}

func (f *apiMatrixFixture) tlsConfig() *tls.Config {
	if f.cert == nil {
		return nil
	}
	roots := x509.NewCertPool()
	roots.AddCert(f.cert)
	return &tls.Config{RootCAs: roots, ServerName: "127.0.0.1", MinVersion: tls.VersionTLS12}
}

func (f *apiMatrixFixture) client() *http.Client {
	return &http.Client{Transport: &http.Transport{TLSClientConfig: f.tlsConfig()}, Timeout: 5 * time.Second}
}

func (f *apiMatrixFixture) scheme() string {
	if f.channel.ws {
		return "ws"
	}
	return "rest"
}

func matrixTokenParts(token string) (string, string) {
	parts := strings.SplitN(strings.TrimPrefix(token, "rush_"), ".", 2)
	if len(parts) != 2 {
		return "", ""
	}
	return parts[0], parts[1]
}
