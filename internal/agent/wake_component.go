package agent

import (
	"context"
	"errors"
	"time"

	"github.com/PHPCraftdream/rush/internal/agent/runtime/batch/batchinst/diskport"
	"github.com/PHPCraftdream/rush/internal/agent/runtime/exec/drainctl"
	"github.com/PHPCraftdream/rush/internal/agent/runtime/wakectl"
	sessionwake "github.com/PHPCraftdream/rush/internal/session/state/sessionwake"
)

type wakeScheduleWorker = wakectl.Scheduler

func (c *coordinator) wakeWorker() *wakeScheduleWorker {
	if c == nil {
		return nil
	}
	return c.wakeScheduler
}

func (c *coordinator) wakeBuildErrorPorts() wakectl.BuildErrorPorts {
	return wakectl.BuildErrorPorts{
		IsProcessDependencyUnavailable: func(err error) bool {
			return errors.Is(err, ErrDiskProviderUnavailable) || errors.Is(err, diskport.ErrCredentialBindingUnavailable)
		},
		ClearFromRecheckSet: c.clearFromRecheckSet,
		NoteDrainRefused:    c.noteDrainRefused,
	}
}

func (c *coordinator) wakeLaunchPorts() wakectl.LaunchPorts {
	return wakectl.LaunchPorts{
		BuildErrorPorts:  c.wakeBuildErrorPorts(),
		BumpHint:         func(id string) { c.driverHints.bumpHint(id) },
		IsExternalDriver: func(id string) bool { return c.driverHints.isExternalDriver(id) },
		DecisionBudget:   func() time.Duration { return recheckDebtCheckBudget },
		Decide: func(ctx context.Context, id string, fact bool) (bool, drainctl.DrainVerdict, error) {
			debt, verdict, err := c.drainDecision(ctx, id, fact)
			return debt, drainctl.DrainVerdict{Kind: verdict.kind, Recheck: verdict.recheck}, err
		},
		AddToRecheckSet: c.addToRecheckSet,
		PrepareDrain: func(ctx context.Context, id string) (func(context.Context) error, func(), error) {
			call, prep, err := c.drainCallFor(ctx, id)
			if err != nil {
				return nil, nil, err
			}
			var release func()
			if prep != nil {
				release = func() { prep.release() }
			}
			return func(ctx context.Context) error {
				_, err := c.agentFor(id).Run(ctx, call)
				return err
			}, release, nil
		},
	}
}

// wakeScheduler is the cancellation-only view the await-timer cleanup holds;
// the type itself lives in wakectl.
type wakeScheduler = wakectl.CancellationView

func (c *coordinator) wakeSchedulerFor() (*wakeScheduler, error) {
	s, err := c.wakeWorker().RequireScheduler()
	if err != nil {
		return nil, err
	}
	return wakectl.NewCancellationView(s), nil
}

// WakeScheduleController is the optional interface the App asserts the
// Coordinator to when wiring the stage-4b wake-schedule worker
// (internal/app). Deliberately NOT part of the Coordinator interface
// itself: every test fake implementing Coordinator would have to grow the
// two methods for nothing (same reasoning as
// coordinator_work_scope.go's own not-on-the-interface note).
type WakeScheduleController = wakectl.WakeScheduleController

func (c *coordinator) SetWakeScheduleStore(store *sessionwake.WakeScheduleStore) {
	if c != nil {
		wakectl.SetWakeScheduleStore(&c.wakeScheduler, store, c)
	}
}

func (c *coordinator) StopWakeScheduler() { c.wakeWorker().Stop() }

func (c *coordinator) WakeSchedulerNotify() { c.wakeWorker().Notify() }
