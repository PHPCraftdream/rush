package server

import (
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/pem"
	"fmt"
)

type TLSPins struct {
	PublicKeyHex  string
	PublicKeyCurl string
	Certificate   string
}

func TLSPinsForCertificatePEM(data []byte) (TLSPins, error) {
	block, _ := pem.Decode(data)
	if block == nil {
		return TLSPins{}, fmt.Errorf("TLS certificate PEM is unavailable")
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return TLSPins{}, err
	}
	return tlsPins(cert)
}

func (s *tlsCertificateStore) pins() (TLSPins, error) {
	snapshot := s.current.Load()
	if snapshot == nil || snapshot.cert == nil || snapshot.cert.Leaf == nil {
		return TLSPins{}, fmt.Errorf("TLS certificate is unavailable")
	}
	return tlsPins(snapshot.cert.Leaf)
}

func tlsPins(cert *x509.Certificate) (TLSPins, error) {
	if cert == nil {
		return TLSPins{}, fmt.Errorf("TLS certificate is unavailable")
	}
	spki, err := x509.MarshalPKIXPublicKey(cert.PublicKey)
	if err != nil {
		return TLSPins{}, err
	}
	publicKeyHash := sha256.Sum256(spki)
	leafHash := sha256.Sum256(cert.Raw)
	return TLSPins{PublicKeyHex: hex.EncodeToString(publicKeyHash[:]), PublicKeyCurl: "sha256//" + base64.StdEncoding.EncodeToString(publicKeyHash[:]), Certificate: "cert-sha256:" + hex.EncodeToString(leafHash[:])}, nil
}
