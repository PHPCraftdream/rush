package verifystatic

// Native port of .githooks/check_db_release_pairing.sh: every tracked
// *_test.go file calling db.Connect/db.ConnectRead must carry at least
// as many db.Release/db.ReleaseAll/db.ReleaseConn occurrences in the
// same file. Same-file text-count heuristic, identical to the shell:
// full-line // comments are stripped first; trailing and /* */ comments
// and string literals are accepted gaps, exactly as disclosed by the
// shell guard.
//
// Failure classes, mirroring the shell exactly:
//   - git grep exit 1 (zero matches) is the legitimate empty case and
//     stays a pass;
//   - git grep itself failing (exit > 1, or a spawn failure) is BROKEN
//     INPUT: the shell prints to stderr and exits 1 there; the port
//     deliberately FAILS CLOSED instead and returns an error verdict
//     the verify runner maps to exit 3 (same tightening as pkg-names),
//     with the first failing excerpt;
//   - fewer releases than connects in one file is an ordinary
//     violation (exit 1).

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
)

var (
	dbConnectRe = regexp.MustCompile(`db\.(Connect|ConnectRead)\(`)
	dbReleaseRe = regexp.MustCompile(`db\.(Release|ReleaseAll|ReleaseConn)\(`)
)

// dbPairExemptions ports the shell's per-file allow-list. Each entry
// carries its documented reason; a blanket *.go exemption would hide a
// genuine reintroduction in a different file.
var dbPairExemptions = map[string]string{
	"internal/app/shutdown_result_test.go":                       "ShutdownWithResult (production file this guard never scans) calls db.Release internally on the graceful path; verified balanced by hand",
	"internal/app/app_broker_shutdown_test.go":                   "hands its connection to App.Shutdown, whose production lifecycle owns the matching db.Release",
	"internal/app/app_heartbeat_test.go":                         "hands its connection to App.Shutdown",
	"internal/app/app_run_workspace_test.go":                     "hands its connection to App.Shutdown",
	"internal/app/app_new_skip_agent_setup_test.go":              "hands its connection to App.Shutdown",
	"internal/app/app_run_admission_race_test.go":                "hands its connection to App.Shutdown",
	"internal/app/app_finalization_e2e_harness_test.go":          "hands its connection to App.Shutdown",
	"internal/app/app_run_json_envelope_golden_test.go":          "hands its connection to App.Shutdown",
	"internal/app/app_run_lockbusy_firstturn_test.go":            "hands its connection to App.Shutdown",
	"internal/app/app_wake_schedule_wiring_test.go":              "hands its connection to App.Shutdown",
	"internal/app/async_phase4_recovery_scenarios_test.go":       "hands its connection to App.Shutdown",
	"internal/app/app_run_toolgate_restore_test.go":              "hands its connection to App.Shutdown",
	"internal/app/p348_p0_1_ordering_race_test.go":               "hands its connection to App.Shutdown",
	"internal/app/p348_p0_1_pump_coordinator_wiring_test.go":     "hands its connection to App.Shutdown",
	"internal/app/p350_coder_agent_selfheal_test.go":             "hands its connection to App.Shutdown",
	"internal/app/p421_p0_1_interrupt_live_continuation_test.go": "hands its connection to App.Shutdown",
}

// RunDbPair ports check_db_release_pairing.sh.
func RunDbPair(root string, paths []string, invoke InvokeFunc) Result {
	started := time.Now()
	r := Result{Name: "db-release-pairing", Status: "pass"}
	// trackedTests is the guard's universe: every tracked *_test.go
	// file. The envelope's examined-files count for this guard reports
	// exactly this number (the same field slice 1 uses for examined
	// files), so a checkout with zero tracked tests is visible as
	// "examined nothing" and fails closed in the verify runner.
	trackedTests := 0
	for _, p := range paths {
		if strings.HasSuffix(p, "_test.go") && strings.HasSuffix(p, ".go") {
			trackedTests++
		}
	}
	out, err := invoke(root, nil, "git", "grep", "-l", "-E", `db\.(Connect|ConnectRead)\(`, "--", "*_test.go")
	var candidates []string
	if err != nil {
		code := -1
		var ec interface{ ExitCode() int }
		if errors.As(err, &ec) {
			code = ec.ExitCode()
		}
		// Deliberate divergence from the shell: shell exits 1 here; the
		// port fails closed with 3 (an error verdict the verify runner
		// maps to exit 3), so a broken candidate listing can never
		// masquerade as a clean pass.
		if code != 1 {
			msg := fmt.Sprintf("git grep failed (exit %d) -- the db-pairing guard failed closed, refusing to report a false pass", code)
			r.Status = "error"
			r.Violations = 1
			r.Excerpt = boundedExcerpt(msg)
			r.Summary = "db-pairing guard: git grep failed; the guard failed closed with exit 3"
			r.Directories = 0
			r.Files = trackedTests
			r.DurationMS = time.Since(started).Milliseconds()
			return r
		}
	}
	for _, line := range strings.Split(strings.TrimSuffix(string(out), "\n"), "\n") {
		line = strings.TrimSuffix(line, "\r")
		if line != "" {
			candidates = append(candidates, line)
		}
	}
	sort.Strings(candidates)
	var violations []string
	for _, file := range candidates {
		if _, ok := dbPairExemptions[file]; ok {
			continue
		}
		data, e := os.ReadFile(filepath.Join(root, filepath.FromSlash(file)))
		if e != nil {
			// The shell's per-file grep silently degrades to empty
			// content (0 connects / 0 releases); mirror that.
			data = nil
		}
		connects, releases := 0, 0
		for _, ln := range strings.Split(string(data), "\n") {
			if strings.HasPrefix(strings.TrimLeft(ln, " \t"), "//") {
				continue
			}
			connects += len(dbConnectRe.FindAllString(ln, -1))
			releases += len(dbReleaseRe.FindAllString(ln, -1))
		}
		if releases < connects {
			violations = append(violations, fmt.Sprintf("  %s: %d db.Connect/ConnectRead call(s), %d db.Release/ReleaseAll/ReleaseConn call(s)", file, connects, releases))
		}
	}
	dirs := map[string]bool{}
	for _, f := range candidates {
		dirs[pathDir(f)] = true
	}
	r.Violations = len(violations)
	r.Messages = violations
	if len(violations) > 0 {
		r.Status = "fail"
		r.Excerpt = boundedExcerpt("unpaired db.Connect/db.ConnectRead found (ref-counted pool contract, see internal/store/db/dbconn/connect.go):\n" + strings.Join(violations, "\n"))
	}
	r.Directories = len(dirs)
	r.Files = trackedTests
	r.Summary = fmt.Sprintf("db-pairing guard: %d tracked test file(s), %d candidate file(s) examined, %d unpaired", trackedTests, len(candidates), len(violations))
	r.DurationMS = time.Since(started).Milliseconds()
	return r
}
