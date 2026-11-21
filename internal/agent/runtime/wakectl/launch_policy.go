package wakectl

import (
	"context"
	"fmt"
	"log/slog"
	"runtime/debug"
	"time"

	"github.com/PHPCraftdream/rush/internal/agent/runtime/exec/drainctl"
)

type BuildErrorPorts struct {
	IsProcessDependencyUnavailable func(error) bool
	ClearFromRecheckSet            func(string)
	NoteDrainRefused               func(string, error)
}

type LaunchPorts struct {
	BuildErrorPorts
	BumpHint         func(string)
	IsExternalDriver func(string) bool
	DecisionBudget   func() time.Duration
	Decide           func(context.Context, string, bool) (bool, drainctl.DrainVerdict, error)
	AddToRecheckSet  func(string)
	PrepareDrain     func(context.Context, string) (run func(context.Context) error, release func(), err error)
}

func WakeSession(p LaunchPorts, ctx context.Context, sessionID string, fact bool) (err error) {
	defer func() {
		if r := recover(); r != nil {
			slog.Error("wakeSession panic", "session_id", sessionID, "panic", r, "stack", string(debug.Stack()))
			err = fmt.Errorf("wakeSession panic: %v", r)
		}
	}()
	if fact {
		p.BumpHint(sessionID)
	}
	if p.IsExternalDriver(sessionID) {
		return nil
	}
	decideCtx, cancel := context.WithTimeout(ctx, p.DecisionBudget())
	debt, v, decErr := p.Decide(decideCtx, sessionID, fact)
	cancel()
	if decErr != nil {
		p.AddToRecheckSet(sessionID)
		return fmt.Errorf("wakeSession: launch decision: %w", decErr)
	}
	if !debt {
		return nil
	}
	switch v.Kind {
	case drainctl.DrainPaced:
		p.AddToRecheckSet(sessionID)
		return nil
	case drainctl.DrainDeferred:
		if v.Recheck {
			p.AddToRecheckSet(sessionID)
		}
		return nil
	case drainctl.DrainStuck:
		return nil
	}
	run, release, buildErr := p.PrepareDrain(ctx, sessionID)
	if buildErr != nil {
		return HandleDrainBuildError(p.BuildErrorPorts, sessionID, buildErr)
	}
	if release != nil {
		defer release()
	}
	if runErr := run(ctx); runErr != nil {
		slog.Warn("drain call failed after its underlying notice was committed", "session_id", sessionID, "err", runErr)
		return runErr
	}
	return nil
}

func HandleDrainBuildError(p BuildErrorPorts, sessionID string, buildErr error) error {
	if p.IsProcessDependencyUnavailable(buildErr) {
		p.ClearFromRecheckSet(sessionID)
		slog.Warn("drain launch refused: a process-bound drain dependency is unavailable; deferred until explicit re-supply", "session_id", sessionID, "err", buildErr)
		return fmt.Errorf("wakeSession: build drain call: %w", buildErr)
	}
	p.NoteDrainRefused(sessionID, buildErr)
	return fmt.Errorf("wakeSession: build drain call: %w", buildErr)
}
