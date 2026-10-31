package mcp

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/PHPCraftdream/rush/internal/config"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/require"
)

func TestMCPHTTPClientsRedirectPolicy(t *testing.T) {
	const authorization = "Bearer secret"
	for _, transportType := range []config.MCPType{config.MCPHttp, config.MCPSSE} {
		for _, crossOrigin := range []bool{false, true} {
			name := "same-origin"
			if crossOrigin {
				name = "cross-origin"
			}
			t.Run(string(transportType)+"/"+name, func(t *testing.T) {
				originHeaders := make(chan string, 2)
				targetHeaders := make(chan string, 1)
				targetRequests := make(chan struct{}, 1)
				target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					targetRequests <- struct{}{}
					targetHeaders <- r.Header.Get("Authorization")
					w.WriteHeader(http.StatusNoContent)
				}))
				defer target.Close()

				origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					originHeaders <- r.Header.Get("Authorization")
					if r.URL.Path == "/start" {
						location := "/final"
						if crossOrigin {
							location = target.URL + "/final"
						}
						http.Redirect(w, r, location, http.StatusFound)
						return
					}
					w.WriteHeader(http.StatusNoContent)
				}))
				defer origin.Close()

				created, err := createTransport(context.Background(), config.MCPConfig{
					Type:    transportType,
					URL:     origin.URL,
					Headers: map[string]string{"Authorization": authorization},
				}, config.IdentityResolver())
				require.NoError(t, err)
				var client *http.Client
				switch tr := created.(type) {
				case *mcp.StreamableClientTransport:
					client = tr.HTTPClient
				case *mcp.SSEClientTransport:
					client = tr.HTTPClient
				default:
					t.Fatalf("unexpected transport %T", created)
				}

				resp, err := client.Get(origin.URL + "/start")
				require.Equal(t, authorization, receiveHeader(t, originHeaders))
				if crossOrigin {
					require.Error(t, err)
					require.EqualError(t, err, fmt.Sprintf("Get %q: mcp: refusing cross-origin redirect from %s to %s", target.URL+"/final", origin.URL+"/start", target.URL+"/final"))
					select {
					case <-targetRequests:
						t.Fatal("cross-origin redirect target received a request")
					default:
					}
				} else {
					require.NoError(t, err)
					require.Equal(t, http.StatusNoContent, resp.StatusCode)
					require.NoError(t, resp.Body.Close())
					require.Equal(t, authorization, receiveHeader(t, originHeaders))
				}
			})
		}
	}
}

func receiveHeader(t *testing.T, headers <-chan string) string {
	t.Helper()
	select {
	case value := <-headers:
		return value
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for HTTP request")
		return ""
	}
}

func TestCheckMCPRedirectOriginComparison(t *testing.T) {
	original, err := http.NewRequest(http.MethodGet, "https://mcp.example.test:443/start", nil)
	require.NoError(t, err)

	tests := []struct {
		name    string
		target  string
		wantErr bool
	}{
		{name: "same origin", target: "https://mcp.example.test/final"},
		{name: "scheme differs", target: "http://mcp.example.test:443/final", wantErr: true},
		{name: "hostname differs", target: "https://other.example.test/final", wantErr: true},
		{name: "port differs", target: "https://mcp.example.test:444/final", wantErr: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			req, err := http.NewRequest(http.MethodGet, tc.target, nil)
			require.NoError(t, err)
			via := []*http.Request{original}
			err = checkMCPRedirect(req, via)
			if tc.wantErr {
				require.EqualError(t, err, fmt.Sprintf("mcp: refusing cross-origin redirect from %s to %s", original.URL.Redacted(), req.URL.Redacted()))
			} else {
				require.NoError(t, err)
			}
		})
	}
}

func TestCheckMCPRedirectUsesFirstRequestInVia(t *testing.T) {
	original, err := http.NewRequest(http.MethodGet, "https://mcp.example.test/start", nil)
	require.NoError(t, err)
	intermediate, err := http.NewRequest(http.MethodGet, "https://other.example.test/intermediate", nil)
	require.NoError(t, err)
	target, err := http.NewRequest(http.MethodGet, "https://mcp.example.test/final", nil)
	require.NoError(t, err)

	err = checkMCPRedirect(target, []*http.Request{original, intermediate})
	require.NoError(t, err)
}

func TestCheckMCPRedirectPreservesLimit(t *testing.T) {
	original, err := http.NewRequest(http.MethodGet, "https://mcp.example.test/", nil)
	require.NoError(t, err)
	via := make([]*http.Request, 10)
	via[0] = original
	req := original.Clone(context.Background())
	req.URL.Path = "/next"
	err = checkMCPRedirect(req, via)
	require.Error(t, err)
	require.ErrorContains(t, err, "10 redirects")
}
