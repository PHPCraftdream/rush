package server

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/PHPCraftdream/rush/internal/config"
)

func rawWireRequest(t *testing.T, address, request string) []byte {
	t.Helper()
	conn, err := (&net.Dialer{Timeout: 10 * time.Second}).DialContext(context.Background(), "tcp", address)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(10 * time.Second))
	if _, err := fmt.Fprint(conn, request); err != nil {
		t.Fatal(err)
	}
	all, err := io.ReadAll(conn)
	if err != nil {
		t.Fatal(err)
	}
	return all
}

func normalizeWireDate(raw []byte) []byte {
	lines := bytes.SplitAfter(raw, []byte("\r\n"))
	for i, line := range lines {
		if len(line) == 2 && bytes.Equal(line, []byte("\r\n")) {
			break
		}
		if bytes.HasPrefix(bytes.ToLower(line), []byte("date:")) {
			lines[i] = []byte("Date: <normalized>\r\n")
		}
	}
	return bytes.Join(lines, nil)
}

func TestRealStoreRawWireAuthenticationFailuresAreUniform(t *testing.T) {
	isolateSecurityGlobals(t)
	store := config.GlobalAPITokenStore()
	now := time.Now().UTC().Truncate(time.Second)
	valid, rec, err := store.CreateAPIToken("valid", []string{"read"}, "ask", nil)
	if err != nil {
		t.Fatal(err)
	}
	revoked, revokedRec, err := store.CreateAPIToken("revoked", []string{"read"}, "ask", nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.RevokeAPIToken(revokedRec.ID); err != nil {
		t.Fatal(err)
	}
	past := now.Add(-time.Hour)
	expired, _, err := store.CreateAPIToken("expired", []string{"read"}, "ask", &past)
	if err != nil {
		t.Fatal(err)
	}
	wrong := []byte(valid)
	at := strings.IndexByte(valid, '.') + 1
	if wrong[at] == 'A' {
		wrong[at] = 'B'
	} else {
		wrong[at] = 'A'
	}
	cases := []struct {
		name, header string
		allowed      []string
	}{
		{"no header", "", nil},
		{"wrong scheme", "Authorization: Basic " + valid + "\r\n", nil},
		{"malformed", "Authorization: Bearer malformed\r\n", nil},
		{"unknown id", "Authorization: Bearer rush_ffffffff." + strings.Repeat("A", 43) + "\r\n", nil},
		{"wrong secret", "Authorization: Bearer " + string(wrong) + "\r\n", nil},
		{"revoked", "Authorization: Bearer " + revoked + "\r\n", nil},
		{"expired", "Authorization: Bearer " + expired + "\r\n", nil},
		{"allow list", "Authorization: Bearer " + valid + "\r\n", []string{"ffffffff"}},
	}
	var baseline []byte
	for i, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			capture := &captureHandler{notify: make(chan struct{}, 8)}
			h := Chain(ChainConfig{AuthConfig: AuthConfig{Verifier: store, AllowedTokenIDs: tc.allowed, Clock: func() time.Time { return now }}, Logger: slog.New(capture)})(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) }))
			srv := httptest.NewServer(h)
			defer srv.Close()
			address := strings.TrimPrefix(srv.URL, "http://")
			request := "GET / HTTP/1.1\r\nHost: " + address + "\r\n" + tc.header + "Connection: close\r\n\r\n"
			got := normalizeWireDate(rawWireRequest(t, address, request))
			if i == 0 {
				baseline = got
			} else if !bytes.Equal(got, baseline) {
				t.Fatalf("response differs from no-header response:\n got %q\nwant %q", got, baseline)
			}
			for _, record := range waitForRecords(t, capture, 1) {
				text := record.message + " " + fmt.Sprint(record.attrs)
				for _, credential := range []string{valid, revoked, expired, string(wrong)} {
					if strings.Contains(text, credential) || strings.Contains(text, credential[strings.IndexByte(credential, '.')+1:]) {
						t.Fatalf("wire audit leaked credential: %s", text)
					}
				}
			}
		})
	}
	if rec.ID == "" {
		t.Fatal("missing token record")
	}
}

func TestRealStoreMalformedFileReturnsFixed503Wire(t *testing.T) {
	isolateSecurityGlobals(t)
	store := config.GlobalAPITokenStore()
	if err := os.MkdirAll(filepath.Dir(store.Path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(store.Path, []byte("malformed"), 0o600); err != nil {
		t.Fatal(err)
	}
	capture := &captureHandler{notify: make(chan struct{}, 2)}
	h := Chain(ChainConfig{AuthConfig: AuthConfig{Verifier: store}, Logger: slog.New(capture)})(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	srv := httptest.NewServer(h)
	defer srv.Close()
	address := strings.TrimPrefix(srv.URL, "http://")
	got := normalizeWireDate(rawWireRequest(t, address, "GET / HTTP/1.1\r\nHost: "+address+"\r\nAuthorization: Bearer rush_12345678."+strings.Repeat("A", 43)+"\r\nConnection: close\r\n\r\n"))
	if !bytes.Equal(got, []byte("HTTP/1.1 503 Service Unavailable\r\nContent-Type: application/problem+json\r\nDate: <normalized>\r\nContent-Length: 66\r\nConnection: close\r\n\r\n"+authUnavailableBody)) {
		t.Fatalf("fixed 503 wire response mismatch:\n got %q\nwant %q", got, []byte("HTTP/1.1 503 Service Unavailable\r\nContent-Type: application/problem+json\r\nDate: <normalized>\r\nContent-Length: 66\r\nConnection: close\r\n\r\n"+authUnavailableBody))
	}
	for _, record := range waitForRecords(t, capture, 2) {
		text := record.message + " " + fmt.Sprint(record.attrs)
		credential := "rush_12345678." + strings.Repeat("A", 43)
		if strings.Contains(text, credential) || strings.Contains(text, strings.Repeat("A", 43)) {
			t.Fatalf("503 audit leaked credential: %s", text)
		}
	}
}
