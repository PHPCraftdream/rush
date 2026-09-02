// Package embedder is the phase-6 proof that Rush's public sdk surface
// is embeddable from a module that is NOT github.com/PHPCraftdream/rush.
// This directory is its own Go module (see go.mod, with a replace
// directive pointing back at the repository) and this file imports ONLY
// github.com/PHPCraftdream/rush/sdk plus the standard library. No
// internal/... import would compile from here at all — that is the
// boundary being demonstrated, and TestExternalModuleCannotImportInternal
// proves the compiler enforces it rather than trusting nobody to try.
package embedder

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/PHPCraftdream/rush/sdk"
)

// embedderFinalText is the exact final answer the mock provider serves.
const embedderFinalText = "EMBEDDER_CHECK_OK"

// isolateGlobalState points every global-scope config and data
// resolution path at a throwaway directory, so sdk.Open reads only the
// rush.json written into the temp working directory below — never the
// operator's real global config. Both GlobalConfig() and
// GlobalConfigData() paths must be isolated separately.
func isolateGlobalState(t *testing.T) {
	t.Helper()
	isolationTmp := t.TempDir()
	t.Setenv("XDG_DATA_HOME", isolationTmp)
	t.Setenv("RUSH_GLOBAL_DATA", isolationTmp)
	configDir := filepath.Join(isolationTmp, "config")
	if err := os.MkdirAll(configDir, 0o755); err != nil {
		t.Fatalf("Failed to create isolated config dir: %v", err)
	}
	t.Setenv("XDG_CONFIG_HOME", configDir)
	t.Setenv("RUSH_GLOBAL_CONFIG", configDir)
	t.Setenv("RUSH_PROVIDER_CACHE_ONLY", "1")
}

// newMockProvider starts a stateless openai-compatible SSE mock: one
// assistant text delta, then a clean stop finish. Being stateless means
// the main turn and any background title-generation request (a fresh
// session without a custom title always asks for one) both get the same
// valid answer, so no request-ordering assumptions are needed.
func newMockProvider(t *testing.T) (*httptest.Server, *atomic.Int64) {
	t.Helper()
	var hits atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.ReadAll(r.Body)
		hits.Add(1)
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
		write([]string{
			fmt.Sprintf(`{"id":"c1","object":"chat.completion.chunk","created":1,"model":"probe","choices":[{"index":0,"delta":{"role":"assistant","content":%q},"finish_reason":null}]}`, embedderFinalText),
			`{"id":"c1","object":"chat.completion.chunk","created":1,"model":"probe","choices":[{"index":0,"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":11,"completion_tokens":3,"total_tokens":14}}`,
		})
	}))
	t.Cleanup(srv.Close)
	return srv, &hits
}

// TestExternalEmbedderOpenAndRun drives one real agent turn through the
// public sdk surface from outside the rush module: sdk.Open on a temp
// working directory whose rush.json points at the mock provider, then
// Client.Run in JSON mode, asserting the typed envelope comes back with
// a clean end_turn finish and the expected final text.
func TestExternalEmbedderOpenAndRun(t *testing.T) {
	isolateGlobalState(t)
	srv, hits := newMockProvider(t)

	workDir := t.TempDir()
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
	if err := os.WriteFile(filepath.Join(workDir, "rush.json"), []byte(rushJSON), 0o644); err != nil {
		t.Fatalf("Failed to write rush.json: %v", err)
	}

	var stdout, stderr bytes.Buffer
	client, err := sdk.Open(context.Background(), sdk.Options{
		WorkingDir: workDir,
		Stdout:     &stdout,
		Stderr:     &stderr,
	})
	if err != nil {
		t.Fatalf("sdk.Open failed: %v", err)
	}
	t.Cleanup(func() { _ = client.Close() })

	res, err := client.Run(context.Background(), sdk.RunRequest{
		Prompt:      "Say exactly: " + embedderFinalText,
		Mode:        sdk.RunModeJSON,
		HideSpinner: true,
	})
	if err != nil {
		t.Fatalf("Client.Run failed: %v", err)
	}
	if res == nil {
		t.Fatal("Client.Run returned a nil envelope for RunModeJSON")
	}

	t.Logf("mock provider handled %d HTTP request(s)", hits.Load())
	if hits.Load() == 0 {
		t.Fatal("mock provider was never contacted — no real provider round-trip happened")
	}
	t.Logf("embedded run finished: session_id=%s exit_reason=%s error=%q warnings=%v final_text=%q",
		res.SessionID, res.ExitReason, res.Error, res.Warnings, res.FinalText)
	if res.ExitReason != "end_turn" {
		t.Fatalf("ExitReason = %q, want %q", res.ExitReason, "end_turn")
	}
	if res.FinalText != embedderFinalText {
		t.Fatalf("FinalText = %q, want %q", res.FinalText, embedderFinalText)
	}
}

// TestExternalErrorSentinelsAreClassifiable is the R5-4 proof: the
// sentinel errors the README/sdk.go docs tell consumers to classify via
// errors.Is (sdk.ErrSessionBusy, sdk.ErrDiskProviderNotDurable) must be
// referenceable AND usable from a module that only imports
// github.com/PHPCraftdream/rush/sdk — never internal/agent, which Go's
// internal/ rule makes unimportable from here (see
// TestExternalModuleCannotImportInternal above). An in-tree test proves
// nothing about this: internal/agent's own tests live under the same
// module and could reference agent.ErrSessionBusy directly forever
// without ever exercising the consumer-facing contract. If either
// sdk.ErrSessionBusy or sdk.ErrDiskProviderNotDurable were ever removed
// from sdk/sdk.go, this whole file fails to COMPILE, not just fails an
// assertion.
func TestExternalErrorSentinelsAreClassifiable(t *testing.T) {
	wrappedBusy := fmt.Errorf("outer: %w", sdk.ErrSessionBusy)
	if !errors.Is(wrappedBusy, sdk.ErrSessionBusy) {
		t.Fatal("errors.Is(wrappedBusy, sdk.ErrSessionBusy) = false, want true")
	}
	wrappedNotDurable := fmt.Errorf("outer: %w", sdk.ErrDiskProviderNotDurable)
	if !errors.Is(wrappedNotDurable, sdk.ErrDiskProviderNotDurable) {
		t.Fatal("errors.Is(wrappedNotDurable, sdk.ErrDiskProviderNotDurable) = false, want true")
	}
}

// TestExternalModuleCannotImportInternal proves the sdk boundary is
// compiler-enforced, not convention: the fixture package under
// testdata/negativeimport imports rush's internal tree from OUTSIDE the
// module, and go build of it must FAIL with the internal-package rule
// violation. The fixture lives under a testdata directory precisely so
// pattern commands (go build ./... / go test ./...) never try to build
// it — only this test does, and it requires the failure.
func TestExternalModuleCannotImportInternal(t *testing.T) {
	goBin, err := exec.LookPath("go")
	if err != nil {
		t.Fatalf("go toolchain not found on PATH: %v", err)
	}
	// The test binary's working directory is this package's directory,
	// i.e. the embedder module root.
	cmd := exec.Command(goBin, "build", "./testdata/negativeimport")
	var combined bytes.Buffer
	cmd.Stdout = &combined
	cmd.Stderr = &combined
	buildErr := cmd.Run()
	t.Logf("fixture build output:\n%s", combined.String())
	if buildErr == nil {
		t.Fatal("go build of the internal-importing fixture SUCCEEDED — rush's internal/ tree is reachable from outside the module, the sdk boundary is broken")
	}
	if !strings.Contains(combined.String(), "internal") {
		t.Fatalf("build failed, but the output does not mention the internal-package rule; got:\n%s", combined.String())
	}
}
