// Package main is the entry point for the Crush CLI, an AI coding assistant
// built for delegation from orchestrators and scripts. The command tree
// lives in internal/cmd; the default `crush` invocation starts the fork's
// TCP-based WebSocket server (internal/server) and opens the web UI: /auth
// and /auth/check issue and verify the session cookie, /ws carries the
// session/agent protocol, and / serves the embedded React build.
package main

import (
	"fmt"
	"log/slog"
	"net/http"
	_ "net/http/pprof"
	"os"

	"github.com/charmbracelet/crush/internal/cmd"
	_ "github.com/charmbracelet/crush/internal/dns"
	"github.com/charmbracelet/crush/internal/platform"
	_ "github.com/joho/godotenv/autoload"
)

func main() {
	// Guard against running from inside the source tree. Package init()
	// functions (godotenv autoload, internal/dns) necessarily run before
	// main(), but everything else — cmd dispatch, job-object assignment, the
	// pprof listener — runs strictly after this check, because a stale dev
	// binary can silently cause incorrect behavior even for --help and
	// --version.
	if err := platform.GuardSourceTreeRun(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}

	if err := platform.AssignToNewJobObject(); err != nil {
		slog.Debug("Job object assignment skipped", "error", err)
	}

	if os.Getenv("CRUSH_PROFILE") != "" {
		go func() {
			slog.Info("Serving pprof at localhost:6060")
			if httpErr := http.ListenAndServe("localhost:6060", nil); httpErr != nil {
				slog.Error("Failed to pprof listen", "error", httpErr)
			}
		}()
	}

	cmd.Execute()
}
