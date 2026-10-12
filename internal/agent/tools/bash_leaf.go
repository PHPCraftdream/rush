package tools

import (
	"context"
	"errors"
	"fmt"
	"time"

	"charm.land/fantasy"
	"github.com/PHPCraftdream/rush/internal/shell"
)

type shellLeafKey struct{}

// ShellLeaf selects retained synchronous execution through the ordinary Bash
// policy boundary. OnStart transfers the handle to its caller before waiting;
// the caller owns disposal after Run and real Done. It must not block or panic.
type ShellLeaf struct {
	OnStart func(*shell.BackgroundExecution)
}

// WithShellLeaf marks a policy-leaf Run, not a raw launch or policy bypass.
func WithShellLeaf(ctx context.Context, leaf ShellLeaf) context.Context {
	return context.WithValue(ctx, shellLeafKey{}, leaf)
}

func runBashLeaf(ctx context.Context, leaf ShellLeaf, manager *shell.BackgroundShellManager, sessionID, workingDir string, params BashParams) (fantasy.ToolResponse, error) {
	if err := ctx.Err(); err != nil {
		return fantasy.ToolResponse{}, err
	}
	start := time.Now()
	processCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	defer cancel()
	execution, err := startRetainedBashShell(processCtx, manager, sessionID, workingDir, params)
	if err != nil {
		return fantasy.NewTextErrorResponse(fmt.Sprintf("error starting shell: %s", err)), nil
	}
	if leaf.OnStart != nil {
		leaf.OnStart(execution)
	} else {
		defer execution.Dispose(context.Background())
	}
	var stopErr error
	select {
	case <-execution.Done():
	case <-ctx.Done():
		controlCtx, controlCancel := context.WithTimeout(context.Background(), killConfirmationTimeout)
		stopErr = execution.Stop(controlCtx)
		controlCancel()
		// A control timeout/error never proves termination or permits early return.
		<-execution.Done()
	}
	stdout, stderr, _, execErr := execution.GetOutput()
	_ = manager.RemoveOwned(sessionID, execution.ID())
	response, renderErr := bashForegroundResponse(start, execution.ID(), workingDir, params, stdout, stderr, execErr)
	if ctx.Err() != nil {
		return response, errors.Join(ctx.Err(), stopErr, renderErr)
	}
	return response, renderErr
}

func bashForegroundResponse(start time.Time, id, workingDir string, params BashParams, stdout, stderr string, execErr error) (fantasy.ToolResponse, error) {
	interrupted := shell.IsInterrupt(execErr)
	exitCode := shell.ExitCode(execErr)
	if exitCode == 0 && !interrupted && execErr != nil {
		return fantasy.ToolResponse{}, fmt.Errorf("[Job %s] error executing command: %w", id, execErr)
	}
	stdout = formatOutput(stdout, stderr, execErr)
	if isSecurityBlockError(execErr) {
		return fantasy.NewTextErrorResponse(stdout), nil
	}
	metadata := BashResponseMetadata{
		StartTime: start.UnixMilli(), EndTime: time.Now().UnixMilli(), Output: stdout,
		Description: params.Description, Background: params.RunInBackground, WorkingDirectory: workingDir,
	}
	if stdout == "" {
		return fantasy.WithResponseMetadata(fantasy.NewTextResponse(BashNoOutput), metadata), nil
	}
	stdout += fmt.Sprintf("\n\n<cwd>%s</cwd>", normalizeWorkingDir(workingDir))
	return fantasy.WithResponseMetadata(fantasy.NewTextResponse(stdout), metadata), nil
}
