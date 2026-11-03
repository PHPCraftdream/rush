package server

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/pem"
	"log/slog"
	"math/big"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/PHPCraftdream/rush/internal/config"
)

func TestServeAPIMTLSProductionListenerRequiresVerifiedClientCertificate(t *testing.T) {
	for _, key := range []string{"RUSH_GLOBAL_DATA", "RUSH_GLOBAL_CONFIG", "RUSH_GLOBAL_DIR"} {
		t.Setenv(key, t.TempDir())
	}
	var logs syncBuffer
	previousLogger := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
	t.Cleanup(func() { slog.SetDefault(previousLogger) })
	dir := t.TempDir()
	now := time.Now()
	caKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	caTemplate := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "integration CA"}, NotBefore: now.Add(-time.Hour), NotAfter: now.Add(time.Hour), IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature}
	caDER, err := x509.CreateCertificate(rand.Reader, caTemplate, caTemplate, &caKey.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	caCert, err := x509.ParseCertificate(caDER)
	if err != nil {
		t.Fatal(err)
	}
	serverKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	serverTemplate := &x509.Certificate{SerialNumber: big.NewInt(2), Subject: pkix.Name{CommonName: "localhost"}, DNSNames: []string{"localhost"}, IPAddresses: []net.IP{net.ParseIP("127.0.0.1")}, NotBefore: now.Add(-time.Hour), NotAfter: now.Add(time.Hour), KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}
	serverDER, err := x509.CreateCertificate(rand.Reader, serverTemplate, caCert, &serverKey.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	serverKeyDER, err := x509.MarshalECPrivateKey(serverKey)
	if err != nil {
		t.Fatal(err)
	}
	serverCertPath, serverKeyPath := filepath.Join(dir, "server.crt"), filepath.Join(dir, "server.key")
	write := func(path string, data []byte) {
		if err := os.WriteFile(path, data, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write(serverCertPath, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: serverDER}))
	write(serverKeyPath, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: serverKeyDER}))
	caPath := filepath.Join(dir, "ca.crt")
	write(caPath, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: caDER}))
	clientKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	clientTemplate := &x509.Certificate{SerialNumber: big.NewInt(3), Subject: pkix.Name{CommonName: "integration client"}, NotBefore: now.Add(-time.Hour), NotAfter: now.Add(time.Hour), KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}}
	clientDER, err := x509.CreateCertificate(rand.Reader, clientTemplate, caCert, &clientKey.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	clientKeyDER, err := x509.MarshalECPrivateKey(clientKey)
	if err != nil {
		t.Fatal(err)
	}
	clientCert, err := tls.X509KeyPair(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: clientDER}), pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: clientKeyDER}))
	if err != nil {
		t.Fatal(err)
	}
	store := config.GlobalAPITokenStore()
	token, tokenRecord, err := store.CreateAPIToken("mtls-integration", []string{"read"}, "ask", nil)
	if err != nil {
		t.Fatal(err)
	}
	policy := ListenerPolicy{Address: "127.0.0.1:0", Surfaces: []ListenerSurface{SurfaceREST}, TLS: ListenerTLS{Cert: serverCertPath, Key: serverKeyPath, ClientCA: caPath}}
	api, err := NewServeAPI(policy, nil, store, nil)
	if err != nil {
		t.Fatal(err)
	}
	api.Configure(&policy)
	srv := NewWithPolicy(nil, policy, nil)
	ctx, cancel := context.WithCancel(t.Context())
	ready := make(chan string, 1)
	done := make(chan error, 1)
	go func() { done <- srv.Start(ctx, func(addr string) { ready <- addr }) }()
	var addr string
	select {
	case addr = <-ready:
	case err := <-done:
		t.Fatalf("server start: %v", err)
	case <-time.After(5 * time.Second):
		t.Fatal("server startup timeout")
	}
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Errorf("server stop: %v", err)
			}
		case <-time.After(5 * time.Second):
			t.Error("server shutdown timeout")
		}
		stopCtx, stopCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer stopCancel()
		if err := api.Stop(stopCtx); err != nil {
			t.Errorf("API stop: %v", err)
		}
	})
	roots := x509.NewCertPool()
	roots.AddCert(caCert)
	client := func(withCert bool) *http.Client {
		cfg := &tls.Config{RootCAs: roots, ServerName: "localhost"}
		if withCert {
			cfg.Certificates = []tls.Certificate{clientCert}
		}
		return &http.Client{Transport: &http.Transport{TLSClientConfig: cfg}, Timeout: 5 * time.Second}
	}
	request := func(c *http.Client, bearer string) *http.Response {
		req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, "https://"+addr+"/api/v1/version", nil)
		if err != nil {
			t.Fatal(err)
		}
		if bearer != "" {
			req.Header.Set("Authorization", "Bearer "+bearer)
		}
		resp, err := c.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		return resp
	}
	noCertReq, err := http.NewRequestWithContext(t.Context(), http.MethodGet, "https://"+addr+"/api/v1/version", nil)
	if err != nil {
		t.Fatal(err)
	}
	if noCertResp, noCertErr := client(false).Do(noCertReq); noCertErr == nil {
		noCertResp.Body.Close()
		t.Fatal("request without client certificate unexpectedly completed")
	}
	resp := request(client(true), "")
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("valid certificate without token status = %d, want 401", resp.StatusCode)
	}
	resp = request(client(true), token)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("valid certificate and token status = %d", resp.StatusCode)
	}

	// A client certificate from a CA the listener does not trust fails the handshake.
	otherKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	otherTemplate := &x509.Certificate{SerialNumber: big.NewInt(9), Subject: pkix.Name{CommonName: "other CA"}, NotBefore: now.Add(-time.Hour), NotAfter: now.Add(time.Hour), IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature}
	otherCADER, err := x509.CreateCertificate(rand.Reader, otherTemplate, otherTemplate, &otherKey.PublicKey, otherKey)
	if err != nil {
		t.Fatal(err)
	}
	otherCA, err := x509.ParseCertificate(otherCADER)
	if err != nil {
		t.Fatal(err)
	}
	strangerKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	strangerDER, err := x509.CreateCertificate(rand.Reader, &x509.Certificate{SerialNumber: big.NewInt(10), Subject: pkix.Name{CommonName: "stranger"}, NotBefore: now.Add(-time.Hour), NotAfter: now.Add(time.Hour), KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}}, otherCA, &strangerKey.PublicKey, otherKey)
	if err != nil {
		t.Fatal(err)
	}
	strangerCert := tls.Certificate{Certificate: [][]byte{strangerDER}, PrivateKey: strangerKey}
	strangerClient := &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: roots, ServerName: "localhost", Certificates: []tls.Certificate{strangerCert}}}, Timeout: 5 * time.Second}
	strangerReq, err := http.NewRequestWithContext(t.Context(), http.MethodGet, "https://"+addr+"/api/v1/version", nil)
	if err != nil {
		t.Fatal(err)
	}
	strangerReq.Header.Set("Authorization", "Bearer "+token)
	if strangerResp, strangerErr := strangerClient.Do(strangerReq); strangerErr == nil {
		strangerResp.Body.Close()
		t.Fatal("a client certificate from an untrusted CA completed a request")
	}

	// The audit line carries the token id and the verified client certificate
	// fingerprint, and never the token.
	sum := sha256.Sum256(clientDER)
	audit := logs.String()
	if !strings.Contains(audit, "token_id="+tokenRecord.ID) || !strings.Contains(audit, "certificate_fingerprint="+hex.EncodeToString(sum[:])) {
		t.Fatalf("audit line lacks the token id or the client certificate fingerprint:\n%s", audit)
	}
	if strings.Contains(audit, token) || strings.Contains(audit, token[strings.IndexByte(token, '.')+1:]) {
		t.Fatal("audit output contains the plaintext token")
	}
}
