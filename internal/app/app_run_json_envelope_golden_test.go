package app

// Golden test pinning the `rush run --json` wire envelope (phase 0 of
// docs/plans/2026-08-29-embeddable-library-refactoring.md).
//
// The envelope type RunResult (app_run_result.go) is declared
// "Wire-stable: fields here are part of the public contract for wrapper
// scripts". The upcoming refactor phases (export the envelope types,
// split RunNonInteractive into compute + render) must not move a single
// JSON tag, field name, field order, or JSON type in that wire format.
// This test is the proof: it drives one deterministic two-step scenario
// (one executed `view` tool call, then a final text answer) through the
// real production path — config.Init + app.New + RunNonInteractive in
// RunModeJSON against a mock openaicompat SSE provider, the same harness
// shape as p421_p0_1_interrupt_live_continuation_test.go — normalizes
// the two run-dependent fields (session_id, duration_ms), and compares
// the re-marshalled envelope byte-for-byte against
// testdata/golden/run_json_basic.json.
//
// What the typed round-trip (unmarshal into the real wire struct, then
// marshal back) protects: every json tag name, every field's presence
// or absence (including omitempty behaviour), field order within each
// object, and JSON value types. What it deliberately does not pin: the
// exact session_id / duration_ms values (normalized) and encoder-level
// byte choices, which are not part of the schema contract.
//
// Regenerate the fixture after an INTENTIONAL wire-format change, and
// review the fixture diff as the public-contract change it is:
//
//	go test ./internal/app -run TestRunJSONEnvelopeGolden -update
//
// Run this test with:
//
//	go test ./internal/app -run TestRunJSONEnvelopeGolden -v

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"charm.land/catwalk/pkg/catwalk"
	"charm.land/fantasy/providers/openaicompat"
	"github.com/PHPCraftdream/rush/internal/config"
	"github.com/PHPCraftdream/rush/internal/db"
	"github.com/PHPCraftdream/rush/internal/message"
	"github.com/stretchr/testify/require"
)

var updateGolden = flag.Bool("update", false, "rewrite the run-json envelope golden fixture")

func TestRunJSONEnvelopeGolden(t *testing.T) {
	// CRITICAL: isolate global config/data resolution before calling
	// app.New() below — see p421_p0_1_interrupt_live_continuation_test.go's
	// identical block for the full rationale (both GlobalConfig() and
	// GlobalConfigData() resolution paths must be isolated separately).
	isolationTmp := t.TempDir()
	t.Setenv("XDG_DATA_HOME", isolationTmp)
	t.Setenv("RUSH_GLOBAL_DATA", isolationTmp)
	isolationConfigDir := filepath.Join(isolationTmp, "config")
	require.NoError(t, os.MkdirAll(isolationConfigDir, 0o755))
	t.Setenv("XDG_CONFIG_HOME", isolationConfigDir)
	t.Setenv("RUSH_GLOBAL_CONFIG", isolationConfigDir)
	t.Setenv("RUSH_PROVIDER_CACHE_ONLY", "1")

	dataDir := t.TempDir()

	// Deterministic view-tool target so the scenario's one tool call
	// succeeds. Its RESULT never enters the envelope (only the call
	// count does), but a clean call keeps the run free of error-path
	// side effects.
	notesPath := filepath.Join(dataDir, "notes.md")
	require.NoError(t, os.WriteFile(notesPath, []byte("hello golden\n"), 0o644))

	// Two-phase mock provider, keyed on REQUEST CONTENT rather than call
	// order (title generation or a retry could otherwise steal a slot —
	// see p421's longer discussion). Round 1 returns exactly one `view`
	// tool call; round 2 is recognized by the tool result referencing
	// our tool-call id "call_1" and returns the final text answer.
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

		if strings.Contains(string(body), `"call_1"`) {
			// Round 2: the turn after the tool result came back.
			write([]string{
				`{"id":"c2","object":"chat.completion.chunk","created":2,"model":"probe","choices":[{"index":0,"delta":{"role":"assistant","content":"Done: notes.md says hello golden."},"finish_reason":null}]}`,
				`{"id":"c2","object":"chat.completion.chunk","created":2,"model":"probe","choices":[{"index":0,"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":17,"completion_tokens":5,"total_tokens":22}}`,
			})
			return
		}

		// Round 1: emit exactly one `view` tool call.
		write([]string{
			`{"id":"c1","object":"chat.completion.chunk","created":1,"model":"probe","choices":[{"index":0,"delta":{"role":"assistant"},"finish_reason":null}]}`,
			`{"id":"c1","object":"chat.completion.chunk","created":1,"model":"probe","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"view","arguments":"{\"file_path\":\"notes.md\"}"}}]},"finish_reason":null}]}`,
			`{"id":"c1","object":"chat.completion.chunk","created":1,"model":"probe","choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":11,"completion_tokens":3,"total_tokens":14}}`,
		})
	}))
	t.Cleanup(srv.Close)

	store, err := config.Init(dataDir, dataDir, false)
	require.NoError(t, err)
	store.Config().Providers.Set("openaicompat", config.ProviderConfig{
		ID:      "openaicompat",
		Type:    openaicompat.Name,
		BaseURL: srv.URL,
		APIKey:  "probe",
		Models: []catwalk.Model{
			{ID: "probe", Name: "probe", ContextWindow: 200000, DefaultMaxTokens: 1000},
		},
	})
	store.SetSelectedModelRuntime(config.SelectedModelTypeSmart, config.SelectedModel{
		Provider: "openaicompat",
		Model:    "probe",
	})
	store.SetSelectedModelRuntime(config.SelectedModelTypeFast, config.SelectedModel{
		Provider: "openaicompat",
		Model:    "probe",
	})
	store.SetupAgents()

	conn, err := db.Connect(context.Background(), dataDir)
	require.NoError(t, err)

	application, err := New(context.Background(), conn, store)
	require.NoError(t, err)
	t.Cleanup(func() {
		if application.RunQueuePump != nil {
			application.RunQueuePump.Stop()
		}
		for range application.dbReleasesNeeded {
			require.NoError(t, db.Release(dataDir))
		}
	})

	// A non-default title plus one pre-existing message keep needsTitle
	// false (agent_turn.go), so the background title-generation provider
	// call — a second, racy consumer of the same mock — never fires in
	// this test at all.
	sess, err := application.Sessions.Create(context.Background(), "golden-run-title")
	require.NoError(t, err)
	_, err = application.Messages.Create(context.Background(), sess.ID, message.CreateMessageParams{
		Role:  message.User,
		Parts: []message.ContentPart{message.TextContent{Text: "seed"}},
	})
	require.NoError(t, err)

	var output bytes.Buffer
	runErr := application.RunNonInteractive(context.Background(), &output, "read notes.md and summarise", RunOverrides{}, true, RunModeJSON, sess.ID, false)
	require.NoError(t, runErr)

	// Semantic sanity first: the scenario really did what it was
	// designed to do (one executed tool call, clean end_turn finish,
	// real final text, no warnings) — otherwise the golden comparison
	// below would be pinning a broken run instead of the contract.
	var envelope RunResult
	require.NoError(t, json.Unmarshal(output.Bytes(), &envelope), "output must be a single JSON envelope")
	require.Equal(t, "end_turn", envelope.ExitReason)
	require.Equal(t, "Done: notes.md says hello golden.", envelope.FinalText)
	require.Equal(t, []ToolCallStat{{Name: "view", Count: 1}}, envelope.ToolCalls)
	require.Empty(t, envelope.Error)
	require.Empty(t, envelope.Warnings)

	// Normalize the two run-dependent fields, then compare the typed
	// round-trip against the fixture: any json-tag rename, added or
	// removed field, omitempty change, field-order change, or JSON type
	// change shows up as a fixture diff.
	envelope.SessionID = "GOLDEN_SESSION_ID"
	envelope.DurationMs = 0
	got, err := json.Marshal(envelope)
	require.NoError(t, err)

	goldenPath := filepath.Join("testdata", "golden", "run_json_basic.json")
	if *updateGolden {
		require.NoError(t, os.MkdirAll(filepath.Dir(goldenPath), 0o755))
		require.NoError(t, os.WriteFile(goldenPath, got, 0o644))
		return
	}
	want, err := os.ReadFile(goldenPath)
	require.NoError(t, err, "golden fixture missing — regenerate with: go test ./internal/app -run TestRunJSONEnvelopeGolden -update")
	if !bytes.Equal(want, got) {
		t.Fatalf("Run --json wire envelope drifted from the golden fixture.\n--- want (golden) ---\n%s\n--- got ---\n%s\nIf this is an INTENTIONAL wire-format change, review the diff as a public-contract change and regenerate with: go test ./internal/app -run TestRunJSONEnvelopeGolden -update", want, got)
	}
}
