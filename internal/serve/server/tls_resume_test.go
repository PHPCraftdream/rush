package server

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/pem"
	"fmt"
	"math/big"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/PHPCraftdream/rush/internal/config"
	"github.com/gorilla/websocket"
)

func TestTLS12RestrictedSuitesAndCBCRefusal(t *testing.T) {
	dir := t.TempDir()
	certPath, keyPath, _ := writeTLSTestPair(t, dir, "server", time.Now(), []string{"localhost"})
	store, err := newTLSCertificateStore(ListenerTLS{Cert: certPath, Key: keyPath})
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := tlsConfig(ListenerTLS{MinVersion: "1.2"}, store)
	if err != nil {
		t.Fatal(err)
	}
	policy := ListenerPolicy{Address: "127.0.0.1:0", Surfaces: []ListenerSurface{SurfaceREST}, TLS: ListenerTLS{Cert: certPath, Key: keyPath, MinVersion: "1.2"}, Auth: DenyAllAuth}
	srv := NewWithPolicy(nil, policy, nil)
	ln, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", policy.Address)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- srv.ServeListener(ctx, ln, nil) }()
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-done:
			if err != nil && err != http.ErrServerClosed {
				t.Errorf("ServeListener: %v", err)
			}
		case <-time.After(5 * time.Second):
			t.Error("listener shutdown timeout")
		}
	})
	connect := func(suites []uint16) error {
		conn, err := dialTLSTest(t, ln.Addr().String(), &tls.Config{InsecureSkipVerify: true, MinVersion: tls.VersionTLS12, MaxVersion: tls.VersionTLS12, CipherSuites: suites})
		if err == nil {
			_ = conn.Close()
		}
		return err
	}
	if err := connect([]uint16{tls.TLS_ECDHE_ECDSA_WITH_AES_128_GCM_SHA256}); err != nil {
		t.Fatalf("restricted ECDHE+AEAD handshake: %v", err)
	}
	if err := connect([]uint16{tls.TLS_ECDHE_ECDSA_WITH_AES_128_CBC_SHA, tls.TLS_ECDHE_RSA_WITH_AES_128_CBC_SHA}); err == nil {
		t.Fatal("CBC-only TLS 1.2 client connected")
	}
	_ = cfg
}

func TestTLSCertificateReloadChangesLiveHandshakeByContentAndKeepsGoodPair(t *testing.T) {
	dir := t.TempDir()
	certPath, keyPath, first := writeTLSTestPair(t, dir, "first", time.Now(), []string{"localhost"})
	secondCertPath, secondKeyPath, second := writeTLSTestPair(t, dir, "second", time.Now(), []string{"localhost"})
	certBytes, err := os.ReadFile(secondCertPath)
	if err != nil {
		t.Fatal(err)
	}
	keyBytes, err := os.ReadFile(secondKeyPath)
	if err != nil {
		t.Fatal(err)
	}
	store, err := newTLSCertificateStore(ListenerTLS{Cert: certPath, Key: keyPath})
	if err != nil {
		t.Fatal(err)
	}
	listener, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	server := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = fmt.Fprint(w, "ok") }), TLSConfig: func() *tls.Config {
		c, e := tlsConfig(ListenerTLS{}, store)
		if e != nil {
			t.Fatal(e)
		}
		return c
	}()}
	done := make(chan error, 1)
	go func() { done <- server.ServeTLS(listener, "", "") }()
	t.Cleanup(func() { _ = server.Close(); <-done })
	leafSerial := func() string {
		t.Helper()
		conn, err := dialTLSTest(t, listener.Addr().String(), &tls.Config{InsecureSkipVerify: true})
		if err != nil {
			t.Fatal(err)
		}
		defer conn.Close()
		return conn.ConnectionState().PeerCertificates[0].SerialNumber.String()
	}
	if got := leafSerial(); got != first.SerialNumber.String() {
		t.Fatalf("initial serial=%s", got)
	}
	old := store.current.Load()
	if err := os.WriteFile(certPath, []byte("broken certificate"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := store.reload(); err == nil {
		t.Fatal("corrupt candidate accepted")
	}
	if store.current.Load() != old {
		t.Fatal("corrupt candidate replaced active certificate")
	}
	if err := os.WriteFile(certPath, certBytes, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(keyPath, keyBytes, 0o600); err != nil {
		t.Fatal(err)
	}
	// reloadAt is the injected deterministic tick action used by runTLSReload.
	if err := store.reloadAt(time.Now()); err != nil {
		t.Fatal(err)
	}
	if got := leafSerial(); got != second.SerialNumber.String() {
		t.Fatalf("reloaded serial=%s, want %s", got, second.SerialNumber)
	}
	before := store.current.Load()
	info, err := os.Stat(certPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(certPath, info.ModTime().Add(time.Second), info.ModTime().Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	if err := store.reloadAt(time.Now()); err != nil {
		t.Fatal(err)
	}
	if store.current.Load() != before {
		t.Fatal("mtime-only touch swapped certificate")
	}
}

func TestTLSStartupRefusalMatrix(t *testing.T) {
	now := time.Now()
	for _, tc := range []struct {
		name   string
		mutate func(t *testing.T, dir string, cert, key string)
		policy func(cert, key string) ListenerTLS
	}{
		{name: "certificate without key", mutate: func(t *testing.T, _ string, _, _ string) {}, policy: func(c, k string) ListenerTLS { return ListenerTLS{Cert: c} }},
		{name: "key without certificate", mutate: func(t *testing.T, _ string, _, _ string) {}, policy: func(c, k string) ListenerTLS { return ListenerTLS{Key: k} }},
		{name: "mismatched pair", mutate: func(t *testing.T, dir string, c, k string) {
			_, other, _ := writeTLSTestPair(t, dir, "other", now, []string{"localhost"})
			data, e := os.ReadFile(other)
			if e != nil {
				t.Fatal(e)
			}
			if e = os.WriteFile(k, data, 0o600); e != nil {
				t.Fatal(e)
			}
		}, policy: func(c, k string) ListenerTLS { return ListenerTLS{Cert: c, Key: k} }},
		{name: "expired", mutate: func(t *testing.T, dir string, c, k string) {
			writeTLSVariant(t, dir, c, k, now.Add(-48*time.Hour), now.Add(-time.Hour), []string{"localhost"}, []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}, 2048)
		}, policy: func(c, k string) ListenerTLS { return ListenerTLS{Cert: c, Key: k} }},
		{name: "not yet valid", mutate: func(t *testing.T, dir string, c, k string) {
			writeTLSVariant(t, dir, c, k, now.Add(time.Hour), now.Add(24*time.Hour), []string{"localhost"}, []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}, 2048)
		}, policy: func(c, k string) ListenerTLS { return ListenerTLS{Cert: c, Key: k} }},
		{name: "no SAN", mutate: func(t *testing.T, dir string, c, k string) {
			writeTLSVariant(t, dir, c, k, now.Add(-time.Hour), now.Add(24*time.Hour), nil, []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}, 2048)
		}, policy: func(c, k string) ListenerTLS { return ListenerTLS{Cert: c, Key: k} }},
		{name: "missing serverAuth", mutate: func(t *testing.T, dir string, c, k string) {
			writeTLSVariant(t, dir, c, k, now.Add(-time.Hour), now.Add(24*time.Hour), []string{"localhost"}, []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}, 2048)
		}, policy: func(c, k string) ListenerTLS { return ListenerTLS{Cert: c, Key: k} }},
		{name: "RSA 1024", mutate: func(t *testing.T, dir string, c, k string) {
			writeTLSVariant(t, dir, c, k, now.Add(-time.Hour), now.Add(24*time.Hour), []string{"localhost"}, []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}, 1024)
		}, policy: func(c, k string) ListenerTLS { return ListenerTLS{Cert: c, Key: k} }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			c, k, _ := writeTLSTestPair(t, dir, "base", now, []string{"localhost"})
			tc.mutate(t, dir, c, k)
			if _, err := newTLSCertificateStore(tc.policy(c, k)); err == nil {
				t.Fatal("invalid TLS configuration accepted")
			}
		})
	}
	for _, tc := range []struct {
		name   string
		mutate func(t *testing.T, dir string) string
	}{
		{name: "unparseable client CA", mutate: func(t *testing.T, dir string) string {
			p := filepath.Join(dir, "ca.pem")
			if e := os.WriteFile(p, []byte("not pem"), 0o600); e != nil {
				t.Fatal(e)
			}
			return p
		}},
		{name: "empty client CA pool", mutate: func(t *testing.T, dir string) string {
			p := filepath.Join(dir, "ca.pem")
			if e := os.WriteFile(p, []byte{}, 0o600); e != nil {
				t.Fatal(e)
			}
			return p
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := loadClientCAPool(tc.mutate(t, t.TempDir())); err == nil {
				t.Fatal("invalid CA pool accepted")
			}
		})
	}
	policy := ListenerPolicy{Address: "127.0.0.1:0", Surfaces: []ListenerSurface{SurfaceREST}, Auth: DenyAllAuth, TLS: ListenerTLS{SelfSigned: true, Cert: "x", Key: "y"}}
	if err := policy.Validate(); err == nil {
		t.Fatal("contradictory self-signed and explicit certificate accepted")
	}
}

func writeTLSVariant(t *testing.T, dir, certPath, keyPath string, before, after time.Time, sans []string, usages []x509.ExtKeyUsage, rsaBits int) {
	t.Helper()
	var private any
	var err error
	var public any
	if rsaBits > 0 {
		var key *rsa.PrivateKey
		key, err = rsa.GenerateKey(rand.Reader, rsaBits)
		if err == nil {
			private, public = key, &key.PublicKey
		}
	} else {
		var key *ecdsa.PrivateKey
		key, err = ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		if err == nil {
			private, public = key, &key.PublicKey
		}
	}
	if err != nil {
		t.Fatal(err)
	}
	var ipAddresses []net.IP
	if sans != nil {
		ipAddresses = []net.IP{net.ParseIP("127.0.0.1")}
	}
	template := &x509.Certificate{SerialNumber: big.NewInt(time.Now().UnixNano()), NotBefore: before, NotAfter: after, DNSNames: sans, IPAddresses: ipAddresses, KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: usages}
	der, err := x509.CreateCertificate(rand.Reader, template, template, public, private)
	if err != nil {
		t.Fatal(err)
	}
	var keyDER []byte
	var keyType string
	if rsaBits > 0 {
		keyDER = x509.MarshalPKCS1PrivateKey(private.(*rsa.PrivateKey))
		keyType = "RSA PRIVATE KEY"
	} else {
		keyDER, err = x509.MarshalECPrivateKey(private.(*ecdsa.PrivateKey))
		keyType = "EC PRIVATE KEY"
		if err != nil {
			t.Fatal(err)
		}
	}
	if err = os.WriteFile(certPath, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o600); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(keyPath, pem.EncodeToMemory(&pem.Block{Type: keyType, Bytes: keyDER}), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestTLSStartupRefusalWithoutSANAndExpiredSelfSigned(t *testing.T) {
	dir := t.TempDir()
	cert, key, err := EnsureSelfSignedCertificate(dir, "127.0.0.1:0", nil, time.Now().Add(-400*24*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := EnsureSelfSignedCertificate(dir, "127.0.0.1:0", nil, time.Now()); err == nil {
		t.Fatal("expired self-signed certificate silently regenerated")
	}
	_, _ = cert, key
}

func TestServeAPIWSSProductionTLSSubprotocolHello(t *testing.T) {
	for _, key := range []string{"RUSH_GLOBAL_DATA", "RUSH_GLOBAL_CONFIG", "RUSH_GLOBAL_DIR"} {
		t.Setenv(key, t.TempDir())
	}
	dir := t.TempDir()
	cert, key, _ := writeTLSTestPair(t, dir, "ws", time.Now(), []string{"localhost", "127.0.0.1"})
	store := config.GlobalAPITokenStore()
	token, _, err := store.CreateAPIToken("tls-ws", []string{"read"}, "ask", nil)
	if err != nil {
		t.Fatal(err)
	}
	policy := ListenerPolicy{Address: "127.0.0.1:0", Surfaces: []ListenerSurface{SurfaceREST, SurfaceWS}, TLS: ListenerTLS{Cert: cert, Key: key}}
	api, err := NewServeAPI(policy, nil, store, nil)
	if err != nil {
		t.Fatal(err)
	}
	api.Configure(&policy)
	srv := NewWithPolicy(nil, policy, nil)
	ln, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", policy.Address)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- srv.ServeListener(ctx, ln, nil) }()
	t.Cleanup(func() {
		cancel()
		select {
		case e := <-done:
			if e != nil && e != http.ErrServerClosed {
				t.Errorf("serve: %v", e)
			}
		case <-time.After(5 * time.Second):
			t.Error("shutdown timeout")
		}
		stopCtx, stopCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer stopCancel()
		if e := api.Stop(stopCtx); e != nil {
			t.Errorf("API stop: %v", e)
		}
	})
	endpoint := "wss://" + ln.Addr().String() + "/ws"
	dialer := websocket.Dialer{HandshakeTimeout: 3 * time.Second, Subprotocols: []string{"rush.v1", "rush.bearer." + token}, TLSClientConfig: &tls.Config{InsecureSkipVerify: true}}
	conn, resp, err := dialer.Dial(endpoint, nil) //nolint:bodyclose // gorilla upgrade responses carry no body
	if err != nil {
		t.Fatalf("TLS websocket handshake: %v response=%v", err, resp)
	}
	defer conn.Close()
	if conn.Subprotocol() != "rush.v1" {
		t.Fatalf("subprotocol=%q", conn.Subprotocol())
	}
	_ = conn.SetReadDeadline(time.Now().Add(3 * time.Second))
	_, frame, err := conn.ReadMessage()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(frame), string(EventHello)) {
		t.Fatalf("hello frame=%q", frame)
	}
}

func TestTLSPinFormatsMatchCertificate(t *testing.T) {
	dir := t.TempDir()
	certPath, keyPath, _ := writeTLSTestPair(t, dir, "pins", time.Now(), []string{"localhost"})
	store, err := newTLSCertificateStore(ListenerTLS{Cert: certPath, Key: keyPath})
	if err != nil {
		t.Fatal(err)
	}
	pins, err := store.pins()
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(certPath)
	if err != nil {
		t.Fatal(err)
	}
	block, _ := pem.Decode(data)
	if block == nil {
		t.Fatal("certificate PEM missing")
	}
	parsed, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	wantPublicKeyHash := sha256.Sum256(parsed.RawSubjectPublicKeyInfo)
	wantLeafHash := sha256.Sum256(parsed.Raw)
	want := TLSPins{
		PublicKeyHex:  hex.EncodeToString(wantPublicKeyHash[:]),
		PublicKeyCurl: "sha256//" + base64.StdEncoding.EncodeToString(wantPublicKeyHash[:]),
		Certificate:   "cert-sha256:" + hex.EncodeToString(wantLeafHash[:]),
	}
	if pins != want {
		t.Fatalf("pins=%+v want=%+v", pins, want)
	}
}
