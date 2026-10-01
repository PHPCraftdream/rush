// The one per-process "process start" log record. Its purpose is
// attribution: when several rush processes append to one shared
// logs/rush.log, each process announces itself once with the facts
// needed to group the remaining lines by origin (pid alone links them,
// ws groups by checkout).
package log

import (
	"log/slog"
	"os"
	"path/filepath"
	"sync"

	"github.com/PHPCraftdream/rush/internal/version"
)

var startOnce sync.Once

// ProcessStart emits the single INFO "process start" record via
// slog.Default(). It is a no-op after the first call in a process, so
// every entry point (setupApp, setupAppLite, the SDK) can call it
// unconditionally.
//
// command is the CLI command path (e.g. "rush run") or an SDK
// identifier; session is the --session value when one was given, ""
// otherwise. argv is deliberately not logged: for `rush run` it
// contains the user's prompt. dev_build is deliberately absent for now
// — the binary has no dev-build marker yet (planned separately), and
// an always-false field would be noise.
func ProcessStart(command, session string) {
	startOnce.Do(func() {
		attrs := []any{
			"pid", os.Getpid(),
			"ppid", os.Getppid(),
			"version", version.VersionLine(),
			"command", command,
			"cwd", cwdOr(""),
			"ws", WorkspaceRoot(),
			"branch", currentBranch(),
			"session", session,
		}
		if dd := dataDirFromLogDir(); dd != "" {
			attrs = append(attrs, "data_dir", dd)
		}
		slog.Info("process start", attrs...)
	})
}

// dataDirFromLogDir recovers the data directory from where Setup put
// the log file: <dataDir>/logs/rush.log means dataDir is the parent of
// the stored logs directory. Empty when logging was set up without a
// file.
func dataDirFromLogDir() string {
	dir, ok := logDir.Load().(string)
	if !ok || dir == "" {
		return ""
	}
	return filepath.Dir(dir)
}
