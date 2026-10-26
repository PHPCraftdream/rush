package lab

import (
	"context"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/PHPCraftdream/rush/internal/jobbatch"
)

// RunOptions supplies bounded cleanup and public harness observation seams.
// OpenEvents is called only after the whole scenario has validated. Async
// close/output and reader hooks have budgeted joins: a hook ignoring cancellation
// yields ExitUnfinished and may outlive the measurement cut. Synchronous hooks
// and writer methods must honor their own bounded-return contract.
type RunOptions struct {
	OpenEvents        func() (io.WriteCloser, error)
	ProcessOptions    ProcessOptions
	CleanupBound      time.Duration
	Oracle            *Oracle
	BeforeAction      func(context.Context, int, *RunHarness) error
	BeforeEventsRead  func(context.Context) error
	BeforeCloseReturn func(context.Context) error
}
type (
	RunHarness struct {
		Runner     *jobbatch.Runner
		Controlled *ControlledExecutor
		Oracle     *Oracle
	}
	RunResult struct {
		ExitCode      int
		Footer        JournalFooter
		Mismatches    []int
		OracleRule    string
		CloseFailures []ScenarioCloseFailure
	}
	ScenarioCloseFailure struct {
		Action   int
		Error    ErrorName
		Failures []jobbatch.DisposeFailure
		Physical []ScenarioPhysicalState
	}
	ScenarioPhysicalState struct {
		Node            jobbatch.ID
		Token           uint64
		StopActive      bool
		Reads           int
		DisposeEntered  bool
		DisposeReturned bool
	}
	scenarioRun struct {
		s           *Scenario
		r           *jobbatch.Runner
		c           *ControlledExecutor
		o           *Oracle
		bound       context.Context
		bindings    map[string]uint64
		async       map[string]scenarioAsync
		inputs      []JournalInput
		roots       map[jobbatch.ID]*jobbatch.Batch
		closeReturn func(context.Context) error
		counts      RequestCounts
	}
)

type scenarioAsync struct {
	action Action
	done   chan error
}

// RunScenario interprets actions against one Runner for the entire forest.
func RunScenario(ctx context.Context, s *Scenario, opts RunOptions) RunResult {
	result := RunResult{ExitCode: ExitInvalid}
	if ValidateScenario(s) != nil {
		return result
	}
	cleanup := opts.CleanupBound
	if cleanup <= 0 {
		cleanup = 5 * time.Second
	}
	o := opts.Oracle
	if o == nil {
		o = NewOracle()
	}
	c := NewControlledExecutor()
	c.observe = func(obs controlledObservation) { o.observe(obs.Token, obs.Kind, obs.Call) }
	routes := map[string]jobbatch.Executor{"controlled": c}
	for _, p := range s.Payloads {
		if p.Kind == "process" {
			po := opts.ProcessOptions
			if po.MaxRetainedBytes <= 0 {
				po.MaxRetainedBytes = 1 << 20
			}
			ex, err := NewProcessExecutor(po)
			if err != nil {
				return result
			}
			routes["process"] = ex
			break
		}
	}
	var writer io.WriteCloser
	if opts.OpenEvents != nil {
		var err error
		writer, err = opts.OpenEvents()
		if err != nil {
			return result
		}
		defer writer.Close()
	}
	host, cancelHost := context.WithCancel(context.Background())
	defer cancelHost()
	r, err := jobbatch.NewRunner(host, jobbatch.RunnerConfig{Limits: s.Limits, Executor: &oracleExecutor{o, routes}, ControlTimeout: time.Duration(s.Runner.ControlTimeoutMS) * time.Millisecond, RecordRetention: s.Runner.RecordRetention})
	if err != nil {
		return result
	}
	o.finalProbe = func() jobbatch.EventPage {
		probe, stop := context.WithCancel(context.Background())
		stop()
		p, _ := r.Events(probe, ^uint64(0))
		return p
	}
	bound, cancel := context.WithTimeout(ctx, time.Duration(s.WaitBoundMS)*time.Millisecond)
	defer cancel()
	run := &scenarioRun{roots: map[jobbatch.ID]*jobbatch.Batch{}, closeReturn: opts.BeforeCloseReturn, s: s, r: r, c: c, o: o, bound: bound, bindings: map[string]uint64{}, async: map[string]scenarioAsync{}}
	capture := newJournalCapture(s, writer, o)
	capture.batches = run.roots
	readerCtx, readerCancel := context.WithCancel(context.Background())
	readerDone := make(chan struct{})
	go func() {
		defer close(readerDone)
		if opts.BeforeEventsRead != nil {
			if opts.BeforeEventsRead(readerCtx) != nil {
				return
			}
		}
		if readerCtx.Err() != nil {
			return // A late external hook must not touch the frozen capture.
		}
		capture.follow(readerCtx, r)
	}()
	unfinished, mismatch := false, false
	closeAttempted, asyncOutput := false, false
	harness := &RunHarness{r, c, o}
	// A rejected return_stop released no held call, so no physical return follows.
	var stopFence *Action
	for i, a := range s.Script {
		if bound.Err() != nil {
			unfinished = true
			break
		}
		if previous := stopFence; previous != nil {
			stopFence = nil
			// §6.6: gate release is not physical return. Fence the next
			// dependent action, without changing ReturnStop's contract.
			if err := o.AwaitLabel(bound, fmt.Sprintf("%s:stop_returned#%d", *previous.Attempt, *previous.Call)); err != nil {
				unfinished = true
				break
			}
		}
		if opts.BeforeAction != nil {
			if err := opts.BeforeAction(bound, i, harness); err != nil {
				mismatch = true
				result.Mismatches = append(result.Mismatches, i)
				continue
			}
		}
		if a.Async != nil {
			asyncOutput = asyncOutput || a.Action == ActionExpectOutput
			done := make(chan error, 1)
			run.async[*a.Async] = scenarioAsync{a, done}
			go func(a Action) { done <- run.action(bound, a) }(a)
			continue
		}
		var actionErr error
		checked := a
		if a.Action == ActionJoin {
			task := run.async[*a.Name]
			checked = task.action
			joinCtx, stop := run.actionContext(bound, a)
			select {
			case actionErr = <-task.done:
				delete(run.async, *a.Name)
			case <-joinCtx.Done():
				actionErr = joinCtx.Err()
			}
			stop()
		} else {
			actionErr = run.action(bound, a)
		}
		if checked.Action == ActionClose {
			closeAttempted = true
			if actionErr != nil {
				info := ScenarioCloseFailure{Action: i, Error: ErrorNameOf(actionErr)}
				var dispose *jobbatch.DisposeError
				if errors.As(actionErr, &dispose) {
					info.Failures = append([]jobbatch.DisposeFailure(nil), dispose.Failures...)
				}
				info.Physical = c.scenarioPhysicalState()
				result.CloseFailures = append(result.CloseFailures, info)
			}
		}
		if errors.Is(actionErr, jobbatch.ErrCloseTimeout) || errors.Is(actionErr, jobbatch.ErrDispose) || (errors.Is(actionErr, context.Canceled) || errors.Is(actionErr, context.DeadlineExceeded)) && checked.ExpectError == nil {
			unfinished = true
		}
		if !scenarioActionMatches(checked, actionErr) {
			mismatch = true
			result.Mismatches = append(result.Mismatches, i)
		}
		if a.Action == ActionReturnStop && actionErr == nil {
			stopFence = &a
		}
	}
	boundExpired := bound.Err() != nil
	if boundExpired {
		unfinished = true
	}
	cleanupCtx, cleanupStop := context.WithTimeout(context.Background(), cleanup)
	defer cleanupStop()
	cancel() // Cancel outstanding async public calls before bounded joining.
	if asyncOutput {
		// Output may already have returned logically on cancellation while its
		// underlying held read is still unwinding. Never release control gates.
		o.mu.Lock()
		held := o.reads != 0
		o.mu.Unlock()
		if held {
			c.releaseCleanupReads()
		}
	}
	for _, task := range run.async {
		select {
		case err := <-task.done:
			if !scenarioActionMatches(task.action, err) {
				mismatch = true
			}
		case <-cleanupCtx.Done():
			// External hooks may ignore context. Async close/output never write
			// harness maps or inputs, so a bounded cut remains safe.
		}
		unfinished = true // An abandoned join is not script completion.
	}
	if !closeAttempted || boundExpired {
		closeCtx := cleanupCtx
		if err := r.Close(closeCtx); err != nil {
			unfinished = true
		}
	}
	readerCancel()
	select {
	case <-readerDone:
	case <-cleanupCtx.Done():
		// A stuck external reader hook cannot enter follow after cancellation.
		unfinished = true
	}
	capture.mu.Lock()
	defer capture.mu.Unlock()
	capture.drain(cleanupCtx, r)
	cancelHost()
	if err := o.finishPhysical(cleanupCtx); err != nil {
		unfinished = true
	}
	result.Footer = capture.footer(s, run.inputs, run.counts)
	if writer != nil {
		capture.writeFooter(result.Footer)
		result.Footer.Capture = capture.status
	}
	code := ExitSuccess
	for _, root := range result.Footer.Roots {
		if root.State != jobbatch.StateCompleted {
			code = ExitFailed
		}
	}
	if !capture.checkOracle(s.Expect, o, false) {
		result.OracleRule = capture.oracleRule
		mismatch = true
	}
	if mismatch {
		code = ExitMismatch
	}
	if writer != nil && result.Footer.Capture.Status != CaptureComplete {
		code = ExitIncompleteJournal
	}
	if unfinished || result.Footer.Outcome.Status == OutcomeUnfinished {
		code = ExitUnfinished
	}
	if s.Expect != nil && s.Expect.ExitCode != nil && *s.Expect.ExitCode != code && code != ExitUnfinished && code != ExitIncompleteJournal {
		code = ExitMismatch
	}
	result.ExitCode = code
	return result
}

func scenarioActionMatches(a Action, err error) bool {
	if a.ExpectError != nil {
		return err != nil && ErrorNameOf(err) == *a.ExpectError
	}
	if a.Action == ActionClose && a.Expect != nil {
		switch *a.Expect {
		case CloseTimeout:
			return errors.Is(err, jobbatch.ErrCloseTimeout)
		case CloseDispose:
			return errors.Is(err, jobbatch.ErrDispose)
		case CloseOK:
			return err == nil
		}
	}
	return err == nil
}

func (run *scenarioRun) actionContext(ctx context.Context, a Action) (context.Context, context.CancelFunc) {
	if a.TimeoutMS != nil {
		return context.WithTimeout(ctx, time.Duration(*a.TimeoutMS)*time.Millisecond)
	}
	return context.WithCancel(ctx)
}
