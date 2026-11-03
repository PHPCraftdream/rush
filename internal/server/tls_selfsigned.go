package server

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

func EnsureSelfSignedCertificate(dataDir, address string, sans []string, now time.Time) (string, string, error) {
	return EnsureSelfSignedCertificateForListener(dataDir, address, address, sans, now)
}

// EnsureSelfSignedCertificateForListener persists the key under a stable configured
// listener identity while using the bound address for the certificate SANs.
func EnsureSelfSignedCertificateForListener(dataDir, identityAddress, address string, sans []string, now time.Time) (string, string, error) {
	return ensureSelfSignedCertificate(dataDir, identityAddress, address, sans, now)
}

func ensureSelfSignedCertificate(dataDir, identityAddress, address string, sans []string, now time.Time) (string, string, error) {
	if dataDir == "" {
		return "", "", errors.New("self-signed TLS requires a data directory")
	}
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		return "", "", fmt.Errorf("self-signed TLS address: %w", err)
	}
	if isWildcardHost(host) && !hasValidSAN(sans) {
		return "", "", errors.New("self-signed TLS on a wildcard bind requires at least one non-blank --tls-san")
	}
	for _, san := range sans {
		if strings.TrimSpace(san) == "" {
			return "", "", errors.New("self-signed TLS SAN must not be blank")
		}
	}
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		return "", "", err
	}
	identity := strings.TrimSpace(identityAddress)
	if identity == "" {
		return "", "", errors.New("self-signed TLS requires a configured listener address")
	}
	suffix := sha256.Sum256([]byte(identity))
	name := strings.Map(func(r rune) rune {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' {
			return r
		}
		return '-'
	}, identity)
	if len(name) > 48 {
		name = name[:48]
	}
	name = strings.Trim(name, "-")
	if name == "" {
		name = "listener"
	}
	base := fmt.Sprintf("serve-tls-%s-%x", name, suffix[:8])
	certPath := filepath.Join(dataDir, base+".crt")
	keyPath := filepath.Join(dataDir, base+".key")
	if _, certErr := os.Stat(certPath); certErr == nil {
		if _, keyErr := os.Stat(keyPath); os.IsNotExist(keyErr) {
			return "", "", errors.New("existing self-signed TLS certificate has no private key; refusing to replace it")
		} else if keyErr != nil {
			return "", "", keyErr
		}
	} else if !os.IsNotExist(certErr) {
		return "", "", certErr
	}
	var key *ecdsa.PrivateKey
	if keyPEM, readErr := os.ReadFile(keyPath); readErr == nil {
		block, _ := pem.Decode(keyPEM)
		if block == nil {
			return "", "", errors.New("invalid persisted self-signed TLS key")
		}
		parsed, parseErr := x509.ParseECPrivateKey(block.Bytes)
		if parseErr != nil {
			return "", "", parseErr
		}
		key = parsed
		if runtime.GOOS != "windows" {
			if err := os.Chmod(keyPath, 0o600); err != nil {
				return "", "", err
			}
		}
	} else if !os.IsNotExist(readErr) {
		return "", "", readErr
	} else {
		if _, certErr := os.Stat(certPath); certErr == nil {
			return "", "", errors.New("existing self-signed TLS certificate has no private key; refusing to replace it")
		} else if !os.IsNotExist(certErr) {
			return "", "", certErr
		}
		key, err = ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		if err != nil {
			return "", "", err
		}
		keyBytes, marshalErr := x509.MarshalECPrivateKey(key)
		if marshalErr != nil {
			return "", "", marshalErr
		}
		keyPEM := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyBytes})
		if err := os.WriteFile(keyPath, keyPEM, 0o600); err != nil {
			return "", "", err
		}
	}
	if certPEM, readErr := os.ReadFile(certPath); readErr == nil {
		block, _ := pem.Decode(certPEM)
		if block != nil {
			if old, parseErr := x509.ParseCertificate(block.Bytes); parseErr == nil && now.After(old.NotAfter) {
				return "", "", errors.New("existing self-signed TLS certificate has expired")
			}
		}
	} else if !os.IsNotExist(readErr) {
		return "", "", readErr
	}
	template := &x509.Certificate{SerialNumber: func() *big.Int { n, _ := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128)); return n }(), Subject: pkix.Name{CommonName: "rush serve"}, NotBefore: now.Add(-time.Hour), NotAfter: now.Add(365 * 24 * time.Hour), KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}, BasicConstraintsValid: true, IsCA: false}
	seen := map[string]bool{}
	addDNS := func(name string) {
		name = strings.TrimSpace(name)
		if name != "" && !seen[name] {
			seen[name] = true
			template.DNSNames = append(template.DNSNames, name)
		}
	}
	addIP := func(value string) {
		if ip := net.ParseIP(value); ip != nil && !seen[ip.String()] {
			seen[ip.String()] = true
			template.IPAddresses = append(template.IPAddresses, ip)
		}
	}
	addDNS("localhost")
	addIP("127.0.0.1")
	addIP("::1")
	if name, err := os.Hostname(); err == nil {
		addDNS(name)
	}
	if !isWildcardHost(host) {
		addIP(host)
		if net.ParseIP(host) == nil {
			addDNS(host)
		}
	}
	for _, san := range sans {
		if net.ParseIP(san) != nil {
			addIP(san)
		} else {
			addDNS(san)
		}
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		return "", "", err
	}
	certOut := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	if err := os.WriteFile(certPath, certOut, 0o600); err != nil {
		return "", "", err
	}
	return certPath, keyPath, nil
}

func hasValidSAN(sans []string) bool {
	for _, san := range sans {
		if strings.TrimSpace(san) != "" {
			return true
		}
	}
	return false
}

func isWildcardHost(host string) bool { return host == "" || host == "0.0.0.0" || host == "::" }
