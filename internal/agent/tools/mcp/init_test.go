package mcp

import (
	"context"
	"maps"
	"os"
	"testing"

	"github.com/PHPCraftdream/rush/internal/config"
	"github.com/PHPCraftdream/rush/internal/kit/sys/env"
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
	// Only goroutines started by this test count: earlier tests (or earlier
	// -count iterations) may legitimately leave server goroutines behind.
	defer goleak.VerifyNone(t, goleak.IgnoreCurrent())

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
			// error, but the non-empty guard in mcpdial.CreateTransport
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
