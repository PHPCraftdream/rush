package app

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"slices"
	"time"

	"github.com/PHPCraftdream/rush/internal/agent"
	"github.com/PHPCraftdream/rush/internal/message"
)

func (app *App) runNonInteractiveWithAsyncResults(ctx context.Context, output io.Writer, prompt string, overrides RunOverrides, hideSpinner bool, mode RunMode, continueSessionID string, useLast bool) (final *RunResult, runErr error) {
	if output == nil {
		output = io.Discard
	}
	source, async := app.AgentCoordinator.(agent.AsyncCompletionSource)
	if !async || overrides.Origin != message.OriginCLI {
		final, runErr = app.ExecuteRun(ctx, RunRequest{
			Prompt: prompt, Overrides: overrides, Mode: mode,
			ContinueSessionID: continueSessionID, UseLast: useLast,
			Origin: overrides.Origin, Stdout: output, Stderr: os.Stderr,
			HideSpinner: hideSpinner,
		})
		if mode == RunModeJSON && final != nil {
			if err := json.NewEncoder(output).Encode(final); err != nil {
				return final, fmt.Errorf("failed to encode JSON result: %w", err)
			}
		}
		return final, runErr
	}

	started := time.Now()
	turnOverrides := overrides
	turnOverrides.OnFinishHook = ""
	defer func() {
		if overrides.OnFinishHook != "" && final != nil {
			runOnFinishHook(overrides.OnFinishHook, final.SessionID, final.ExitReason,
				final.Usage.DeltaCostUSD, final.Usage.DeltaTokens, time.Since(started))
		}
	}()
	counts := make(map[string]int)
	var warnings []string
	var subAgentOutputs []SubAgentOutput
	var totalTokens int64
	var totalCost float64
	firstTurn := true
	sessionID := continueSessionID
	for {
		var buffered bytes.Buffer
		turnOutput := output
		switch mode {
		case RunModeTerse:
			turnOutput = &buffered
		case RunModeJSON:
			turnOutput = io.Discard
		}
		turnCtx := ctx
		if !firstTurn {
			turnCtx = agent.WithBackgroundJobNotice(ctx)
		}
		result, err := app.ExecuteRun(turnCtx, RunRequest{
			Prompt: prompt, Overrides: turnOverrides, Mode: mode,
			ContinueSessionID: continueSessionID, UseLast: useLast,
			Origin: overrides.Origin, Stdout: turnOutput, Stderr: os.Stderr,
			HideSpinner:       hideSpinner,
			captureResult:     true,
			onSessionResolved: func(resolved string) { sessionID = resolved },
		})
		if result != nil {
			final = result
			sessionID = result.SessionID
			totalTokens += result.Usage.DeltaTokens
			totalCost += result.Usage.DeltaCostUSD
			warnings = append(warnings, result.Warnings...)
			subAgentOutputs = append(subAgentOutputs, result.SubAgentOutputs...)
			for _, stat := range result.ToolCalls {
				counts[stat.Name] += stat.Count
			}
		}
		runErr = err
		if sessionID == "" || ctx.Err() != nil {
			return final, runErr
		}
		completion, hasCompletion, waitErr := source.NextAsyncCompletion(ctx, sessionID)
		if waitErr != nil {
			if final != nil {
				final.ExitReason = "canceled"
				final.Error = waitErr.Error()
			}
			return final, waitErr
		}
		if !hasCompletion {
			if final == nil {
				return nil, runErr
			}
			final.Usage.DeltaTokens = totalTokens
			final.Usage.DeltaCostUSD = totalCost
			final.Warnings = warnings
			final.SubAgentOutputs = subAgentOutputs
			final.DurationMs = time.Since(started).Milliseconds()
			final.ToolCalls = final.ToolCalls[:0]
			for name, count := range counts {
				final.ToolCalls = append(final.ToolCalls, ToolCallStat{Name: name, Count: count})
			}
			slices.SortFunc(final.ToolCalls, func(a, b ToolCallStat) int { return cmpName(a.Name, b.Name) })
			if mode == RunModeTerse {
				if _, writeErr := io.Copy(output, &buffered); writeErr != nil {
					return final, writeErr
				}
			}
			if mode == RunModeJSON {
				if encErr := json.NewEncoder(output).Encode(final); encErr != nil {
					return final, fmt.Errorf("failed to encode JSON result: %w", encErr)
				}
			}
			return final, runErr
		}
		prompt = agent.FormatAsyncCompletion(completion)
		continueSessionID = sessionID
		useLast = false
		firstTurn = false
	}
}

func cmpName(a, b string) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	default:
		return 0
	}
}
