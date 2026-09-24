package cmd

import (
	"errors"
	"strings"
	"testing"

	"github.com/spf13/cobra"
)

const codexNotifyTestThreadID = "c40edc8b-6d48-4e13-8ce8-4e2d0eafed45"

func runEWithCodexThreadFlag(threadID, sessionID string, includeThreadFlag bool) error {
	command := &cobra.Command{}
	command.Flags().String("role", "", "")
	command.Flags().String("session", sessionID, "")
	if includeThreadFlag {
		command.Flags().String("codex-thread-id", threadID, "")
	}
	return runCmd.RunE(command, nil)
}

func TestRunECodexThreadFlagWiring(t *testing.T) {
	tests := []struct {
		name            string
		threadID        string
		includeFlag     bool
		wantCalls       int
		wantThread      string
		wantErrorPhrase string
	}{
		{name: "valid thread notifies on early run error", threadID: codexNotifyTestThreadID, includeFlag: true, wantCalls: 1, wantThread: codexNotifyTestThreadID, wantErrorPhrase: "--role is required"},
		{name: "no flag does not notify", wantCalls: 0, wantErrorPhrase: "--role is required"},
		{name: "invalid UUID fails before queueing", threadID: "not-a-uuid", includeFlag: true, wantCalls: 0, wantErrorPhrase: "--codex-thread-id: expected a UUID"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			calls := 0
			previousNotifier := codexCompletionNotifier
			codexCompletionNotifier = func(threadID, message string) error {
				calls++
				if threadID != tt.wantThread {
					t.Errorf("notification thread id = %q, want %q", threadID, tt.wantThread)
				}
				if !strings.Contains(message, "Run failed") {
					t.Errorf("notification did not describe failure: %q", message)
				}
				return errors.New("simulated delivery failure")
			}
			t.Cleanup(func() { codexCompletionNotifier = previousNotifier })

			err := runEWithCodexThreadFlag(tt.threadID, "requested session", tt.includeFlag)
			if err == nil || !strings.Contains(err.Error(), tt.wantErrorPhrase) {
				t.Fatalf("RunE error = %v, want phrase %q", err, tt.wantErrorPhrase)
			}
			if calls != tt.wantCalls {
				t.Fatalf("notification calls = %d, want %d", calls, tt.wantCalls)
			}
			if tt.wantCalls == 1 && strings.Contains(err.Error(), "simulated delivery failure") {
				t.Fatalf("delivery failure replaced the RunE error: %v", err)
			}
		})
	}
}
