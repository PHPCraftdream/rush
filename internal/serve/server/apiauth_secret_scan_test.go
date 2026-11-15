package server

import (
	"bufio"
	"bytes"
	cryptorand "crypto/rand"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/PHPCraftdream/rush/internal/config"
)

func TestChainRealListenerNeverLeaksCredentialSecrets(t *testing.T) {
	isolateSecurityGlobals(t)
	store := config.GlobalAPITokenStore()
	verifier := store
	capture := &captureHandler{notify: make(chan struct{}, 64)}
	credentials := []string{}
	randomBytes := bytes.Repeat([]byte{0xfb, 0xff}, 16)
	randomBytes = append(randomBytes, 1, 2, 3, 4)
	oldRandomReader := cryptorand.Reader
	defer func() { cryptorand.Reader = oldRandomReader }()
	cryptorand.Reader = bytes.NewReader(randomBytes)
	token, _, err := store.CreateAPIToken("secret-scan", []string{"read"}, "ask", nil)
	cryptorand.Reader = oldRandomReader
	if err != nil {
		t.Fatal(err)
	}
	wrong := []byte(token)
	separator := strings.IndexByte(token, '.') + 1
	if wrong[separator] == 'A' {
		wrong[separator] = 'B'
	} else {
		wrong[separator] = 'A'
	}
	revokedToken, revokedRecord, err := store.CreateAPIToken("revoked", []string{"read"}, "ask", nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.RevokeAPIToken(revokedRecord.ID); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	past := now.Add(-time.Hour)
	expiredToken, _, err := store.CreateAPIToken("expired", []string{"read"}, "ask", &past)
	if err != nil {
		t.Fatal(err)
	}
	secret := token[strings.IndexByte(token, '.')+1:]
	if len(secret) != 43 || !strings.Contains(secret, "-") || !strings.Contains(secret, "_") {
		t.Fatalf("generated secret must contain both '-' and '_': %q", secret)
	}
	credentials = append(credentials, token, secret, revokedToken, revokedToken[strings.IndexByte(revokedToken, '.')+1:], expiredToken, expiredToken[strings.IndexByte(expiredToken, '.')+1:], string(wrong), string(wrong[separator:]), strings.Repeat("A", 43))
	request := func(next http.Handler, raw string) []byte {
		t.Helper()
		srv := httptest.NewServer(next)
		defer srv.Close()
		address := strings.TrimPrefix(srv.URL, "http://")
		conn, err := (&net.Dialer{Timeout: 5 * time.Second}).DialContext(t.Context(), "tcp", address)
		if err != nil {
			t.Fatal(err)
		}
		defer conn.Close()
		if err := conn.SetDeadline(time.Now().Add(5 * time.Second)); err != nil {
			t.Fatal(err)
		}
		if _, err := fmt.Fprint(conn, raw); err != nil {
			t.Fatal(err)
		}
		response, err := io.ReadAll(conn)
		if err != nil {
			t.Fatal(err)
		}
		for _, line := range strings.Split(raw, "\r\n") {
			if at := strings.Index(line, "Bearer "); at >= 0 {
				suffix := line[at+len("Bearer "):]
				credentials = append(credentials, suffix, strings.TrimSpace(suffix))
			}
		}
		return response
	}
	makeChain := func(cfg ChainConfig, next http.Handler) http.Handler {
		cfg.Verifier = verifier
		cfg.Logger = slog.New(capture)
		return Chain(cfg)(next)
	}
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/panic" {
			panic("controlled panic")
		}
		if r.URL.Path == "/oversize" {
			_, _ = io.Copy(io.Discard, r.Body)
			return
		}
		w.WriteHeader(http.StatusOK)
	})
	base := ChainConfig{AuthConfig: AuthConfig{Verifier: verifier}, Logger: slog.New(capture), RPS: 1000, Burst: 100, MaxBody: 2}
	good := "GET /ok HTTP/1.1\r\nHost: local\r\nAuthorization: Bearer " + token + "\r\nConnection: close\r\n\r\n"
	requests := []string{}
	requests = append(requests, good)
	for _, bad := range []string{"", "Authorization: Basic " + token + "\r\n", "Authorization: Bearer malformed\r\n", "Authorization: Bearer rush_ffffffff." + strings.Repeat("A", 43) + "\r\n", "Authorization: Bearer " + string(wrong) + "\r\n", "Authorization: Bearer " + revokedToken + "\r\n", "Authorization: Bearer " + expiredToken + "\r\n"} {
		requests = append(requests, "GET /bad HTTP/1.1\r\nHost: local\r\n"+bad+"Connection: close\r\n\r\n")
	}
	cases := [][]byte{}
	for _, raw := range requests {
		cases = append(cases, request(makeChain(base, next), raw))
	}
	cases = append(cases, request(makeChain(ChainConfig{AuthConfig: AuthConfig{Verifier: verifier, AllowedTokenIDs: []string{"ffffffff"}}, RPS: 1000, Burst: 100}, next), "GET /allow-list HTTP/1.1\r\nHost: local\r\nAuthorization: Bearer "+token+"\r\nConnection: close\r\n\r\n"))
	// Back-off uses its own chain and starts at offense ten; all ten responses are scanned.
	backoffNow := time.Now()
	backoffHandler := makeChain(ChainConfig{AuthConfig: AuthConfig{Verifier: verifier, Clock: func() time.Time { return backoffNow }}, RPS: 1000, Burst: 100}, next)
	for i := 0; i < 10; i++ {
		cases = append(cases, request(backoffHandler, "GET /bad HTTP/1.1\r\nHost: local\r\nAuthorization: Bearer malformed\r\nConnection: close\r\n\r\n"))
	}
	cases = append(cases, request(backoffHandler, "GET /blocked HTTP/1.1\r\nHost: local\r\nConnection: close\r\n\r\n"))
	limited := makeChain(ChainConfig{AuthConfig: AuthConfig{Verifier: verifier}, RPS: 0.0001, Burst: 1}, next)
	cases = append(cases, request(limited, "GET /one HTTP/1.1\r\nHost: local\r\nAuthorization: Bearer "+token+"\r\nConnection: close\r\n\r\n"))
	cases = append(cases, request(limited, "GET /two HTTP/1.1\r\nHost: local\r\nAuthorization: Bearer "+token+"\r\nConnection: close\r\n\r\n"))
	oversize := makeChain(base, next)
	cases = append(cases, request(oversize, "POST /oversize HTTP/1.1\r\nHost: local\r\nAuthorization: Bearer "+token+"\r\nContent-Length: 9\r\nConnection: close\r\n\r\n123456789"))
	panicResponse := request(makeChain(base, next), "GET /panic HTTP/1.1\r\nHost: local\r\nAuthorization: Bearer "+token+"\r\nConnection: close\r\n\r\n")
	cases = append(cases, panicResponse)
	for _, path := range []string{"/?token=" + token, "/?access_token=" + token} {
		cases = append(cases, request(makeChain(base, next), "GET "+path+" HTTP/1.1\r\nHost: local\r\nAuthorization: Bearer "+token+"\r\nConnection: close\r\n\r\n"))
	}
	cases = append(cases, request(makeChain(base, next), "GET /spaces HTTP/1.1\r\nHost: local\r\nAuthorization:  Bearer  "+token+"  \r\nConnection: close\r\n\r\n"))
	for _, raw := range append(requests, "GET /allow-list HTTP/1.1\r\nAuthorization: Bearer "+token+"\r\n", "GET /spaces HTTP/1.1\r\nAuthorization:  Bearer  "+token+"  \r\n") {
		for _, line := range strings.Split(raw, "\r\n") {
			if at := strings.Index(line, "Bearer "); at >= 0 {
				suffix := line[at+len("Bearer "):]
				credentials = append(credentials, suffix, strings.TrimSpace(suffix))
			}
		}
	}
	filteredCredentials := credentials[:0]
	for _, credential := range credentials {
		if credential != "" {
			filteredCredentials = append(filteredCredentials, credential)
		}
	}
	credentials = filteredCredentials
	standaloneSecrets := []string{}
	for _, credential := range credentials {
		trimmed := strings.TrimSpace(credential)
		if dot := strings.IndexByte(trimmed, '.'); dot >= 0 && dot+1 < len(trimmed) {
			standaloneSecrets = append(standaloneSecrets, trimmed[dot+1:])
		}
	}
	credentials = append(credentials, standaloneSecrets...)
	for _, response := range cases {
		for _, credential := range credentials {
			if strings.Contains(string(response), credential) {
				t.Fatalf("raw response leaked credential %q", credential)
			}
		}
	}
	records := waitForRecords(t, capture, len(cases))
	for i, response := range cases {
		wantStatus := http.StatusUnauthorized
		switch {
		case i == 0 || i == 20:
			wantStatus = http.StatusOK
		case i >= 9 && i <= 18:
			wantStatus = http.StatusUnauthorized
		case i == 19 || i == 21:
			wantStatus = http.StatusTooManyRequests
		case i == 22:
			wantStatus = http.StatusRequestEntityTooLarge
		case i == 23:
			if len(response) != 0 {
				t.Fatalf("panic response %d was not empty: %q", i, response)
			}
			continue
		}
		if strings.Contains(string(response), token) || strings.Contains(string(response), secret) || strings.Contains(string(response), "Bearer "+token) {
			t.Fatalf("raw response %d leaked credential: %q", i, response)
		}
		if len(response) == 0 {
			t.Fatalf("response %d was empty, want HTTP %d", i, wantStatus)
		}
		parsed, err := http.ReadResponse(bufio.NewReader(strings.NewReader(string(response))), nil)
		if err != nil {
			t.Fatalf("parse response %d: %v", i, err)
		}
		defer parsed.Body.Close()
		if parsed.StatusCode != wantStatus {
			t.Errorf("response %d status=%d want %d", i, parsed.StatusCode, wantStatus)
		}
		for name, values := range parsed.Header {
			for _, credential := range credentials {
				if credential != "" && strings.Contains(name, credential) {
					t.Fatalf("response %d header name leaked credential", i)
				}
			}
			for _, value := range values {
				for _, credential := range credentials {
					if credential != "" && strings.Contains(value, credential) {
						t.Fatalf("response %d header value leaked credential", i)
					}
				}
			}
		}
		body, err := io.ReadAll(parsed.Body)
		closeErr := parsed.Body.Close()
		if err != nil || closeErr != nil {
			t.Fatalf("read/close response %d body: %v / %v", i, err, closeErr)
		}
		for _, credential := range credentials {
			if credential != "" && strings.Contains(string(body), credential) {
				t.Fatalf("response %d body leaked credential %q", i, credential)
			}
		}
	}
	if records[23].attrs["status"] != http.StatusInternalServerError {
		t.Fatalf("panic audit status=%v, want 500", records[23].attrs["status"])
	}
	for _, record := range records {
		text := fmt.Sprintf("%s %#v", record.message, record.attrs)
		for _, credential := range credentials {
			if credential != "" && strings.Contains(text, credential) {
				t.Fatalf("captured record leaked credential %q: %s", credential, text)
			}
		}
		for _, value := range record.attrs {
			for _, credential := range credentials {
				if credential != "" && strings.Contains(fmt.Sprint(value), credential) {
					t.Fatalf("captured attribute leaked credential %q: %#v", credential, value)
				}
			}
		}
	}
}
