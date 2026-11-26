package lockwrap

import (
	"context"
	"io"
	"os"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/PHPCraftdream/rush/internal/agent/safety/permission"
	"github.com/PHPCraftdream/rush/internal/kit/sys/machinelock"
	"github.com/PHPCraftdream/rush/internal/kit/sys/shell"
)

var lockWrapPatterns struct {
	sync.RWMutex
	patterns []string
}

// SetLockWrapPatterns configures command patterns from application wiring.
func SetLockWrapPatterns(patterns []string) {
	lockWrapPatterns.Lock()
	lockWrapPatterns.patterns = append([]string(nil), patterns...)
	lockWrapPatterns.Unlock()
}

func CurrentLockWrapPatterns() []string {
	lockWrapPatterns.RLock()
	defer lockWrapPatterns.RUnlock()
	return append([]string(nil), lockWrapPatterns.patterns...)
}

func runLocked(ctx context.Context, name string, record machinelock.Record, argv []string, stdout, stderr *os.File) (int, error) {
	return machinelock.RunWithOptions(ctx, name, record, machinelock.Options{
		Wait: 2 * time.Hour, Timeout: 0, Idle: 10 * time.Minute, OnStuck: machinelock.StuckWait,
	}, argv, stdout, stderr)
}

func RunLockedCommand(ctx context.Context, name, rawCommand, checkout string, argv []string) (string, string, int, error) {
	out, err := os.CreateTemp("", "rush-lock-stdout-*")
	if err != nil {
		return "", "", 1, err
	}
	defer os.Remove(out.Name())
	defer out.Close()
	errFile, err := os.CreateTemp("", "rush-lock-stderr-*")
	if err != nil {
		return "", "", 1, err
	}
	defer os.Remove(errFile.Name())
	defer errFile.Close()
	code, runErr := runLocked(ctx, name, machinelock.Record{Command: rawCommand, Checkout: checkout}, argv, out, errFile)
	if _, err := out.Seek(0, 0); err != nil {
		return "", "", 1, err
	}
	if _, err := errFile.Seek(0, 0); err != nil {
		return "", "", 1, err
	}
	stdout, readErr := io.ReadAll(out)
	if readErr != nil {
		return "", "", 1, readErr
	}
	stderr, readErr := io.ReadAll(errFile)
	if readErr != nil {
		return "", "", 1, readErr
	}
	return string(stdout), string(stderr), code, runErr
}

// lockWrapMatch returns whether an eligible command matches a lock_wrap pattern
// and the stable machine-wide lock name derived from that pattern.
func lockWrapMatch(patterns []string, command string, background, retained bool) (bool, string) {
	if background || retained || strings.TrimSpace(command) == "" || shell.IsCompoundCommand(command) {
		return false, ""
	}
	for _, pattern := range patterns {
		if !permission.MatchBashCommand([]string{pattern}, command) {
			continue
		}
		name := strings.TrimPrefix(strings.TrimPrefix(strings.TrimPrefix(pattern, "exact:"), "glob:"), "regex:")
		name = strings.ToLower(name)
		name = regexp.MustCompile(`[^a-z0-9]+`).ReplaceAllString(name, "-")
		name = strings.Trim(name, "-")
		if name == "" {
			continue
		}
		return true, name
	}
	return false, ""
}

// LockWrapDecision exposes the command matching decision for focused tests.
func Decision(patterns []string, command string, background, retained bool) (bool, string) {
	return lockWrapMatch(patterns, command, background, retained)
}
