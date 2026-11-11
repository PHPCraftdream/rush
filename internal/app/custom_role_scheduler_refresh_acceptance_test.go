package app

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"testing"
	"time"

	"github.com/PHPCraftdream/rush/internal/config"
	"github.com/PHPCraftdream/rush/internal/db"
	"github.com/PHPCraftdream/rush/internal/ktavcodec"
	"github.com/PHPCraftdream/rush/internal/session"
	"github.com/stretchr/testify/require"
)

const schedulerCustomRoleName = "Research Analyst"

type schedulerRoleGate struct {
	ch   chan struct{}
	once sync.Once
}

func newSchedulerRoleGate(t *testing.T) *schedulerRoleGate {
	t.Helper()
	gate := &schedulerRoleGate{ch: make(chan struct{})}
	t.Cleanup(gate.Release)
	return gate
}

func (g *schedulerRoleGate) Release() {
	g.once.Do(func() { close(g.ch) })
}

type schedulerRoleRequest struct {
	Model       string
	MaxTokens   int64
	Temperature *float64
	System      string
	Err         error
}

type schedulerRoleWireMessage struct {
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"`
}

type schedulerRoleWireBody struct {
	Model               string                     `json:"model"`
	MaxTokens           int64                      `json:"max_tokens"`
	MaxCompletionTokens int64                      `json:"max_completion_tokens"`
	Temperature         *float64                   `json:"temperature"`
	Messages            []schedulerRoleWireMessage `json:"messages"`
}

type schedulerRoleHandlerDiagnostics struct {
	mu          sync.Mutex
	count       int
	first       error
	requests    int
	gates       int
	model       string
	stage       string
	lastRequest string
	events      []string
}

func (d *schedulerRoleHandlerDiagnostics) eventLocked(event string) {
	if len(d.events) >= 32 {
		d.events = d.events[1:]
	}
	d.events = append(d.events, event)
}

func (d *schedulerRoleHandlerDiagnostics) setStage(requestID int, stage, model string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.stage = stage
	if model != "" {
		d.model = model
	}
	d.eventLocked(fmt.Sprintf("request#%d %s model=%q", requestID, stage, d.model))
}

func (d *schedulerRoleHandlerDiagnostics) record(err error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.count++
	if d.first == nil {
		d.first = err
	}
	d.eventLocked(fmt.Sprintf("handler error#%d: %v", d.count, err))
}

func (d *schedulerRoleHandlerDiagnostics) requestRecorded(request schedulerRoleRequest) int {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.requests++
	requestID := d.requests
	d.model = request.Model
	temperature := "unset"
	if request.Temperature != nil {
		temperature = fmt.Sprint(*request.Temperature)
	}
	d.lastRequest = fmt.Sprintf("model=%q max_tokens=%d temperature=%s system_bytes=%d err=%v", request.Model, request.MaxTokens, temperature, len(request.System), request.Err)
	d.stage = "request recorded"
	d.eventLocked(fmt.Sprintf("request#%d recorded {%s}", requestID, d.lastRequest))
	return requestID
}

func (d *schedulerRoleHandlerDiagnostics) gateWaiting(requestID int) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.stage = fmt.Sprintf("request#%d waiting for a response gate", requestID)
	d.eventLocked(d.stage)
}

func (d *schedulerRoleHandlerDiagnostics) gateReceived(requestID int) int {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.gates++
	gateID := d.gates
	d.stage = fmt.Sprintf("request#%d received gate#%d; waiting for release", requestID, gateID)
	d.eventLocked(d.stage)
	return gateID
}

func (d *schedulerRoleHandlerDiagnostics) gateReleased(requestID, gateID int) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.stage = fmt.Sprintf("request#%d gate#%d released", requestID, gateID)
	d.eventLocked(d.stage)
}

func (d *schedulerRoleHandlerDiagnostics) snapshot() string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return fmt.Sprintf("requests=%d gates=%d last_model=%q stage=%q last_request={%s} errors=%d first_error=%v history=%q", d.requests, d.gates, d.model, d.stage, d.lastRequest, d.count, d.first, d.events)
}

func (d *schedulerRoleHandlerDiagnostics) err() error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.count == 0 {
		return nil
	}
	return fmt.Errorf("provider handler recorded %d error(s); first: %w", d.count, d.first)
}

type customRoleSchedulerApp struct {
	app                *App
	store              *config.ConfigStore
	foreign            *config.ConfigStore
	dataDir            string
	requests           chan schedulerRoleRequest
	gates              chan *schedulerRoleGate
	handlerDiagnostics *schedulerRoleHandlerDiagnostics
	lifecycleCtx       context.Context
	teardownDiagnostic *func()
}

func newCustomRoleSchedulerApp(t *testing.T) customRoleSchedulerApp {
	t.Helper()
	isolationDir := t.TempDir()
	t.Setenv("XDG_DATA_HOME", isolationDir)
	t.Setenv("RUSH_GLOBAL_DATA", isolationDir)
	configDir := filepath.Join(isolationDir, "config")
	require.NoError(t, os.MkdirAll(configDir, 0o755))
	t.Setenv("XDG_CONFIG_HOME", configDir)
	t.Setenv("RUSH_GLOBAL_CONFIG", configDir)
	t.Setenv("RUSH_PROVIDER_CACHE_ONLY", "1")

	requests := make(chan schedulerRoleRequest, 8)
	gates := make(chan *schedulerRoleGate, 8)
	lifecycleCtx, cancelLifecycle := context.WithCancel(t.Context())
	handlerDiagnostics := &schedulerRoleHandlerDiagnostics{}
	teardownDiagnostic := new(func())
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		handlerDiagnostics.setStage(0, "reading request", "")
		body, readErr := io.ReadAll(r.Body)
		recorded := schedulerRoleRequest{Err: readErr}
		if readErr == nil {
			var decoded schedulerRoleWireBody
			if err := json.Unmarshal(body, &decoded); err != nil {
				recorded.Err = err
			} else {
				recorded.Model = decoded.Model
				recorded.MaxTokens = decoded.MaxTokens
				if recorded.MaxTokens == 0 {
					recorded.MaxTokens = decoded.MaxCompletionTokens
				}
				recorded.Temperature = decoded.Temperature
				for _, message := range decoded.Messages {
					if message.Role == "system" {
						if err := json.Unmarshal(message.Content, &recorded.System); err != nil {
							recorded.System = string(message.Content)
						}
						break
					}
				}
			}
		}
		if recorded.Err != nil {
			handlerDiagnostics.record(recorded.Err)
		}
		requestID := handlerDiagnostics.requestRecorded(recorded)
		select {
		case requests <- recorded:
		case <-r.Context().Done():
			handlerDiagnostics.record(fmt.Errorf("provider request canceled before recording: %w", r.Context().Err()))
			return
		case <-lifecycleCtx.Done():
			handlerDiagnostics.record(fmt.Errorf("provider lifecycle ended before recording: %w", lifecycleCtx.Err()))
			return
		default:
			handlerDiagnostics.record(fmt.Errorf("provider request recording overflow (capacity %d)", cap(requests)))
			http.Error(w, "provider request recording overflow", http.StatusServiceUnavailable)
			return
		}

		var gate *schedulerRoleGate
		var gateID int
		handlerDiagnostics.gateWaiting(requestID)
		select {
		case gate = <-gates:
			gateID = handlerDiagnostics.gateReceived(requestID)
			handlerDiagnostics.setStage(requestID, fmt.Sprintf("waiting for gate#%d release", gateID), recorded.Model)
		case <-r.Context().Done():
			return
		case <-lifecycleCtx.Done():
			return
		}
		select {
		case <-gate.ch:
			handlerDiagnostics.gateReleased(requestID, gateID)
			handlerDiagnostics.setStage(requestID, fmt.Sprintf("writing SSE for gate#%d", gateID), recorded.Model)
		case <-r.Context().Done():
			return
		case <-lifecycleCtx.Done():
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		for _, event := range []string{
			`data: {"id":"role-scheduler","object":"chat.completion.chunk","created":1,"model":"probe","choices":[{"index":0,"delta":{"role":"assistant","content":"ok"},"finish_reason":null}]}` + "\n\n",
			`data: {"id":"role-scheduler","object":"chat.completion.chunk","created":1,"model":"probe","choices":[{"index":0,"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}` + "\n\n",
			"data: [DONE]\n\n",
		} {
			if _, err := fmt.Fprint(w, event); err != nil {
				handlerDiagnostics.record(fmt.Errorf("provider response write failed: %w", err))
				return
			}
		}
		handlerDiagnostics.setStage(requestID, fmt.Sprintf("SSE body writes returned for gate#%d", gateID), recorded.Model)
	}))
	var application *App
	t.Cleanup(func() {
		for {
			select {
			case gate := <-gates:
				if gate != nil {
					gate.Release()
				}
			default:
				cancelLifecycle()
				if application != nil {
					shutdownDone := make(chan struct{})
					go func() {
						application.Shutdown()
						close(shutdownDone)
					}()
					select {
					case <-shutdownDone:
					case <-time.After(20 * time.Second):
						t.Errorf("App.Shutdown did not finish within 20s")
						if teardownDiagnostic != nil && *teardownDiagnostic != nil {
							(*teardownDiagnostic)()
						}
					}
				}
				serverCloseDone := make(chan struct{})
				go func() {
					server.Close()
					close(serverCloseDone)
				}()
				select {
				case <-serverCloseDone:
				case <-time.After(5 * time.Second):
					t.Errorf("provider test server did not close within 5s")
					if teardownDiagnostic != nil && *teardownDiagnostic != nil {
						(*teardownDiagnostic)()
					}
				}
				if err := handlerDiagnostics.err(); err != nil {
					t.Errorf("%v", err)
				}
				return
			}
		}
	})

	dataDir := t.TempDir()
	initialConfig := fmt.Sprintf(`{
  "options": {"disable_default_providers": true},
  "providers": {
    "openaicompat": {
      "id":"openaicompat", "type":"openai-compat", "base_url":%q,
      "api_key":"probe", "discover_models":false,
      "models":[
        {"id":"smart-default","context_window":200000,"default_max_tokens":1000},
        {"id":"fast-default","context_window":200000,"default_max_tokens":1000},
        {"id":"worker-default","context_window":200000,"default_max_tokens":1000},
        {"id":"reviewer-default","context_window":200000,"default_max_tokens":1000},
        {"id":"role-before","context_window":200000,"default_max_tokens":1000},
        {"id":"role-after","context_window":200000,"default_max_tokens":1000}
      ]
    }
  },
  "models": {
    "smart":{"provider":"openaicompat","model":"smart-default"},
    "fast":{"provider":"openaicompat","model":"fast-default"},
    "worker":{"provider":"openaicompat","model":"worker-default"},
    "reviewer":{"provider":"openaicompat","model":"reviewer-default"}
  },
  "roles": {
    "Research Analyst":{"provider":"openaicompat","model":"role-before","max_tokens":111,"temperature":0.25,"tools":["view","grep"]}
  }
}`, server.URL)
	require.NoError(t, os.WriteFile(filepath.Join(dataDir, "rush.json"), []byte(initialConfig), 0o600))
	store, err := config.Init(dataDir, dataDir, false)
	require.NoError(t, err)
	require.NotNil(t, store.Config().Options)
	require.True(t, store.Config().Options.DisableDefaultProviders)
	store.SetupAgents()
	conn, err := db.Connect(context.Background(), dataDir)
	require.NoError(t, err)
	newApp, err := New(context.Background(), conn, store)
	if err != nil {
		err = fmt.Errorf("%w (db release: %v)", err, db.ReleaseConn(conn))
	}
	require.NoError(t, err)
	application = newApp

	foreign, err := config.Init(dataDir, dataDir, false)
	require.NoError(t, err)
	active := config.ActiveConfigFileName(filepath.Join(dataDir, "rush.json"))
	require.Equal(t, ".ktav", filepath.Ext(active), "the supported writer must target the migrated active Ktav slot")
	require.FileExists(t, active)

	return customRoleSchedulerApp{
		app:                application,
		store:              store,
		foreign:            foreign,
		dataDir:            dataDir,
		requests:           requests,
		gates:              gates,
		handlerDiagnostics: handlerDiagnostics,
		lifecycleCtx:       lifecycleCtx,
		teardownDiagnostic: teardownDiagnostic,
	}
}

func startCustomRoleSchedulerRun(t *testing.T, application *App, ctx context.Context, sess session.Session, prompt string) schedulerRoleRun {
	t.Helper()
	ctx, cancel := context.WithTimeout(ctx, time.Minute)
	t.Cleanup(cancel)
	outcome := make(chan runOutcome, 1)
	go func() {
		result, err := application.ExecuteRun(ctx, RunRequest{
			Prompt:            prompt,
			ContinueSessionID: sess.ID,
			Overrides:         RunOverrides{RequestedRole: schedulerCustomRoleName},
			Mode:              RunModeJSON,
			Stdout:            io.Discard,
			Stderr:            io.Discard,
			HideSpinner:       true,
			FailIfSessionBusy: true,
		})
		outcome <- runOutcome{result: result, err: err}
	}()
	return schedulerRoleRun{ctx: ctx, outcome: outcome}
}

type schedulerRoleRun struct {
	ctx     context.Context
	outcome <-chan runOutcome
}

type schedulerRoleRunState struct {
	mu        sync.Mutex
	sessionID string
	run       *schedulerRoleRun
	captured  bool
}

func (s *schedulerRoleRunState) setSession(sessionID string) {
	s.mu.Lock()
	s.sessionID = sessionID
	s.mu.Unlock()
}

func (s *schedulerRoleRunState) setRun(run *schedulerRoleRun) {
	s.mu.Lock()
	s.run = run
	s.mu.Unlock()
}

func (s *schedulerRoleRunState) clearRun() {
	s.mu.Lock()
	s.run = nil
	s.mu.Unlock()
}

func (s *schedulerRoleRunState) capture(t *testing.T, h customRoleSchedulerApp) {
	t.Helper()
	diagnostics := h.handlerDiagnostics
	s.mu.Lock()
	if s.captured {
		s.mu.Unlock()
		return
	}
	s.captured = true
	sessionID := s.sessionID
	run := s.run
	s.mu.Unlock()

	cfg, generation := h.store.Snapshot()
	role, exists := cfg.Roles[schedulerCustomRoleName]
	busy := sessionID != "" && h.app.AgentCoordinator.IsSessionBusy(sessionID)
	runErr := "not started"
	outcomeState := "run completed before failure snapshot"
	if run != nil {
		runErr = fmt.Sprint(run.ctx.Err())
		select {
		case outcome := <-run.outcome:
			outcomeState = fmt.Sprintf("completed result_nil=%v err=%v", outcome.result == nil, outcome.err)
		default:
			outcomeState = "pending"
		}
	}
	t.Logf("custom-role scheduler trace: generation=%d role_exists=%v model=%q active_config=%q disk_sync_running=%v session_id=%q session_busy=%v run_context_err=%s run_outcome=%s provider={%s} queued_requests=%d", generation, exists, role.Model, config.ActiveConfigFileName(filepath.Join(h.dataDir, "rush.json")), h.store.ConfigDiskSyncRunning(), sessionID, busy, runErr, outcomeState, diagnostics.snapshot(), len(h.requests))
	stacks := make([]byte, 4<<20)
	n := runtime.Stack(stacks, true)
	if n == len(stacks) {
		t.Log("goroutine trace may be truncated at 4 MiB")
	}
	t.Logf("goroutine trace:\n%s", stacks[:n])
}

func waitCustomRoleSchedulerRequest(t *testing.T, requests <-chan schedulerRoleRequest, timeoutTrace func()) schedulerRoleRequest {
	t.Helper()
	select {
	case request := <-requests:
		return request
	case <-time.After(30 * time.Second):
		timeoutTrace()
		t.Fatal("no custom-role provider request arrived within 30s")
		return schedulerRoleRequest{}
	}
}

func waitCustomRoleSchedulerRun(t *testing.T, run schedulerRoleRun, timeoutTrace func()) runOutcome {
	t.Helper()
	select {
	case result := <-run.outcome:
		return result
	case <-time.After(30 * time.Second):
		timeoutTrace()
		t.Fatalf("custom-role ExecuteRun did not finish within 30s (run_context_err=%v)", run.ctx.Err())
		return runOutcome{}
	}
}

func readSchedulerCustomRolesFromKtav(t *testing.T, path string) map[string]config.CustomRole {
	t.Helper()
	raw, err := os.ReadFile(path)
	require.NoError(t, err)
	tree, err := ktavcodec.DecodeKtav(raw)
	require.NoError(t, err)
	jsonBytes, err := ktavcodec.ToJSON(tree)
	require.NoError(t, err)
	var document struct {
		Roles map[string]config.CustomRole `json:"roles"`
	}
	require.NoError(t, json.Unmarshal(jsonBytes, &document))
	return document.Roles
}

func readSchedulerCustomRoleFromKtav(t *testing.T, path string) config.CustomRole {
	t.Helper()
	role, ok := readSchedulerCustomRolesFromKtav(t, path)[schedulerCustomRoleName]
	require.True(t, ok, "the role update must be present in the active Ktav document")
	return role
}

func TestExecuteRunCustomRoleRefreshesFromForeignKtavWriteByScheduler(t *testing.T) {
	h := newCustomRoleSchedulerApp(t)
	activeRun := &schedulerRoleRunState{}
	trace := func() { activeRun.capture(t, h) }
	*h.teardownDiagnostic = trace
	t.Cleanup(func() {
		if t.Failed() {
			trace()
		}
	})
	require.True(t, h.store.ConfigDiskSyncRunning(), "App.New must own the live config disk scheduler")
	initialGeneration := h.store.Generation()
	sess := createRoleSyncSession(t, h.app, "custom-role-scheduler-refresh")
	activeRun.setSession(sess.ID)
	firstGate := newSchedulerRoleGate(t)
	h.gates <- firstGate
	firstOutcome := startCustomRoleSchedulerRun(t, h.app, h.lifecycleCtx, sess, "first role request")
	activeRun.setRun(&firstOutcome)

	first := waitCustomRoleSchedulerRequest(t, h.requests, trace)
	require.NoError(t, first.Err)
	require.Equal(t, "role-before", first.Model)
	require.EqualValues(t, 111, first.MaxTokens)
	require.NotNil(t, first.Temperature)
	require.Equal(t, 0.25, *first.Temperature)
	require.NotEmpty(t, first.System, "the provider request records the prompt pinned to this in-flight call")

	rootContracts, ok := h.app.Sessions.(session.SessionRootRoleContractStore)
	require.True(t, ok, "root custom-role admission must be durable")
	logicalID, admitted, found, err := rootContracts.GetRootRoleContract(t.Context(), sess.ID)
	require.NoError(t, err)
	require.True(t, found)
	require.NotEmpty(t, logicalID)
	require.Equal(t, schedulerCustomRoleName, admitted.Name)
	require.Equal(t, "role-before", admitted.Model.Model)
	require.EqualValues(t, 111, admitted.Model.MaxTokens)
	require.NotNil(t, admitted.Model.Temperature)
	require.Equal(t, 0.25, *admitted.Model.Temperature)
	require.Equal(t, []string{"view", "grep"}, *admitted.Tools)
	admittedSnapshot := admitted.Clone()

	newTemperature := 0.75
	require.NoError(t, h.foreign.SetCustomRole(config.ScopeWorkspace, schedulerCustomRoleName, config.CustomRoleUpdate{
		Model: config.SelectedModel{
			Provider:    "openaicompat",
			Model:       "role-after",
			MaxTokens:   222,
			Temperature: &newTemperature,
		},
	}))
	active := config.ActiveConfigFileName(filepath.Join(h.dataDir, "rush.json"))
	diskRole := readSchedulerCustomRoleFromKtav(t, active)
	require.Equal(t, "role-after", diskRole.Model)
	require.EqualValues(t, 222, diskRole.MaxTokens)
	require.NotNil(t, diskRole.Tools)
	require.Equal(t, []string{"view", "grep"}, *diskRole.Tools, "model-only supported writes retain the durable tool ceiling")

	require.Eventually(t, func() bool {
		cfg, generation := h.store.Snapshot()
		role, exists := cfg.Roles[schedulerCustomRoleName]
		return exists && role.Model == "role-after" && generation > initialGeneration
	}, 30*time.Second, 10*time.Millisecond, "the App-owned background scheduler must publish the foreign role write")

	_, stillAdmitted, found, err := rootContracts.GetRootRoleContract(t.Context(), sess.ID)
	require.NoError(t, err)
	require.True(t, found)
	require.True(t, admittedSnapshot.Equal(stillAdmitted), "refresh cannot widen or replace an already admitted root contract")
	require.Equal(t, "role-before", first.Model, "the held provider request keeps its original model")
	require.EqualValues(t, 111, first.MaxTokens, "the held request keeps its original output ceiling")
	require.Equal(t, 0.25, *first.Temperature, "the held request keeps its original sampling option")
	stored, err := h.app.Sessions.Get(t.Context(), sess.ID)
	require.NoError(t, err)
	require.Equal(t, first.System, stored.SystemPrompt, "the in-flight prompt remains the prompt admitted before publication")

	firstGate.Release()
	completed := waitCustomRoleSchedulerRun(t, firstOutcome, trace)
	require.NoError(t, completed.err)
	require.NotNil(t, completed.result)
	activeRun.clearRun()

	secondGate := newSchedulerRoleGate(t)
	secondGate.Release()
	h.gates <- secondGate
	secondOutcome := startCustomRoleSchedulerRun(t, h.app, h.lifecycleCtx, sess, "next role request")
	activeRun.setRun(&secondOutcome)
	second := waitCustomRoleSchedulerRequest(t, h.requests, trace)
	require.NoError(t, second.Err)
	require.Equal(t, "role-after", second.Model, "the next ExecuteRun must resolve the scheduler-published role")
	require.EqualValues(t, 222, second.MaxTokens)
	require.NotNil(t, second.Temperature)
	require.Equal(t, 0.75, *second.Temperature)
	require.Equal(t, first.System, second.System, "the same session continues using its stored system prompt after the model refresh")
	completed = waitCustomRoleSchedulerRun(t, secondOutcome, trace)
	require.NoError(t, completed.err)
	require.NotNil(t, completed.result)
	activeRun.clearRun()

	_, latestAdmission, found, err := rootContracts.GetRootRoleContract(t.Context(), sess.ID)
	require.NoError(t, err)
	require.True(t, found)
	require.Equal(t, "role-after", latestAdmission.Model.Model)
	require.EqualValues(t, 222, latestAdmission.Model.MaxTokens)
	require.NotNil(t, latestAdmission.Tools)
	require.Equal(t, []string{"view", "grep"}, *latestAdmission.Tools, "new admission retains the original immutable tool ceiling")

	beforeDeleteGeneration := h.store.Generation()
	require.NoError(t, h.foreign.RemoveCustomRole(config.ScopeWorkspace, schedulerCustomRoleName))
	require.NotContains(t, readSchedulerCustomRolesFromKtav(t, active), schedulerCustomRoleName, "the foreign deletion must be persisted in the active Ktav document")
	require.Eventually(t, func() bool {
		cfg, generation := h.store.Snapshot()
		_, exists := cfg.Roles[schedulerCustomRoleName]
		return !exists && generation > beforeDeleteGeneration
	}, 30*time.Second, 10*time.Millisecond, "the scheduler must publish role deletion before the next admission")
	beforeSessions, err := h.app.Sessions.ListAll(h.lifecycleCtx)
	require.NoError(t, err)
	_, err = h.app.ExecuteRun(h.lifecycleCtx, RunRequest{
		Prompt:      "must be rejected before session creation",
		Overrides:   RunOverrides{RequestedRole: schedulerCustomRoleName},
		Mode:        RunModeJSON,
		Stdout:      io.Discard,
		Stderr:      io.Discard,
		HideSpinner: true,
	})
	require.ErrorContains(t, err, schedulerCustomRoleName)
	afterSessions, err := h.app.Sessions.ListAll(h.lifecycleCtx)
	require.NoError(t, err)
	require.Len(t, afterSessions, len(beforeSessions), "a deleted role must refuse before creating a session")
	require.Empty(t, h.requests, "a deleted role must refuse before reaching the provider")
	require.NoError(t, h.handlerDiagnostics.err())
}
