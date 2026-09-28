package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
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
	// lastBuffered is the terse output of the last REAL turn (a no-turn Drain
	// iteration prints nothing and must not blank it).
	lastBuffered := &bytes.Buffer{}
	sessionID := continueSessionID
	for {
		buffered := &bytes.Buffer{}
		turnOutput := output
		switch mode {
		case RunModeTerse:
			turnOutput = buffered
		case RunModeJSON:
			turnOutput = io.Discard
		}
		turnCtx := ctx
		if !firstTurn {
			// Phase-4 step 3 (doc sec.3.4): this turn carries an empty
			// prompt (below) and must be built as a Drain call -- lifts
			// ErrEmptyPrompt, skips createUserMessage, and reacts only to
			// whatever this turn's own turn-start pull moves into history.
			turnCtx = agent.WithDrainCall(agent.WithBackgroundJobNotice(ctx))
		}
		result, err := app.ExecuteRun(turnCtx, RunRequest{
			Prompt: prompt, Overrides: turnOverrides, Mode: mode,
			ContinueSessionID: continueSessionID, UseLast: useLast,
			Origin: overrides.Origin, Stdout: turnOutput, Stderr: os.Stderr,
			HideSpinner:   hideSpinner,
			captureResult: true,
			onSessionResolved: func(resolved string) {
				sessionID = resolved
				source.ClaimAsyncCompletions(resolved)
			},
		})
		// A Drain iteration that ran no provider turn -- nothing wake-worthy
		// was pending (typically the notice was already pulled at a step
		// boundary of the previous turn), or it queued behind another owner
		// that pulls it itself -- surfaces as ErrRunQueued with an empty
		// envelope. Not a turn: it must neither replace the last turn's
		// result nor become the run's error.
		drainNoTurn := !firstTurn && errors.Is(err, ErrRunQueued)
		if result != nil && !drainNoTurn {
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
		if !drainNoTurn {
			runErr = err
			lastBuffered = buffered
		}
		if sessionID == "" || ctx.Err() != nil {
			return final, runErr
		}
		// Phase-4 step 3: the in-memory ready queue is now used purely as a
		// wake SIGNAL (doc sec.5 step 3) -- the completion's own text is
		// NEVER used as the next turn's prompt anymore (that would duplicate
		// the notice the driver's own turn-start pull is about to insert
		// into history from the durable async_jobs/session_notices row).
		// s.ready/markDrained/ClaimAsyncCompletions/NextAsyncCompletion
		// removal is deferred to step 4's DB-driven CLI loop rewrite.
		_, hasCompletion, waitErr := source.NextAsyncCompletion(ctx, sessionID)
		if waitErr != nil {
			if final != nil {
				final.ExitReason = "canceled"
				final.Error = waitErr.Error()
			}
			return final, waitErr
		}
		if !hasCompletion {
			// next() (workLedger.next(), internal/agent/work_ledger.go)
			// returns false ONLY when sessionID has neither a ready
			// completion nor an outstanding/undelivered job -- which
			// already includes a delegation armed for sessionID as its
			// parent (work_ledger_delegation.go's byChild-backed record
			// stays in bySession[owner].jobs until the child's own scope
			// drains, docs/plans/2026-09-28-async-phase3-spec.md §1.1-1.3).
			// That is exactly "the root's scope is closed" -- next() itself
			// already blocked, event-driven, for the whole time the scope
			// was open (§1.2), so there is nothing left to poll for here.
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
				if _, writeErr := io.Copy(output, lastBuffered); writeErr != nil {
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
		// Empty prompt: a Drain-kind turn (doc sec.3.4). Its own turn-start
		// pull moves whatever pending notice(s) woke this signal into
		// history and decides, from there, whether to react at all --
		// never from this completion's own (unused) text.
		prompt = ""
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
