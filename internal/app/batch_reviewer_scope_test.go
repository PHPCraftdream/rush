package app

// R12-H2a fix round 2 (R2-3): a reviewer pass on a CUSTOM-provider
// invocation must BUILD and RELEASE its own constructions.
//
// Production mechanism: reviewerCallOptions (app_run_reviewer.go) mints the
// review turn's OWN build-resource scope via agent.EnsureCallBuildScope —
// the borrow copies the immutable binding handle, never the primary's
// scope. RunWithOverrides' entry-point defer closes that scope when the
// review turn returns, releasing its constructions.
//
// Nil-scope mutation caught: reviewerCallOptions leaving buildScope nil on
// the borrowed options. finishToolAssembly (internal/agent/coordinator_tools.go)
// then refuses a custom provider whose options carry no build scope
// (ErrDiskProviderUnavailable), so the review turn deterministically fails
// tool construction: ExecuteRun fails (NoError oracle) or the envelope
// carries no reviewer verdict (result.Review oracle). A leak variant
// (borrowing the PRIMARY's scope, or never closing the review scope)
// fails the owner-set oracle below.

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"testing"
	"time"

	"charm.land/catwalk/pkg/catwalk"
	"charm.land/fantasy/providers/openaicompat"

	"github.com/PHPCraftdream/rush/internal/agent"
	"github.com/PHPCraftdream/rush/internal/config"
	"github.com/PHPCraftdream/rush/internal/db"
	"github.com/PHPCraftdream/rush/internal/permission"
	"github.com/PHPCraftdream/rush/internal/session"
	"github.com/stretchr/testify/require"
)

// reviewerScopeTexts are the scripted plain-text turns: the PRIMARY model
// emits ONLY text (no tool calls, so no batch root is ever submitted) and
// the reviewer model emits the verdict that must land in result.Review.
const (
	reviewerScopePrimaryText  = "REVIEWER SCOPE PRIMARY: plain text, no tools."
	reviewerScopeVerdictText  = "REVIEWER SCOPE VERDICT: work verified, no gaps found."
	reviewerScopeReviewerMdl  = "reviewer-scope-r"
	reviewerScopePrimaryModel = "reviewer-scope-smart"
)

// TestReviewerPassCustomProviderBuildsAndReleasesConstruction drives a REAL
// flagged New() App through ExecuteRun with a custom DiskProvider + scoped
// FolderScopes; the primary turn runs plain-text, then the reviewer pass
// runs over the SAME session through the borrowed binding under its OWN
// build scope. Oracles: ExecuteRun succeeds, result.Review carries the
// reviewer's verdict, and after ExecuteRun returns the capability manager's
// owner set is EMPTY (the primary's scope closed by runInternal's round-1
// defer; the review turn's own scope closed by RunWithOverrides' entry
// defer).
func TestReviewerPassCustomProviderBuildsAndReleasesConstruction(t *testing.T) {
	// The batch runtime flag must be on BEFORE New (the flagged install
	// happens inside New), mirroring newShutdownAcceptanceApp.
	t.Setenv("RUSH_BATCH_RUNTIME", "1")

	isolationTmp := t.TempDir()
	t.Setenv("XDG_DATA_HOME", isolationTmp)
	t.Setenv("RUSH_GLOBAL_DATA", isolationTmp)
	isolationConfigDir := filepath.Join(isolationTmp, "config")
	require.NoError(t, os.MkdirAll(isolationConfigDir, 0o755))
	t.Setenv("XDG_CONFIG_HOME", isolationConfigDir)
	t.Setenv("RUSH_GLOBAL_CONFIG", isolationConfigDir)
	t.Setenv("RUSH_PROVIDER_CACHE_ONLY", "1")

	dataDir := t.TempDir()

	// Scripted openaicompat SSE server keyed by the requested model id —
	// same emission shape as newReviewerPassApp's server, but EVERY turn is
	// plain text (no tool calls anywhere in this run).
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		require.NoError(t, err)
		var req struct {
			Model string `json:"model"`
		}
		require.NoError(t, json.Unmarshal(body, &req))
		content := reviewerScopePrimaryText
		if req.Model == reviewerScopeReviewerMdl {
			content = reviewerScopeVerdictText
		}
		contentJSON, mErr := json.Marshal(content)
		require.NoError(t, mErr)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = fmt.Fprintf(w, "data: %s\n\n", `{"id":"rs","object":"chat.completion.chunk","created":1,"model":"probe","choices":[{"index":0,"delta":{"role":"assistant","content":`+string(contentJSON)+`},"finish_reason":null}]}`)
		_, _ = fmt.Fprintf(w, "data: %s\n\n", `{"id":"rs","object":"chat.completion.chunk","created":1,"model":"probe","choices":[{"index":0,"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`)
		_, _ = fmt.Fprintln(w, "data: [DONE]")
	}))
	t.Cleanup(srv.Close)
	srvURL, err := url.Parse(srv.URL)
	require.NoError(t, err)

	store, err := config.Init(dataDir, dataDir, false)
	require.NoError(t, err)
	store.Config().Providers.Set("openaicompat", config.ProviderConfig{
		ID:      "openaicompat",
		Type:    openaicompat.Name,
		BaseURL: srvURL.String(),
		APIKey:  "probe",
		Models: []catwalk.Model{
			{ID: reviewerScopePrimaryModel, Name: "scope smart", ContextWindow: 200000, DefaultMaxTokens: 1000},
			{ID: reviewerScopeReviewerMdl, Name: "scope reviewer", ContextWindow: 200000, DefaultMaxTokens: 1000},
		},
	})
	store.SetSelectedModelRuntime(config.SelectedModelTypeSmart, config.SelectedModel{
		Provider: "openaicompat", Model: reviewerScopePrimaryModel,
	})
	store.SetSelectedModelRuntime(config.SelectedModelTypeFast, config.SelectedModel{
		Provider: "openaicompat", Model: reviewerScopePrimaryModel,
	})
	// THREE slots: smart, fast AND reviewer.
	store.SetSelectedModelRuntime(config.SelectedModelTypeReviewer, config.SelectedModel{
		Provider: "openaicompat", Model: reviewerScopeReviewerMdl,
	})
	store.SetupAgents()

	conn, err := db.Connect(context.Background(), dataDir)
	require.NoError(t, err)
	// Pair the refCounted pool handle (check_db_release_pairing.sh). Registered
	// BEFORE the Shutdown cleanup so LIFO runs it AFTER Shutdown.
	t.Cleanup(func() { _ = db.Release(dataDir) })
	application, err := New(context.Background(), conn, store)
	require.NoError(t, err)
	t.Cleanup(application.Shutdown)

	require.NotNil(t, application.batchInstall, "flag on: the batch runtime is installed through the app install path")
	require.NotNil(t, application.batchInstall.DiskBindings, "flag on: the App owns a capability manager")

	// Flag-on fact wiring (fixture-level, mirroring the R12-H wiring shape
	// documented on BindCoordinatorBatchCutoverFacts): bind the App's own
	// coordinator to the node-backed fact source so the review turn's
	// drain/launch decisions read real batch state instead of the
	// conservative unknownBatchCutoverFacts fail-safe (which answers every
	// read with "node source unavailable" and fails the launch closed).
	cutoverWriter := application.batchCutoverWriter()
	agent.BindCoordinatorBatchCutoverFacts(application.AgentCoordinator,
		agent.NewBatchCutoverNodeFacts(session.NewBatchStore(cutoverWriter),
			application.batchLiveReader, cutoverWriter))

	sess, err := application.Sessions.Create(context.Background(), "reviewer-scope")
	require.NoError(t, err)

	// Custom provider: the ungated fake disk pattern from
	// app_run_construction_leak_test.go, seeded for the fs_* ancestor walk
	// exactly like the shutdown fixture seeds its provider.
	fake := newLeakFakeDisk()
	scopedDir := filepath.Join(dataDir, "scoped")
	require.NoError(t, os.MkdirAll(scopedDir, 0o755))
	fake.seedDir(scopedDir)
	fake.seedDir(filepath.ToSlash(scopedDir))

	result, err := application.ExecuteRun(context.Background(), RunRequest{
		Prompt:            "reviewer scope probe",
		ContinueSessionID: sess.ID,
		Mode:              RunModeJSON,
		Stdout:            io.Discard,
		Stderr:            io.Discard,
		HideSpinner:       true,
		Overrides: RunOverrides{
			// A clean --role smart run with a configured reviewer is the
			// reviewer-pass trigger (shouldRunReviewerPass).
			ModelRole: config.SelectedModelTypeSmart,
			FolderScopes: []permission.FolderScopeEntry{{
				Dir: scopedDir,
				Ops: []permission.FileOp{permission.FileOpCreate, permission.FileOpRead},
			}},
			DiskProvider: fake,
		},
	})

	// ORACLE 1: the run — primary turn AND reviewer pass — completes.
	require.NoError(t, err, "ExecuteRun (primary + reviewer pass on a custom provider) must complete")
	require.NotNil(t, result)

	// ORACLE 2: the reviewer pass ran and its verdict landed in the review
	// field (asserted exactly like
	// TestExecuteRunReviewerPassContinuesCleanSmartRunWithReviewerModel).
	require.Equal(t, reviewerScopeVerdictText, result.Review,
		"the reviewer's verdict must land in the review field (A10)")

	// ORACLE 3: public-run cleanup returns all manager diagnostics and
	// runtime registry entries to empty without a manual drain.
	require.Eventually(t, func() bool {
		owners, constructions, registries := application.batchInstall.DiskBindings.OwnerDiagnostics()
		return owners == 0 && constructions == 0 && registries == 0 && application.batchInstall.Runtime.RegistryCount() == 0
	}, 10*time.Second, 50*time.Millisecond,
		"all capability owners, constructions, and registry entries must be gone after ExecuteRun")
}
