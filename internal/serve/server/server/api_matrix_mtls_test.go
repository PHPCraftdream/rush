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
	"github.com/gorilla/websocket"
	"github.com/stretchr/testify/require"
)

type matrixCA struct {
	cert *x509.Certificate
	key  *ecdsa.PrivateKey
	pem  []byte
}

func newMatrixCA(t *testing.T, name string) matrixCA {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)
	now := time.Now()
	template := &x509.Certificate{SerialNumber: big.NewInt(now.UnixNano()), Subject: pkix.Name{CommonName: name}, NotBefore: now.Add(-time.Hour), NotAfter: now.Add(time.Hour), IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	require.NoError(t, err)
	cert, err := x509.ParseCertificate(der)
	require.NoError(t, err)
	return matrixCA{cert: cert, key: key, pem: pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})}
}

// issue signs a leaf for the usage; the returned DER is the certificate the peer presents.
func (ca matrixCA) issue(t *testing.T, name string, usage x509.ExtKeyUsage, sans []string) (tls.Certificate, []byte, []byte) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)
	now := time.Now()
	template := &x509.Certificate{SerialNumber: big.NewInt(now.UnixNano() + 1), Subject: pkix.Name{CommonName: name}, DNSNames: sans, IPAddresses: []net.IP{net.ParseIP("127.0.0.1")}, NotBefore: now.Add(-time.Hour), NotAfter: now.Add(time.Hour), KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{usage}}
	der, err := x509.CreateCertificate(rand.Reader, template, ca.cert, &key.PublicKey, ca.key)
	require.NoError(t, err)
	keyDER, err := x509.MarshalECPrivateKey(key)
	require.NoError(t, err)
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})
	pair, err := tls.X509KeyPair(certPEM, keyPEM)
	require.NoError(t, err)
	return pair, certPEM, keyPEM
}

// TestAPIMatrixMTLSOnTLSChannels runs the client-certificate rules over https and wss.
func TestAPIMatrixMTLSOnTLSChannels(t *testing.T) {
	for _, ch := range apiMatrixChannels {
		if !ch.tls {
			continue
		}
		t.Run(ch.name, func(t *testing.T) {
			isolateAPIGlobals(t)
			var logs syncBuffer
			previous := slog.Default()
			slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
			t.Cleanup(func() { slog.SetDefault(previous) })

			dir := t.TempDir()
			ca := newMatrixCA(t, "matrix CA")
			_, serverCertPEM, serverKeyPEM := ca.issue(t, "matrix server", x509.ExtKeyUsageServerAuth, []string{"localhost"})
			serverCert, serverKey, caPath := filepath.Join(dir, "server.crt"), filepath.Join(dir, "server.key"), filepath.Join(dir, "ca.crt")
			require.NoError(t, os.WriteFile(serverCert, serverCertPEM, 0o600))
			require.NoError(t, os.WriteFile(serverKey, serverKeyPEM, 0o600))
			require.NoError(t, os.WriteFile(caPath, ca.pem, 0o600))
			clientPair, clientCertPEM, _ := ca.issue(t, "matrix client", x509.ExtKeyUsageClientAuth, nil)
			stranger := newMatrixCA(t, "stranger CA")
			strangerPair, _, _ := stranger.issue(t, "stranger client", x509.ExtKeyUsageClientAuth, nil)

			workingDir := t.TempDir()
			app := newAttachmentsTestApp(t, workingDir, t.TempDir())
			store := config.GlobalAPITokenStore()
			token, tokenRecord, err := store.CreateAPIToken("matrix mtls", []string{"read"}, "ask", nil)
			require.NoError(t, err)
			policy := ListenerPolicy{
				Address: "127.0.0.1:0", Workspace: workingDir,
				Surfaces: []ListenerSurface{SurfaceREST, SurfaceWS},
				Limits:   ListenerLimits{MaxConnsPerToken: 20, RPS: 100000, Burst: 100000},
				TLS:      ListenerTLS{Cert: serverCert, Key: serverKey, ClientCA: caPath},
			}
			api, err := NewServeAPI(policy, app, store, nil)
			require.NoError(t, err)
			api.Configure(&policy)
			base := "https" + strings.TrimPrefix(startPolicyServer(t, NewWithPolicy(app, policy, nil)), "http")
			t.Cleanup(func() {
				ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer cancel()
				require.NoError(t, api.Stop(ctx))
			})
			roots := x509.NewCertPool()
			roots.AddCert(ca.cert)
			clientTLS := func(certs ...tls.Certificate) *tls.Config {
				return &tls.Config{RootCAs: roots, ServerName: "localhost", Certificates: certs, MinVersion: tls.VersionTLS13}
			}

			// connect reports the HTTP status of a REST request or of a WebSocket upgrade, or the transport error.
			connect := func(cfg *tls.Config, bearer string) (int, error) {
				if ch.ws {
					dialer := websocket.Dialer{HandshakeTimeout: 3 * time.Second, TLSClientConfig: cfg}
					if bearer != "" {
						dialer.Subprotocols = []string{"rush.v1", "rush.bearer." + bearer}
					}
					conn, resp, dialErr := dialer.Dial("wss"+strings.TrimPrefix(base, "https")+"/ws", nil)
					if conn != nil {
						_ = conn.Close()
					}
					if resp != nil {
						defer resp.Body.Close()
						if dialErr != nil || resp.StatusCode == http.StatusSwitchingProtocols {
							return resp.StatusCode, nil
						}
					}
					return 0, dialErr
				}
				req, reqErr := http.NewRequestWithContext(t.Context(), http.MethodGet, base+"/api/v1/sessions", nil)
				require.NoError(t, reqErr)
				if bearer != "" {
					req.Header.Set("Authorization", "Bearer "+bearer)
				}
				resp, doErr := (&http.Client{Transport: &http.Transport{TLSClientConfig: cfg}, Timeout: 5 * time.Second}).Do(req)
				if doErr != nil {
					return 0, doErr
				}
				defer resp.Body.Close()
				return resp.StatusCode, nil
			}
			ok := http.StatusOK
			if ch.ws {
				ok = http.StatusSwitchingProtocols
			}

			_, err = connect(clientTLS(), token)
			require.Error(t, err, "no client certificate must fail the handshake")
			_, err = connect(clientTLS(strangerPair), token)
			require.Error(t, err, "a client certificate from another CA must fail the handshake")
			status, err := connect(clientTLS(clientPair), "")
			require.NoError(t, err)
			require.Equal(t, http.StatusUnauthorized, status, "a valid certificate without a token is still unauthenticated")
			status, err = connect(clientTLS(clientPair), token)
			require.NoError(t, err)
			require.Equal(t, ok, status, "a valid certificate and a valid token must be admitted")

			// A server certificate for another name is refused by a verifying client.
			wrongName := clientTLS(clientPair)
			wrongName.ServerName = "other.example"
			_, err = connect(wrongName, token)
			require.Error(t, err, "a certificate for another name must fail client verification")

			block, _ := pem.Decode(clientCertPEM)
			sum := sha256.Sum256(block.Bytes)
			audit := logs.String()
			require.Contains(t, audit, "token_id="+tokenRecord.ID)
			require.Contains(t, audit, "certificate_fingerprint="+hex.EncodeToString(sum[:]), "the audit line must carry the verified client certificate fingerprint")
			require.NotContains(t, audit, token)
			require.NotContains(t, audit, token[strings.IndexByte(token, '.')+1:])
		})
	}
}
