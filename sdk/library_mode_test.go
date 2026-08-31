package sdk_test

// Tests for sdk.Open's library mode (sdk.ModeLibrary): an embeddable,
// fully self-contained Open that takes its provider wiring from an
// explicit sdk.LibraryConfig instead of rush.json discovery.
//
//   - no WorkingDir  => an ephemeral in-memory SQLite database; nothing
//     on disk, sessions round-trip while the client lives, Close tears
//     it down;
//   - a WorkingDir    => the dir is auto-created and the DB persisted at
//     <WorkingDir>/.rush/rush.db, surviving Close and reopen;
//   - rush.json in the WorkingDir is IGNORED in library mode (config
//     discovery is fully skipped) — the LibraryConfig wins;
//   - Open validates the LibraryConfig: it is required in library mode,
//     and the smart role must be defined.
//
// Application mode (the default) is covered by the sibling test files.

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/PHPCraftdream/rush/internal/db"
	"github.com/PHPCraftdream/rush/internal/session"
	"github.com/PHPCraftdream/rush/sdk"
	"github.com/stretchr/testify/require"
)

// libraryConfigFor builds a LibraryConfig pointing every role at the
// single given OpenAI-compatible test server.
func libraryConfigFor(baseURL, apiKey string) *sdk.LibraryConfig {
	return &sdk.LibraryConfig{
		Credentials: []sdk.Credential{
			{
				Provider: "library-provider",
				Type:     sdk.ProviderTypeOpenAICompat,
				APIKey:   apiKey,
				BaseURL:  baseURL,
				Models: []sdk.CredentialModel{
					{ID: "library-model", ContextWindow: 200000, DefaultMaxTokens: 1000},
				},
			},
		},
		Models: map[sdk.Role]sdk.ModelChoice{
			sdk.RoleSmart:    {Provider: "library-provider", Model: "library-model"},
			sdk.RoleFast:     {Provider: "library-provider", Model: "library-model"},
			sdk.RoleWorker:   {Provider: "library-provider", Model: "library-model"},
			sdk.RoleReviewer: {Provider: "library-provider", Model: "library-model"},
		},
	}
}

// runLibraryPrompt performs one marker-prompt Run against the client and
// asserts the canonical success shape (end_turn + exact marker text).
func runLibraryPrompt(t *testing.T, client *sdk.Client, sessionID string) *sdk.RunResult {
	t.Helper()
	var buf bytes.Buffer
	req := sdk.RunRequest{
		Prompt:      "reply with exactly the marker text and nothing else",
		Mode:        sdk.RunModeJSON,
		Stdout:      &buf,
		HideSpinner: true,
	}
	if sessionID != "" {
		req.ContinueSessionID = sessionID
	}
	res, err := client.Run(context.Background(), req)
	require.NoError(t, err, "run failed (output %q)", buf.String())
	require.NotNil(t, res)
	require.Equal(t, "end_turn", res.ExitReason, "output %q", buf.String())
	require.NotEmpty(t, res.FinalText)
	return res
}

func TestOpenLibraryMode_EphemeralInMemorySession(t *testing.T) {
	isolateGlobalConfigForWorkdirTest(t)

	server := newCredentialServer(t, "EPHEMERAL_LIBRARY_OK")

	// No WorkingDir at all: library mode keeps everything in an
	// in-memory database. NOTE: there are deliberately NO disk
	// assertions in this test — the architecture keeps the whole
	// store in memory, and with no WorkingDir there is no path to
	// inspect; the meaningful proof is behavioral (Run works and a
	// session round-trips while the client lives).
	client, err := sdk.Open(context.Background(), sdk.Options{
		Mode:          sdk.ModeLibrary,
		LibraryConfig: libraryConfigFor(server.srv.URL, "sk-library-secret"),
	})
	require.NoError(t, err)
	require.NotNil(t, client)
	t.Cleanup(func() { _ = client.Close() })

	res1 := runLibraryPrompt(t, client, "")
	require.Equal(t, "EPHEMERAL_LIBRARY_OK", res1.FinalText)
	require.NotEmpty(t, res1.SessionID)

	// The in-memory DB must actually round-trip: continue the first
	// session in a second Run on the same client.
	res2 := runLibraryPrompt(t, client, res1.SessionID)
	require.Equal(t, "EPHEMERAL_LIBRARY_OK", res2.FinalText)

	// Every request the server saw used exactly the library key.
	for auth := range server.hits() {
		require.Equal(t, "Bearer sk-library-secret", auth,
			"library mode must only ever send the LibraryConfig API key")
	}

	// Close must return cleanly (no panic); errors are irrelevant here.
	require.NotPanics(t, func() { _ = client.Close() })
}

func TestOpenLibraryMode_PersistedWorkDirSessionSurvivesReopen(t *testing.T) {
	isolateGlobalConfigForWorkdirTest(t)

	parent := t.TempDir()
	workDir := filepath.Join(parent, "fresh-lib-workspace")
	if _, err := os.Stat(workDir); !os.IsNotExist(err) {
		t.Fatalf("precondition: %q must not exist yet", workDir)
	}

	server := newCredentialServer(t, "PERSISTED_LIBRARY_OK")
	client, err := sdk.Open(context.Background(), sdk.Options{
		Mode:          sdk.ModeLibrary,
		WorkingDir:    workDir,
		LibraryConfig: libraryConfigFor(server.srv.URL, "sk-library-secret"),
	})
	require.NoError(t, err)
	require.NotNil(t, client)

	res := runLibraryPrompt(t, client, "")
	require.Equal(t, "PERSISTED_LIBRARY_OK", res.FinalText)
	sessionID := res.SessionID
	require.NotEmpty(t, sessionID)
	require.Empty(t, client.Close().CleanupErrors)

	// The DB really is on disk, under the auto-created working dir.
	dbPath := filepath.Join(workDir, ".rush", "rush.db")
	_, statErr := os.Stat(dbPath)
	require.NoError(t, statErr, "library mode with WorkingDir must persist .rush/rush.db")

	// The session must be readable from the persisted DB entirely
	// independently of any client.
	ctx := context.Background()
	dataDir := filepath.Join(workDir, ".rush")
	conn, err := db.Connect(ctx, dataDir)
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Release(dataDir) })
	sessions := session.NewService(db.New(conn), conn)
	_, err = sessions.Get(ctx, sessionID)
	require.NoError(t, err, "session must have been persisted to the on-disk DB")

	// Reopen library mode on the same WorkingDir and continue the
	// persisted session.
	server2 := newCredentialServer(t, "PERSISTED_LIBRARY_OK")
	client2, err := sdk.Open(context.Background(), sdk.Options{
		Mode:          sdk.ModeLibrary,
		WorkingDir:    workDir,
		LibraryConfig: libraryConfigFor(server2.srv.URL, "sk-library-secret"),
	})
	require.NoError(t, err)
	res2 := runLibraryPrompt(t, client2, sessionID)
	require.Equal(t, "PERSISTED_LIBRARY_OK", res2.FinalText)
	require.Empty(t, client2.Close().CleanupErrors)
}

func TestOpenLibraryMode_IgnoresRushJSON(t *testing.T) {
	isolateGlobalConfigForWorkdirTest(t)

	// The provider that rush.json would select: library mode must
	// never contact it.
	operator := newCredentialServer(t, "OPERATOR_PROVIDER_MUST_NOT_BE_HIT")
	library := newCredentialServer(t, "LIBRARY_CONFIG_WINS")

	workDir := t.TempDir()
	rushJSON := fmt.Sprintf(`{
  "disable_default_providers": true,
  "providers": {
    "operator": {
      "id": "operator",
      "name": "operator",
      "type": "openai-compat",
      "base_url": %q,
      "api_key": "operator-key",
      "discover_models": false,
      "models": [
        {"id": "operator-model", "name": "operator-model", "context_window": 200000, "default_max_tokens": 1000}
      ]
    }
  },
  "models": {
    "smart": {"provider": "operator", "model": "operator-model"},
    "fast": {"provider": "operator", "model": "operator-model"}
  }
}`, operator.srv.URL)
	require.NoError(t, os.WriteFile(filepath.Join(workDir, "rush.json"), []byte(rushJSON), 0o644))

	client, err := sdk.Open(context.Background(), sdk.Options{
		Mode:          sdk.ModeLibrary,
		WorkingDir:    workDir,
		LibraryConfig: libraryConfigFor(library.srv.URL, "sk-library-secret"),
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = client.Close() })

	res := runLibraryPrompt(t, client, "")
	require.Equal(t, "LIBRARY_CONFIG_WINS", res.FinalText,
		"library mode must serve runs from the LibraryConfig provider, not rush.json")

	// The ignored rush.json provider saw zero requests.
	require.Equal(t, 0, operator.totalRequests(),
		"rush.json config discovery must be fully skipped in library mode")

	// The library server was hit, and only with the library key.
	require.NotEmpty(t, library.hits(), "the LibraryConfig provider must receive the requests")
	for auth := range library.hits() {
		require.Equal(t, "Bearer sk-library-secret", auth)
	}
}

func TestOpenLibraryMode_ValidationErrors(t *testing.T) {
	isolateGlobalConfigForWorkdirTest(t)

	t.Run("requires LibraryConfig", func(t *testing.T) {
		isolateGlobalConfigForWorkdirTest(t)
		_, err := sdk.Open(context.Background(), sdk.Options{Mode: sdk.ModeLibrary})
		require.Error(t, err)
		require.Contains(t, err.Error(), "LibraryConfig is required")
	})

	t.Run("requires smart role", func(t *testing.T) {
		isolateGlobalConfigForWorkdirTest(t)
		server := newCredentialServer(t, "SHOULD_NOT_BE_CALLED")
		lib := libraryConfigFor(server.srv.URL, "sk-library-secret")
		lib.Models = map[sdk.Role]sdk.ModelChoice{
			sdk.RoleFast: {Provider: "library-provider", Model: "library-model"},
		}
		_, err := sdk.Open(context.Background(), sdk.Options{
			Mode:          sdk.ModeLibrary,
			LibraryConfig: lib,
		})
		require.Error(t, err)
		require.Contains(t, err.Error(), "must define the smart role")
	})
}
