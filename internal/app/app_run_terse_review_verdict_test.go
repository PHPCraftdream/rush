package app

// In SDK terse/stream mode WITHOUT captureResult, a successful reviewer pass
// prints the EXECUTOR's text (c38c1664, A10) but the verdict was lost: the
// switch's first case attaches the review only when primaryResult != nil, and
// attachReview is also what prints the one stderr verdict line, so a FAIL
// review was visible nowhere. The fix surfaces it on stderr from a throwaway
// result; stdout stays the executor's text alone.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/PHPCraftdream/rush/internal/config"
	"github.com/PHPCraftdream/rush/internal/db"
	"github.com/stretchr/testify/require"
)

const (
	terseVerdictPrimaryText = "PRIMARY ANSWER: the work is done."
	terseVerdictReviewModel = "terse-verdict-reviewer"
)

// newTerseVerdictApp builds a real App with a reviewer slot whose answers the
// test scripts per reviewer-model request number; the smart slot always
// answers terseVerdictPrimaryText and every other model a plain "ok".
func newTerseVerdictApp(t *testing.T, reviewStub func(n int, w http.ResponseWriter)) *App {
	t.Helper()
	isolationTmp := t.TempDir()
	t.Setenv("XDG_DATA_HOME", isolationTmp)
	t.Setenv("RUSH_GLOBAL_DATA", isolationTmp)
	configDir := filepath.Join(isolationTmp, "config")
	require.NoError(t, os.MkdirAll(configDir, 0o755))
	t.Setenv("XDG_CONFIG_HOME", configDir)
	t.Setenv("RUSH_GLOBAL_CONFIG", configDir)
	t.Setenv("RUSH_PROVIDER_CACHE_ONLY", "1")

	var mu sync.Mutex
	requestsByModel := map[string]int{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		require.NoError(t, err)
		var req struct {
			Model string `json:"model"`
		}
		require.NoError(t, json.Unmarshal(body, &req))
		mu.Lock()
		requestsByModel[req.Model]++
		n := requestsByModel[req.Model]
		mu.Unlock()

		w.Header().Set("Content-Type", "text/event-stream")
		switch {
		case req.Model == terseVerdictReviewModel:
			reviewStub(n, w)
		case req.Model != "smart-default":
			_, _ = fmt.Fprint(w, sseChunk("ok"))
			_, _ = fmt.Fprint(w, sseStopChunk)
			_, _ = fmt.Fprint(w, sseDone)
		default:
			_, _ = fmt.Fprint(w, sseChunk(terseVerdictPrimaryText))
			_, _ = fmt.Fprint(w, sseStopChunk)
			_, _ = fmt.Fprint(w, sseDone)
		}
	}))
	t.Cleanup(srv.Close)

	dataDir := t.TempDir()
	rushJSON := fmt.Sprintf(`{
  "disable_default_providers": true,
  "providers": {
    "openaicompat": {
      "id": "openaicompat", "type": "openai-compat", "base_url": %q,
      "api_key": "probe", "discover_models": false,
      "models": [
        {"id":"smart-default","context_window":200000,"default_max_tokens":1000},
        {"id":"fast-default","context_window":200000,"default_max_tokens":1000},
        {"id":%q,"context_window":200000,"default_max_tokens":1000}
      ]
    }
  },
  "models": {
    "smart": {"provider":"openaicompat","model":"smart-default"},
    "fast": {"provider":"openaicompat","model":"fast-default"},
    "reviewer": {"provider":"openaicompat","model":%q}
  }
}`, srv.URL, terseVerdictReviewModel, terseVerdictReviewModel)
	require.NoError(t, os.WriteFile(filepath.Join(dataDir, "rush.json"), []byte(rushJSON), 0o644))

	store, err := config.Init(dataDir, dataDir, false)
	require.NoError(t, err)
	store.SetSelectedModelRuntime(config.SelectedModelTypeSmart, config.SelectedModel{Provider: "openaicompat", Model: "smart-default"})
	store.SetSelectedModelRuntime(config.SelectedModelTypeFast, config.SelectedModel{Provider: "openaicompat", Model: "fast-default"})
	store.SetSelectedModelRuntime(config.SelectedModelTypeReviewer, config.SelectedModel{Provider: "openaicompat", Model: terseVerdictReviewModel})
	store.SetupAgents()

	conn, err := db.Connect(context.Background(), dataDir)
	require.NoError(t, err)
	application, err := New(context.Background(), conn, store)
	if err != nil {
		err = errors.Join(err, db.ReleaseConn(conn))
	}
	require.NoError(t, err)
	t.Cleanup(application.Shutdown)
	return application
}

func runTerseVerdictExecuteRun(t *testing.T, application *App) (stdout, stderr string) {
	t.Helper()
	var stdoutBuf, stderrBuf bytes.Buffer
	res, err := application.ExecuteRun(context.Background(), RunRequest{
		Prompt:      "do the thing",
		Overrides:   RunOverrides{ModelRole: config.SelectedModelTypeSmart},
		Mode:        RunModeTerse,
		Stdout:      &stdoutBuf,
		Stderr:      &stderrBuf,
		HideSpinner: true,
	})
	require.NoError(t, err)
	require.Nil(t, res, "terse mode without captureResult carries no structured result")
	return stdoutBuf.String(), stderrBuf.String()
}

// REVERT CHECK: dropping the primaryResult == nil attachReview call (the
// throwaway-result branch) in ExecuteRun's reviewer switch loses the stderr
// verdict line again and the FAIL assertion below goes red while stdout stays
// correct.
func TestExecuteRunTerseReviewerFailVerdictPrintsStderrLine(t *testing.T) {
	application := newTerseVerdictApp(t, func(n int, w http.ResponseWriter) {
		require.Equal(t, 1, n, "the reviewer answers exactly one request")
		_, _ = fmt.Fprint(w, sseChunk("VERDICT: FAIL\n\nthe change does not do what was asked"))
		_, _ = fmt.Fprint(w, sseStopChunk)
		_, _ = fmt.Fprint(w, sseDone)
	})

	stdout, stderr := runTerseVerdictExecuteRun(t, application)

	require.Equal(t, terseVerdictPrimaryText+"\n", stdout,
		"stdout stays exactly the executor's text")
	require.Contains(t, stderr, "rush run: reviewer verdict: fail",
		"the FAIL verdict must be visible on stderr even without an envelope, got %q", stderr)
}

// CONTROL: a PASS the reviewer earned with its own read-tool check prints no
// verdict line at all — the fix must surface only the verdicts that need
// attention, exactly as attachReview does for an envelope.
func TestExecuteRunTerseReviewerPassWithCheckPrintsNoVerdictLine(t *testing.T) {
	viewTarget := filepath.Join(t.TempDir(), "notes.md")
	require.NoError(t, os.WriteFile(viewTarget, []byte("some notes"), 0o644))
	toolArgs, err := json.Marshal(map[string]string{"file_path": viewTarget})
	require.NoError(t, err)

	application := newTerseVerdictApp(t, func(n int, w http.ResponseWriter) {
		switch n {
		case 1:
			// The reviewer's own read-tool check comes first...
			_, _ = fmt.Fprint(w, sseToolCallChunk("view", string(toolArgs)))
			_, _ = fmt.Fprint(w, sseToolCallsFinishChunk())
			_, _ = fmt.Fprint(w, sseDone)
		default:
			// ...then its verified PASS.
			_, _ = fmt.Fprint(w, sseChunk("VERDICT: PASS\n\nview notes.md: the claim is confirmed."))
			_, _ = fmt.Fprint(w, sseStopChunk)
			_, _ = fmt.Fprint(w, sseDone)
		}
	})

	stdout, stderr := runTerseVerdictExecuteRun(t, application)

	require.Equal(t, terseVerdictPrimaryText+"\n", stdout,
		"stdout stays exactly the executor's text")
	require.NotContains(t, stderr, "reviewer verdict",
		"a verified pass prints no verdict line, got %q", stderr)
}
