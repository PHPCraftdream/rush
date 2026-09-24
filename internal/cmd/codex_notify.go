package cmd

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"
	"unicode"

	"github.com/PHPCraftdream/rush/internal/app"
	"github.com/PHPCraftdream/rush/internal/platform"
	"github.com/PHPCraftdream/rush/internal/session"
)

const codexNotifyTimeout = 8 * time.Second

type codexCommand struct {
	path string
	args []string
}

var (
	codexNotifyExec         = executeCodexNotify
	codexCompletionNotifier = deliverCodexCompletion
)

func notifyCodexRunCompletion(threadID, sessionID string, result *app.RunResult, runErr error, panicValue any) error {
	if panicValue != nil {
		runErr = fmt.Errorf("panic: %v", panicValue)
	}
	return codexCompletionNotifier(threadID, buildCodexCompletionMessage(sessionID, result, runErr))
}

func buildCodexCompletionMessage(requestedSession string, result *app.RunResult, runErr error) string {
	sessionID := "unstarted"
	if requestedSession != "" {
		sessionID = requestedSession
	}
	if result != nil && result.SessionID != "" {
		sessionID = result.SessionID
	}
	marker := "rush_done:" + safeSessionToken(sessionID)
	sessionDisplay := "Session: " + codexMessageFragment(sessionID, 120)

	if result == nil {
		if runErr == nil {
			return marker + "\n" + sessionDisplay + "\nRun finished without a result."
		}
		return marker + "\n" + sessionDisplay + "\nRun failed before a result was produced: " + codexMessageFragment(runErr.Error(), 240)
	}

	reason := result.ExitReason
	switch reason {
	case "awaiting_answer":
		return marker + "\n" + sessionDisplay + "\nRun is awaiting an answer: " + codexMessageFragment(firstNonEmpty(result.Error, result.FinalText), 240)
	case "queued":
		return marker + "\n" + sessionDisplay + "\nRun was queued; this CLI attempt did not execute an agent turn."
	case "canceled":
		return marker + "\n" + sessionDisplay + "\nRun was canceled: " + codexMessageFragment(firstNonEmpty(result.Error, errorString(runErr)), 240)
	case "error", "invalid_json":
		return marker + "\n" + sessionDisplay + "\nRun failed (" + reason + "): " + codexMessageFragment(firstNonEmpty(result.Error, errorString(runErr)), 240)
	default:
		if runErr != nil {
			return marker + "\n" + sessionDisplay + "\nRun failed: " + codexMessageFragment(runErr.Error(), 240)
		}
		return marker + "\n" + sessionDisplay + "\nRun completed (" + reason + "): " + codexMessageFragment(result.FinalText, 600)
	}
}

func safeSessionToken(sessionID string) string {
	var b strings.Builder
	for _, r := range sessionID {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || strings.ContainsRune("._-", r) {
			b.WriteRune(r)
		} else {
			b.WriteByte('_')
		}
		if b.Len() >= 80 {
			break
		}
	}
	if b.Len() == 0 {
		return "unstarted"
	}
	return b.String()
}

func codexMessageFragment(value string, maxRunes int) string {
	var b strings.Builder
	space := false
	for _, r := range value {
		if unicode.IsControl(r) || unicode.Is(unicode.Cf, r) || unicode.IsSpace(r) {
			space = b.Len() > 0
			continue
		}
		if space {
			b.WriteByte(' ')
			space = false
		}
		b.WriteRune(r)
	}
	value = b.String()
	runes := []rune(value)
	if len(runes) > maxRunes {
		value = string(runes[:maxRunes]) + "…"
	}
	if strings.TrimSpace(value) == "" {
		return "(no details provided)"
	}
	return value
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}

func errorString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

func deliverCodexCompletion(threadID, message string) error {
	ctx, cancel := context.WithTimeout(context.Background(), codexNotifyTimeout)
	defer cancel()

	return sendCodexQueueMessage(ctx, runtime.GOOS, threadID, message, exec.LookPath, os.Stat, codexNotifyExec)
}

func sendCodexQueueMessage(ctx context.Context, goos, threadID, message string, lookPath func(string) (string, error), stat func(string) (os.FileInfo, error), execute func(context.Context, string, []string) error) error {
	command, err := resolveCodexQueueCommand(goos, lookPath, stat)
	if err == nil {
		args := codexQueueArgs(command.args, threadID, message)
		err = execute(ctx, command.path, args)
	}
	return err
}

func codexQueueArgs(prefix []string, threadID, message string) []string {
	args := append([]string(nil), prefix...)
	return append(args, "queue", "--thread", threadID, "--message", message)
}

func resolveCodexQueueCommand(goos string, lookPath func(string) (string, error), stat func(string) (os.FileInfo, error)) (codexCommand, error) {
	if goos != "windows" {
		path, err := lookPath("codex")
		if err != nil {
			return codexCommand{}, fmt.Errorf("find codex executable: %w", err)
		}
		return codexCommand{path: path}, nil
	}

	if path, err := lookPath("codex.exe"); err == nil {
		return codexCommand{path: path}, nil
	}
	shim, err := lookPath("codex.cmd")
	if err != nil {
		return codexCommand{}, errors.New("find codex.exe or a safe @openai/codex npm installation on PATH")
	}
	base := filepath.Dir(shim)
	candidates := []string{
		filepath.Join(base, "node_modules", "@openai", "codex", "bin", "codex.js"),
		filepath.Join(filepath.Dir(base), "@openai", "codex", "bin", "codex.js"),
	}
	var script string
	for _, candidate := range candidates {
		if _, err := stat(candidate); err == nil {
			script = candidate
			break
		}
	}
	if script == "" {
		return codexCommand{}, fmt.Errorf("codex.cmd was found at %q, but its @openai/codex/bin/codex.js target was not found", shim)
	}
	node, err := lookPath("node.exe")
	if err != nil {
		node, err = lookPath("node")
	}
	if err != nil {
		return codexCommand{}, fmt.Errorf("find node.exe for Codex npm installation: %w", err)
	}
	return codexCommand{path: node, args: []string{script}}, nil
}

func executeCodexNotify(ctx context.Context, path string, args []string) error {
	command := platform.Command(ctx, path, args...)
	configureCodexChild(command)
	command.Cancel = func() error {
		if command.Process == nil {
			return os.ErrProcessDone
		}
		return session.KillProcess(command.Process.Pid)
	}
	var stdout, stderr bytes.Buffer
	command.Stdout = &stdout
	command.Stderr = &stderr
	if err := command.Start(); err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}
	_ = session.TrackProcessTree(command.Process.Pid)
	err := command.Wait()
	session.UntrackProcessTree(command.Process.Pid)
	if err != nil {
		details := codexMessageFragment(stdout.String()+" "+stderr.String(), 300)
		if details == "(no details provided)" {
			return fmt.Errorf("%s: %w", path, err)
		}
		return fmt.Errorf("%s: %w: %s", path, err, details)
	}
	return nil
}
