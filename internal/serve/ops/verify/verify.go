package verify

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/PHPCraftdream/rush/internal/kit/sys/platform"
	static "github.com/PHPCraftdream/rush/internal/serve/ops/verify/static"
)

var errTarget = errors.New("invalid verify target")

type Guard struct {
	Name        string `json:"name"`
	Status      string `json:"status"`
	DurationMS  int64  `json:"duration_ms"`
	ExitCode    int    `json:"exit_code"`
	Directories int    `json:"examined_directories"`
	Files       int    `json:"examined_files"`
	Violations  int    `json:"violations"`
	Summary     string `json:"summary"`
	Excerpt     string `json:"first_failure_excerpt"`
}
type (
	Provenance struct {
		Operation string   `json:"operation"`
		Args      []string `json:"argv"`
		CWD       string   `json:"cwd"`
		ExitCode  int      `json:"exit_code"`
	}
	Stage struct {
		Name       string       `json:"name"`
		Status     string       `json:"status"`
		DurationMS int64        `json:"duration_ms"`
		ExitCode   int          `json:"exit_code"`
		Summary    string       `json:"summary"`
		Excerpt    string       `json:"first_failure_excerpt"`
		Provenance []Provenance `json:"provenance"`
		Guards     []Guard      `json:"guards"`
	}
)

type Envelope struct {
	Target     string  `json:"target"`
	Commit     string  `json:"commit"`
	Base       string  `json:"base"`
	Stages     []Stage `json:"stages"`
	OK         bool    `json:"ok"`
	StartedAt  string  `json:"started_at"`
	FinishedAt string  `json:"finished_at"`
}

const (
	excerptLimit  = 4096
	excerptMarker = "\n…[truncated]"
)

func boundedExcerpt(s string) string {
	if len(s) <= excerptLimit {
		return s
	}
	return s[:excerptLimit-len(excerptMarker)] + excerptMarker
}

func failureEnvelope(target string, err error, exit int) Envelope {
	started := time.Now().UTC().Format(time.RFC3339Nano)
	var provenance []Provenance
	var commandErr *commandError
	if errors.As(err, &commandErr) {
		provenance = commandErr.provenance
	}
	return Envelope{Target: target, Stages: []Stage{{Name: "guards", Status: "error", ExitCode: exit, Summary: "guard stage could not execute", Excerpt: boundedExcerpt(err.Error()), Provenance: provenance, Guards: brokenGuards(err)}}, OK: false, StartedAt: started, FinishedAt: started}
}

type commandError struct {
	message    string
	provenance []Provenance
}

func (e *commandError) Error() string { return e.message }

func Run(ctx context.Context, target string, countTests bool) (Envelope, int, error) {
	runStarted := time.Now().UTC()
	candidate, err := filepath.Abs(target)
	if err != nil {
		return Envelope{}, 2, fmt.Errorf("%w: %v", errTarget, err)
	}
	var provenance []Provenance
	invoke := func(cwd string, args ...string) ([]byte, error) {
		p := Provenance{Operation: strings.Join(args, " "), Args: append([]string(nil), args...), CWD: cwd}
		cmd := platform.Command(ctx, args[0], args[1:]...)
		cmd.Dir = cwd
		out, err := cmd.Output()
		if err != nil {
			var exit *exec.ExitError
			if errors.As(err, &exit) {
				p.ExitCode = exit.ExitCode()
				provenance = append(provenance, p)
			}
			// A spawn error (for example a missing binary) is not evidence that the process started.
		} else {
			p.ExitCode = 0
			provenance = append(provenance, p)
		}
		return out, err
	}
	rootBytes, err := invoke(candidate, "git", "rev-parse", "--show-toplevel")
	if err != nil {
		return Envelope{}, 3, &commandError{message: fmt.Sprintf("not a git work tree: %v", err), provenance: provenance}
	}
	root := strings.TrimSpace(string(rootBytes))
	root, err = filepath.Abs(root)
	if err != nil {
		return Envelope{}, 3, &commandError{message: err.Error(), provenance: provenance}
	}
	if info, e := os.Stat(root); e != nil || !info.IsDir() {
		return Envelope{}, 3, &commandError{message: fmt.Sprintf("git returned invalid repository root %q", root), provenance: provenance}
	}
	commit := "none"
	commitBytes, commitErr := invoke(root, "git", "rev-parse", "HEAD")
	if commitErr == nil && strings.TrimSpace(string(commitBytes)) != "" {
		commit = strings.TrimSpace(string(commitBytes))
	}
	raw, err := invoke(root, "git", "ls-files", "-z")
	if err != nil {
		return Envelope{}, 3, &commandError{message: fmt.Sprintf("git ls-files failed: %v", err), provenance: provenance}
	}
	raw = []byte(strings.TrimSuffix(string(raw), "\x00"))
	paths := []string{}
	if len(raw) > 0 {
		paths = strings.Split(string(raw), "\x00")
	}
	results, guardErr := static.Run(root, paths, countTests)
	env := Envelope{Target: root, Commit: commit, Stages: []Stage{{Name: "guards", Status: "pass", Provenance: provenance}}, StartedAt: runStarted.Format(time.RFC3339Nano)}
	stage := &env.Stages[0]
	stage.Provenance = provenance
	if guardErr != nil {
		stage.Status = "error"
		stage.ExitCode = 3
		stage.Summary = "guard inputs unavailable"
		stage.Excerpt = boundedExcerpt(guardErr.Error())
		stage.Guards = brokenGuards(guardErr)
		env.OK = false
		env.FinishedAt = time.Now().UTC().Format(time.RFC3339Nano)
		return env, 3, nil
	}
	stage.Guards = make([]Guard, 0, len(results))
	code := 0
	for _, r := range results {
		if (r.Name == "dir-entries" || r.Name == "dir-files") && (r.Directories == 0 || r.Files == 0) || strings.HasPrefix(r.Name, "file-size") && r.Files == 0 {
			r.Status = "error"
			r.Violations++
			r.Excerpt = fmt.Sprintf("%s examined no applicable inputs", r.Name)
			code = 3
		}
		gc := 0
		switch r.Status {
		case "fail":
			gc = 1
			if code == 0 {
				code = 1
			}
		case "error":
			gc = 3
		}
		stage.Guards = append(stage.Guards, Guard{Name: r.Name, Status: r.Status, DurationMS: r.DurationMS, ExitCode: gc, Directories: r.Directories, Files: r.Files, Violations: r.Violations, Summary: r.Summary, Excerpt: boundedExcerpt(r.Excerpt)})
	}
	for _, g := range stage.Guards {
		stage.DurationMS += g.DurationMS
		if g.Status != "pass" && stage.Excerpt == "" {
			stage.Excerpt = boundedExcerpt(g.Excerpt)
		}
	}
	stage.ExitCode = code
	switch code {
	case 3:
		stage.Status = "error"
		stage.Summary = "one or more guards examined no applicable inputs"
	case 1:
		stage.Status = "fail"
		stage.Summary = "one or more guards reported violations"
	default:
		stage.Summary = "all four guards passed"
	}
	env.OK = code == 0
	env.FinishedAt = time.Now().UTC().Format(time.RFC3339Nano)
	return env, code, nil
}

func brokenGuards(err error) []Guard {
	out := make([]Guard, 0, 4)
	for _, name := range []string{"dir-entries", "dir-files", "file-size", "file-size-900"} {
		out = append(out, Guard{Name: name, Status: "error", ExitCode: 3, Summary: "guard input error", Excerpt: boundedExcerpt(err.Error())})
	}
	return out
}
func Encode(env Envelope) ([]byte, error) { return json.MarshalIndent(env, "", "  ") }
