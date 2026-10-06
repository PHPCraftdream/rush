package log

import (
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"runtime/debug"
	"sync"
	"sync/atomic"
	"time"

	"github.com/PHPCraftdream/rush/internal/audit"
	"github.com/charmbracelet/x/term"
)

var (
	initOnce    sync.Once
	initialized atomic.Bool
)

// NewLogger builds an independent *slog.Logger wired to the log file
// plus any extra writers (e.g. an IPC pipe), tagging every entry with
// the process PID and the workspace root (ws). It deliberately does NOT call slog.SetDefault: library
// consumers can use it without hijacking the host program's slog.Default().
//
// The v1 boundary: this gives the caller a logger for its own needs, but it
// does not isolate rush's internal logs. The rush core (internal/agent,
// internal/session, and so on) still calls the package-level slog.Info,
// slog.Warn and slog.Error in many places, and those go to whatever the
// CURRENT slog.Default() is — independent of what NewLogger returned. Full
// isolation of core logs is out of scope for v1; use Setup for that.
func NewLogger(logFile string, debug bool, ws ...io.Writer) *slog.Logger {
	// Append-only file writer, shared safely between processes: several
	// rush processes (web server, parallel `rush run`) commonly write to
	// the same logs/rush.log. There is deliberately no in-process
	// rotation: lumberjack's non-appending open truncated other
	// processes' lines, and its rename-based rotation both lost lines on
	// POSIX and permanently disabled logging on Windows while another
	// process held the file. Size is reclaimed externally via
	// `rush logs prune`, whose truncate is safe for O_APPEND writers.
	fileWriter := io.Discard
	if logFile != "" {
		if err := os.MkdirAll(filepath.Dir(logFile), 0o755); err != nil {
			fileWriter = os.Stderr
		} else if afw, err := openAppend(logFile); err == nil {
			// The handle intentionally lives for the whole process.
			fileWriter = afw
		} else {
			// Never silently drop logs: fall back to stderr rather
			// than an unwritable path.
			fileWriter = os.Stderr
		}
	}

	level := slog.LevelInfo
	if debug {
		level = slog.LevelDebug
	}

	opts := &slog.HandlerOptions{
		Level:     level,
		AddSource: true,
	}

	// Tag every entry with this process's PID and its workspace root.
	// When two rush processes share a .rush dir (common with parallel
	// `rush run --session X` orchestration), the log file gets
	// interleaved writes from both. `pid` splits records by process
	// (`jq 'select(.pid==N)'`), `ws` groups them by checkout when
	// several worktrees share the data directory. Both are cheap (one
	// int and one string per log line) and harmless with a single
	// process.
	// `launch_cwd` is the directory the process was started in (audit.LaunchCwd), which can differ from `ws`.
	pid := os.Getpid()
	wsRoot := WorkspaceRoot()
	attrs := []slog.Attr{
		slog.Int("pid", pid),
		slog.String("ws", wsRoot),
		slog.String("launch_cwd", audit.LaunchCwd()),
	}
	var handlers []slog.Handler
	handlers = append(handlers, slog.NewJSONHandler(fileWriter, opts).WithAttrs(attrs))

	for _, w := range ws {
		if w == nil {
			continue
		}
		if f, ok := w.(term.File); ok && term.IsTerminal(f.Fd()) {
			handlers = append(handlers, slog.NewTextHandler(w, opts).WithAttrs(attrs))
		} else {
			handlers = append(handlers, slog.NewJSONHandler(w, opts).WithAttrs(attrs))
		}
	}

	return slog.New(slog.NewMultiHandler(handlers...))
}

func Setup(logFile string, debug bool, ws ...io.Writer) {
	initOnce.Do(func() {
		// Remember where logs live so DumpGoroutines can drop hang dumps
		// next to rush.log without threading config through every caller.
		if logFile != "" {
			logDir.Store(filepath.Dir(logFile))
		}
		slog.SetDefault(NewLogger(logFile, debug, ws...))
		initialized.Store(true)
	})
}

func Initialized() bool {
	return initialized.Load()
}

func RecoverPanic(name string, cleanup func()) {
	if r := recover(); r != nil {
		// Create a timestamped panic log file
		timestamp := time.Now().Format("20060102-150405")
		filename := fmt.Sprintf("rush-panic-%s-%s.log", name, timestamp)

		file, err := os.Create(filename)
		if err == nil {
			defer file.Close()

			// Write panic information and stack trace
			fmt.Fprintf(file, "Panic in %s: %v\n\n", name, r)
			fmt.Fprintf(file, "Time: %s\n\n", time.Now().Format(time.RFC3339))
			fmt.Fprintf(file, "Stack Trace:\n%s\n", debug.Stack())

			// Execute cleanup function if provided
			if cleanup != nil {
				cleanup()
			}
		}
	}
}
