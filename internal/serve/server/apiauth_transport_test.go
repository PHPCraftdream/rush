package server

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/PHPCraftdream/rush/internal/config"
)

const transportTestCredential = "rush_12345678.aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"

func assertNoTransportCredential(t *testing.T, capture *captureHandler) {
	t.Helper()
	for _, record := range capture.snapshot() {
		text := record.message + " " + fmt.Sprint(record.attrs)
		if strings.Contains(text, transportTestCredential) || strings.Contains(text, strings.TrimPrefix(transportTestCredential, "rush_12345678.")) {
			t.Fatalf("transport audit leaked credential: %#v", record)
		}
	}
}

func TestChainRealListenerHijackRequiresBearer(t *testing.T) {
	isolateAPIGlobals(t)
	v := &fakeVerifier{token: config.APIToken{ID: "12345678"}, ok: true}
	capture := &captureHandler{notify: make(chan struct{}, 8)}
	h := Chain(ChainConfig{AuthConfig: AuthConfig{Verifier: v}, Logger: slog.New(capture), Burst: 20})(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, rw, err := http.NewResponseController(w).Hijack()
		if err != nil {
			return
		}
		defer conn.Close()
		_, _ = rw.WriteString("HTTP/1.1 101 Switching Protocols\r\nConnection: Upgrade\r\nUpgrade: test\r\n\r\n")
		_ = rw.Flush()
		buf := make([]byte, 5)
		if _, err := io.ReadFull(rw, buf); err == nil {
			_, _ = rw.Write(buf)
			_ = rw.Flush()
		}
	}))
	srv := httptest.NewServer(h)
	defer srv.Close()
	address := strings.TrimPrefix(srv.URL, "http://")
	request := func(token, upgrade bool) net.Conn {
		conn, err := (&net.Dialer{Timeout: 10 * time.Second}).DialContext(context.Background(), "tcp", address)
		if err != nil {
			t.Fatal(err)
		}
		_ = conn.SetDeadline(time.Now().Add(10 * time.Second))
		if upgrade {
			_, _ = fmt.Fprintf(conn, "GET / HTTP/1.1\r\nHost: %s\r\nConnection: Upgrade\r\nUpgrade: test\r\n", address)
		} else {
			_, _ = fmt.Fprintf(conn, "GET / HTTP/1.1\r\nHost: %s\r\nConnection: keep-alive\r\n", address)
		}
		if token {
			_, _ = fmt.Fprint(conn, "Authorization: Bearer "+transportTestCredential+"\r\n")
		}
		_, _ = fmt.Fprint(conn, "\r\n")
		return conn
	}
	conn := request(true, true)
	reader := bufio.NewReader(conn)
	line, err := reader.ReadString('\n')
	if err != nil || !strings.HasPrefix(line, "HTTP/1.1 101") {
		t.Fatalf("upgrade status=%q err=%v", line, err)
	}
	for {
		line, err = reader.ReadString('\n')
		if err != nil {
			t.Fatal(err)
		}
		if line == "\r\n" {
			break
		}
	}
	_, _ = conn.Write([]byte("hello"))
	echo := make([]byte, 5)
	if _, err := io.ReadFull(reader, echo); err != nil || string(echo) != "hello" {
		t.Fatalf("echo=%q err=%v", echo, err)
	}
	_ = conn.Close()
	unauthorized := request(false, true)
	defer unauthorized.Close()
	resp, err := http.ReadResponse(bufio.NewReader(unauthorized), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	reference := request(false, false)
	defer reference.Close()
	referenceResponse, err := http.ReadResponse(bufio.NewReader(reference), nil)
	if err != nil {
		t.Fatal(err)
	}
	referenceBody, err := io.ReadAll(referenceResponse.Body)
	closeErr := referenceResponse.Body.Close()
	if err != nil || closeErr != nil {
		t.Fatalf("read/close reference response: %v / %v", err, closeErr)
	}
	if got, want := resp.Status+"\n"+rawHeadersExceptDate(resp.Header)+"\n"+string(body), referenceResponse.Status+"\n"+rawHeadersExceptDate(referenceResponse.Header)+"\n"+string(referenceBody); got != want {
		t.Fatalf("hijack unauthorized response=%q, normal 401 reference=%q", got, want)
	}
	waitForRecords(t, capture, 3)
	assertNoTransportCredential(t, capture)
}

func TestChainRealListenerFlushesBeforeHandlerReturns(t *testing.T) {
	isolateAPIGlobals(t)
	v := &fakeVerifier{token: config.APIToken{ID: "12345678"}, ok: true}
	capture := &captureHandler{notify: make(chan struct{}, 8)}
	release := make(chan struct{})
	returned := make(chan struct{})
	var releaseOnce sync.Once
	h := Chain(ChainConfig{AuthConfig: AuthConfig{Verifier: v}, Logger: slog.New(capture), Burst: 10})(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		defer close(returned)
		_, _ = io.WriteString(w, "event: ready\n\n")
		w.(http.Flusher).Flush()
		select {
		case <-release:
		case <-time.After(10 * time.Second):
		}
	}))
	srv := httptest.NewServer(h)
	defer srv.Close()
	defer releaseOnce.Do(func() { close(release) })
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+transportTestCredential)
	resp, err := (&http.Client{Timeout: 10 * time.Second}).Do(req) //nolint:bodyclose // Closed by the deferred Close below; the body is also read in a goroutine.
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	read := make(chan string, 1)
	go func() { line, _ := bufio.NewReader(resp.Body).ReadString('\n'); read <- line }()
	select {
	case line := <-read:
		if line != "event: ready\n" {
			t.Fatalf("first event=%q", line)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("flush did not reach client before handler returned")
	}
	select {
	case <-returned:
		t.Fatal("handler returned before the flushed event was observed")
	default:
	}
	releaseOnce.Do(func() { close(release) })
	select {
	case <-returned:
	case <-time.After(10 * time.Second):
		t.Fatal("handler did not return after release")
	}
	waitForRecords(t, capture, 1)
	assertNoTransportCredential(t, capture)
}

var _ http.Flusher = (*statusWriter)(nil)
