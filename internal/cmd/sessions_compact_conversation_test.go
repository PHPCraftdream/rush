package cmd

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/PHPCraftdream/rush/internal/app"
	"github.com/PHPCraftdream/rush/internal/config"
	"github.com/PHPCraftdream/rush/internal/message"
	"github.com/PHPCraftdream/rush/internal/session"
	"github.com/spf13/cobra"
	"github.com/stretchr/testify/require"
)

const compactDiagnosticRequestLimit = 4

type compactObservedRequest struct {
	method          string
	path            string
	modelClass      string
	localTarget     bool
	authorization   bool
	queryPresent    bool
	userinfoPresent bool
}

type compactRequestCapture struct {
	mu           sync.Mutex
	count        int
	records      []compactObservedRequest
	notify       chan struct{}
	expectedHost string
}

func newCompactRequestCapture(expectedHost string) *compactRequestCapture {
	return &compactRequestCapture{
		notify:       make(chan struct{}, 1),
		expectedHost: expectedHost,
	}
}

func (c *compactRequestCapture) record(r *http.Request, model string) {
	observed := compactObservedRequest{
		method:          compactMethodClass(r.Method),
		path:            compactPathClass(r.URL.Path),
		modelClass:      compactModelClass(model),
		localTarget:     r.Host == c.expectedHost,
		authorization:   compactHeaderClass(r.Header.Get("Authorization")) == "fixture-key",
		queryPresent:    r.URL.RawQuery != "",
		userinfoPresent: r.URL.User != nil,
	}
	c.mu.Lock()
	c.count++
	if len(c.records) < compactDiagnosticRequestLimit {
		c.records = append(c.records, observed)
	}
	c.mu.Unlock()
	select {
	case c.notify <- struct{}{}:
	default:
	}
}

func (c *compactRequestCapture) snapshot() (int, []compactObservedRequest) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.count, append([]compactObservedRequest(nil), c.records...)
}

func compactModelClass(model string) string {
	switch model {
	case "worker-model":
		return "worker"
	case "smart-model":
		return "smart"
	case "fast-model":
		return "fast"
	case "":
		return "missing"
	default:
		return "other"
	}
}

func compactHeaderClass(value string) string {
	if value == "Bearer fixture-only-key" {
		return "fixture-key"
	}
	if value == "" {
		return "missing"
	}
	return "other"
}

func compactMethodClass(method string) string {
	if method == http.MethodPost {
		return "POST"
	}
	return "other"
}

func compactPathClass(path string) string {
	if strings.HasSuffix(path, "/chat/completions") {
		return "chat-completions"
	}
	return "other"
}

type compactSessionFacts struct {
	historyCount    int
	summaryCount    int
	summaryPointer  bool
	activityKind    string
	unreadableFacts int
	osLockHeld      bool
}

type compactFailureDiagnostics struct {
	mu         sync.Mutex
	stage      string
	ctx        context.Context
	requests   *compactRequestCapture
	facts      compactSessionFacts
	factsKnown bool
}

type compactContextInfo struct {
	state             string
	cancelled         bool
	deadlineExceeded  bool
	deadlineSet       bool
	deadlineRemaining time.Duration
}

func newCompactFailureDiagnostics(t *testing.T) *compactFailureDiagnostics {
	t.Helper()
	d := &compactFailureDiagnostics{stage: "test started"}
	// Register first so this failure-only report runs after fixture cleanup,
	// but is already present if setup or the first assertion fails.
	t.Cleanup(func() {
		if !t.Failed() {
			return
		}
		d.mu.Lock()
		stage, ctx, facts, factsKnown, requests := d.stage, d.ctx, d.facts, d.factsKnown, d.requests
		d.mu.Unlock()
		contextInfo := compactContextSnapshot(ctx)
		requestCount, records := 0, []compactObservedRequest(nil)
		if requests != nil {
			requestCount, records = requests.snapshot()
		}
		t.Logf("sessions compact diagnostic: stage=%q context_state=%q context_cancelled=%t deadline_exceeded=%t deadline_set=%t deadline_remaining=%s provider_request_count=%d", stage, contextInfo.state, contextInfo.cancelled, contextInfo.deadlineExceeded, contextInfo.deadlineSet, contextInfo.deadlineRemaining, requestCount)
		for i, request := range records {
			t.Logf("sessions compact request[%d]: method=%q path=%q model_class=%q local_target_match=%t authorization_present=%t query_present=%t userinfo_present=%t", i, request.method, request.path, request.modelClass, request.localTarget, request.authorization, request.queryPresent, request.userinfoPresent)
		}
		if factsKnown {
			t.Logf("sessions compact session facts: activity_kind=%q unreadable_facts=%d os_lock_held=%t history_messages=%d summary_messages=%d summary_pointer_set=%t", facts.activityKind, facts.unreadableFacts, facts.osLockHeld, facts.historyCount, facts.summaryCount, facts.summaryPointer)
		}
	})
	return d
}

func (d *compactFailureDiagnostics) setStage(stage string) {
	d.mu.Lock()
	d.stage = stage
	d.mu.Unlock()
}

func compactContextSnapshot(ctx context.Context) compactContextInfo {
	info := compactContextInfo{state: "not-started"}
	if ctx == nil {
		return info
	}
	info.state = "active"
	if err := ctx.Err(); err != nil {
		info.state = err.Error()
		info.cancelled = errors.Is(err, context.Canceled)
		info.deadlineExceeded = errors.Is(err, context.DeadlineExceeded)
	}
	if deadline, ok := ctx.Deadline(); ok {
		info.deadlineSet = true
		info.deadlineRemaining = time.Until(deadline).Round(time.Millisecond)
	}
	return info
}

func (d *compactFailureDiagnostics) setContext(ctx context.Context) {
	d.mu.Lock()
	d.ctx = ctx
	d.mu.Unlock()
}

func (d *compactFailureDiagnostics) snapshotContext(ctx context.Context) {
	d.mu.Lock()
	d.ctx = ctx
	d.mu.Unlock()
}

func (d *compactFailureDiagnostics) setRequests(requests *compactRequestCapture) {
	d.mu.Lock()
	d.requests = requests
	d.mu.Unlock()
}

func (d *compactFailureDiagnostics) setSessionFacts(facts compactSessionFacts) {
	d.mu.Lock()
	d.facts = facts
	d.factsKnown = true
	d.mu.Unlock()
}

func TestSessionsCompactConversation_DispatchesHashPrefixWithDefaultWorker(t *testing.T) {
	diag := newCompactFailureDiagnostics(t)
	require.Equal(t, "worker", sessionsCompactCmd.Flags().Lookup("role").DefValue)
	cmd, seed, _, requests, sess := seedSessionsCompactConversation(t, diag, true)
	prefix := session.HashID(sess.ID)[:12]
	diag.setStage("executing session compaction by hash prefix")
	err := invokeSessionsCompactConversation(t, diag, cmd, prefix)
	captureCompactSessionFacts(t, diag, seed, sess, false)
	require.NoError(t, err)
	rows, err := seed.Messages.List(t.Context(), sess.ID)
	require.NoError(t, err)
	require.Len(t, rows, 1)
	require.True(t, rows[0].IsSummaryMessage)

	diag.setStage("awaiting bounded fake-provider observation")
	request := awaitSessionsCompactRequest(t, requests)
	require.Equal(t, 1, requests.snapshotCount())
	require.Equal(t, "POST", request.method)
	require.Equal(t, "chat-completions", request.path)
	require.True(t, request.localTarget, "fake provider request must target this test's local server")
	require.True(t, request.authorization, "fixture API key must produce the expected Authorization header")
	require.False(t, request.queryPresent)
	require.False(t, request.userinfoPresent)
	require.Equal(t, "worker", request.modelClass)
	require.Contains(t, cmd.OutOrStdout().(*bytes.Buffer).String(), "compacted session "+sess.ID)
	require.Contains(t, cmd.OutOrStdout().(*bytes.Buffer).String(), "("+prefix[:8]+") with worker model")
	facts := compactFactsSnapshot(diag)
	require.Equal(t, 0, facts.historyCount)
	require.Equal(t, 1, facts.summaryCount)
	require.True(t, facts.summaryPointer)
	updated, err := seed.Sessions.Get(t.Context(), sess.ID)
	require.NoError(t, err)
	require.Equal(t, rows[0].ID, updated.SummaryMessageID)
}

func TestSessionsCompactConversation_RejectsInvalidRolesAndDatabaseFlags(t *testing.T) {
	for _, tc := range []struct {
		name string
		args []string
		want string
	}{
		{name: "unsupported role", args: []string{"--role", "unknown", "session"}, want: "unsupported summary role"},
		{name: "role without session", args: []string{"--role", "worker"}, want: "only when compacting"},
		{name: "database flag with session", args: []string{"session", "--dry-run"}, want: "database compaction"},
		{name: "false database flag with session", args: []string{"session", "--dry-run=false"}, want: "database compaction"},
		{name: "force flag with session", args: []string{"session", "--force=false"}, want: "database compaction"},
		{name: "json flag with session", args: []string{"--json", "session"}, want: "database compaction"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			diag := newCompactFailureDiagnostics(t)
			cmd := newSessionsCompactConversationTestCommand(t, "")
			diag.setStage("checking strict CLI flag and role validation")
			err := invokeSessionsCompactConversation(t, diag, cmd, tc.args...)
			require.ErrorContains(t, err, tc.want)
		})
	}
}

func TestSessionsCompactConversation_MissingWorkerDoesNotFallback(t *testing.T) {
	diag := newCompactFailureDiagnostics(t)
	cmd, seed, server, requests, sess := seedSessionsCompactConversation(t, diag, false)
	diag.setStage("requesting missing worker role")
	err := invokeSessionsCompactConversation(t, diag, cmd, sess.ID)
	captureCompactSessionFacts(t, diag, seed, sess, false)
	require.ErrorContains(t, err, "worker model is unavailable")
	count, _ := requests.snapshot()
	require.Zero(t, count, "missing worker must not make a provider request or fall back")
	facts := compactFactsSnapshot(diag)
	require.Equal(t, 1, facts.historyCount)
	require.Zero(t, facts.summaryCount)
	require.False(t, facts.summaryPointer)
	server.Close()
}

func TestSessionsCompactConversation_LiveSessionPreservesHistory(t *testing.T) {
	diag := newCompactFailureDiagnostics(t)
	cmd, seed, server, requests, sess := seedSessionsCompactConversation(t, diag, true)
	dataDir := seed.Config().Options.DataDirectory
	lock, err := session.TryAcquireSessionLock(dataDir, sess.ID)
	require.NoError(t, err)
	defer lock.Release()
	captureCompactSessionFacts(t, diag, seed, sess, true)
	diag.setStage("refusing live session before summary request")
	err = invokeSessionsCompactConversation(t, diag, cmd, sess.ID)
	captureCompactSessionFacts(t, diag, seed, sess, true)
	require.ErrorContains(t, err, "still being worked on")
	count, _ := requests.snapshot()
	require.Zero(t, count, "busy-session refusal must not request a summary")
	facts := compactFactsSnapshot(diag)
	require.Equal(t, 1, facts.historyCount)
	require.Zero(t, facts.summaryCount)
	require.False(t, facts.summaryPointer)
	require.True(t, facts.osLockHeld)
	server.Close()
}

func seedSessionsCompactConversation(t *testing.T, diag *compactFailureDiagnostics, worker bool) (*cobra.Command, *app.App, *httptest.Server, *compactRequestCapture, session.Session) {
	t.Helper()
	diag.setStage("isolating global configuration and data roots")
	tmp := t.TempDir()
	globalData := filepath.Join(tmp, "global-data")
	globalConfig := filepath.Join(tmp, "global-config")
	for key, value := range map[string]string{
		"RUSH_GLOBAL_DATA":         globalData,
		"RUSH_GLOBAL_CONFIG":       globalConfig,
		"XDG_DATA_HOME":            globalData,
		"XDG_CONFIG_HOME":          globalConfig,
		"HOME":                     tmp,
		"USERPROFILE":              tmp,
		"RUSH_PROVIDER_CACHE_ONLY": "1",
	} {
		t.Setenv(key, value)
	}
	config.ResetProviderCacheForTests()
	for _, dir := range []string{globalData, globalConfig} {
		require.NoError(t, os.MkdirAll(dir, 0o755))
	}
	projectDir := filepath.Join(tmp, "project")
	dataDir := filepath.Join(tmp, "project-state")
	require.NoError(t, os.MkdirAll(projectDir, 0o755))
	require.NoError(t, os.MkdirAll(dataDir, 0o755))

	diag.setStage("starting local fake provider")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Model string `json:"model"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		diag.requestsRecord(r, req.Model)
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprintf(w, "data: {\"id\":\"summary\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":%q,\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"summary\"},\"finish_reason\":null}]}\n\n", req.Model)
		fmt.Fprintf(w, "data: {\"id\":\"summary\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":%q,\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":4,\"completion_tokens\":2,\"total_tokens\":6}}\n\n", req.Model)
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	requests := newCompactRequestCapture(server.Listener.Addr().String())
	diag.setRequests(requests)
	diag.setStage("local fake provider ready; writing project-local config")
	t.Cleanup(server.Close)

	models := map[string]any{
		"smart": map[string]string{"provider": "probe", "model": "smart-model"},
		"fast":  map[string]string{"provider": "probe", "model": "fast-model"},
	}
	if worker {
		models["worker"] = map[string]string{"provider": "probe", "model": "worker-model"}
	}
	projectConfig := map[string]any{
		"options": map[string]string{"data_directory": dataDir},
		"providers": map[string]any{"probe": map[string]any{
			"id": "probe", "name": "Probe", "type": "openai-compat", "base_url": server.URL, "api_key": "fixture-only-key",
			"models": []map[string]string{{"id": "smart-model"}, {"id": "fast-model"}, {"id": "worker-model"}},
		}},
		"models": models,
	}
	configBytes, err := json.Marshal(projectConfig)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(projectDir, "rush.json"), configBytes, 0o600))

	originalCwd, err := os.Getwd()
	require.NoError(t, err)
	t.Cleanup(func() { _ = os.Chdir(originalCwd) })
	setupCtx, cancelSetup := context.WithTimeout(t.Context(), 8*time.Second)
	diag.setContext(setupCtx)
	t.Cleanup(cancelSetup)
	cmd := newSessionsCompactConversationTestCommand(t, projectDir)
	cmd.SetContext(setupCtx)
	diag.setStage("constructing app from isolated project config")
	seed, err := setupAppLite(cmd)
	require.NoError(t, err)
	t.Cleanup(seed.Shutdown)
	diag.setStage("app ready; creating session")
	sess, err := seed.Sessions.CreateWithID(t.Context(), "compact-command-session", "command history")
	require.NoError(t, err)
	_, err = seed.Messages.Create(t.Context(), sess.ID, message.CreateMessageParams{
		Role:  message.User,
		Parts: []message.ContentPart{message.TextContent{Text: strings.Repeat("important history ", 200)}},
	})
	require.NoError(t, err)
	captureCompactSessionFacts(t, diag, seed, sess, false)
	diag.setStage("fixture ready; one original history message seeded")
	return cmd, seed, server, requests, sess
}

func (d *compactFailureDiagnostics) requestsRecord(r *http.Request, model string) {
	d.mu.Lock()
	requests := d.requests
	d.mu.Unlock()
	if requests != nil {
		requests.record(r, model)
	}
	d.setStage("fake provider observed request")
}

func captureCompactSessionFacts(t *testing.T, diag *compactFailureDiagnostics, a *app.App, sess session.Session, lockHeld bool) {
	t.Helper()
	activity, activityErr := a.SessionActivity(t.Context(), sess.ID)
	rows, rowsErr := a.Messages.List(t.Context(), sess.ID)
	updated, sessionErr := a.Sessions.Get(t.Context(), sess.ID)
	facts := compactSessionFacts{osLockHeld: lockHeld, activityKind: "unavailable"}
	if activityErr == nil {
		facts.activityKind = string(activity.Verdict.Kind)
		facts.unreadableFacts = len(activity.Facts.Unreadable)
	}
	if rowsErr == nil {
		for _, row := range rows {
			if row.IsSummaryMessage {
				facts.summaryCount++
			} else {
				facts.historyCount++
			}
		}
	}
	facts.summaryPointer = sessionErr == nil && updated.SummaryMessageID != ""
	diag.setSessionFacts(facts)
}

func compactFactsSnapshot(diag *compactFailureDiagnostics) compactSessionFacts {
	diag.mu.Lock()
	defer diag.mu.Unlock()
	return diag.facts
}

func newSessionsCompactConversationTestCommand(t *testing.T, projectDir string) *cobra.Command {
	t.Helper()
	cmd := &cobra.Command{
		Use:           sessionsCompactCmd.Use,
		Args:          sessionsCompactCmd.Args,
		RunE:          sessionsCompactCmd.RunE,
		SilenceErrors: true,
		SilenceUsage:  true,
	}
	cmd.Flags().Bool("dry-run", false, "")
	cmd.Flags().Bool("force", false, "")
	cmd.Flags().Bool("json", false, "")
	cmd.Flags().String("role", sessionsCompactCmd.Flags().Lookup("role").DefValue, "")
	cmd.Flags().String("cwd", "", "")
	cmd.Flags().String("password", "", "")
	ensureRootFlagStandIns(cmd, "")
	if projectDir != "" {
		if err := cmd.Flags().Set("cwd", projectDir); err != nil {
			t.Fatal(err)
		}
	}
	cmd.SetContext(t.Context())
	cmd.SetOut(&bytes.Buffer{})
	return cmd
}

func invokeSessionsCompactConversation(t *testing.T, diag *compactFailureDiagnostics, cmd *cobra.Command, args ...string) error {
	t.Helper()
	ctx, cancel := context.WithTimeout(cmd.Context(), 8*time.Second)
	defer cancel()
	diag.setContext(ctx)
	diag.setStage("parsing and dispatching command arguments")
	cmd.SetContext(ctx)
	cmd.SetArgs(args)
	err := cmd.Execute()
	diag.snapshotContext(ctx)
	if err != nil {
		diag.setStage("command returned an error")
	} else {
		diag.setStage("command returned successfully")
	}
	return err
}

func awaitSessionsCompactRequest(t *testing.T, requests *compactRequestCapture) compactObservedRequest {
	t.Helper()
	select {
	case <-requests.notify:
		_, records := requests.snapshot()
		if len(records) == 0 {
			t.Fatal("provider request count changed without a bounded request record")
			return compactObservedRequest{}
		}
		return records[0]
	case <-time.After(8 * time.Second):
		t.Fatal("timed out waiting for standalone summary request")
		return compactObservedRequest{}
	}
}

func (c *compactRequestCapture) snapshotCount() int {
	count, _ := c.snapshot()
	return count
}
