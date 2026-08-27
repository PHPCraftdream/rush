package tools

import (
	"bytes"
	"cmp"
	"context"
	_ "embed"
	"errors"
	"fmt"
	"html/template"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"charm.land/fantasy"
	"github.com/PHPCraftdream/rush/internal/agent/agentguard"
	"github.com/PHPCraftdream/rush/internal/permission"
	"github.com/PHPCraftdream/rush/internal/platform"
	"github.com/PHPCraftdream/rush/internal/shell"
	"mvdan.cc/sh/v3/syntax"
)

// RunCommandToolName is the name of the no-shell program runner tool.
const RunCommandToolName = "run_command"

const (
	// runCommandDefaultTimeoutSeconds is long enough for a normal
	// test/build invocation to finish without the model having to think
	// about timeouts at all.
	runCommandDefaultTimeoutSeconds = 120

	// runCommandMaxTimeoutSeconds is the hard ceiling so a single
	// run_command call can never hang the turn indefinitely.
	runCommandMaxTimeoutSeconds = 600
)

type RunCommandParams struct {
	Program        string   `json:"program" description:"The executable to run, resolved via the normal PATH lookup (e.g. \"go\", \"git\", \"C:/tools/tool.exe\"). Shell builtins (echo, cd, test, set) are NOT available — there is no shell"`
	Args           []string `json:"args,omitempty" description:"Arguments passed to the program EXACTLY as given. There is NO shell: no pipes, no ; or &&, no $( ), no backticks, no $VAR expansion, no glob expansion. If you need an env var's value or a glob expansion, resolve it yourself (read the value, list the files) and pass literal strings"`
	WorkingDir     string   `json:"working_dir,omitempty" description:"Working directory for the program, relative to the current working directory. Defaults to the current working directory. Must stay inside the current working directory"`
	Description    string   `json:"description,omitempty" description:"A brief description of what the command does, try to keep it under 30 characters or so"`
	TimeoutSeconds int      `json:"timeout_seconds,omitempty" description:"Maximum seconds to wait for the program to finish before killing it (default 120, maximum 600)"`
}

type RunCommandPermissionsParams struct {
	Program        string   `json:"program"`
	Args           []string `json:"args"`
	WorkingDir     string   `json:"working_dir"`
	Description    string   `json:"description"`
	TimeoutSeconds int      `json:"timeout_seconds"`
}

// RunAllowlistCommand synthesizes a single shell-like command string from
// program and args so `--allow-bash` patterns written for the bash tool
// match run_command calls the same way (e.g. "go test" matches
// program "go", args ["test", "./..."]). Each argument that is not
// already shell-safe is quoted with mvdan.cc/sh/v3/syntax.Quote
// (LangBash) so metacharacters inside an argument can never make the
// synthesized string parse as a compound command.
func (p RunCommandPermissionsParams) RunAllowlistCommand() string {
	if p.Program == "" {
		return ""
	}
	parts := []string{p.Program}
	for _, arg := range p.Args {
		quoted, err := syntax.Quote(arg, syntax.LangBash)
		if err != nil {
			quoted = strconv.Quote(arg)
		}
		parts = append(parts, quoted)
	}
	return strings.Join(parts, " ")
}

type RunCommandResponseMetadata struct {
	StartTime        int64  `json:"start_time"`
	EndTime          int64  `json:"end_time"`
	Output           string `json:"output"`
	Description      string `json:"description"`
	WorkingDirectory string `json:"working_directory"`
}

//go:embed run_command.md.tpl
var runCommandDescriptionTmpl []byte

var runCommandDescriptionTpl = template.Must(
	template.New("runCommandDescription").
		Parse(string(runCommandDescriptionTmpl)),
)

type runCommandDescriptionData struct {
	BannedCommands  string
	MaxOutputLength int
	DefaultTimeout  int
	MaxTimeout      int
}

func runCommandDescription() string {
	return renderTemplate(runCommandDescriptionTpl, runCommandDescriptionData{
		BannedCommands:  strings.Join(bannedCommands, ", "),
		MaxOutputLength: MaxOutputLength,
		DefaultTimeout:  runCommandDefaultTimeoutSeconds,
		MaxTimeout:      runCommandMaxTimeoutSeconds,
	})
}

// NewRunCommandTool builds the no-shell program runner scoped to workingDir.
func NewRunCommandTool(permissions permission.Service, workingDir string) fantasy.AgentTool {
	return fantasy.NewAgentTool(
		RunCommandToolName,
		runCommandDescription(),
		func(ctx context.Context, params RunCommandParams, call fantasy.ToolCall) (fantasy.ToolResponse, error) {
			if params.Program == "" {
				return fantasy.NewTextErrorResponse("program is required"), nil
			}

			argv := append([]string{params.Program}, params.Args...)

			// Same shared block list as the bash tool (banned programs AND
			// argument blockers like `go install` / `npm install --global` /
			// `go test -exec`) — deliberately NOT a second list, so the two
			// surfaces cannot drift apart.
			for _, blocker := range blockFuncs() {
				if blocker(argv) {
					return fantasy.NewTextErrorResponse(fmt.Sprintf(
						"program %q with these arguments is not allowed (run_command reuses the bash tool's block list)",
						params.Program)), nil
				}
			}

			// Same fork architectural rule as bash.go: an agent inside rush
			// should EXECUTE work, not re-delegate to yet another agent.
			// Keep this site aligned with bash.go's identical CheckAll call
			// so the two surfaces stay in lockstep.
			if guardErr := agentguard.CheckAll(RunCommandPermissionsParams(params).RunAllowlistCommand()); guardErr != nil {
				return fantasy.NewTextErrorResponse(guardErr.Error()), nil
			}

			execDir := workingDir
			if params.WorkingDir != "" {
				if msg := pathInsideWorkingDir(workingDir, params.WorkingDir); msg != "" {
					return fantasy.NewTextErrorResponse(msg), nil
				}
				// Join (validated above) so execDir is absolute — cmd.Dir
				// is resolved against THIS process's cwd, not workingDir.
				execDir = filepath.Join(workingDir, params.WorkingDir)
			}

			n := params.TimeoutSeconds
			clampNotice := ""
			if n == 0 {
				n = runCommandDefaultTimeoutSeconds
			}
			if n < 0 {
				return fantasy.NewTextErrorResponse("timeout_seconds must be positive"), nil
			}
			if n > runCommandMaxTimeoutSeconds {
				clampNotice = fmt.Sprintf("(timeout_seconds %d clamped to %d)\n", n, runCommandMaxTimeoutSeconds)
				n = runCommandMaxTimeoutSeconds
			}
			runCtx, cancel := context.WithTimeout(ctx, time.Duration(n)*time.Second)
			defer cancel()

			sessionID := GetSessionFromContext(ctx)
			if sessionID == "" {
				return fantasy.ToolResponse{}, fmt.Errorf("session ID is required for executing commands")
			}

			// Every run_command call executes a program — there is no
			// safe-read-only fast path, so a permission request is ALWAYS
			// raised.
			p, err := permissions.Request(ctx, permission.CreatePermissionRequest{
				SessionID:   sessionID,
				Path:        execDir,
				ToolCallID:  call.ID,
				ToolName:    RunCommandToolName,
				Action:      "execute",
				Description: cmp.Or(params.Description, fmt.Sprintf("Run %s", params.Program)),
				Params:      RunCommandPermissionsParams(params),
			})
			if err != nil {
				return fantasy.ToolResponse{}, err
			}
			if !p {
				return NewPermissionDeniedResponse(), nil
			}

			startTime := time.Now()
			cmd := platform.Command(runCtx, params.Program, params.Args...)
			cmd.Dir = execDir
			var buf bytes.Buffer
			cmd.Stdout = &buf
			cmd.Stderr = &buf
			execErr := cmd.Run()
			endTime := time.Now()

			metadata := RunCommandResponseMetadata{
				StartTime:        startTime.UnixMilli(),
				EndTime:          endTime.UnixMilli(),
				Output:           truncateOutput(buf.String()),
				Description:      params.Description,
				WorkingDirectory: execDir,
			}

			// Error contract: model-correctable failures come back as
			// error RESPONSES (nil Go error); only caller cancellation is
			// fatal.
			if execErr != nil {
				switch {
				case ctx.Err() == context.Canceled:
					return fantasy.ToolResponse{}, ctx.Err()
				case runCtx.Err() == context.DeadlineExceeded || errors.Is(execErr, context.DeadlineExceeded):
					text := truncateOutput(buf.String())
					if text == "" {
						text = execErr.Error()
					}
					metadata.Output = text
					return fantasy.WithResponseMetadata(fantasy.NewTextErrorResponse(fmt.Sprintf(
						"program %q timed out after %ds and was killed\n%s",
						params.Program, n, text)), nil), nil
				case errors.Is(execErr, exec.ErrNotFound) || errors.As(execErr, new(*exec.Error)):
					underlying := execErr.Error()
					var execError *exec.Error
					if errors.As(execErr, &execError) {
						underlying = execError.Err.Error()
					}
					return fantasy.NewTextErrorResponse(fmt.Sprintf(
						"program %q not found in PATH: %s", params.Program, underlying)), nil
				default:
					// Non-zero exit (or other spawn-time failure) — the
					// model must see it and can correct. NOT a Go error.
					text := truncateOutput(buf.String())
					if code := shell.ExitCode(execErr); code != 0 {
						text += fmt.Sprintf("\nExit code %d", code)
					}
					if clampNotice != "" {
						text = clampNotice + text
					}
					metadata.Output = text
					return fantasy.WithResponseMetadata(fantasy.NewTextErrorResponse(text), metadata), nil
				}
			}

			text := truncateOutput(buf.String())
			if text == "" {
				text = BashNoOutput
			}
			if clampNotice != "" {
				text = clampNotice + text
			}
			metadata.Output = text
			return fantasy.WithResponseMetadata(fantasy.NewTextResponse(text), metadata), nil
		},
	)
}
