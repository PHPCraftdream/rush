package app

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/PHPCraftdream/rush/internal/agent"
	"github.com/PHPCraftdream/rush/internal/agent/tools/mcp"
	"github.com/PHPCraftdream/rush/internal/config"
	"github.com/PHPCraftdream/rush/internal/db"
	"github.com/PHPCraftdream/rush/internal/message"
	"github.com/PHPCraftdream/rush/internal/platform"
	"github.com/stretchr/testify/require"
)

const (
	pcrChildEnv    = "PCR_PHASE4_CRASH_CHILD"
	pcrDataEnv     = "PCR_PHASE4_DATA_DIR"
	pcrSessionEnv  = "PCR_PHASE4_SESSION_ID"
	pcrProviderEnv = "PCR_PHASE4_PROVIDER_URL"
	pcrReadyEnv    = "PCR_PHASE4_READY_FILE"
)

type pcrCrashProcess struct {
	kill func() error
	done <-chan struct{}
}

func (process *pcrCrashProcess) pcrKillAndWait() error {
	if err := process.kill(); err != nil {
		return err
	}
	<-process.done
	return nil
}

func pcrRunCrashScenario(t *testing.T) {
	if os.Getenv(pcrChildEnv) == "1" {
		pcrCrashChild(t)
		return
	}
	dataDir := t.TempDir()
	t.Cleanup(func() { require.NoError(t, os.RemoveAll(dataDir)) })
	isolation := t.TempDir()
	t.Setenv("XDG_DATA_HOME", isolation)
	t.Setenv("RUSH_GLOBAL_DATA", isolation)
	configDir := filepath.Join(isolation, "config")
	require.NoError(t, os.MkdirAll(configDir, 0o755))
	t.Setenv("XDG_CONFIG_HOME", configDir)
	t.Setenv("RUSH_GLOBAL_CONFIG", configDir)
	t.Setenv("RUSH_PROVIDER_CACHE_ONLY", "1")
	jobCommand, jobRelease := recoveryJobCommand(t)
	readyPath := filepath.Join(t.TempDir(), "ready")
	var requestsA atomic.Int32
	handlerA := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		_, _, lastTool := lastTurnParts(body)
		switch requestsA.Add(1) {
		case 1:
			admissionWriteSSE(w, []string{admissionSSEToolCall("pcr-start", "call-1", "bash", `{"command":`+jsonString(jobCommand)+`,"description":"long job"}`), admissionSSEStop("pcr-start", "tool_calls")})
		case 2:
			if !pp4StartedSeen(lastTool, "call-1") {
				t.Errorf("missing started result: %q", lastTool)
				http.Error(w, "missing started result", http.StatusBadRequest)
				return
			}
			admissionWriteSSE(w, []string{admissionSSEText("pcr-yield", "root yielded"), admissionSSEStop("pcr-yield", "stop")})
		default:
			t.Errorf("unexpected A request %d", requestsA.Load())
			http.Error(w, "unexpected request", http.StatusBadRequest)
		}
	})
	var requestsB atomic.Int32
	var sawInterrupted atomic.Bool
	handlerB := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if requestsB.Add(1) != 1 || !strings.Contains(string(body), "was interrupted") {
			http.Error(w, "missing first-turn interruption", http.StatusBadRequest)
			return
		}
		sawInterrupted.Store(true)
		admissionWriteSSE(w, []string{admissionSSEText("pcr-continue", "continuing"), admissionSSEStop("pcr-continue", "stop")})
	})
	srvA, srvB := httptest.NewServer(handlerA), httptest.NewServer(handlerB)
	t.Cleanup(srvA.Close)
	t.Cleanup(srvB.Close)
	seedStore, err := config.Init(dataDir, dataDir, false)
	require.NoError(t, err)
	seedConn, err := db.Connect(context.Background(), dataDir)
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.ReleaseConn(seedConn) })
	seedApp, err := New(context.Background(), seedConn, seedStore)
	require.NoError(t, err)
	t.Cleanup(seedApp.Shutdown)
	sess, err := seedApp.Sessions.Create(context.Background(), "phase4-crash")
	require.NoError(t, err)
	_, err = seedApp.Messages.Create(context.Background(), sess.ID, message.CreateMessageParams{Role: message.User, Parts: []message.ContentPart{message.TextContent{Text: "seed"}}})
	require.NoError(t, err)
	seedResult := seedApp.ShutdownWithResult()
	require.False(t, seedResult.Forced)
	require.Empty(t, seedResult.CleanupErrors)

	timeoutCtx, timeoutCancel := context.WithTimeout(context.Background(), 90*time.Second)
	t.Cleanup(timeoutCancel)
	cmd := platform.Command(timeoutCtx, os.Args[0], "-test.run=^TestTwoAppScenarioA_CrashThenContinueDeliversOneInterrupted$")
	cmd.Env = append(os.Environ(), pcrChildEnv+"=1", pcrDataEnv+"="+dataDir, pcrSessionEnv+"="+sess.ID, pcrProviderEnv+"="+srvA.URL, pcrReadyEnv+"="+readyPath, "RUSH_BATCH_RUNTIME=true")
	cmd.Stdout, cmd.Stderr = io.Discard, io.Discard
	require.NoError(t, cmd.Start())
	done := make(chan struct{})
	var waitErr error
	go func() { waitErr = cmd.Wait(); close(done) }()
	var killOnce sync.Once
	var killErr error
	process := &pcrCrashProcess{done: done}
	process.kill = func() error {
		killOnce.Do(func() {
			if runtime.GOOS == "windows" {
				killCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
				defer cancel()
				kill := platform.Command(killCtx, "taskkill", "/F", "/T", "/PID", strconv.Itoa(cmd.Process.Pid))
				killErr = kill.Run()
			} else {
				killErr = cmd.Process.Kill()
			}
		})
		return killErr
	}
	t.Cleanup(func() {
		if !pcrProcessDone(done) {
			if err := process.kill(); err != nil {
				t.Errorf("kill child process: %v", err)
			}
		}
		<-done
		releaseRecoveryJob(t, jobRelease)
	})
	require.Eventually(t, func() bool { _, err := os.Stat(readyPath); return err == nil }, 60*time.Second, 20*time.Millisecond)
	require.EqualValues(t, 2, requestsA.Load())
	pp4CrashSimulate(t, nil, process)
	require.Error(t, waitErr)
	configB, err := config.Init(dataDir, dataDir, false)
	require.NoError(t, err)
	configureRecoveryProbeProvider(configB, srvB.URL)
	connB, err := db.Connect(context.Background(), dataDir)
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.ReleaseConn(connB) })
	owner, err := mcp.AcquireStandalone()
	require.NoError(t, err)
	appB, err := New(context.Background(), connB, configB, WithMCPOwner(owner))
	require.NoError(t, err)
	t.Cleanup(func() {
		shutdown := appB.ShutdownWithResult()
		require.False(t, shutdown.Forced)
		require.Empty(t, shutdown.CleanupErrors)
	})
	agent.SetInlineWindowForTest(appB.AgentCoordinator, false)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	result, err := appB.RunNonInteractiveWithResult(ctx, io.Discard, "continue please", RunOverrides{Origin: message.OriginCLI}, true, RunModeJSON, sess.ID, false)
	require.NoError(t, err)
	require.NotNil(t, result)
	require.Equal(t, "end_turn", result.ExitReason)
	require.EqualValues(t, 1, requestsB.Load())
	require.True(t, sawInterrupted.Load())
	fact := pp4JobFactByCall(t, appB, sess.ID, "call-1")
	require.Equal(t, "interrupted", fact.State)
	require.Equal(t, "host_lost", fact.OutcomeCode)
	require.EqualValues(t, 0, fact.Wake)
	require.Equal(t, 1, pp4SelectedNoticeCount(t, appB, sess.ID))
}

func pcrCrashChild(t *testing.T) {
	dataDir, sessionID := os.Getenv(pcrDataEnv), os.Getenv(pcrSessionEnv)
	store, err := config.Init(dataDir, dataDir, false)
	require.NoError(t, err)
	configureRecoveryProbeProvider(store, os.Getenv(pcrProviderEnv))
	conn, err := db.Connect(context.Background(), dataDir)
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.ReleaseConn(conn) })
	owner, err := mcp.AcquireStandalone()
	require.NoError(t, err)
	application, err := New(context.Background(), conn, store, WithMCPOwner(owner))
	require.NoError(t, err)
	agent.SetInlineWindowForTest(application.AgentCoordinator, false)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	_, err = application.ExecuteRun(ctx, RunRequest{Prompt: "start a long job", Origin: message.OriginCLI, Overrides: RunOverrides{Origin: message.OriginCLI}, Mode: RunModeJSON, ContinueSessionID: sessionID, Stdout: io.Discard, Stderr: io.Discard, HideSpinner: true})
	require.NoError(t, err)
	fact, ok := pp4JobFactRead(application, sessionID, "call-1")
	require.True(t, ok)
	require.True(t, fact.Found)
	require.Equal(t, "running", fact.State)
	require.EqualValues(t, 1, fact.Announced)
	require.Equal(t, pp4DriverHostID(t, application), fact.HostID)
	require.NoError(t, os.WriteFile(os.Getenv(pcrReadyEnv), nil, 0o600))
	<-make(chan struct{})
}

func pcrProcessDone(done <-chan struct{}) bool {
	select {
	case <-done:
		return true
	default:
		return false
	}
}
