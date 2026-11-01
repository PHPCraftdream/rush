package server

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"fmt"
	"io"
	"log/slog"
	"math/big"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/PHPCraftdream/rush/internal/config"
)

func makeTestClientCertificate(t *testing.T, issuer *x509.Certificate, issuerKey *ecdsa.PrivateKey) (tls.Certificate, *x509.Certificate) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	template := &x509.Certificate{SerialNumber: big.NewInt(now.UnixNano()), Subject: pkix.Name{CommonName: "client"}, NotBefore: now.Add(-time.Hour), NotAfter: now.Add(time.Hour), KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}}
	parent, parentKey := issuer, any(issuerKey)
	chain := [][]byte{}
	if parent == nil {
		parent = template
		parent.IsCA = true
		parent.BasicConstraintsValid = true
		parent.KeyUsage |= x509.KeyUsageCertSign
		parentKey = key
	} else {
		parentDER, marshalErr := x509.MarshalPKIXPublicKey(issuerKey.Public())
		_ = parentDER
		_ = marshalErr
	}
	der, err := x509.CreateCertificate(rand.Reader, template, parent, &key.PublicKey, parentKey)
	if err != nil {
		t.Fatal(err)
	}
	leaf, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	chain = append(chain, der)
	if issuer != nil {
		issuerDER, err := x509.CreateCertificate(rand.Reader, issuer, issuer, &issuerKey.PublicKey, issuerKey)
		if err != nil {
			t.Fatal(err)
		}
		chain = append(chain, issuerDER)
	}
	return tls.Certificate{Certificate: chain, PrivateKey: key, Leaf: leaf}, leaf
}

func TestChainCertificateIsIndependentAndFingerprintsOnlyVerifiedChains(t *testing.T) {
	isolateSecurityGlobals(t)
	store := config.GlobalAPITokenStore()
	token, _, err := store.CreateAPIToken("tls", []string{"read", "run"}, "ask", nil)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	caKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	caTemplate := &x509.Certificate{SerialNumber: big.NewInt(now.UnixNano()), Subject: pkix.Name{CommonName: "test CA"}, NotBefore: now.Add(-time.Hour), NotAfter: now.Add(time.Hour), IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature}
	caDER, err := x509.CreateCertificate(rand.Reader, caTemplate, caTemplate, &caKey.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	ca, err := x509.ParseCertificate(caDER)
	if err != nil {
		t.Fatal(err)
	}
	caPool := x509.NewCertPool()
	caPool.AddCert(ca)
	clientCert, leaf := makeTestClientCertificate(t, ca, caKey)
	selfSigned, _ := makeTestClientCertificate(t, nil, nil)
	capture := &captureHandler{notify: make(chan struct{}, 16)}
	type handlerTLSInfo struct {
		peerCertificates int
		verifiedChains   int
	}
	principalResults := make(chan Principal, 4)
	tlsResults := make(chan handlerTLSInfo, 4)
	handler := Chain(ChainConfig{AuthConfig: AuthConfig{Verifier: store}, Logger: slog.New(capture)})(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		info := handlerTLSInfo{}
		if r.TLS != nil {
			info.peerCertificates = len(r.TLS.PeerCertificates)
			info.verifiedChains = len(r.TLS.VerifiedChains)
		}
		tlsResults <- info
		p, ok := PrincipalFrom(r.Context())
		if !ok {
			http.Error(w, "missing principal", http.StatusInternalServerError)
			return
		}
		principalResults <- p
		w.WriteHeader(http.StatusOK)
	}))
	serve := func(cert tls.Certificate, clientAuth tls.ClientAuthType, roots *x509.CertPool, bearer bool) (int, Principal, handlerTLSInfo, string) {
		t.Helper()
		server := httptest.NewUnstartedServer(handler)
		server.EnableHTTP2 = false
		server.TLS = &tls.Config{ClientAuth: clientAuth, ClientCAs: roots, MinVersion: tls.VersionTLS12}
		server.StartTLS()
		defer server.Close()
		transport := server.Client().Transport.(*http.Transport).Clone()
		if transport.TLSClientConfig == nil {
			transport.TLSClientConfig = &tls.Config{MinVersion: tls.VersionTLS12}
		} else {
			transport.TLSClientConfig = transport.TLSClientConfig.Clone()
		}
		transport.TLSClientConfig.Certificates = []tls.Certificate{cert}
		if len(cert.Certificate) > 0 {
			transport.TLSClientConfig.GetClientCertificate = func(*tls.CertificateRequestInfo) (*tls.Certificate, error) {
				return &cert, nil
			}
		}
		defer transport.CloseIdleConnections()
		request, err := http.NewRequestWithContext(t.Context(), http.MethodGet, server.URL, nil)
		if err != nil {
			t.Fatal(err)
		}
		if bearer {
			request.Header.Set("Authorization", "Bearer "+token)
		}
		response, err := (&http.Client{Transport: transport, Timeout: 5 * time.Second}).Do(request)
		if err != nil {
			t.Fatal(err)
		}
		if response.StatusCode == http.StatusUnauthorized {
			body, readErr := io.ReadAll(response.Body)
			closeErr := response.Body.Close()
			if readErr != nil || closeErr != nil {
				t.Fatalf("read/close unauthorized response: %v / %v", readErr, closeErr)
			}
			return response.StatusCode, Principal{}, handlerTLSInfo{}, response.Status + "\n" + rawHeadersExceptDate(response.Header) + "\n" + string(body)
		}
		_, readErr := io.Copy(io.Discard, response.Body)
		closeErr := response.Body.Close()
		if readErr != nil || closeErr != nil {
			t.Fatalf("read/close response: %v / %v", readErr, closeErr)
		}
		var principal Principal
		var tlsInfo handlerTLSInfo
		if response.StatusCode == http.StatusOK {
			select {
			case principal = <-principalResults:
			case <-time.After(5 * time.Second):
				t.Fatal("timed out waiting for principal")
			}
			select {
			case tlsInfo = <-tlsResults:
			case <-time.After(5 * time.Second):
				t.Fatal("timed out waiting for handler TLS info")
			}
		}
		return response.StatusCode, principal, tlsInfo, ""
	}
	status, withCert, verifiedTLS, _ := serve(clientCert, tls.RequireAndVerifyClientCert, caPool, true)
	if status != http.StatusOK {
		t.Fatalf("valid client certificate and bearer status=%d", status)
	}
	wantSum := sha256.Sum256(leaf.Raw)
	wantFingerprint := hex.EncodeToString(wantSum[:])
	if withCert.CertFingerprint != wantFingerprint {
		t.Fatalf("fingerprint=%q want %q", withCert.CertFingerprint, wantFingerprint)
	}
	if verifiedTLS.peerCertificates == 0 || verifiedTLS.verifiedChains == 0 {
		t.Fatalf("verified TLS did not present and verify peer certificate: %+v", verifiedTLS)
	}
	verifiedRecords := waitForRecords(t, capture, 1)
	if got := verifiedRecords[len(verifiedRecords)-1].attrs["certificate_fingerprint"]; got != wantFingerprint {
		t.Fatalf("verified audit fingerprint=%v, want %q", got, wantFingerprint)
	}
	status, _, _, unauthorizedResponse := serve(clientCert, tls.RequireAndVerifyClientCert, caPool, false)
	if status != http.StatusUnauthorized {
		t.Fatalf("certificate without bearer status=%d, want 401", status)
	}
	refStatus, _, _, referenceResponse := serve(tls.Certificate{}, tls.NoClientCert, nil, false)
	if refStatus != http.StatusUnauthorized || unauthorizedResponse != referenceResponse {
		t.Fatalf("certificate 401=%q, no-certificate/no-bearer reference status=%d response=%q", unauthorizedResponse, refStatus, referenceResponse)
	}
	status, withoutCert, _, _ := serve(tls.Certificate{}, tls.NoClientCert, nil, true)
	if status != http.StatusOK {
		t.Fatalf("bearer without certificate status=%d", status)
	}
	if withCert.TokenID != withoutCert.TokenID || strings.Join(withCert.Scopes, ",") != strings.Join(withoutCert.Scopes, ",") {
		t.Fatalf("certificate changed principal: with=%#v without=%#v", withCert, withoutCert)
	}
	recordsBeforeUnverified := len(capture.snapshot())
	status, unverifiedPrincipal, unverifiedTLS, _ := serve(selfSigned, tls.RequestClientCert, caPool, true)
	if status != http.StatusOK {
		t.Fatalf("unverified client cert request status=%d", status)
	}
	if unverifiedTLS.peerCertificates == 0 || unverifiedTLS.verifiedChains != 0 {
		t.Fatalf("negative case must present a peer certificate with no verified chains: %+v", unverifiedTLS)
	}
	if unverifiedPrincipal.CertFingerprint != "" {
		t.Fatalf("unverified chain principal fingerprint=%q, want empty", unverifiedPrincipal.CertFingerprint)
	}
	finalRecords := waitForRecords(t, capture, recordsBeforeUnverified+1)
	finalAudit := finalRecords[len(finalRecords)-1]
	if got := finalAudit.attrs["certificate_fingerprint"]; got != "" {
		t.Fatalf("unverified chain fingerprint=%v, want empty", got)
	}
	for _, record := range capture.snapshot() {
		text := record.message + " " + fmt.Sprint(record.attrs)
		if strings.Contains(text, token) || strings.Contains(text, token[strings.IndexByte(token, '.')+1:]) {
			t.Fatalf("certificate audit leaked credential: %s", text)
		}
	}
}
