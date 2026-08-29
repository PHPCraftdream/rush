package sdk_test

// R1 audit regression test (phase 4 of
// docs/plans/2026-08-29-embeddable-library-refactoring.md): sdk.Open must
// never call os.Chdir, and every tool must resolve paths against the
// configured working directory — not the process cwd (risk R1). This test
// opens a Client on a directory that is deliberately NOT the process cwd,
// never changes the process cwd anywhere, and drives ONE client through
// two runs:
//
//   - run 1 (RunModeTerse, Options-level stream defaults): a real agent
//     turn whose single tool call is `view` on a file that exists ONLY in
//     Options.WorkingDir; proves via the tool RESULT (echoed into the
//     mock provider's second-turn request) that the file content was
//     actually read from Options.WorkingDir, and that Options.Stdout /
//     Options.Stderr received the output.
//   - run 2 (RunModeJSON, same client/session): pins the typed envelope
//     path (ExecuteRun only returns an envelope for JSON mode).
//
// ExecuteRun's documented contract: non-JSON modes return a nil envelope
// (final text lands on stdout instead); JSON mode returns the envelope.

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"

	"github.com/PHPCraftdream/rush/internal/db"
	"github.com/PHPCraftdream/rush/internal/message"
	"github.com/PHPCraftdream/rush/internal/session"
	"github.com/PHPCraftdream/rush/sdk"
	"github.com/stretchr/testify/require"
)

const (
	cwdAuditFileName  = "sdk_cwd_audit_target_8f3d.txt"
	cwdAuditSentinel  = "CWD-AUDIT-SENTINEL-8f3d9e2a"
	cwdAuditFinalText = "CWD_AUDIT_OK"
)

func TestOpenRunsWithWorkingDirDifferentFromProcessCwd(t *testing.T) {
	// Isolate global config/data resolution (same env set as
	// internal/app's golden envelope test) so config.Init inside
	// sdk.Open reads only the rush.json written below into the temp
	// working directory — never the operator's real global config.
	isolationTmp := t.TempDir()
	t.Setenv("XDG_DATA_HOME", isolationTmp)
	t.Setenv("RUSH_GLOBAL_DATA", isolationTmp)
	isolationConfigDir := filepath.Join(isolationTmp, "config")
	require.NoError(t, os.MkdirAll(isolationConfigDir, 0o755))
	t.Setenv("XDG_CONFIG_HOME", isolationConfigDir)
	t.Setenv("RUSH_GLOBAL_CONFIG", isolationConfigDir)
	t.Setenv("RUSH_PROVIDER_CACHE_ONLY", "1")

	procCwd, err := os.Getwd()
	require.NoError(t, err)
	t.Cleanup(func() {
		// The contract under test: nothing in the sdk path ever calls
		// os.Chdir. Assert the process cwd survived the whole test.
		after, err := os.Getwd()
		require.NoError(t, err)
		require.Equal(t, procCwd, after, "sdk must never change the process working directory")
	})

	workDir := t.TempDir()
	require.NotEqual(t, procCwd, workDir,
		"precondition: Options.WorkingDir must differ from the process cwd")

	// The file the agent will view lives ONLY in Options.WorkingDir.
	// Unique name rules out coincidence with anything in the process
	// cwd (the test binary's package directory).
	targetPath := filepath.Join(workDir, cwdAuditFileName)
	require.NoError(t, os.WriteFile(targetPath, []byte(cwdAuditSentinel+"\n"), 0o644))

	// sawSentinel fires when the model's second-turn request carries the
	// view tool's RESULT — i.e. file content actually read from
	// Options.WorkingDir reached the provider.
	var sawSentinel atomic.Bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		fl, _ := w.(http.Flusher)
		write := func(chunks []string) {
			for _, c := range chunks {
				fmt.Fprintf(w, "data: %s\n\n", c)
				if fl != nil {
					fl.Flush()
				}
			}
			fmt.Fprint(w, "data: [DONE]\n\n")
			if fl != nil {
				fl.Flush()
			}
		}
		if bytes.Contains(body, []byte(`"call_1"`)) {
			// Round 2: the turn after the tool result came back.
			if bytes.Contains(body, []byte(cwdAuditSentinel)) {
				sawSentinel.Store(true)
			}
			write([]string{
				fmt.Sprintf(`{"id":"c2","object":"chat.completion.chunk","created":2,"model":"probe","choices":[{"index":0,"delta":{"role":"assistant","content":%q},"finish_reason":null}]}`, cwdAuditFinalText),
				`{"id":"c2","object":"chat.completion.chunk","created":2,"model":"probe","choices":[{"index":0,"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":17,"completion_tokens":5,"total_tokens":22}}`,
			})
			return
		}
		// Round 1: emit exactly one `view` tool call with a RELATIVE
		// path — resolution against the client's working directory is
		// the whole point of this test.
		write([]string{
			`{"id":"c1","object":"chat.completion.chunk","created":1,"model":"probe","choices":[{"index":0,"delta":{"role":"assistant"},"finish_reason":null}]}`,
			`{"id":"c1","object":"chat.completion.chunk","created":1,"model":"probe","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"view","arguments":"{\"file_path\":\"` + cwdAuditFileName + `\"}"}}]},"finish_reason":null}]}`,
			`{"id":"c1","object":"chat.completion.chunk","created":1,"model":"probe","choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":11,"completion_tokens":3,"total_tokens":14}}`,
		})
	}))
	t.Cleanup(srv.Close)

	// The provider is configured through rush.json in Options.WorkingDir
	// itself — the realistic embedded path: sdk.Open must discover and
	// load it from the working directory it was given.
	rushJSON := fmt.Sprintf(`{
  "disable_default_providers": true,
  "providers": {
    "probe": {
      "id": "probe",
      "name": "probe",
      "type": "openai-compat",
      "base_url": %q,
      "api_key": "probe",
      "discover_models": false,
      "models": [
        {"id": "probe", "name": "probe", "context_window": 200000, "default_max_tokens": 1000}
      ]
    }
  },
  "models": {
    "smart": {"provider": "probe", "model": "probe"},
    "fast": {"provider": "probe", "model": "probe"}
  }
}`, srv.URL)
	require.NoError(t, os.WriteFile(filepath.Join(workDir, "rush.json"), []byte(rushJSON), 0o644))

	// Options-level stream defaults: Run below leaves its own fields nil,
	// so these must receive the output.
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	client, err := sdk.Open(context.Background(), sdk.Options{
		WorkingDir: workDir,
		Stdout:     &stdout,
		Stderr:     &stderr,
	})
	require.NoError(t, err)
	require.NotNil(t, client)
	t.Cleanup(func() { _ = client.Close() })

	// Seed a session with a non-default title plus one pre-existing
	// message (same trick as internal/app's golden test) so the
	// background title-generation provider call never fires and the mock
	// serves only the main flow. Uses the same .rush data directory the
	// client opened.
	dataDir := filepath.Join(workDir, ".rush")
	conn, err := db.Connect(context.Background(), dataDir)
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Release(dataDir) })
	q := db.New(conn)
	sessions := session.NewService(q, conn)
	sess, err := sessions.Create(context.Background(), "sdk-cwd-audit-title")
	require.NoError(t, err)
	messages := message.NewService(q)
	_, err = messages.Create(context.Background(), sess.ID, message.CreateMessageParams{
		Role:  message.User,
		Parts: []message.ContentPart{message.TextContent{Text: "seed"}},
	})
	require.NoError(t, err)

	// Run 1: terse mode with nil req.Stdout/req.Stderr, so the
	// Options-level defaults from Open are used. ExecuteRun's contract:
	// non-JSON modes return a nil envelope and stream the final text to
	// stdout instead.
	res1, err := client.Run(context.Background(), sdk.RunRequest{
		Prompt:            "read " + cwdAuditFileName + " and summarise",
		Mode:              sdk.RunModeTerse,
		ContinueSessionID: sess.ID,
		HideSpinner:       true,
	})
	require.NoError(t, err)
	require.Nil(t, res1, "terse mode returns a nil envelope by contract; final text lands on stdout")

	// Options-level stream defaults actually received the output.
	require.Equal(t, cwdAuditFinalText+"\n", stdout.String(),
		"Options.Stdout set at Open must receive the final text when RunRequest.Stdout is nil")
	require.Contains(t, stderr.String(), "view",
		"Options.Stderr set at Open must receive the tool-call heartbeat when RunRequest.Stderr is nil")

	// The core R1 assertion: the view tool result carried the sentinel —
	// the file was actually read from Options.WorkingDir.
	require.True(t, sawSentinel.Load(),
		"the view tool result did not carry the sentinel file content: the tool resolved its path somewhere other than Options.WorkingDir (or failed to read the file)")

	// Run 2: JSON mode on the same client/session — the typed envelope
	// path. The session history now contains "call_1", so the mock's
	// round-2 branch fires and returns a clean final answer.
	res2, err := client.Run(context.Background(), sdk.RunRequest{
		Prompt:            "summarise once more",
		Mode:              sdk.RunModeJSON,
		ContinueSessionID: sess.ID,
		HideSpinner:       true,
	})
	require.NoError(t, err)
	require.NotNil(t, res2)
	require.Equal(t, "end_turn", res2.ExitReason, "run must finish cleanly; res2.Error=%q res2.Warnings=%v", res2.Error, res2.Warnings)
	require.Equal(t, cwdAuditFinalText, res2.FinalText)

	// Typed result is usable through the sdk aliases.
	envelope := *res2
	require.Equal(t, res2.SessionID, envelope.SessionID)
}
