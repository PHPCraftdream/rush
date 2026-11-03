package server

import (
	"crypto/rsa"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"log"
	"log/slog"
	"os"
	"runtime"
	"strings"
	"sync/atomic"
	"time"
)

type certificateSnapshot struct {
	cert              *tls.Certificate
	certHash, keyHash [32]byte
}

type tlsCertificateStore struct {
	current           atomic.Pointer[certificateSnapshot]
	certPath, keyPath string
}

func newTLSCertificateStore(policy ListenerTLS) (*tlsCertificateStore, error) {
	if (policy.Cert == "") != (policy.Key == "") {
		return nil, errors.New("TLS certificate and key must be specified together")
	}
	if policy.Cert == "" {
		return nil, nil
	}
	store := &tlsCertificateStore{certPath: policy.Cert, keyPath: policy.Key}
	if err := store.reload(); err != nil {
		return nil, err
	}
	return store, nil
}

func (s *tlsCertificateStore) reload() error {
	return s.reloadAt(time.Now())
}

func (s *tlsCertificateStore) reloadAt(now time.Time) error {
	certPEM, err := os.ReadFile(s.certPath)
	if err != nil {
		return fmt.Errorf("read TLS certificate: %w", err)
	}
	keyPEM, err := os.ReadFile(s.keyPath)
	if err != nil {
		return fmt.Errorf("read TLS key: %w", err)
	}
	certHash, keyHash := sha256Sum(certPEM), sha256Sum(keyPEM)
	if current := s.current.Load(); current != nil && current.certHash == certHash && current.keyHash == keyHash {
		return nil
	}
	cert, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		return fmt.Errorf("load TLS certificate pair: %w", err)
	}
	leaf, err := x509.ParseCertificate(cert.Certificate[0])
	if err != nil {
		return err
	}
	cert.Leaf = leaf
	if now.Before(leaf.NotBefore) || now.After(leaf.NotAfter) {
		return errors.New("TLS certificate is outside its validity window")
	}
	if len(leaf.DNSNames)+len(leaf.IPAddresses) == 0 {
		return errors.New("TLS certificate has no SAN")
	}
	serverAuth := false
	for _, usage := range leaf.ExtKeyUsage {
		if usage == x509.ExtKeyUsageServerAuth {
			serverAuth = true
		}
	}
	if !serverAuth {
		return errors.New("TLS certificate lacks serverAuth EKU")
	}
	if key, ok := cert.PrivateKey.(*rsa.PrivateKey); ok && key.N.BitLen() < 2048 {
		return errors.New("TLS RSA key must be at least 2048 bits")
	}
	if info, err := os.Stat(s.keyPath); err != nil {
		if runtime.GOOS == "windows" {
			slog.Warn("TLS private-key ACL permissions could not be verified", "error", err)
		} else {
			return fmt.Errorf("stat TLS key permissions: %w", err)
		}
	} else if runtime.GOOS == "windows" {
		slog.Warn("TLS private-key ACL permissions are not verified by file mode")
	} else if info.Mode().Perm()&0o077 != 0 {
		return errors.New("TLS key is group/world readable")
	}
	snap := &certificateSnapshot{cert: &cert, certHash: certHash, keyHash: keyHash}
	s.current.Store(snap)
	return nil
}

func (s *tlsCertificateStore) certificate(*tls.ClientHelloInfo) (*tls.Certificate, error) {
	snap := s.current.Load()
	if snap == nil {
		return nil, errors.New("TLS certificate unavailable")
	}
	return snap.cert, nil
}

func tlsConfig(policy ListenerTLS, store *tlsCertificateStore) (*tls.Config, error) {
	if store == nil {
		return nil, errors.New("TLS certificate and key are required")
	}
	minimum := uint16(tls.VersionTLS13)
	switch strings.TrimSpace(policy.MinVersion) {
	case "", "1.3":
	case "1.2":
		minimum = tls.VersionTLS12
	default:
		return nil, fmt.Errorf("unsupported TLS min_version %q", policy.MinVersion)
	}
	config := &tls.Config{MinVersion: minimum, SessionTicketsDisabled: true, GetCertificate: store.certificate, NextProtos: []string{"http/1.1"}}
	if minimum == tls.VersionTLS12 {
		config.CipherSuites = []uint16{tls.TLS_ECDHE_ECDSA_WITH_AES_128_GCM_SHA256, tls.TLS_ECDHE_RSA_WITH_AES_128_GCM_SHA256, tls.TLS_ECDHE_ECDSA_WITH_AES_256_GCM_SHA384, tls.TLS_ECDHE_RSA_WITH_AES_256_GCM_SHA384, tls.TLS_ECDHE_ECDSA_WITH_CHACHA20_POLY1305_SHA256, tls.TLS_ECDHE_RSA_WITH_CHACHA20_POLY1305_SHA256}
	}
	if policy.ClientCA != "" {
		clientCAs, err := loadClientCAPool(policy.ClientCA)
		if err != nil {
			return nil, err
		}
		config.ClientCAs = clientCAs
		config.ClientAuth = tls.RequireAndVerifyClientCert
	}
	return config, nil
}

func sha256Sum(data []byte) [32]byte { return sha256.Sum256(data) }

func tlsErrorLogger() *log.Logger { return log.New(&rateLimitedTLSWriter{}, "", 0) }

type rateLimitedTLSWriter struct{ last atomic.Int64 }

func (w *rateLimitedTLSWriter) Write(data []byte) (int, error) {
	now := time.Now().UnixNano()
	last := w.last.Load()
	if now-last > int64(time.Second) && w.last.CompareAndSwap(last, now) {
		slog.Error("HTTP server error", "error", strings.TrimSpace(string(data)))
	}
	return len(data), nil
}
