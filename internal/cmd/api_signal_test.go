package cmd

import (
	"bytes"
	"context"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/PHPCraftdream/rush/internal/config"
)

// interruptHarness replaces the signal seam so a test can deliver "Ctrl+C":
// os.Interrupt cannot be sent to a Windows test process. The production
// derivation (ctx cancelled while the parent is alive) is what is exercised.
func interruptHarness(t *testing.T) (interrupt func()) {
	t.Helper()
	cancels := make(chan context.CancelFunc, 1)
	previous := apiNotifyContext
	apiNotifyContext = func(parent context.Context, _ ...os.Signal) (context.Context, context.CancelFunc) {
		ctx, cancel := context.WithCancel(parent)
		cancels <- cancel
		return ctx, cancel
	}
	t.Cleanup(func() { apiNotifyContext = previous })
	return func() {
		select {
		case cancel := <-cancels:
			cancel()
		case <-time.After(10 * time.Second):
			t.Error("the command never installed its signal context")
		}
	}
}

// runInProcess executes `rush api <args>` against the fake with a profile
// stored in the isolated global config and returns the mapped exit code.
func runInProcess(t *testing.T, fake *fakeRunServer, args ...string) (code int, stdout, stderr string) {
	t.Helper()
	newAPIProcessFixture(t)
	t.Setenv("RUSH_FAULT_TOKEN", apiFaultToken)
	if err := config.SetAPIRemote("fault", config.APIRemote{URL: fake.URL, TokenEnv: "RUSH_FAULT_TOKEN"}, false); err != nil {
		t.Fatal(err)
	}
	if err := config.SetDefaultAPIRemote("fault"); err != nil {
		t.Fatal(err)
	}
	var out, errOut bytes.Buffer
	cmd := newAPICmd()
	cmd.SetOut(&out)
	cmd.SetErr(&errOut)
	cmd.SetArgs(args)
	err := cmd.Execute()
	code, ok := apiExitCode(err)
	if err == nil {
		code, ok = 0, true
	}
	if !ok {
		t.Fatalf("error without an exit code: %v", err)
	}
	return code, out.String(), errOut.String()
}

func waitForFakeCall(t *testing.T, fake *fakeRunServer, method, suffix string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if len(fake.seen(method, suffix)) > 0 {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("fake never saw %s %s", method, suffix)
}

func TestAPICLIRunInterruptCancelsRemoteRunAndExits130(t *testing.T) {
	for _, args := range [][]string{{"run", "s1", "x"}, {"run", "s1", "x", "--stream"}} {
		fake := newFakeRunServer(t, "running")
		interrupt := interruptHarness(t)
		go func() {
			waitForFakeCall(t, fake, http.MethodGet, "/runs/r1")
			interrupt()
		}()
		code, _, stderr := runInProcess(t, fake, args...)
		if code != 130 {
			t.Fatalf("%v: exit %d, want 130; stderr=%q", args, code, stderr)
		}
		if got := fake.seen(http.MethodPost, "/runs/r1/cancel"); len(got) != 1 {
			t.Fatalf("%v: remote cancel calls = %d, want 1", args, len(got))
		}
	}
}

func TestAPICLIEventsFollowInterruptExits130WithoutCancellingRuns(t *testing.T) {
	fake := newFakeRunServer(t)
	fake.events = []string{sseFrame(1, "message", `{"n":1}`)}
	interrupt := interruptHarness(t)
	go func() {
		waitForFakeCall(t, fake, http.MethodGet, "/sessions/s1/events")
		interrupt()
	}()
	code, stdout, _ := runInProcess(t, fake, "events", "s1", "--follow", "--idle", "100ms")
	if code != 130 {
		t.Fatalf("exit %d, want 130", code)
	}
	if got := fake.seen(http.MethodPost, "/runs/r1/cancel"); len(got) != 0 {
		t.Fatalf("events cancelled a run: %+v", got)
	}
	_ = stdout
}

// TestAPISignalContextDistinguishesSignalFromDeadline pins the contract the
// exit codes rely on: a cancelled parent is never reported as an interrupt.
func TestAPISignalContextDistinguishesSignalFromDeadline(t *testing.T) {
	parent, cancel := context.WithCancel(context.Background())
	ctx, stop, interrupted := apiSignalContext(parent)
	defer stop()
	if interrupted() {
		t.Fatal("interrupted before anything happened")
	}
	cancel()
	<-ctx.Done()
	if interrupted() {
		t.Fatal("a parent cancellation was reported as an interrupt")
	}

}

// TestAPICLIRunStreamEndsOnRunEventNotOnPolling disables the polling fallback
// (an hour) so only the terminal run EVENT can end the stream.
func TestAPICLIRunStreamEndsOnRunEventNotOnPolling(t *testing.T) {
	previous := apiRunStatePollInterval
	apiRunStatePollInterval = time.Hour
	t.Cleanup(func() { apiRunStatePollInterval = previous })
	fake := newFakeRunServer(t, "running", "succeeded")
	fake.events = []string{sseFrame(1, "run", `{"state":"running"}`), sseFrame(2, "run", `{"state":"succeeded"}`)}
	code, stdout, stderr := runInProcess(t, fake, "run", "s1", "x", "--stream")
	if code != 0 || !strings.Contains(stdout, `"state":"succeeded"`) {
		t.Fatalf("exit %d stdout=%q stderr=%q", code, stdout, stderr)
	}
}
