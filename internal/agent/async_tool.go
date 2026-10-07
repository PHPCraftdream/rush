package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"charm.land/fantasy"
	"github.com/PHPCraftdream/rush/internal/agent/tools"
	"github.com/PHPCraftdream/rush/internal/message"
	"github.com/PHPCraftdream/rush/internal/permission"
	"github.com/PHPCraftdream/rush/internal/shell"
)

type asyncTool struct {
	inner       fantasy.AgentTool
	coordinator *coordinator
	name        string
}

type asyncToolMetadata struct {
	Async bool `json:"async"`
	// Inline marks the A14 immediate-answer form: this result IS the job's
	// outcome (a natural completed/failed finish that committed within the
	// inline window). Web must treat async:false as not-pending
	// (isPendingJob, ActionRow.tsx).
	Inline         bool   `json:"inline,omitempty"`
	JobID          string `json:"job_id"`
	ChildSessionID string `json:"child_session_id,omitempty"`
	Status         string `json:"status"`
	// ClaimID tags this as the job's OWN "started" result (ackTag): the ack
	// gate fuses only a result carrying the job's claim, never one that
	// merely shares its tool_call_id.
	ClaimID string `json:"claim_id,omitempty"`
}

func (t *asyncTool) Info() fantasy.ToolInfo {
	info := t.inner.Info()
	info.Description += "\n\nIn web and CLI sessions this tool starts asynchronously. It returns a job ID immediately; Rush sends the result as a new session message and resumes the agent. Continue independent work instead of blocking or polling for this job."
	if t.inlineEnabled() {
		info.Description += fmt.Sprintf("\n\nA command that finishes within %s returns its result directly in this response instead.", inlineWindow)
	}
	return info
}

// inlineEnabled reports whether this tool's calls run inside the A14 inline
// window: a non-sync bash/run_command job whose ledger wired the window.
func (t *asyncTool) inlineEnabled() bool {
	return t.coordinator != nil && t.coordinator.asyncJobs != nil &&
		t.coordinator.asyncJobs.inlineWindow > 0 && inlineWindowApplies(t.name)
}

func (t *asyncTool) ProviderOptions() fantasy.ProviderOptions { return t.inner.ProviderOptions() }

func (t *asyncTool) SetProviderOptions(opts fantasy.ProviderOptions) {
	t.inner.SetProviderOptions(opts)
}

func (t *asyncTool) Run(ctx context.Context, call fantasy.ToolCall) (fantasy.ToolResponse, error) {
	origin := CallOriginFrom(ctx)
	// sync is true for every origin that used to bypass the registry
	// entirely (SDK, unspecified): one execution path now, not a separate
	// branch (§4.2, closes #1037's "45-minute limit only applies on the
	// synchronous branch" -- after phase 2 "synchronous" means "same
	// registry, same explicit-timeout support, delivery within the call".
	sync := origin != message.OriginCLI && origin != message.OriginWeb
	sessionID := tools.GetSessionFromContext(ctx)
	if sessionID == "" || call.ID == "" {
		return fantasy.ToolResponse{}, fmt.Errorf("async %s requires session and tool call IDs", t.name)
	}
	childSessionID, err := t.childSessionID(ctx, sessionID, call)
	if err != nil {
		return fantasy.NewTextErrorResponse(err.Error()), nil
	}
	// #1157/#1212: a resume naming a child paused on its held delegation is
	// answered INTO that delegation -- decided by the held question itself,
	// not the call origin (a Drain-woken parent carries none at all).
	if t.name == AgentToolName && t.coordinator != nil && t.coordinator.asyncJobs != nil {
		var params AgentParams
		if json.Unmarshal([]byte(call.Input), &params) == nil && params.ResumeSessionID != "" {
			if resp, answered := t.coordinator.asyncJobs.answerHeldDelegation(ctx, sessionID, childSessionID, params.Prompt); answered {
				return resp, nil
			}
		}
	}
	timeoutSpec, err := parseTimeoutParam(t.name, call.Input)
	if err != nil {
		return fantasy.NewTextErrorResponse(err.Error()), nil
	}
	if timeoutSpec == nil && (t.name == tools.BashToolName || t.name == tools.RunCommandToolName) {
		// Opt-in operator default (item 5): a bash/run_command call with no
		// explicit timeout gets the configured terminate_and_wake deadline.
		if d := t.coordinator.resolveBackgroundJobDefaultTimeout(ctx); d > 0 {
			timeoutSpec = &TimeoutSpec{
				Deadline: time.Now().Add(d),
				Kind:     timeoutTerminateAndWake,
				Seconds:  int(d.Seconds()),
			}
		}
	}
	// Only CLI/web jobs are detached from the triggering turn's ctx: they
	// must survive the turn that started them ending (their result is
	// delivered LATER, as a notice). A sync job has no "deliver later" path
	// at all (its only consumer is this same call, blocked below in
	// awaitAndFinish) -- cancelling the caller's ctx must cancel it too,
	// exactly like the pre-async-wrapper t.inner.Run(ctx, call) did.
	// Detaching it anyway would leak a goroutine and a ledger entry nobody
	// ever collects once the caller gives up.
	var jobCtx context.Context
	var cancel context.CancelFunc
	if sync {
		jobCtx, cancel = context.WithCancel(ctx)
	} else {
		jobCtx, cancel = context.WithCancel(context.WithoutCancel(ctx))
	}
	job, existing, err := t.coordinator.asyncJobs.Start(sessionID, call.ID, call.Input, t.name, childSessionID, origin == message.OriginCLI, sync, timeoutSpec, cancel)
	if err != nil {
		cancel()
		var capErr *asyncCapError
		if errors.As(err, &capErr) {
			// ASYNC-12: the refusal names what to do (free slots or end the
			// turn) and is tagged so the step guard (#1149) can later count
			// it as a no-progress step. No claim_id: the refusal never
			// reached store.Claim, so there is no row and no ack.
			slog.Warn("async job cap reached",
				"session", sessionID, "tool", t.name,
				"running", capErr.Running, "limit", capErr.Limit,
				"wait_timers", capErr.Timers)
			var meta asyncCapMetadata
			meta.AsyncCap.Running = capErr.Running
			meta.AsyncCap.Limit = capErr.Limit
			meta.AsyncCap.WaitTimers = capErr.Timers
			return fantasy.WithResponseMetadata(fantasy.NewTextErrorResponse(capErr.Error()), meta), nil
		}
		return fantasy.NewTextErrorResponse(err.Error()), nil
	}
	if existing {
		// Idempotent retry of the same (owner, toolCallID): #1038. The
		// executor already runs (or ran) for this call; do not start a
		// second one -- a provider retrying the same tool call must not
		// repeat the underlying side effect. This call's own jobCtx/cancel
		// were never handed to an executor, so cancel it here instead of
		// leaking it.
		cancel()
		if sync {
			return t.awaitAndFinish(ctx, job)
		}
		return t.startedResponse(call.ID, job.childSession, job.claimID), nil
	}
	return t.launchExecutor(ctx, jobCtx, cancel, sessionID, childSessionID, call, sync, job)
}

// launchExecutor runs the fresh-job setup (permission inheritance,
// supervision noteWorkStarted) and launches t.run -- everything between
// Start succeeding and the executor goroutine actually existing. B15: Start
// already claimed this job's durable row (announced=0, state='running')
// before this is ever called, so a panic anywhere in this window, before
// "go t.run" ever runs, must not leave that row looking like a
// legitimately started, live job forever: nothing would ever call
// finish()/transition() for it again. The recover here fails the job with
// the launch error directly (failLaunch) and answers with an error response
// tagged with the job's claim (failedStartResponse), which the ack gate
// treats as the job's own result, so the row is announced and its failure
// delivered like any other.
func (t *asyncTool) launchExecutor(ctx, jobCtx context.Context, cancel context.CancelFunc, sessionID, childSessionID string, call fantasy.ToolCall, sync bool, job *asyncJob) (resp fantasy.ToolResponse, err error) {
	defer func() {
		if recovered := recover(); recovered != nil {
			msg := fmt.Sprintf("async %s failed to start: %v", t.name, recovered)
			t.failLaunch(job, msg)
			cancel()
			resp = failedStartResponse(msg, job.claimID)
			err = nil
		}
	}()
	if childSessionID != "" && t.coordinator.permissions != nil {
		t.coordinator.permissions.InheritSessionAutoApprove(sessionID, childSessionID)
		if mgr, ok := t.coordinator.permissions.(permission.SessionRunAllowlistManager); ok {
			// §6.2: bind to whatever driver generation is CURRENTLY
			// registered for childSessionID, if any -- 0 (subAgentDriverRegistry
			// never issues 0) for a fresh delegation with no driver yet. This
			// call always runs BEFORE t.run reaches runSubAgent (the
			// underlying agent/agentic_fetch tool), whose own
			// InheritSessionRunAllowlistForGeneration call moments later
			// overwrites this entry under the driver's real, freshly
			// assigned generation -- this early call only needs to be inert
			// against a stale clear, not durable itself.
			driver, _ := t.coordinator.subAgentDrivers.get(childSessionID)
			mgr.InheritSessionRunAllowlistForGeneration(sessionID, childSessionID, driver.generation)
		}
	}
	if !sync {
		if t.inlineEnabled() {
			// A14: a windowed bash/run_command job answers noteWorkStarted
			// only on the "started" fallback -- an inline-answered call is
			// over before the window closes, and must not lift the
			// supervision pause (supervision.go) for work that produced no
			// open scope.
			go t.run(jobCtx, cancel, job, sessionID, childSessionID, call, sync)
			return t.launchInline(ctx, job, childSessionID, call.ID)
		}
		// Supervision (design doc §7): new open work for a CLI/web owner
		// arms (or resumes) its root-session check-in timer. A no-op for a
		// delegated child session (never supervised directly) or when
		// supervision is disabled -- see noteWorkStarted's own doc.
		t.coordinator.asyncJobs.noteWorkStarted(ctx, sessionID)
	}
	go t.run(jobCtx, cancel, job, sessionID, childSessionID, call, sync)
	if sync {
		return t.awaitAndFinish(ctx, job)
	}
	return t.startedResponse(call.ID, childSessionID, job.claimID), nil
}

// launchInline waits the inline window for the job's natural Tx1 and
// answers with its result (the inline response, tagged with the job's claim
// so persistToolResult fuses Tx2); anything else inside the window falls
// back to the ordinary "started" response, noteWorkStarted included, exactly
// like a pre-window job.
func (t *asyncTool) launchInline(ctx context.Context, job *asyncJob, childSessionID, jobID string) (fantasy.ToolResponse, error) {
	result, inline := t.coordinator.asyncJobs.awaitInline(ctx, job)
	if !inline {
		t.coordinator.asyncJobs.noteWorkStarted(ctx, job.owner)
		return t.startedResponse(jobID, childSessionID, job.claimID), nil
	}
	status := "completed"
	if result.isError {
		status = "failed"
	}
	return fantasy.WithResponseMetadata(fantasy.ToolResponse{
		Type: "text", Content: result.content, Metadata: result.metadata, IsError: result.isError,
	}, asyncToolMetadata{
		Async: false, Inline: true, JobID: jobID, ChildSessionID: childSessionID,
		Status: status, ClaimID: job.claimID,
	}), nil
}

// awaitAndFinish blocks until job's sync outcome is ready (workLedger.
// awaitSync) and returns it as this call's own ToolResponse -- byte-for-byte
// the same content/error/metadata shape a direct t.inner.Run(ctx, call)
// would have returned before phase 2 (§4.3), just delivered through the
// registry instead of directly.
func (t *asyncTool) awaitAndFinish(ctx context.Context, job *asyncJob) (fantasy.ToolResponse, error) {
	result, err := t.coordinator.asyncJobs.awaitSync(ctx, job)
	if err != nil {
		return fantasy.ToolResponse{}, err
	}
	return fantasy.ToolResponse{Type: "text", Content: result.content, Metadata: result.metadata, IsError: result.isError}, nil
}

// startedResponse builds the "started" tool response for jobID/childSession.
// Shared by the fresh-start and idempotent-retry (existing job) paths, so a
// retried tool call reports the SAME child session id the first Start call
// registered.
//
// A14: an idempotent retry arriving while the FIRST call is still inside
// its inline window gets this "started" response too, but the window
// holder (job.inlinePending) wins the ack -- the retried call's tagged
// result is persisted as an ordinary tool result and the job's outcome
// arrives as the first call's inline response.
func (t *asyncTool) startedResponse(jobID, childSessionID, claimID string) fantasy.ToolResponse {
	content := fmt.Sprintf("Async %s job %s started. Its result will arrive as a new session message; continue independent work, or end your turn if none is left. Do not wait with sleep/echo commands: each completion wakes you again.", t.name, jobID)
	return fantasy.WithResponseMetadata(fantasy.NewTextResponse(content), asyncToolMetadata{
		Async: true, JobID: jobID, ChildSessionID: childSessionID, Status: "running", ClaimID: claimID,
	})
}

func (t *asyncTool) childSessionID(ctx context.Context, parentID string, call fantasy.ToolCall) (string, error) {
	if t.name != AgentToolName && t.name != tools.AgenticFetchToolName {
		return "", nil
	}
	if t.name == AgentToolName {
		var params AgentParams
		if err := json.Unmarshal([]byte(call.Input), &params); err == nil && params.ResumeSessionID != "" {
			child, err := t.coordinator.sessions.Get(ctx, params.ResumeSessionID)
			if err != nil {
				return "", fmt.Errorf("resume_session_id %q not found: %w", params.ResumeSessionID, err)
			}
			if child.ParentSessionID != parentID {
				return "", fmt.Errorf("resume_session_id %q is not a child of this session", params.ResumeSessionID)
			}
			return child.ID, nil
		}
	}
	messageID := tools.GetMessageFromContext(ctx)
	if messageID == "" {
		return "", fmt.Errorf("async %s requires an agent message ID", t.name)
	}
	return t.coordinator.sessions.CreateAgentToolSessionID(messageID, call.ID), nil
}

func (t *asyncTool) run(ctx context.Context, cancel context.CancelFunc, job *asyncJob, sessionID, childSessionID string, call fantasy.ToolCall, sync bool) {
	defer cancel()
	// No `defer ClearSessionRunAllowlist` here (phase 2, §6.2): the child
	// may still own async jobs/background shells after this turn returns
	// (structural concurrency, #1049), and a woken turn re-inherits from
	// subAgentDriver.parentSessionID on every wake (wakeSession's
	// drainCallFor) instead -- clearing on return would strand a woken
	// turn under the process-wide gate.
	completion := AsyncCompletion{SessionID: sessionID, ToolCallID: call.ID, ToolName: t.name}
	defer func() {
		if recovered := recover(); recovered != nil {
			completion.IsError = true
			completion.Content = fmt.Sprintf("async %s panicked: %v", t.name, recovered)
		}
		t.finalize(job, childSessionID, completion)
	}()
	if t.name == tools.BashToolName && !sync {
		// Unchanged for CLI/web: forcing run_in_background is what lets
		// awaitShell below watch it through BackgroundShellManager, so a
		// CLI/web bash job survives the triggering turn ending. A sync
		// caller's ctx already spans the whole wait (§4.2's jobCtx
		// attachment), so there is nothing to survive past -- the inner
		// bash tool's own synchronous exec path already blocks correctly,
		// and forcing background here would silently change a sync bash
		// call's response format (asyncToolMetadata/backgroundJobSummary
		// instead of the tool's own BashResponseMetadata) for an origin
		// (SDK) that must see byte-for-byte the same response as before
		// phase 2.
		ctx = tools.WithoutBackgroundCallback(ctx)
		var params map[string]json.RawMessage
		if json.Unmarshal([]byte(call.Input), &params) == nil && params != nil {
			params["run_in_background"] = json.RawMessage("true")
			if input, err := json.Marshal(params); err == nil {
				call.Input = string(input)
			}
		}
	}
	if t.name == tools.RunCommandToolName && !sync {
		// Task #1023 §3: registers the job's live output buffer with the
		// ledger as soon as run_command.go's process starts, so job_output
		// can read progressive output and job_kill's stopped notice can
		// quote it while the job is still running.
		ctx = tools.WithLiveOutputSink(ctx, func(buf tools.LiveOutputBuffer) {
			t.coordinator.asyncJobs.setRunCommandBuffer(job, buf)
		})
	}
	response, err := t.inner.Run(ctx, call)
	if err != nil {
		completion.IsError = true
		completion.Content = err.Error()
		return
	}
	completion.IsError = response.IsError
	completion.Content = response.Content
	completion.Metadata = response.Metadata
	if t.name == tools.BashToolName && !sync {
		t.awaitShell(ctx, job, sessionID, response, &completion)
	}
	if t.name == tools.BashToolName && sync {
		t.claimBackgroundShellRow(job, sessionID, response, &completion)
	}
	completion.Content = tools.TruncateOutput(strings.TrimSpace(completion.Content))
}

// finalize delivers one async tool's completion. For the delegation tools
// with a child session id (`agent`, `agentic_fetch`) the child's Run()
// returning is only the end of a model TURN: the child may have started its
// own async tools or background shells and merely yielded. Those are armed
// (see work_ledger_delegation.go) and released exactly once when the
// child's own work is terminal, so the parent is never told "finished"
// over content that says the child is still waiting.
//
// Every other tool — bash, run_command — has no child session and no
// self-directed follow-on work, so its completion is finished immediately,
// exactly as before.
//
// job is the executor's own ledger entry (A11): the completion is reported
// for THAT job only, never for whatever job the key names by the time the
// executor returns.
func (t *asyncTool) finalize(job *asyncJob, childSessionID string, completion AsyncCompletion) {
	if t.coordinator == nil || t.coordinator.asyncJobs == nil {
		return
	}
	result := jobResult{content: completion.Content, isError: completion.IsError, metadata: completion.Metadata}
	if childSessionID != "" && (t.name == AgentToolName || t.name == tools.AgenticFetchToolName) {
		t.coordinator.asyncJobs.armDelegation(job, result)
		return
	}
	t.coordinator.asyncJobs.finish(job, result)
}

func (t *asyncTool) awaitShell(ctx context.Context, job *asyncJob, sessionID string, response fantasy.ToolResponse, completion *AsyncCompletion) {
	var metadata tools.BashResponseMetadata
	if json.Unmarshal([]byte(response.Metadata), &metadata) != nil || metadata.ShellID == "" {
		return
	}
	// Task #1053: record the shell id now, while the job is still running,
	// so job_kill/job_output can resolve the job id the model saw to it.
	// Before this the shell id stayed inside this goroutine until the job
	// was already terminal, making both tools unusable for a live job.
	t.coordinator.asyncJobs.setShellID(job, metadata.ShellID)
	manager := t.coordinator.background
	if manager == nil {
		completion.IsError = true
		completion.Content = "background shell manager is unavailable"
		return
	}
	sh, ok := manager.GetOwned(sessionID, metadata.ShellID)
	if !ok {
		completion.IsError = true
		completion.Content = fmt.Sprintf("background shell %s is no longer available", metadata.ShellID)
		return
	}
	if !sh.WaitContext(ctx) {
		killCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		_ = manager.KillOwned(killCtx, sessionID, sh.ID)
		cancel()
	}
	stdout, stderr, _, runErr := sh.GetOutput()
	completion.IsError = runErr != nil || shell.ExitCode(runErr) != 0
	completion.Content = backgroundJobSummary(sh.ID, sh.Command, stdout, stderr, shell.ExitCode(runErr), sh.Elapsed())
}

// timeoutSecondsFloor/timeoutSecondsCeil bound timeout.seconds (contract
// §3/§6). The ceiling is a typo/footgun guard, not a product limit --
// changed by editing this constant, not by product policy.
const (
	timeoutSecondsFloor = 5
	timeoutSecondsCeil  = 604800 // 7 days
)

// timeoutParamInput is the generic, tool-agnostic shape parseTimeoutParam
// reads from raw call.Input -- the same pattern childSessionID already uses
// for AgentParams and t.run uses for run_in_background.
type timeoutParamInput struct {
	// Raw so a bare-number timeout (accepted since #1159) survives this
	// generic re-parse instead of being silently dropped here.
	Timeout json.RawMessage `json:"timeout,omitempty"`
	// TimeoutSeconds mirrors run_command.RunCommandParams.TimeoutSeconds --
	// read generically here (like Timeout above) rather than importing the
	// typed struct, so this function stays tool-agnostic. Zero for
	// bash/agent (they have no such field; a stray value would mean the
	// model sent an unknown field, silently ignored like any other
	// unexpected JSON key).
	TimeoutSeconds int `json:"timeout_seconds,omitempty"`
}

// parseTimeoutParam extracts and validates the optional explicit timeout for
// bash/run_command/agent (NOT agentic_fetch -- its own HTTP client already
// carries a fixed timeout, and it does not describe this field in its JSON
// schema). A bare JSON number is accepted as {"seconds": N, "kind": "wake_only"}.
// Both new timeout{} and legacy run_command.timeout_seconds set on
// the SAME call is a validation error (wake-tools-contract.md §3): the
// legacy field is a "terminate_and_wake" ALIAS, not an independent axis, so
// both present is an unresolvable ambiguity, not a silent precedence rule.
func parseTimeoutParam(toolName, input string) (*TimeoutSpec, error) {
	if toolName != tools.BashToolName && toolName != tools.RunCommandToolName && toolName != AgentToolName {
		return nil, nil
	}
	var parsed timeoutParamInput
	if err := json.Unmarshal([]byte(input), &parsed); err != nil {
		return nil, nil
	}
	raw := bytes.TrimSpace(parsed.Timeout)
	timeoutSet := len(raw) > 0 && string(raw) != "null"
	if timeoutSet && parsed.TimeoutSeconds != 0 {
		return nil, fmt.Errorf("provide at most one of timeout or the legacy timeout_seconds, not both")
	}
	if !timeoutSet {
		if parsed.TimeoutSeconds == 0 {
			return nil, nil
		}
		// Legacy alias: run_command.timeout_seconds's OWN validation
		// (runCommandDefaultTimeoutSeconds/runCommandMaxTimeoutSeconds
		// clamp, tools/run_command.go) is untouched and runs separately, as
		// it does today, for the process-kill mechanism itself -- this only
		// additionally records the SAME deadline in the ledger so the owner
		// gets a distinguishable timed_out outcome instead of today's
		// generic failed.
		return &TimeoutSpec{
			Deadline: time.Now().Add(time.Duration(parsed.TimeoutSeconds) * time.Second),
			Kind:     timeoutTerminateAndWake,
			Seconds:  parsed.TimeoutSeconds,
		}, nil
	}
	const shape = `timeout must be an object {"seconds": N, "kind": "wake_only"|"terminate_and_wake"} or a bare number of seconds`
	var (
		seconds int
		kind    string
	)
	if raw[0] == '{' {
		var obj struct {
			Seconds int    `json:"seconds"`
			Kind    string `json:"kind"`
		}
		if err := json.Unmarshal(raw, &obj); err != nil {
			return nil, fmt.Errorf("%s: %s", shape, err)
		}
		seconds, kind = obj.Seconds, obj.Kind
	} else {
		if err := json.Unmarshal(raw, &seconds); err != nil {
			return nil, fmt.Errorf("%s, got %s", shape, raw)
		}
		// Bare number: the safe kind -- the deadline never kills the work.
		kind = "wake_only"
	}
	if seconds < timeoutSecondsFloor || seconds > timeoutSecondsCeil {
		return nil, fmt.Errorf("timeout.seconds must be between %d and %d (7 days), got %d", timeoutSecondsFloor, timeoutSecondsCeil, seconds)
	}
	var tk timeoutKind
	switch kind {
	case "wake_only":
		tk = timeoutWakeOnly
	case "terminate_and_wake":
		tk = timeoutTerminateAndWake
	default:
		return nil, fmt.Errorf("timeout.kind must be %q or %q, got %q", "wake_only", "terminate_and_wake", kind)
	}
	return &TimeoutSpec{
		Deadline: time.Now().Add(time.Duration(seconds) * time.Second),
		Kind:     tk,
		Seconds:  seconds,
	}, nil
}

func (c *coordinator) wrapAsyncTools(list []fantasy.AgentTool) []fantasy.AgentTool {
	if c.asyncJobs == nil {
		return list
	}
	for i, tool := range list {
		switch tool.Info().Name {
		case tools.BashToolName, tools.RunCommandToolName, AgentToolName, tools.AgenticFetchToolName:
			list[i] = &asyncTool{inner: tool, coordinator: c, name: tool.Info().Name}
		}
	}
	return list
}

// failLaunch ends a job whose executor never started: straight to a failed
// outcome carrying msg. It deliberately bypasses the delegation re-check
// (armDelegation -> recheckChild), whose refresh would read a resumed
// child's OLD last message as this call's result while the tool result says
// "failed to start" (R2B-14).
func (t *asyncTool) failLaunch(job *asyncJob, msg string) {
	if t.coordinator == nil || t.coordinator.asyncJobs == nil {
		return
	}
	t.coordinator.asyncJobs.finish(job, jobResult{content: msg, isError: true})
}

// failedStartResponse is the error response of a launch that panicked. When
// the job has a durable claim it carries the claim tag, so the ack gate
// treats it as the job's own result (see ackTag); a sync job has no row to
// acknowledge and gets the plain error.
func failedStartResponse(msg, claimID string) fantasy.ToolResponse {
	resp := fantasy.NewTextErrorResponse(msg)
	if claimID == "" {
		return resp
	}
	return fantasy.WithResponseMetadata(resp, ackTag{ClaimID: claimID})
}
