package cmd

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"testing"

	"github.com/PHPCraftdream/rush/internal/app"
)

func TestCodexQueueArgsPreserveMessageAsSingleArgument(t *testing.T) {
	message := `rush_done:s1
Run completed (stop): quotes ' " & % $(echo x) \ Unicode שלום` + "\nsecond line"
	got := codexQueueArgs([]string{"codex-wrapper", "--some-prefix"}, "thread-id", message)
	want := []string{"codex-wrapper", "--some-prefix", "queue", "--thread", "thread-id", "--message", message}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("argv mismatch:\n got: %#v\nwant: %#v", got, want)
	}
}

func TestSendCodexQueueMessageAttemptsExactlyOnceAndDoesNotRewriteFailure(t *testing.T) {
	wantErr := errors.New("codex rejected message")
	calls := 0
	message := `rush_done:s1\nquotes " ' & % $(echo x) \\ שלום`
	err := sendCodexQueueMessage(context.Background(), "linux", "thread", message,
		func(name string) (string, error) {
			if name != "codex" {
				t.Fatalf("unexpected executable lookup %q", name)
			}
			return "/usr/bin/codex", nil
		},
		func(string) (os.FileInfo, error) { return nil, os.ErrNotExist },
		func(_ context.Context, path string, args []string) error {
			calls++
			if path != "/usr/bin/codex" {
				t.Fatalf("unexpected executable path %q", path)
			}
			want := []string{"queue", "--thread", "thread", "--message", message}
			if !reflect.DeepEqual(args, want) {
				t.Fatalf("unexpected argv: got %#v want %#v", args, want)
			}
			return wantErr
		})
	if !errors.Is(err, wantErr) {
		t.Fatalf("expected original delivery error, got %v", err)
	}
	if calls != 1 {
		t.Fatalf("expected one delivery attempt, got %d", calls)
	}
}

func TestBuildCodexCompletionMessage(t *testing.T) {
	longText := strings.Repeat("界", 700)
	tests := []struct {
		name      string
		sessionID string
		result    *app.RunResult
		runErr    error
		want      string
		wantHas   string
	}{
		{name: "success", result: &app.RunResult{SessionID: "s1", ExitReason: "stop", FinalText: "  Done\nwith work.  "}, want: "rush_done:s1\nSession: s1\nRun completed (stop): Done with work."},
		{name: "agent error", result: &app.RunResult{SessionID: "s2", ExitReason: "error", Error: "provider failed"}, want: "rush_done:s2\nSession: s2\nRun failed (error): provider failed"},
		{name: "early failure uses requested session", sessionID: "requested", runErr: errors.New("bad config\nsecret"), want: "rush_done:requested\nSession: requested\nRun failed before a result was produced: bad config secret"},
		{name: "unstarted", runErr: errors.New("setup failed"), want: "rush_done:unstarted\nSession: unstarted\nRun failed before a result was produced: setup failed"},
		{name: "canceled", result: &app.RunResult{SessionID: "s3", ExitReason: "canceled"}, wantHas: "Run was canceled:"},
		{name: "awaiting answer", result: &app.RunResult{SessionID: "s4", ExitReason: "awaiting_answer", FinalText: "Which option?"}, want: "rush_done:s4\nSession: s4\nRun is awaiting an answer: Which option?"},
		{name: "queued", result: &app.RunResult{SessionID: "s5", ExitReason: "queued"}, want: "rush_done:s5\nSession: s5\nRun was queued; this CLI attempt did not execute an agent turn."},
		{name: "empty final text", result: &app.RunResult{SessionID: "s6", ExitReason: "stop"}, want: "rush_done:s6\nSession: s6\nRun completed (stop): (no details provided)"},
		{name: "long final text", result: &app.RunResult{SessionID: "s7", ExitReason: "stop", FinalText: longText}, wantHas: strings.Repeat("界", 600) + "…"},
		{name: "JSON encoding failure", result: &app.RunResult{SessionID: "s8", ExitReason: "stop", FinalText: "result ready"}, runErr: errors.New("failed to encode JSON result"), want: "rush_done:s8\nSession: s8\nRun failed: failed to encode JSON result"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := buildCodexCompletionMessage(tt.sessionID, tt.result, tt.runErr)
			if tt.want != "" && got != tt.want {
				t.Fatalf("message mismatch:\n got: %q\nwant: %q", got, tt.want)
			}
			if tt.wantHas != "" && !strings.Contains(got, tt.wantHas) {
				t.Fatalf("message %q does not contain %q", got, tt.wantHas)
			}
			if strings.ContainsAny(got, "\r\n\t") && strings.Count(got, "\n") != 2 {
				t.Fatalf("message contains unexpected control characters: %q", got)
			}
		})
	}
}

func TestBuildCodexCompletionMessageUsesSafeMarkerToken(t *testing.T) {
	requestedSession := "team A\r\n'\"&%"
	message := buildCodexCompletionMessage(requestedSession, nil, errors.New("setup failed"))
	lines := strings.Split(message, "\n")
	if len(lines) < 3 || !regexp.MustCompile(`^rush_done:[A-Za-z0-9._-]+$`).MatchString(lines[0]) {
		t.Fatalf("marker is not one safe token: %q", lines[0])
	}
	if !strings.Contains(lines[1], `Session: team A '"&%`) {
		t.Fatalf("informative session display was lost: %q", lines[1])
	}
}

func TestNotifyCodexRunCompletionReportsPanicAsFailure(t *testing.T) {
	previous := codexCompletionNotifier
	t.Cleanup(func() { codexCompletionNotifier = previous })
	var calls int
	codexCompletionNotifier = func(threadID, message string) error {
		calls++
		if threadID != "thread" {
			t.Fatalf("unexpected thread ID %q", threadID)
		}
		want := "rush_done:s1\nSession: s1\nRun failed: panic: \"boom\" &"
		if message != want {
			t.Fatalf("panic notification = %q, want %q", message, want)
		}
		return nil
	}

	err := notifyCodexRunCompletion("thread", "s1", &app.RunResult{
		SessionID: "s1", ExitReason: "stop", FinalText: "optimistic success",
	}, nil, `"boom" &`)
	if err != nil || calls != 1 {
		t.Fatalf("notification error = %v, calls = %d; want one successful delivery", err, calls)
	}
}

func TestResolveCodexQueueCommandWindowsPrefersNativeBinary(t *testing.T) {
	lookups := map[string]string{"codex.exe": `C:\tools\codex.exe`}
	got, err := resolveCodexQueueCommand("windows", func(name string) (string, error) {
		if path, ok := lookups[name]; ok {
			return path, nil
		}
		return "", os.ErrNotExist
	}, func(string) (os.FileInfo, error) { return nil, os.ErrNotExist })
	if err != nil {
		t.Fatal(err)
	}
	if got.path != lookups["codex.exe"] || len(got.args) != 0 {
		t.Fatalf("unexpected native command: %#v", got)
	}
}

func TestResolveCodexQueueCommandWindowsRunsNpmScriptWithNode(t *testing.T) {
	root := t.TempDir()
	shimDir := filepath.Join(root, "node_modules", ".bin")
	script := filepath.Join(root, "node_modules", "@openai", "codex", "bin", "codex.js")
	if err := os.MkdirAll(filepath.Dir(script), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(script, []byte(""), 0o600); err != nil {
		t.Fatal(err)
	}
	lookups := map[string]string{
		"codex.cmd": filepath.Join(shimDir, "codex.cmd"),
		"node.exe":  `C:\node\node.exe`,
	}
	got, err := resolveCodexQueueCommand("windows", func(name string) (string, error) {
		if path, ok := lookups[name]; ok {
			return path, nil
		}
		return "", os.ErrNotExist
	}, os.Stat)
	if err != nil {
		t.Fatal(err)
	}
	if got.path != lookups["node.exe"] || !reflect.DeepEqual(got.args, []string{script}) {
		t.Fatalf("unexpected npm command: %#v", got)
	}
}

func TestResolveCodexQueueCommandRequiresSafeWindowsTarget(t *testing.T) {
	shim := filepath.Join(t.TempDir(), "codex.cmd")
	_, err := resolveCodexQueueCommand("windows", func(name string) (string, error) {
		if name == "codex.cmd" {
			return shim, nil
		}
		return "", os.ErrNotExist
	}, func(string) (os.FileInfo, error) { return nil, os.ErrNotExist })
	if err == nil || !strings.Contains(err.Error(), "codex.js target was not found") {
		t.Fatalf("expected missing safe target error, got %v", err)
	}
}
