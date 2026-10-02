package codex

import (
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestCallbackOnlySendsFirstResult(t *testing.T) {
	result := make(chan callbackResult, 1)
	handler := callbackHandler("expected-state", result)
	request := func(code string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(http.MethodGet, "http://localhost:1455/auth/callback?state=expected-state&code="+code, nil)
		r.Host = "localhost:1455"
		r.RemoteAddr = "127.0.0.1:12345"
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, r)
		return response
	}

	if response := request("first-code"); response.Code != http.StatusOK {
		t.Fatalf("first callback status = %d, want %d", response.Code, http.StatusOK)
	}
	first := <-result
	if first.err != nil || first.code != "first-code" {
		t.Fatalf("first callback result = %#v", first)
	}

	secondDone := make(chan *httptest.ResponseRecorder, 1)
	go func() { secondDone <- request("duplicate-code") }()
	select {
	case response := <-secondDone:
		if response.Code != http.StatusOK {
			t.Fatalf("duplicate callback status = %d, want %d", response.Code, http.StatusOK)
		}
	case <-time.After(time.Second):
		t.Fatal("duplicate callback handler blocked")
	}
	select {
	case stale := <-result:
		t.Fatalf("duplicate callback left a stale result: %#v", stale)
	default:
	}
}

func TestLoopbackListenerBindsIPv4AndAvailableIPv6(t *testing.T) {
	probe, probeErr := net.Listen("tcp6", net.JoinHostPort("::1", "0"))
	ipv6Available := probeErr == nil
	if probeErr == nil {
		if err := probe.Close(); err != nil {
			t.Fatal(err)
		}
	}

	listener, err := listenLoopback("0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	addr, ok := listener.Addr().(*net.TCPAddr)
	if !ok {
		t.Fatalf("callback listener address = %T, want *net.TCPAddr", listener.Addr())
	}
	port := strconv.Itoa(addr.Port)
	for _, host := range []string{"127.0.0.1", "::1"} {
		if host == "::1" && !ipv6Available {
			continue
		}
		conn, err := net.DialTimeout("tcp", net.JoinHostPort(host, port), time.Second)
		if err != nil {
			t.Fatalf("could not connect to callback listener at %s: %v", net.JoinHostPort(host, port), err)
		}
		conn.Close()
	}
}

func TestLoopbackListenerIPv6ProbeFallbackAndFailure(t *testing.T) {
	newFake := func(ip string, port int) *testNetListener {
		return &testNetListener{addr: &net.TCPAddr{IP: net.ParseIP(ip), Port: port}}
	}

	t.Run("unsupported IPv6 uses IPv4 listener", func(t *testing.T) {
		ipv4 := newFake("127.0.0.1", 1455)
		listener, err := listenLoopbackWith("0", func(network, address string) (net.Listener, error) {
			if network == "tcp4" {
				return ipv4, nil
			}
			return nil, syscall.EAFNOSUPPORT
		})
		if err != nil {
			t.Fatal(err)
		}
		if listener != ipv4 {
			t.Fatalf("listener = %T, want IPv4-only listener", listener)
		}
		listener.Close()
	})

	t.Run("unexpected probe failure is surfaced", func(t *testing.T) {
		ipv4 := newFake("127.0.0.1", 1455)
		probeErr := errors.New("temporary socket resource failure")
		listener, err := listenLoopbackWith("0", func(network, address string) (net.Listener, error) {
			if network == "tcp4" {
				return ipv4, nil
			}
			return nil, probeErr
		})
		if listener != nil || !errors.Is(err, probeErr) {
			t.Fatalf("listener/error = %v, %v; want nil and probe error", listener, err)
		}
		if !ipv4.closed {
			t.Fatal("IPv4 listener was left open after probe failure")
		}
	})

	t.Run("IPv6 port collision closes IPv4 listener", func(t *testing.T) {
		ipv4 := newFake("127.0.0.1", 1455)
		probe := newFake("::1", 38000)
		calls := 0
		listener, err := listenLoopbackWith("0", func(network, address string) (net.Listener, error) {
			calls++
			switch {
			case network == "tcp4":
				return ipv4, nil
			case strings.HasSuffix(address, ":0"):
				return probe, nil
			default:
				if address != "[::1]:1455" {
					t.Errorf("IPv6 listener address = %q, want [::1]:1455", address)
				}
				return nil, syscall.EADDRINUSE
			}
		})
		if listener != nil || !errors.Is(err, syscall.EADDRINUSE) {
			t.Fatalf("listener/error = %v, %v; want nil and address-in-use", listener, err)
		}
		if calls != 3 || !ipv4.closed || !probe.closed {
			t.Fatalf("listen calls=%d, IPv4 closed=%v, probe closed=%v", calls, ipv4.closed, probe.closed)
		}
	})
}

type testNetListener struct {
	addr   net.Addr
	closed bool
}

func (l *testNetListener) Accept() (net.Conn, error) { return nil, net.ErrClosed }
func (l *testNetListener) Close() error {
	l.closed = true
	return nil
}
func (l *testNetListener) Addr() net.Addr { return l.addr }
