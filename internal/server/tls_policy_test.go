package server

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

func writeTLSTestPair(t *testing.T, dir, name string, now time.Time, sans []string) (string, string, *x509.Certificate) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(time.Now().UnixNano()), Subject: pkix.Name{CommonName: name}, NotBefore: now.Add(-time.Hour), NotAfter: now.Add(24 * time.Hour), KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}, DNSNames: sans, IPAddresses: []net.IP{net.ParseIP("127.0.0.1")}}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	certPath, keyPath := filepath.Join(dir, name+".crt"), filepath.Join(dir, name+".key")
	if err = os.WriteFile(certPath, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o600); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(keyPath, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}), 0o600); err != nil {
		t.Fatal(err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	return certPath, keyPath, cert
}

func TestTLSConfigEnforcesProtocolPolicy(t *testing.T) {
	dir := t.TempDir()
	cert, key, _ := writeTLSTestPair(t, dir, "server", time.Now(), []string{"localhost"})
	store, err := newTLSCertificateStore(ListenerTLS{Cert: cert, Key: key})
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := tlsConfig(ListenerTLS{}, store)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.MinVersion != tls.VersionTLS13 || !cfg.SessionTicketsDisabled || len(cfg.NextProtos) != 1 || cfg.NextProtos[0] != "http/1.1" {
		t.Fatalf("unexpected TLS policy: %#v", cfg)
	}
	if _, err = tlsConfig(ListenerTLS{MinVersion: "1.2"}, store); err != nil {
		t.Fatal(err)
	}
	if _, err = tlsConfig(ListenerTLS{MinVersion: "1.1"}, store); err == nil {
		t.Fatal("unsupported minimum accepted")
	}
}

func TestTLSCertificateReloadKeepsLastGoodOnInvalidCandidate(t *testing.T) {
	dir := t.TempDir()
	cert, key, first := writeTLSTestPair(t, dir, "one", time.Now(), []string{"localhost"})
	store, err := newTLSCertificateStore(ListenerTLS{Cert: cert, Key: key})
	if err != nil {
		t.Fatal(err)
	}
	old := store.current.Load()
	if err = os.WriteFile(cert, []byte("not a certificate"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err = store.reload(); err == nil {
		t.Fatal("invalid replacement accepted")
	}
	if store.current.Load() != old || !store.current.Load().cert.Leaf.Equal(first) {
		t.Fatal("failed reload replaced active certificate")
	}
}

func TestEnsureSelfSignedCertificateReusesKeyAndRejectsWildcardWithoutSAN(t *testing.T) {
	dir := t.TempDir()
	now := time.Now()
	certPath, keyPath, err := EnsureSelfSignedCertificate(dir, "127.0.0.1:0", nil, now)
	if err != nil {
		t.Fatal(err)
	}
	keyBefore, err := os.ReadFile(keyPath)
	if err != nil {
		t.Fatal(err)
	}
	certPath2, keyPath2, err := EnsureSelfSignedCertificate(dir, "127.0.0.1:0", nil, now)
	if err != nil {
		t.Fatal(err)
	}
	keyAfter, err := os.ReadFile(keyPath2)
	if err != nil {
		t.Fatal(err)
	}
	if certPath != certPath2 || keyPath != keyPath2 || string(keyBefore) != string(keyAfter) {
		t.Fatal("self-signed key was not reused")
	}
	if _, _, err = EnsureSelfSignedCertificate(t.TempDir(), "0.0.0.0:0", nil, now); err == nil {
		t.Fatal("wildcard self-signed TLS accepted without SAN")
	}
}

func TestSelfSignedCertificatesUseStableDistinctListenerIdentity(t *testing.T) {
	dir := t.TempDir()
	now := time.Now()
	firstCert, firstKey, err := EnsureSelfSignedCertificateForListener(dir, "127.0.0.1:0", "127.0.0.1:40123", nil, now)
	if err != nil {
		t.Fatal(err)
	}
	firstKeyBytes, err := os.ReadFile(firstKey)
	if err != nil {
		t.Fatal(err)
	}
	secondCert, secondKey, err := EnsureSelfSignedCertificateForListener(dir, "127.0.0.1:0", "127.0.0.1:50987", nil, now)
	if err != nil {
		t.Fatal(err)
	}
	secondKeyBytes, err := os.ReadFile(secondKey)
	if err != nil {
		t.Fatal(err)
	}
	if firstCert != secondCert || firstKey != secondKey || string(firstKeyBytes) != string(secondKeyBytes) {
		t.Fatal("ephemeral bound port changed persistent listener identity")
	}

	otherCert, otherKey, err := EnsureSelfSignedCertificateForListener(dir, "127.0.0.1:9443", "127.0.0.1:9443", nil, now)
	if err != nil {
		t.Fatal(err)
	}
	if otherCert == firstCert || otherKey == firstKey {
		t.Fatal("distinct configured listeners shared self-signed certificate files")
	}
}

func TestTLSKeyPermissionsAreRestricted(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX permission bits are not enforced on Windows")
	}
	dir := t.TempDir()
	cert, key, _ := writeTLSTestPair(t, dir, "server", time.Now(), []string{"localhost"})
	if err := os.Chmod(key, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := newTLSCertificateStore(ListenerTLS{Cert: cert, Key: key}); err == nil {
		t.Fatal("group-readable TLS key accepted")
	}
}

func TestTLSListenerNegotiatesHTTP11AndRejectsTLS12ByDefault(t *testing.T) {
	dir := t.TempDir()
	cert, key, _ := writeTLSTestPair(t, dir, "server", time.Now(), []string{"localhost"})
	policy := ListenerPolicy{Address: "127.0.0.1:0", Surfaces: []ListenerSurface{SurfaceREST}, TLS: ListenerTLS{Cert: cert, Key: key}, Auth: DenyAllAuth}
	srv := NewWithPolicy(nil, policy, nil)
	ln, err := net.Listen("tcp", policy.Address)
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
	client := &tls.Config{InsecureSkipVerify: true, NextProtos: []string{"h2", "http/1.1"}} // test certificate is generated locally
	conn, err := tls.Dial("tcp", ln.Addr().String(), client)
	if err != nil {
		t.Fatal(err)
	}
	state := conn.ConnectionState()
	_ = conn.Close()
	if state.NegotiatedProtocol != "http/1.1" {
		t.Fatalf("ALPN = %q", state.NegotiatedProtocol)
	}
	tls12 := &tls.Config{InsecureSkipVerify: true, MinVersion: tls.VersionTLS12, MaxVersion: tls.VersionTLS12} // test certificate is generated locally
	if conn, err = tls.Dial("tcp", ln.Addr().String(), tls12); err == nil {
		conn.Close()
		t.Fatal("TLS 1.2 accepted under default policy")
	}
}
