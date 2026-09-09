package mcp

import (
	"context"
	"io"
	"maps"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/PHPCraftdream/rush/internal/config"
	"github.com/PHPCraftdream/rush/internal/env"
	"github.com/PHPCraftdream/rush/internal/pubsub"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/require"
	"go.uber.org/goleak"
)

// shellResolverWithPath builds a shell resolver whose env carries PATH
// plus any caller-supplied overrides. Without PATH, $(cat), $(echo),
// etc. can't find their binaries in a test process where the shell env
// is otherwise empty.
func shellResolverWithPath(t *testing.T, overrides map[string]string) config.VariableResolver {
	t.Helper()
	m := map[string]string{"PATH": os.Getenv("PATH")}
	maps.Copy(m, overrides)
	return config.NewShellVariableResolver(env.NewFromMap(m))
}

func TestMCPSession_CancelOnClose(t *testing.T) {
	defer goleak.VerifyNone(t)

	serverTransport, clientTransport := mcp.NewInMemoryTransports()

	server := mcp.NewServer(&mcp.Implementation{Name: "test-server"}, nil)
	serverSession, err := server.Connect(context.Background(), serverTransport, nil)
	require.NoError(t, err)
	defer serverSession.Close()

	ctx, cancel := context.WithCancel(context.Background())

	client := mcp.NewClient(&mcp.Implementation{Name: "rush-test"}, nil)
	clientSession, err := client.Connect(ctx, clientTransport, nil)
	require.NoError(t, err)

	sess := &ClientSession{ClientSession: clientSession, cancel: cancel}

	// Verify the context is not cancelled before close.
	require.NoError(t, ctx.Err())

	err = sess.Close()
	require.NoError(t, err)

	// After Close, the context must be cancelled.
	require.ErrorIs(t, ctx.Err(), context.Canceled)
}

// TestCreateTransport_URLResolution pins that m.URL goes through the
// same resolver seam as command, args, env, and headers. Covers both
// the HTTP and SSE branches, success and failure, so a regression in
// ResolvedURL wiring is caught at the transport layer rather than only
// at the config layer.
func TestCreateTransport_URLResolution(t *testing.T) {
	t.Parallel()

	shell := config.NewShellVariableResolver(env.NewFromMap(map[string]string{
		"MCP_HOST": "mcp.example.com",
	}))

	t.Run("http success expands $VAR", func(t *testing.T) {
		t.Parallel()
		m := config.MCPConfig{
			Type: config.MCPHttp,
			URL:  "https://$MCP_HOST/api",
		}
		tr, err := createTransport(t.Context(), m, shell)
		require.NoError(t, err)
		require.NotNil(t, tr)
		sct, ok := tr.(*mcp.StreamableClientTransport)
		require.True(t, ok, "expected StreamableClientTransport, got %T", tr)
		require.Equal(t, "https://mcp.example.com/api", sct.Endpoint)
	})

	t.Run("sse success expands $(cmd)", func(t *testing.T) {
		t.Parallel()
		m := config.MCPConfig{
			Type: config.MCPSSE,
			URL:  "https://$(echo mcp.example.com)/events",
		}
		tr, err := createTransport(t.Context(), m, shell)
		require.NoError(t, err)
		sse, ok := tr.(*mcp.SSEClientTransport)
		require.True(t, ok, "expected SSEClientTransport, got %T", tr)
		require.Equal(t, "https://mcp.example.com/events", sse.Endpoint)
	})

	t.Run("http failing $(cmd) surfaces error, no transport created", func(t *testing.T) {
		t.Parallel()
		// Under lenient nounset, unset $VAR expands to "" silently,
		// so the only way a URL resolution *errors* is a failing
		// $(cmd). Mirror the SSE subtest so both transports share
		// coverage for the url-resolve-failure path.
		m := config.MCPConfig{
			Type: config.MCPHttp,
			URL:  "https://$(false)/api",
		}
		tr, err := createTransport(t.Context(), m, shellResolverWithPath(t, nil))
		require.Error(t, err)
		require.Nil(t, tr)
		require.Contains(t, err.Error(), "url:")
		require.Contains(t, err.Error(), "$(false)")
	})

	t.Run("http unset var expands empty", func(t *testing.T) {
		t.Parallel()
		// Pinning test for the new lenient-nounset default: an
		// unset bare $VAR in the URL is *not* an error. It
		// expands to "" and, here, leaves a syntactically weird
		// but non-empty URL that the existing non-empty guard
		// still lets through. Guards against a future regression
		// that flips strict-by-default back on.
		m := config.MCPConfig{
			Type: config.MCPHttp,
			URL:  "https://$MCP_MISSING_HOST/api",
		}
		tr, err := createTransport(t.Context(), m, shell)
		require.NoError(t, err)
		sct, ok := tr.(*mcp.StreamableClientTransport)
		require.True(t, ok)
		require.Equal(t, "https:///api", sct.Endpoint)
	})

	t.Run("sse failing $(cmd) surfaces error, no transport created", func(t *testing.T) {
		t.Parallel()
		m := config.MCPConfig{
			Type: config.MCPSSE,
			URL:  "https://$(false)/events",
		}
		tr, err := createTransport(t.Context(), m, shell)
		require.Error(t, err)
		require.Nil(t, tr)
		require.Contains(t, err.Error(), "url:")
		require.Contains(t, err.Error(), "$(false)")
	})

	t.Run("http empty-after-resolve still fails the non-empty guard", func(t *testing.T) {
		t.Parallel()
		// ${MCP_EMPTY:-} resolves to the empty string (no error),
		// then the existing TrimSpace guard in createTransport must
		// reject it so we never spawn a transport against "".
		m := config.MCPConfig{
			Type: config.MCPHttp,
			URL:  "${MCP_EMPTY:-}",
		}
		tr, err := createTransport(t.Context(), m, shell)
		require.Error(t, err)
		require.Nil(t, tr)
		require.Contains(t, err.Error(), "non-empty 'url'")
	})

	t.Run("identity resolver round-trips template verbatim", func(t *testing.T) {
		t.Parallel()
		// Client mode forwards the template to the server; no local
		// expansion, no error on unset vars.
		tmpl := "https://$MCP_MISSING_HOST/api"
		m := config.MCPConfig{Type: config.MCPHttp, URL: tmpl}
		tr, err := createTransport(t.Context(), m, config.IdentityResolver())
		require.NoError(t, err)
		sct, ok := tr.(*mcp.StreamableClientTransport)
		require.True(t, ok)
		require.Equal(t, tmpl, sct.Endpoint)
	})
}

// TestCreateTransport_StdioResolution pins that command, args, and env
// for stdio MCPs go through the same resolver seam as the other
// transports. Covers both success (expansion produced the expected
// exec.Cmd) and failure (any one field erroring prevents transport
// creation).
func TestCreateTransport_StdioResolution(t *testing.T) {
	t.Parallel()

	t.Run("success expands command, args, and env", func(t *testing.T) {
		t.Parallel()
		r := shellResolverWithPath(t, map[string]string{
			"MY_TOKEN": "hunter2",
		})
		m := config.MCPConfig{
			Type:    config.MCPStdio,
			Command: "forgejo-mcp",
			Args:    []string{"--token", "$MY_TOKEN", "--host", "$(echo example.com)"},
			Env: map[string]string{
				"SECRET":    "$(echo shh)",
				"PLAIN":     "literal",
				"REFERENCE": "$MY_TOKEN",
			},
		}
		tr, err := createTransport(t.Context(), m, r)
		require.NoError(t, err)
		require.NotNil(t, tr)

		ct, ok := tr.(*mcp.CommandTransport)
		require.True(t, ok, "expected CommandTransport, got %T", tr)

		// exec.Cmd.Args[0] is the command name; the rest are positional
		// args as passed.
		require.Equal(t, []string{"forgejo-mcp", "--token", "hunter2", "--host", "example.com"}, ct.Command.Args)

		// Env is os.Environ() + resolved entries (sorted). Check the
		// resolved entries are present with their expanded values.
		require.Contains(t, ct.Command.Env, "SECRET=shh")
		require.Contains(t, ct.Command.Env, "PLAIN=literal")
		require.Contains(t, ct.Command.Env, "REFERENCE=hunter2")
	})

	t.Run("env resolution failure surfaces error, no transport created", func(t *testing.T) {
		t.Parallel()
		r := shellResolverWithPath(t, nil)
		m := config.MCPConfig{
			Type:    config.MCPStdio,
			Command: "forgejo-mcp",
			Env:     map[string]string{"TOKEN": "$(false)"},
		}
		tr, err := createTransport(t.Context(), m, r)
		require.Error(t, err)
		require.Nil(t, tr)
		require.Contains(t, err.Error(), "env TOKEN")
	})

	t.Run("failing env command is a hard error", func(t *testing.T) {
		t.Parallel()
		// Under lenient nounset a bare $UNSET expands to ""
		// silently — see the pinning subtest below. The remaining
		// failure mode for env resolution is a $(cmd) that exits
		// non-zero, which must still error out and prevent exec so
		// we never hand a broken credential to the child process.
		r := shellResolverWithPath(t, nil)
		m := config.MCPConfig{
			Type:    config.MCPStdio,
			Command: "forgejo-mcp",
			Env:     map[string]string{"FORGEJO_ACCESS_TOKEN": "$(exit 5)"},
		}
		tr, err := createTransport(t.Context(), m, r)
		require.Error(t, err)
		require.Nil(t, tr)
		require.Contains(t, err.Error(), "env FORGEJO_ACCESS_TOKEN")
	})

	t.Run("unset env var expands empty", func(t *testing.T) {
		t.Parallel()
		// Pinning test for the lenient-nounset default: a bare
		// $UNSET in an env value expands to "" without error, and
		// the empty entry is kept on the resulting exec.Cmd (env
		// entries, unlike headers, are not dropped — see design
		// decision #18). Guards against a regression that flips
		// strict-by-default back on and silently breaks users
		// with configs like FORGEJO_ACCESS_TOKEN=$FORGEJO_TOKEN.
		r := shellResolverWithPath(t, nil)
		m := config.MCPConfig{
			Type:    config.MCPStdio,
			Command: "forgejo-mcp",
			Env:     map[string]string{"FORGEJO_ACCESS_TOKEN": "$FORGEJO_TOKEN_UNSET"},
		}
		tr, err := createTransport(t.Context(), m, r)
		require.NoError(t, err)
		ct, ok := tr.(*mcp.CommandTransport)
		require.True(t, ok)
		require.Contains(t, ct.Command.Env, "FORGEJO_ACCESS_TOKEN=")
	})

	t.Run("args resolution failure surfaces error, no transport created", func(t *testing.T) {
		t.Parallel()
		r := shellResolverWithPath(t, nil)
		m := config.MCPConfig{
			Type:    config.MCPStdio,
			Command: "forgejo-mcp",
			Args:    []string{"--token", "$(false)"},
		}
		tr, err := createTransport(t.Context(), m, r)
		require.Error(t, err)
		require.Nil(t, tr)
		require.Contains(t, err.Error(), "arg 1")
	})

	t.Run("command resolution failure surfaces error, no transport created", func(t *testing.T) {
		t.Parallel()
		r := shellResolverWithPath(t, nil)
		m := config.MCPConfig{
			Type:    config.MCPStdio,
			Command: "$(false)",
		}
		tr, err := createTransport(t.Context(), m, r)
		require.Error(t, err)
		require.Nil(t, tr)
		require.Contains(t, err.Error(), "invalid mcp command")
	})

	t.Run("identity resolver round-trips templates verbatim", func(t *testing.T) {
		t.Parallel()
		// Client mode: no local expansion, no error on unset vars.
		m := config.MCPConfig{
			Type:    config.MCPStdio,
			Command: "forgejo-mcp",
			Args:    []string{"--token", "$MCP_MISSING"},
			Env:     map[string]string{"TOKEN": "$(vault read -f token)"},
		}
		tr, err := createTransport(t.Context(), m, config.IdentityResolver())
		require.NoError(t, err)
		ct, ok := tr.(*mcp.CommandTransport)
		require.True(t, ok)
		require.Equal(t, []string{"forgejo-mcp", "--token", "$MCP_MISSING"}, ct.Command.Args)
		require.Contains(t, ct.Command.Env, "TOKEN=$(vault read -f token)")
	})
}

// TestCreateTransport_HeadersResolution pins that a single failing
// header aborts HTTP/SSE transport creation and that the successful
// resolver passes every expanded header through to the round tripper.
func TestCreateTransport_HeadersResolution(t *testing.T) {
	t.Parallel()

	t.Run("http headers success expands $(cmd)", func(t *testing.T) {
		t.Parallel()
		r := shellResolverWithPath(t, map[string]string{
			"GITHUB_TOKEN": "gh-secret",
		})
		m := config.MCPConfig{
			Type: config.MCPHttp,
			URL:  "https://mcp.example.com/api",
			Headers: map[string]string{
				"Authorization": "$(echo Bearer $GITHUB_TOKEN)",
				"X-Static":      "kept",
			},
		}
		tr, err := createTransport(t.Context(), m, r)
		require.NoError(t, err)

		sct, ok := tr.(*mcp.StreamableClientTransport)
		require.True(t, ok)
		rt, ok := sct.HTTPClient.Transport.(*headerRoundTripper)
		require.True(t, ok, "expected headerRoundTripper, got %T", sct.HTTPClient.Transport)
		require.Equal(t, map[string]string{
			"Authorization": "Bearer gh-secret",
			"X-Static":      "kept",
		}, rt.headers)
	})

	t.Run("http failing header surfaces error, no transport", func(t *testing.T) {
		t.Parallel()
		r := shellResolverWithPath(t, nil)
		m := config.MCPConfig{
			Type:    config.MCPHttp,
			URL:     "https://mcp.example.com/api",
			Headers: map[string]string{"Authorization": "$(false)"},
		}
		tr, err := createTransport(t.Context(), m, r)
		require.Error(t, err)
		require.Nil(t, tr)
		require.Contains(t, err.Error(), "header Authorization")
	})

	t.Run("sse failing header surfaces error, no transport", func(t *testing.T) {
		t.Parallel()
		// Under lenient nounset a bare $MISSING expands to "",
		// which ResolvedHeaders drops — no error. The failing
		// $(cmd) path is the remaining way this can fail loudly;
		// cover it on the SSE branch to mirror the HTTP subtest.
		r := shellResolverWithPath(t, nil)
		m := config.MCPConfig{
			Type:    config.MCPSSE,
			URL:     "https://mcp.example.com/events",
			Headers: map[string]string{"Authorization": "$(false)"},
		}
		tr, err := createTransport(t.Context(), m, r)
		require.Error(t, err)
		require.Nil(t, tr)
		require.Contains(t, err.Error(), "header Authorization")
	})

	t.Run("sse unset var header drops silently", func(t *testing.T) {
		t.Parallel()
		// Pinning test for empty-header drop + lenient nounset:
		// a header whose value resolves to "" (here because the
		// bare $VAR is unset) is omitted from the round tripper
		// rather than sent as "X-Header:". Guards against a
		// regression that either re-introduces strict-by-default
		// or stops dropping empty headers.
		r := shellResolverWithPath(t, nil)
		m := config.MCPConfig{
			Type:    config.MCPSSE,
			URL:     "https://mcp.example.com/events",
			Headers: map[string]string{"Authorization": "$MISSING_TOKEN"},
		}
		tr, err := createTransport(t.Context(), m, r)
		require.NoError(t, err)
		sse, ok := tr.(*mcp.SSEClientTransport)
		require.True(t, ok)
		rt, ok := sse.HTTPClient.Transport.(*headerRoundTripper)
		require.True(t, ok)
		require.NotContains(t, rt.headers, "Authorization")
	})
}

type contextRecordingResolver struct {
	contexts []context.Context
	markers  []any
	values   []string
}

func (r *contextRecordingResolver) ResolveValue(string) (string, error) {
	panic("runtime MCP resolution used the background-only API")
}

func (r *contextRecordingResolver) ResolveValueContext(ctx context.Context, value string) (string, error) {
	r.contexts = append(r.contexts, ctx)
	r.markers = append(r.markers, ctx.Value(contextMarkerKey{}))
	r.values = append(r.values, value)
	return value, nil
}

type contextMarkerKey struct{}

func TestCreateTransport_PassesLifetimeContextToEveryRuntimeField(t *testing.T) {
	t.Parallel()
	marker := "mcp-operation"
	ctx := context.WithValue(context.Background(), contextMarkerKey{}, marker)

	t.Run("stdio", func(t *testing.T) {
		r := &contextRecordingResolver{}
		m := config.MCPConfig{
			Type:    config.MCPStdio,
			Command: "echo",
			Args:    []string{"arg-1", "arg-2"},
			Env:     map[string]string{"A": "env-a", "B": "env-b"},
		}
		tr, err := createTransport(ctx, m, r)
		require.NoError(t, err)
		require.NotNil(t, tr)
		for _, got := range r.contexts {
			require.Same(t, ctx, got)
		}
		require.Equal(t, []any{marker, marker, marker, marker, marker}, r.markers)
		require.Equal(t, []string{"echo", "arg-1", "arg-2", "env-a", "env-b"}, r.values)
	})

	t.Run("http", func(t *testing.T) {
		r := &contextRecordingResolver{}
		m := config.MCPConfig{
			Type:    config.MCPHttp,
			URL:     "https://example.test/mcp",
			Headers: map[string]string{"Authorization": "token", "X-Trace": "trace"},
		}
		tr, err := createTransport(ctx, m, r)
		require.NoError(t, err)
		require.NotNil(t, tr)
		for _, got := range r.contexts {
			require.Same(t, ctx, got)
		}
		require.Equal(t, []any{marker, marker, marker}, r.markers)
		require.Equal(t, []string{"https://example.test/mcp", "token", "trace"}, r.values)
	})

	t.Run("sse", func(t *testing.T) {
		r := &contextRecordingResolver{}
		m := config.MCPConfig{
			Type:    config.MCPSSE,
			URL:     "https://example.test/events",
			Headers: map[string]string{"Authorization": "token", "X-Trace": "trace"},
		}
		tr, err := createTransport(ctx, m, r)
		require.NoError(t, err)
		require.NotNil(t, tr)
		for _, got := range r.contexts {
			require.Same(t, ctx, got)
		}
		require.Equal(t, []any{marker, marker, marker}, r.markers)
		require.Equal(t, []string{"https://example.test/events", "token", "trace"}, r.values)
	})
}

func TestCreateTransport_PreCanceledContextWinsFieldValidation(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	resolver := &contextRecordingResolver{}
	for _, m := range []config.MCPConfig{
		{Type: config.MCPStdio, Command: ""},
		{Type: config.MCPHttp, URL: "$"},
		{Type: config.MCPSSE, URL: ""},
	} {
		tr, err := createTransport(ctx, m, resolver)
		require.Nil(t, tr)
		require.ErrorIs(t, err, context.Canceled)
	}
	require.Empty(t, resolver.contexts)
}

func TestOwnerCloseJoinsCanceledRuntimeMCPResolution(t *testing.T) {
	owner, err := Acquire()
	require.NoError(t, err)

	const name = "runtime-resolution-close"
	m := config.MCPConfig{Type: config.MCPStdio, Command: "$(blocked)"}
	store := config.NewLibraryStore(&config.Config{MCP: config.MCPs{name: m}}, t.TempDir())
	admission, err := owner.admitServerForConfig(context.Background(), store, name, m, true)
	require.NoError(t, err)

	entered := make(chan struct{})
	resolver := config.NewShellVariableResolver(env.NewFromMap(nil), config.WithExpander(func(ctx context.Context, _ string, _ []string) (string, error) {
		close(entered)
		<-ctx.Done()
		return "", ctx.Err()
	}))
	result := make(chan error, 1)
	go func() {
		defer admission.done()
		_, err := createSessionWithAdmission(context.Background(), name, m, resolver, &admission)
		result <- err
	}()

	<-entered
	closeDone := make(chan error, 1)
	go func() { closeDone <- owner.Close(context.Background()) }()

	select {
	case err := <-result:
		require.ErrorIs(t, err, context.Canceled)
		require.NoError(t, <-closeDone)
	case err := <-closeDone:
		require.NoError(t, err)
		require.ErrorIs(t, <-result, context.Canceled)
	}
}

// TestCreateSession_ResolutionFailureUpdatesState pins the user-visible
// half of the regression fix: when any of command/args/env/headers/url
// fails to resolve, createSession must publish StateError to the state
// map so rush_info and the TUI's MCP status card can render a real
// error instead of the MCP silently sitting in "starting" or being
// spawned with an empty credential.
//
// These subtests cannot run in parallel: `states` is a package-level
// csync.Map and each assertion reads the entry written by the call
// under test. They do use unique MCP names per subtest to keep them
// independent regardless of ordering.
func TestCreateSession_ResolutionFailureUpdatesState(t *testing.T) {
	r := shellResolverWithPath(t, nil)

	tests := []struct {
		name            string
		mcpName         string
		cfg             config.MCPConfig
		wantErrContains string
	}{
		{
			name:    "stdio env failure",
			mcpName: "test-stdio-env-fail",
			cfg: config.MCPConfig{
				Type:    config.MCPStdio,
				Command: "echo",
				Env:     map[string]string{"FORGEJO_ACCESS_TOKEN": "$(false)"},
			},
			wantErrContains: "env FORGEJO_ACCESS_TOKEN",
		},
		{
			// Args that reference an unset bare $VAR no longer
			// error out under lenient nounset; the only remaining
			// failure mode for arg resolution is a failing $(cmd).
			name:    "stdio args failure",
			mcpName: "test-stdio-args-fail",
			cfg: config.MCPConfig{
				Type:    config.MCPStdio,
				Command: "echo",
				Args:    []string{"--token", "$(false)"},
			},
			wantErrContains: "arg 1",
		},
		{
			// Likewise for URL: bare $UNSET expands to ""
			// silently, so we need a failing $(cmd) to exercise
			// the "url:" wrap from ResolvedURL.
			name:    "http url failure",
			mcpName: "test-http-url-fail",
			cfg: config.MCPConfig{
				Type: config.MCPHttp,
				URL:  "https://$(false)/api",
			},
			wantErrContains: "url:",
		},
		{
			// A URL whose shell expansion yields the empty
			// string (here via ${VAR:-}) is not a ResolvedURL
			// error, but the non-empty guard in createTransport
			// must still reject it so the state card renders an
			// error instead of spawning a transport against "".
			name:    "http empty-resolved url",
			mcpName: "test-http-url-empty",
			cfg: config.MCPConfig{
				Type: config.MCPHttp,
				URL:  "${MCP_URL_EMPTY:-}",
			},
			wantErrContains: "non-empty 'url'",
		},
		{
			name:    "http header failure",
			mcpName: "test-http-header-fail",
			cfg: config.MCPConfig{
				Type:    config.MCPHttp,
				URL:     "https://mcp.example.com/api",
				Headers: map[string]string{"Authorization": "$(false)"},
			},
			wantErrContains: "header Authorization",
		},
		{
			name:    "sse url failure",
			mcpName: "test-sse-url-fail",
			cfg: config.MCPConfig{
				Type: config.MCPSSE,
				URL:  "https://$(false)/events",
			},
			wantErrContains: "url:",
		},
		{
			// Bare $MISSING in a header resolves to "" silently
			// and is then dropped. The "header Authorization"
			// wrap only surfaces on a $(cmd) failure; that is
			// what this subtest now pins for the SSE path.
			name:    "sse header failure",
			mcpName: "test-sse-header-fail",
			cfg: config.MCPConfig{
				Type:    config.MCPSSE,
				URL:     "https://mcp.example.com/events",
				Headers: map[string]string{"Authorization": "$(false)"},
			},
			wantErrContains: "header Authorization",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			// Guarantee a clean slate on the shared state map so a
			// stale entry from another test can't satisfy the
			// assertion.
			states.Del(tc.mcpName)
			t.Cleanup(func() { states.Del(tc.mcpName) })

			sess, err := createSession(t.Context(), tc.mcpName, tc.cfg, r)
			require.Error(t, err)
			require.Nil(t, sess)
			require.Contains(t, err.Error(), tc.wantErrContains)

			info, ok := GetState(tc.mcpName)
			require.True(t, ok, "state entry must be written for %q", tc.mcpName)
			require.Equal(t, StateError, info.State, "expected StateError, got %s", info.State)
			require.Error(t, info.Error, "state must carry the failure error")
			require.Contains(t, info.Error.Error(), tc.wantErrContains)
			require.Nil(t, info.Client, "no client session on failure")
		})
	}
}

// TestInitialize_RestrictToCLIEnabled pins the enabled_in_cli gate used by
// non-interactive invocations (see internal/app.RestrictMCPToCLI and
// internal/cmd/root.go's mcpAppOptions): with restrictToCLIEnabled=true, a
// server without EnabledInCLI must be skipped exactly like Disabled — no
// connection attempt at all, StateDisabled — while a server with
// EnabledInCLI is still attempted. With restrictToCLIEnabled=false
// (interactive web/TUI), both must be attempted regardless of the field.
//
// Both servers point `command` at a real "echo" binary that is not
// actually an MCP server, so any ATTEMPTED connection fails fast with a
// protocol error (StateError) — that failure is expected and irrelevant
// to this test; what matters is whether a connection was attempted at
// all (anything other than StateDisabled) versus skipped outright
// (StateDisabled, set synchronously before initClient ever runs).
func TestInitialize_RestrictToCLIEnabled(t *testing.T) {
	const (
		offName         = "test-restrict-cli-off"
		onName          = "test-restrict-cli-on"
		interactiveName = "test-restrict-interactive"
	)
	for _, name := range []string{offName, onName, interactiveName} {
		states.Del(name)
		t.Cleanup(func() { states.Del(name) })
	}

	dataDir := t.TempDir()
	store, err := config.Init(dataDir, dataDir, false)
	require.NoError(t, err)

	store.Config().MCP = config.MCPs{
		offName: {Type: config.MCPStdio, Command: "echo"},
		onName:  {Type: config.MCPStdio, Command: "echo", EnabledInCLI: true},
	}
	Initialize(t.Context(), nil, store, true)

	offState, ok := GetState(offName)
	require.True(t, ok)
	require.Equal(t, StateDisabled, offState.State,
		"a server without enabled_in_cli must be skipped, not attempted, when restricted to CLI-enabled servers")

	onState, ok := GetState(onName)
	require.True(t, ok)
	require.NotEqual(t, StateDisabled, onState.State,
		"a server with enabled_in_cli must still be attempted when restricted to CLI-enabled servers")

	// Interactive mode (restrictToCLIEnabled=false): the SAME server that
	// was skipped above must be attempted when the gate is off.
	store.Config().MCP = config.MCPs{
		interactiveName: {Type: config.MCPStdio, Command: "echo"},
	}
	Initialize(t.Context(), nil, store, false)

	interactiveState, ok := GetState(interactiveName)
	require.True(t, ok)
	require.NotEqual(t, StateDisabled, interactiveState.State,
		"interactive mode must attempt every non-disabled server regardless of enabled_in_cli")
}

func TestOwnerCloseResetsRegistryAndAllowsNextLifecycle(t *testing.T) {
	owner, err := Acquire()
	require.NoError(t, err)

	_, err = Acquire()
	require.ErrorIs(t, err, ErrOwnerBusy,
		"a live application owner must not be replaced even while its registry is empty")

	states.Set("owner-probe", ClientInfo{Name: "owner-probe", State: StateConnected})

	_, err = Acquire()
	require.ErrorIs(t, err, ErrOwnerBusy,
		"a second application owner must not share the live MCP registry")

	require.NoError(t, owner.Close(context.Background()))
	require.Empty(t, GetStates())
	require.Empty(t, func() map[string][]*Tool {
		got := map[string][]*Tool{}
		for name, tools := range Tools() {
			got[name] = tools
		}
		return got
	}())

	next, err := Acquire()
	require.NoError(t, err, "a completed owner must not poison a later lifecycle")
	require.NoError(t, next.Close(context.Background()))
}

func TestOwnerCloseCancelsBlockedStartupBeforeCleanup(t *testing.T) {
	started := make(chan struct{})
	canceled := make(chan struct{}, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-started:
		default:
			close(started)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush()
		<-r.Context().Done()
		signalStarted(canceled)
	}))
	defer server.Close()

	dataDir := t.TempDir()
	store, err := config.Init(dataDir, dataDir, false)
	require.NoError(t, err)
	store.Config().MCP = config.MCPs{
		"blocked-startup": {
			Type:    config.MCPHttp,
			URL:     server.URL,
			Timeout: 60,
		},
	}

	owner, err := Acquire()
	require.NoError(t, err)
	initFinished := make(chan struct{})
	go func() {
		owner.Initialize(context.Background(), nil, store, false)
		close(initFinished)
	}()

	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("MCP startup did not reach the blocked transport")
	}

	require.NoError(t, owner.Close(context.Background()))
	select {
	case <-canceled:
	case <-time.After(5 * time.Second):
		t.Fatal("owner close did not cancel the blocked HTTP request")
	}
	select {
	case <-initFinished:
	case <-time.After(5 * time.Second):
		t.Fatal("MCP initialization must finish before Owner.Close returns")
	}
	require.Empty(t, GetStates())
	require.Empty(t, func() map[string][]*Tool {
		got := map[string][]*Tool{}
		for name, tools := range Tools() {
			got[name] = tools
		}
		return got
	}())
}

func TestOwnerCloseVsRenewalDoesNotPublishLateSession(t *testing.T) {
	owner, err := Acquire()
	require.NoError(t, err)
	const name = "late-renewal"
	store := config.NewTestStore(&config.Config{MCP: config.MCPs{
		name: {Type: config.MCPStdio, Command: "unused"},
	}})
	admission, err := owner.snapshotServerAdmission(context.Background(), store, name)
	require.NoError(t, err)

	serverTransport, clientTransport := mcp.NewInMemoryTransports()
	server := mcp.NewServer(&mcp.Implementation{Name: "renewal-server"}, nil)
	serverSession, err := server.Connect(context.Background(), serverTransport, nil)
	require.NoError(t, err)
	defer serverSession.Close()
	createdCtx, createdCancel := context.WithCancel(context.Background())
	defer createdCancel()
	clientSession, err := mcp.NewClient(&mcp.Implementation{Name: "renewal-client"}, nil).
		Connect(context.Background(), clientTransport, nil)
	require.NoError(t, err)
	closeCalls := 0
	created := &ClientSession{ClientSession: clientSession, cancel: func() {
		closeCalls++
		createdCancel()
	}}

	require.True(t, owner.beginInit())
	closeStarted := make(chan error, 1)
	go func() {
		closeStarted <- owner.Close(context.Background())
	}()

	deadline := time.Now().Add(time.Second)
	for owner.acceptsSession() && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	require.False(t, owner.acceptsSession())
	require.ErrorIs(t, owner.commitRenewal(&admission, name, created, Counts{}), context.Canceled)
	require.ErrorIs(t, createdCtx.Err(), context.Canceled)
	require.Equal(t, 1, closeCalls, "a rejected renewal session must be closed exactly once")
	require.Empty(t, func() map[string]*ClientSession {
		got := map[string]*ClientSession{}
		for name, session := range sessions.Seq2() {
			got[name] = session
		}
		return got
	}())

	owner.endInit()
	require.NoError(t, <-closeStarted)
	require.Eventually(t, func() bool {
		_, ok := GetState(name)
		return !ok
	}, time.Second, time.Millisecond)
}

func TestOwnerCommitRenewalPublishesSuccessfulSession(t *testing.T) {
	owner, err := Acquire()
	require.NoError(t, err)
	const name = "successful-renewal"
	store := config.NewTestStore(&config.Config{MCP: config.MCPs{
		name: {Type: config.MCPStdio, Command: "unused"},
	}})
	admission, err := owner.snapshotServerAdmission(context.Background(), store, name)
	require.NoError(t, err)

	serverTransport, clientTransport := mcp.NewInMemoryTransports()
	server := mcp.NewServer(&mcp.Implementation{Name: "renewal-server"}, nil)
	serverSession, err := server.Connect(context.Background(), serverTransport, nil)
	require.NoError(t, err)
	defer serverSession.Close()
	clientSession, err := mcp.NewClient(&mcp.Implementation{Name: "renewal-client"}, nil).
		Connect(context.Background(), clientTransport, nil)
	require.NoError(t, err)
	_, clientCancel := context.WithCancel(context.Background())
	defer clientCancel()
	session := &ClientSession{ClientSession: clientSession, cancel: clientCancel}

	states.Set(name, ClientInfo{Name: name, State: StateError})
	eventsCtx, eventsCancel := context.WithCancel(context.Background())
	defer eventsCancel()
	events := SubscribeEvents(eventsCtx)

	require.True(t, owner.beginInit())
	commitDone := make(chan error, 1)
	go func() {
		commitDone <- owner.commitRenewal(&admission, name, session, Counts{Tools: 1})
	}()

	select {
	case err := <-commitDone:
		require.NoError(t, err)
	case <-time.After(time.Second):
		t.Fatal("successful renewal deadlocked while publishing its state")
	}

	got, ok := sessions.Get(name)
	require.True(t, ok)
	require.Same(t, session, got)
	state, ok := GetState(name)
	require.True(t, ok)
	require.Equal(t, StateConnected, state.State)
	require.Same(t, session, state.Client)

	select {
	case event := <-events:
		require.Equal(t, pubsub.UpdatedEvent, event.Type)
		require.Equal(t, name, event.Payload.Name)
		require.Equal(t, StateConnected, event.Payload.State)
	case <-time.After(time.Second):
		t.Fatal("successful renewal did not publish a state event")
	}

	owner.endInit()
	require.NoError(t, owner.Close(context.Background()))
}

func TestRenewalRejectsCrossStoreDiskReplacement(t *testing.T) {
	testCrossStoreRenewalAdmission(t, false)
}

func TestRenewalRejectsCrossStoreDiskRemoval(t *testing.T) {
	testCrossStoreRenewalAdmission(t, true)
}

func TestFailClosedMCPHonorsCallerCancellation(t *testing.T) {
	owner, err := Acquire()
	require.NoError(t, err)
	defer func() { require.NoError(t, owner.Close(context.Background())) }()
	const name = "fail-closed-canceled"
	lease := serverLeaseFor(name)
	lease.Lock()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err = failClosedMCP(ctx, name)
	lease.Unlock()
	require.ErrorIs(t, err, context.Canceled)
}

func testCrossStoreRenewalAdmission(t *testing.T, remove bool) {
	t.Helper()
	const name = "cross-store-renewal-admission"
	store := persistedMCPStore(t, name, "http://old-renewal.example", false)
	contender, err := config.Init(store.WorkingDir(), store.WorkingDir(), false)
	require.NoError(t, err)
	owner, err := Acquire()
	require.NoError(t, err)
	defer func() { require.NoError(t, owner.Close(context.Background())) }()

	admission, err := owner.snapshotServerAdmission(context.Background(), store, name)
	require.NoError(t, err)
	if remove {
		require.NoError(t, contender.PersistRemoveMCPConfigExact(config.ScopeGlobal, name))
	} else {
		require.NoError(t, contender.PersistMCPFieldsExact(config.ScopeGlobal, name, map[string]any{
			"url": "http://new-renewal.example",
		}))
	}
	session := newInMemoryRenewalSession(t)
	require.True(t, owner.beginInit())
	err = owner.commitRenewal(&admission, name, session, Counts{})
	owner.endInit()
	require.ErrorIs(t, err, config.ErrMCPMutationStale)
	_, published := sessions.Get(name)
	require.False(t, published, "a renewal from a stale cross-store snapshot must not publish")
}

func newInMemoryRenewalSession(t *testing.T) *ClientSession {
	t.Helper()
	serverTransport, clientTransport := mcp.NewInMemoryTransports()
	server := mcp.NewServer(&mcp.Implementation{Name: "renewal-server"}, nil)
	serverSession, err := server.Connect(context.Background(), serverTransport, nil)
	require.NoError(t, err)
	clientSession, err := mcp.NewClient(&mcp.Implementation{Name: "renewal-client"}, nil).
		Connect(context.Background(), clientTransport, nil)
	require.NoError(t, err)
	t.Cleanup(func() {
		_ = clientSession.Close()
		_ = serverSession.Close()
	})
	return &ClientSession{ClientSession: clientSession}
}

func TestSessionContextPromotionLinearizesCancellation(t *testing.T) {
	t.Run("caller cancellation is observed synchronously", func(t *testing.T) {
		ownerCtx, ownerCancel := context.WithCancel(context.Background())
		defer ownerCancel()
		callerCtx, callerCancel := context.WithCancel(context.Background())
		candidateCtx, candidateCancel := context.WithCancel(context.Background())
		handoff := newSessionContextWithCaller(ownerCtx, candidateCtx, callerCtx, candidateCancel, nil)
		callerCancel()

		require.False(t, handoff.promote())
		require.ErrorIs(t, handoff.Err(), context.Canceled)
		select {
		case <-handoff.Done():
		case <-time.After(time.Second):
			t.Fatal("caller cancellation did not close the handoff context")
		}
		select {
		case <-handoff.workerDone:
		case <-time.After(time.Second):
			t.Fatal("rejected promotion left its handoff goroutine running")
		}
	})

	t.Run("cancellation wins", func(t *testing.T) {
		ownerCtx, ownerCancel := context.WithCancel(context.Background())
		defer ownerCancel()
		candidateCtx, candidateCancel := context.WithCancel(context.Background())
		candidateCancel()
		handoff := &sessionContext{
			owner: ownerCtx, candidate: candidateCtx, done: make(chan struct{}),
		}

		require.True(t, handoff.finish(candidateCtx, true))
		require.False(t, handoff.promote())
		require.ErrorIs(t, handoff.Err(), context.Canceled)
		select {
		case <-handoff.Done():
		default:
			t.Fatal("cancellation winner did not close the handoff context")
		}
	})

	t.Run("promotion wins", func(t *testing.T) {
		ownerCtx, ownerCancel := context.WithCancel(context.Background())
		candidateCtx, candidateCancel := context.WithCancel(context.Background())
		handoff := &sessionContext{
			owner: ownerCtx, candidate: candidateCtx, done: make(chan struct{}),
		}

		require.True(t, handoff.promote())
		candidateCancel()
		require.False(t, handoff.finish(candidateCtx, true))
		select {
		case <-handoff.Done():
			t.Fatal("candidate cancellation closed a promoted handoff context")
		default:
		}

		ownerCancel()
		require.True(t, handoff.finish(ownerCtx, false))
		select {
		case <-handoff.Done():
		default:
			t.Fatal("owner cancellation did not close the promoted handoff context")
		}
	})
}

func TestClientSessionCloseAbortsPromotedCandidate(t *testing.T) {
	ownerCtx, ownerCancel := context.WithCancel(context.Background())
	defer ownerCancel()
	candidateCtx, candidateCancel := context.WithCancel(context.Background())
	defer candidateCancel()
	handoff := newSessionContext(ownerCtx, candidateCtx)
	require.True(t, handoff.promote())
	session := &ClientSession{
		cancel:   func() {},
		terminal: handoff.abort,
	}

	require.NoError(t, session.Close())
	select {
	case <-handoff.Done():
	case <-time.After(time.Second):
		t.Fatal("closing a promoted candidate did not abort its context")
	}
	select {
	case <-handoff.workerDone:
	case <-time.After(time.Second):
		t.Fatal("closing a promoted candidate left its handoff goroutine running")
	}
	require.ErrorIs(t, handoff.Err(), context.Canceled)
}

func TestPublishedSessionIgnoresCandidateCancellationUntilClose(t *testing.T) {
	ownerCtx, ownerCancel := context.WithCancel(context.Background())
	defer ownerCancel()
	candidateCtx, candidateCancel := context.WithCancel(context.Background())
	handoff := newSessionContext(ownerCtx, candidateCtx)
	require.True(t, handoff.promote())
	session := &ClientSession{
		cancel:   func() {},
		terminal: handoff.abort,
	}

	candidateCancel()
	select {
	case <-handoff.Done():
		t.Fatal("candidate cancellation closed a published session context")
	default:
	}

	require.NoError(t, session.Close())
	select {
	case <-handoff.workerDone:
	case <-time.After(time.Second):
		t.Fatal("closing the published session did not stop its handoff goroutine")
	}
}

func TestRepeatedPromotedCandidateCloseStopsHandoff(t *testing.T) {
	for range 32 {
		ownerCtx, ownerCancel := context.WithCancel(context.Background())
		candidateCtx, candidateCancel := context.WithCancel(context.Background())
		handoff := newSessionContext(ownerCtx, candidateCtx)
		require.True(t, handoff.promote())
		session := &ClientSession{cancel: func() {}, terminal: handoff.abort}
		require.NoError(t, session.Close())
		select {
		case <-handoff.workerDone:
		case <-time.After(time.Second):
			t.Fatal("repeated candidate close left a handoff goroutine running")
		}
		ownerCancel()
		candidateCancel()
	}
}

func TestCommitRenewalPublishesOnlyAfterPromotion(t *testing.T) {
	const (
		rejectedName = "promotion-rejected"
		acceptedName = "promotion-accepted"
	)
	store := config.NewTestStore(&config.Config{MCP: config.MCPs{
		rejectedName: {Type: config.MCPStdio, Command: "unused"},
		acceptedName: {Type: config.MCPStdio, Command: "unused"},
	}})
	owner, err := Acquire()
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, owner.Close(context.Background())) })

	rejectedAdmission, err := owner.snapshotServerAdmission(context.Background(), store, rejectedName)
	require.NoError(t, err)
	rejectedCandidate, reject := context.WithCancel(context.Background())
	reject()
	rejectedHandoff := &sessionContext{
		owner: owner.lifecycleCtx, candidate: rejectedCandidate, done: make(chan struct{}),
	}
	require.True(t, rejectedHandoff.finish(rejectedCandidate, true))
	rejectedCloseCalls := 0
	rejected := &ClientSession{
		cancel:  func() { rejectedCloseCalls++ },
		promote: rejectedHandoff.promote,
	}
	require.ErrorIs(t, owner.commitRenewal(&rejectedAdmission, rejectedName, rejected, Counts{}), ErrOwnerBusy)
	require.Equal(t, 1, rejectedCloseCalls)
	_, ok := sessions.Get(rejectedName)
	require.False(t, ok)
	_, ok = GetState(rejectedName)
	require.False(t, ok)

	acceptedAdmission, err := owner.snapshotServerAdmission(context.Background(), store, acceptedName)
	require.NoError(t, err)
	acceptedCandidate, cancelCandidate := context.WithCancel(context.Background())
	acceptedHandoff := newSessionContext(owner.lifecycleCtx, acceptedCandidate)
	acceptedCloseCalls := 0
	accepted := &ClientSession{
		cancel:  func() { acceptedCloseCalls++ },
		promote: acceptedHandoff.promote,
	}
	require.NoError(t, owner.commitRenewal(&acceptedAdmission, acceptedName, accepted, Counts{}))
	cancelCandidate()
	require.False(t, acceptedHandoff.finish(acceptedCandidate, true))
	select {
	case <-acceptedHandoff.Done():
		t.Fatal("candidate cancellation closed the committed session context")
	default:
	}
	current, ok := sessions.Get(acceptedName)
	require.True(t, ok)
	require.Same(t, accepted, current)
	require.Equal(t, 0, acceptedCloseCalls)

	require.NoError(t, owner.Close(context.Background()))
	require.Equal(t, 1, acceptedCloseCalls)
}

func TestClientLeaseProtectsOperationFromRenewalAndClose(t *testing.T) {
	const name = "leased-operation"
	started := make(chan struct{})
	release := make(chan struct{})
	serverTransport, clientTransport := mcp.NewInMemoryTransports()
	server := mcp.NewServer(&mcp.Implementation{Name: "lease-server"}, nil)
	mcp.AddTool(server, &mcp.Tool{Name: "blocked"}, func(context.Context, *mcp.CallToolRequest, any) (*mcp.CallToolResult, any, error) {
		close(started)
		<-release
		return &mcp.CallToolResult{}, nil, nil
	})
	serverSession, err := server.Connect(context.Background(), serverTransport, nil)
	require.NoError(t, err)
	defer serverSession.Close()
	clientCtx, clientCancel := context.WithCancel(context.Background())
	clientSession, err := mcp.NewClient(&mcp.Implementation{Name: "lease-client"}, nil).
		Connect(clientCtx, clientTransport, nil)
	require.NoError(t, err)
	session := &ClientSession{ClientSession: clientSession, cancel: clientCancel}

	owner, err := Acquire()
	require.NoError(t, err)
	sessions.Set(name, session)
	states.Set(name, ClientInfo{Name: name, State: StateConnected, Client: session})
	dataDir := t.TempDir()
	store, err := config.Init(dataDir, dataDir, false)
	require.NoError(t, err)
	store.Config().MCP[name] = config.MCPConfig{Type: config.MCPStdio, Command: "echo"}

	lease, err := getOrRenewClient(context.Background(), store, name)
	require.NoError(t, err)
	callDone := make(chan error, 1)
	go func() {
		_, callErr := lease.session.CallTool(lease.ctx, &mcp.CallToolParams{Name: "blocked"})
		callDone <- callErr
	}()
	<-started

	serverLease := serverLeaseFor(name)
	renewalDone := make(chan struct{})
	go func() {
		serverLease.Lock()
		serverLease.Unlock()
		close(renewalDone)
	}()
	select {
	case <-renewalDone:
	case <-time.After(time.Second):
		t.Fatal("renewal remained blocked by an MCP operation")
	}

	closeCtx, closeCancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	require.ErrorIs(t, owner.Close(closeCtx), context.DeadlineExceeded)
	closeCancel()

	close(release)
	select {
	case <-callDone:
	case <-time.After(time.Second):
		t.Fatal("MCP operation did not finish after the server was released")
	}
	lease.close()
	require.NoError(t, owner.Close(context.Background()))
}

func TestHeaderRoundTripperKeepsOwnerCancellationUntilBodyClose(t *testing.T) {
	serverCanceled := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush()
		<-r.Context().Done()
		close(serverCanceled)
	}))
	defer server.Close()

	ownerCtx, ownerCancel := context.WithCancel(context.Background())
	transport, err := cloneHTTPTransport()
	require.NoError(t, err)
	rt := &headerRoundTripper{ctx: ownerCtx, transport: transport}
	req, err := http.NewRequest(http.MethodGet, server.URL, nil)
	require.NoError(t, err)
	resp, err := rt.RoundTrip(req)
	require.NoError(t, err)
	require.NotNil(t, resp.Body)

	ownerCancel()
	select {
	case <-serverCanceled:
	case <-time.After(time.Second):
		t.Fatal("owner cancellation did not reach the open response body")
	}
	require.NoError(t, resp.Body.Close())
}

type closeAfterContextBody struct {
	ctx     context.Context
	started chan struct{}
}

func (*closeAfterContextBody) Read([]byte) (int, error) {
	return 0, io.EOF
}

func (b *closeAfterContextBody) Close() error {
	close(b.started)
	<-b.ctx.Done()
	return nil
}

func TestOwnerResponseBodyCloseCancelsBeforeUnderlyingClose(t *testing.T) {
	requestCtx, requestCancel := context.WithCancel(context.Background())
	defer requestCancel()
	underlying := &closeAfterContextBody{ctx: requestCtx, started: make(chan struct{})}
	stopCalls := 0
	body := &ownerResponseBody{
		ReadCloser: underlying,
		stop: func() bool {
			stopCalls++
			return true
		},
		cancel: requestCancel,
	}

	closeDone := make(chan error, 1)
	go func() { closeDone <- body.Close() }()
	<-underlying.started
	select {
	case err := <-closeDone:
		require.NoError(t, err)
	case <-time.After(time.Second):
		requestCancel()
		<-closeDone
		t.Fatal("response body Close did not cancel the request before closing the underlying body")
	}
	require.ErrorIs(t, requestCtx.Err(), context.Canceled)
	require.Equal(t, 1, stopCalls)
}

func TestOwnerCloseDeadlineRetainsFenceUntilStuckSessionCloses(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	server := mcp.NewServer(&mcp.Implementation{Name: "stuck-server"}, nil)
	mcp.AddTool(server, &mcp.Tool{Name: "stuck"}, func(context.Context, *mcp.CallToolRequest, any) (*mcp.CallToolResult, any, error) {
		close(started)
		<-release
		return &mcp.CallToolResult{}, nil, nil
	})
	serverTransport, clientTransport := mcp.NewInMemoryTransports()
	serverSession, err := server.Connect(context.Background(), serverTransport, nil)
	require.NoError(t, err)
	defer serverSession.Close()
	clientSession, err := mcp.NewClient(&mcp.Implementation{Name: "stuck-client"}, nil).
		Connect(context.Background(), clientTransport, nil)
	require.NoError(t, err)
	_, clientCancel := context.WithCancel(context.Background())
	sess := &ClientSession{ClientSession: clientSession, cancel: clientCancel}
	defer clientCancel()

	owner, err := Acquire()
	require.NoError(t, err)
	sessions.Set("stuck", sess)
	states.Set("stuck", ClientInfo{Name: "stuck", State: StateConnected, Client: sess})

	callDone := make(chan error, 1)
	go func() {
		_, err := sess.CallTool(context.Background(), &mcp.CallToolParams{Name: "stuck"})
		callDone <- err
	}()
	<-started

	closeCtx, closeCancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer closeCancel()
	require.ErrorIs(t, owner.Close(closeCtx), context.DeadlineExceeded)
	_, err = Acquire()
	require.ErrorIs(t, err, ErrOwnerBusy)

	close(release)
	require.Eventually(t, func() bool {
		select {
		case <-callDone:
			return true
		default:
			return false
		}
	}, time.Second, time.Millisecond)
	require.NoError(t, owner.Close(context.Background()))
	next, err := Acquire()
	require.NoError(t, err)
	require.NoError(t, next.Close(context.Background()))
}
