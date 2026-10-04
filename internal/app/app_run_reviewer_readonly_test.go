// T4 and T5 of docs/plans/2026-10-02-reviewer-pass-verification.md §8: the
// end-to-end proof that the automatic reviewer pass is genuinely READ-ONLY,
// not merely documented as such.
//
// Both tests reuse the established end-to-end harness of
// app_run_reviewer_pass_test.go — a REAL App whose provider is a stub
// openai-compat httptest server keyed by the requested model id, with the
// coordinator, message persistence and the two-phase event loop all running
// for real — so what is asserted is what the run actually did, not what a
// fixture arranged.
//
// T4 (TestReviewerPass_WireOffersOnlyReadTools) reads the toolset the REVIEW
// turn was offered on the wire. (The plan's §8 table names a harness
// `wireModelAndTools`; the dispatch's contract is the existing
// reviewerPassApp.requestedToolSets(), which records exactly the same thing
// per request, so that is what is used.)
//
// T5 (TestReviewerPass_ReadToolRunsWriteDoesNot) is the behavioural half: the
// stub REVIEWER is scripted to read the marker file, then to attempt a write
// into the run's working directory, then to give its verdict. The read lands
// in the transcript; the write never executes, so the file is never created
// and the run still ends with a verified pass.
//
// REVERT-CHECK for BOTH tests: removing
// `agent = applyCallReviewerReadOnly(ctx, agent, isSubAgent)`
// from buildTools in internal/agent/coordinator_tools.go (:572, the line that
// runs after applyCallFolderScope) drops the reviewer-role intersection with
// reviewerReadOnlyToolNames. The review turn is then built with the plain
// coder toolset, so on the wire bash/write/edit/... reappear (T4's NotContains
// assertions and the sets[0] control both go red) and, in T5, the `write` tool
// is a REAL tool of the review turn: the attempted write executes and
// <dataDir>/reviewer-wrote-this.txt is created, which is exactly what
// require.NoFileExists below forbids.

package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/PHPCraftdream/rush/internal/config"
	"github.com/PHPCraftdream/rush/internal/db"
	"github.com/PHPCraftdream/rush/internal/message"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// reviewerReadonlyMarkerLine is the unique content T5's marker file carries.
// It is what the reviewer must read back and what the transcript's `view`
// tool_result must contain.
const reviewerReadonlyMarkerLine = "REVIEWER-READONLY-MARKER-7c3f9a1e: the reviewer must read this line and must not be able to write this file"

// reviewerReadonlyVerdictText is the stub reviewer's THIRD answer: a real
// verdict line, so parseReviewVerdict reads "pass", plus the reviewer's own
// account of the two calls it made. Without a verdict line attachReview would
// record "unparsed" instead of "pass".
const reviewerReadonlyVerdictText = "VERDICT: PASS\n\n## Checked myself\nview reviewer-marker.txt: the marker line is present.\nwrite: no such tool exists in this session, so nothing was written.\n"

// reviewerReadonlyWriteContent is what the stub reviewer tries to write. It
// must never reach the filesystem.
const reviewerReadonlyWriteContent = "the reviewer pass is read-only; this file must never exist"

// TestReviewerPass_WireOffersOnlyReadTools (T4) pins the read-only shape of
// the review turn's toolset as the provider SEES it: the review request
// carries only read tools, and the control request — the primary smart turn
// of the very same run — still carries bash, so the narrowing is per-call and
// not a global tool floor.
//
// Revert-check: remove applyCallReviewerReadOnly from buildTools
// (internal/agent/coordinator_tools.go) and the review turn is offered the
// coder's full toolset; the NotContains assertions and the control both go
// red.
func TestReviewerPass_WireOffersOnlyReadTools(t *testing.T) {
	h := newReviewerPassAppOpts(t, reviewerPassAppOpts{withReviewer: true})
	sess := createModelOverrideSession(t, h.app, "reviewer-readonly-wire")

	result, err := runReviewerPassExecuteRun(t, h, sess.ID, RunOverrides{ModelRole: config.SelectedModelTypeSmart})
	require.NoError(t, err)
	require.NotNil(t, result)

	sets := h.requestedToolSets()
	require.Len(t, sets, 2, "the primary turn and exactly one review turn")
	for _, name := range []string{"bash", "write", "edit", "multiedit", "run_command", "todos", "ask_question", "download", "fetch", "agent", "agentic_fetch", "job_kill"} {
		assert.NotContains(t, sets[1], name, "the reviewer turn must not be offered %q on the wire", name)
	}
	assert.Contains(t, sets[1], "git_read")
	assert.Contains(t, sets[1], "view")
	assert.Contains(t, sets[0], "bash", "control: the primary smart turn keeps bash")
}

// TestReviewerPass_ReadToolRunsWriteDoesNot (T5) is the behavioural proof.
// The stub reviewer is scripted through THREE provider requests:
//
//	step 1 — view <dataDir>/reviewer-marker.txt   (a read the reviewer MUST do)
//	step 2 — write <dataDir>/reviewer-wrote-this.txt (the write it must NOT do)
//	step 3 — the verdict text
//
// Asserted afterwards against the real DB transcript of the real run: the
// `view` tool_result carries the marker line, the `write` tool_result is an
// error saying no such tool exists, the file the write targeted does not
// exist, and the envelope keeps the executor's answer with a verified pass.
//
// Revert-check: remove applyCallReviewerReadOnly from buildTools
// (internal/agent/coordinator_tools.go) and `write` becomes a real tool of the
// review turn — the attempted write executes for real, the file is created and
// require.NoFileExists below goes red.
func TestReviewerPass_ReadToolRunsWriteDoesNot(t *testing.T) {
	application, sessID, dataDir, requests := newReviewerWriteAttemptApp(t, reviewerPassAppOpts{withReviewer: true})

	markerPath := filepath.Join(dataDir, "reviewer-marker.txt")
	require.NoError(t, os.WriteFile(markerPath, []byte(reviewerReadonlyMarkerLine+"\n"), 0o644))

	result, err := application.ExecuteRun(context.Background(), RunRequest{
		Prompt:            "do the thing",
		ContinueSessionID: sessID,
		Overrides:         RunOverrides{ModelRole: config.SelectedModelTypeSmart},
		Mode:              RunModeJSON,
		Stdout:            io.Discard,
		Stderr:            io.Discard,
		HideSpinner:       true,
	})
	require.NoError(t, err)
	require.NotNil(t, result)

	// The three scripted reviewer requests after the one primary request: the
	// read, the write attempt, the verdict.
	require.Equal(t,
		[]string{"smart-default", reviewerPassReviewerModel, reviewerPassReviewerModel, reviewerPassReviewerModel},
		*requests,
		"the review turn must run exactly the scripted read, write attempt and verdict")

	msgs, err := application.Messages.List(context.Background(), sessID)
	require.NoError(t, err)

	var writeResult, viewResult *message.ToolResult
	for i := range msgs {
		for _, tr := range msgs[i].ToolResults() {
			switch tr.Name {
			case "write":
				if writeResult == nil {
					kept := tr
					writeResult = &kept
				}
			case "view":
				if viewResult == nil {
					kept := tr
					viewResult = &kept
				}
			}
		}
	}

	// The write attempt must be answered by the agent layer as a refusal, not
	// executed. The transcript's own tool_result is the witness.
	require.NotNil(t, writeResult, "the review turn must have attempted a write, and the transcript must carry its result")
	assert.True(t, writeResult.IsError, "the refused write must be recorded as an error result, got %q", writeResult.Content)
	assert.Contains(t, writeResult.Content, "tool not found",
		"the write tool must simply not exist for the reviewer turn; the transcript's tool_result says: %q", writeResult.Content)

	// The whole point: the file the reviewer tried to write does not exist.
	require.NoFileExists(t, filepath.Join(dataDir, "reviewer-wrote-this.txt"),
		"the reviewer pass is read-only: a write call must never create a file")

	// The reader path works: the reviewer's own read of the marker file is in
	// the transcript, with the marker's content.
	require.NotNil(t, viewResult, "the review turn must have read the marker file, and the transcript must carry its result")
	assert.Contains(t, viewResult.Content, reviewerReadonlyMarkerLine,
		"the reviewer's own read must land in the transcript with the marker line, got %q", viewResult.Content)

	// A10: the executor's answer stays the run's final_text; the reviewer's
	// verdict lands in the additive review field. Two read-tool-class calls
	// were made by the review turn itself, so attachReview keeps the plain
	// "pass" instead of downgrading it to "unverified".
	require.Equal(t, "pass", result.ReviewVerdict)
	require.Contains(t, result.Review, "VERDICT: PASS")
	require.Equal(t, reviewerPassPrimaryText, result.FinalText)
}

// newReviewerWriteAttemptApp is newReviewerPassAppOpts's harness with three
// changes, and with the run's working directory handed back to the test — the
// harness of app_run_reviewer_pass_test.go builds dataDir as t.TempDir() and
// never exposes it, and T5 needs it to place the marker file and to point the
// stub reviewer's read and write at real paths.
//
//  1. The provider stub answers the REVIEWER model's requests with the scripted
//     sequence: a view of the marker file, then a write into the working
//     directory, then the verdict text. One tool call per step is the reliable
//     shape, so the write is its own step rather than batched with the read.
//     Every other model keeps reviewerPassPrimaryText, byte for byte the same
//     answer the shared harness gives.
//  2. The requested model ids are recorded and handed back, so the test can
//     pin that exactly three review requests ran. The pointer is read only
//     after ExecuteRun has returned, when no producer is left.
//  3. dataDir is returned. It IS the run's working directory:
//     config.Init(dataDir, dataDir, false) passes it as both workingDir and
//     dataDir, and the tools resolve paths against it.
func newReviewerWriteAttemptApp(t *testing.T, opts reviewerPassAppOpts) (*App, string, string, *[]string) {
	t.Helper()
	isolationTmp := t.TempDir()
	t.Setenv("XDG_DATA_HOME", isolationTmp)
	t.Setenv("RUSH_GLOBAL_DATA", isolationTmp)
	configDir := filepath.Join(isolationTmp, "config")
	require.NoError(t, os.MkdirAll(configDir, 0o755))
	t.Setenv("XDG_CONFIG_HOME", configDir)
	t.Setenv("RUSH_GLOBAL_CONFIG", configDir)
	t.Setenv("RUSH_PROVIDER_CACHE_ONLY", "1")

	// Before the server: the handler's scripted read and write paths live in
	// the run's working directory, which is this dataDir.
	dataDir := t.TempDir()
	markerPath := filepath.Join(dataDir, "reviewer-marker.txt")
	writePath := filepath.Join(dataDir, "reviewer-wrote-this.txt")

	var mu sync.Mutex
	requests := make([]string, 0, 4)
	reviewerSteps := 0

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		require.NoError(t, err)
		var req struct {
			Model string `json:"model"`
		}
		require.NoError(t, json.Unmarshal(body, &req))

		mu.Lock()
		requests = append(requests, req.Model)
		step := 0
		if req.Model == reviewerPassReviewerModel {
			reviewerSteps++
			step = reviewerSteps
		}
		mu.Unlock()

		if step > 0 {
			switch step {
			case 1:
				// The read the reviewer must be able to do.
				admissionWriteSSE(w, []string{
					admissionSSEToolCall("ro-view", "reviewer-view-1", "view",
						`{"file_path":`+jsonString(markerPath)+`}`),
					admissionSSEStop("ro-view", "tool_calls"),
				})
			case 2:
				// The write the reviewer must not be able to do.
				admissionWriteSSE(w, []string{
					admissionSSEToolCall("ro-write", "reviewer-write-1", "write",
						`{"file_path":`+jsonString(writePath)+`,"content":`+jsonString(reviewerReadonlyWriteContent)+`}`),
					admissionSSEStop("ro-write", "tool_calls"),
				})
			default:
				admissionWriteSSE(w, []string{
					admissionSSEText("ro-verdict", reviewerReadonlyVerdictText),
					admissionSSEStop("ro-verdict", "stop"),
				})
			}
			return
		}

		contentJSON, mErr := json.Marshal(reviewerPassPrimaryText)
		require.NoError(t, mErr)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = fmt.Fprintf(w, "data: %s\n\n", `{"id":"rw","object":"chat.completion.chunk","created":1,"model":"probe","choices":[{"index":0,"delta":{"role":"assistant","content":`+string(contentJSON)+`},"finish_reason":null}]}`)
		_, _ = fmt.Fprintf(w, "data: %s\n\n", `{"id":"rw","object":"chat.completion.chunk","created":1,"model":"probe","choices":[{"index":0,"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`)
		_, _ = fmt.Fprintln(w, "data: [DONE]")
	}))
	t.Cleanup(srv.Close)

	selected := []string{
		`"smart": {"provider":"openaicompat","model":"smart-default"}`,
		`"fast": {"provider":"openaicompat","model":"fast-default"}`,
	}
	providerModels := []string{
		`{"id":"smart-default","context_window":200000,"default_max_tokens":1000}`,
		`{"id":"fast-default","context_window":200000,"default_max_tokens":1000}`,
	}
	if opts.withReviewer {
		selected = append(selected, `"reviewer": {"provider":"openaicompat","model":"`+reviewerPassReviewerModel+`"}`)
		providerModels = append(providerModels, `{"id":"`+reviewerPassReviewerModel+`","context_window":200000,"default_max_tokens":1000}`)
	}
	if opts.withWorker {
		selected = append(selected, `"worker": {"provider":"openaicompat","model":"reviewer-pass-worker"}`)
		providerModels = append(providerModels, `{"id":"reviewer-pass-worker","context_window":200000,"default_max_tokens":1000}`)
	}
	modelsJSON := `"models": {
    ` + strings.Join(selected, `,
    `) + `
  }`
	rushJSON := fmt.Sprintf(`{
  "disable_default_providers": true,
  "providers": {
    "openaicompat": {
      "id": "openaicompat", "type": "openai-compat", "base_url": %q,
      "api_key": "probe", "discover_models": false,
      "models": [
        %s
      ]
    }
  },
  %s
}`, srv.URL, strings.Join(providerModels, `,
        `), modelsJSON)
	require.NoError(t, os.WriteFile(filepath.Join(dataDir, "rush.json"), []byte(rushJSON), 0o644))
	store, err := config.Init(dataDir, dataDir, false)
	require.NoError(t, err)
	store.SetSelectedModelRuntime(config.SelectedModelTypeSmart, config.SelectedModel{Provider: "openaicompat", Model: "smart-default"})
	store.SetSelectedModelRuntime(config.SelectedModelTypeFast, config.SelectedModel{Provider: "openaicompat", Model: "fast-default"})
	if opts.withReviewer {
		store.SetSelectedModelRuntime(config.SelectedModelTypeReviewer, config.SelectedModel{Provider: "openaicompat", Model: reviewerPassReviewerModel})
	}
	if opts.withWorker {
		store.SetSelectedModelRuntime(config.SelectedModelTypeWorker, config.SelectedModel{Provider: "openaicompat", Model: "reviewer-pass-worker"})
	}
	store.SetupAgents()

	conn, err := db.Connect(context.Background(), dataDir)
	require.NoError(t, err)
	application, err := New(context.Background(), conn, store)
	if err != nil {
		err = errors.Join(err, db.ReleaseConn(conn))
	}
	require.NoError(t, err)
	t.Cleanup(application.Shutdown)

	sess := createModelOverrideSession(t, application, "reviewer-readonly-write")
	return application, sess.ID, dataDir, &requests
}
