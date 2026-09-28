package agent

import (
	"context"
	"encoding/json"
	"fmt"
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
	Async          bool   `json:"async"`
	JobID          string `json:"job_id"`
	ChildSessionID string `json:"child_session_id,omitempty"`
	Status         string `json:"status"`
}

func (t *asyncTool) Info() fantasy.ToolInfo {
	info := t.inner.Info()
	info.Description += "\n\nIn web and CLI sessions this tool starts asynchronously. It returns a job ID immediately; Rush sends the result as a new session message and resumes the agent. Continue independent work instead of blocking or polling for this job."
	return info
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
	timeoutSpec, err := parseTimeoutParam(t.name, call.Input)
	if err != nil {
		return fantasy.NewTextErrorResponse(err.Error()), nil
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
		return t.startedResponse(call.ID, job.childSession), nil
	}
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
	go t.run(jobCtx, cancel, sessionID, childSessionID, call, sync)
	if sync {
		return t.awaitAndFinish(ctx, job)
	}
	return t.startedResponse(call.ID, childSessionID), nil
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
func (t *asyncTool) startedResponse(jobID, childSessionID string) fantasy.ToolResponse {
	content := fmt.Sprintf("Async %s job %s started. Its result will arrive as a new session message; continue independent work.", t.name, jobID)
	return fantasy.WithResponseMetadata(fantasy.NewTextResponse(content), asyncToolMetadata{
		Async: true, JobID: jobID, ChildSessionID: childSessionID, Status: "running",
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

func (t *asyncTool) run(ctx context.Context, cancel context.CancelFunc, sessionID, childSessionID string, call fantasy.ToolCall, sync bool) {
	defer cancel()
	// No `defer ClearSessionRunAllowlist` here (phase 2, §6.2): the child
	// may still own async jobs/background shells after this turn returns
	// (structural concurrency, #1049), and a woken turn re-inherits from
	// subAgentDriver.parentSessionID on every wake (wakeSession's
	// wakeNoticeCall) instead -- clearing on return would strand a woken
	// turn under the process-wide gate.
	completion := AsyncCompletion{SessionID: sessionID, ToolCallID: call.ID, ToolName: t.name}
	defer func() {
		if recovered := recover(); recovered != nil {
			completion.IsError = true
			completion.Content = fmt.Sprintf("async %s panicked: %v", t.name, recovered)
		}
		t.finalize(ctx, sessionID, childSessionID, completion)
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
			t.coordinator.asyncJobs.setRunCommandBuffer(sessionID, call.ID, buf)
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
		t.awaitShell(ctx, sessionID, response, &completion)
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
func (t *asyncTool) finalize(_ context.Context, _, childSessionID string, completion AsyncCompletion) {
	if t.coordinator == nil || t.coordinator.asyncJobs == nil {
		return
	}
	result := jobResult{content: completion.Content, isError: completion.IsError, metadata: completion.Metadata}
	if childSessionID != "" && (t.name == AgentToolName || t.name == tools.AgenticFetchToolName) {
		t.coordinator.asyncJobs.armDelegation(completion.SessionID, completion.ToolCallID, result)
		return
	}
	t.coordinator.asyncJobs.finish(completion.SessionID, completion.ToolCallID, result)
}

func (t *asyncTool) awaitShell(ctx context.Context, sessionID string, response fantasy.ToolResponse, completion *AsyncCompletion) {
	var metadata tools.BashResponseMetadata
	if json.Unmarshal([]byte(response.Metadata), &metadata) != nil || metadata.ShellID == "" {
		return
	}
	// Task #1053: record the shell id now, while the job is still running,
	// so job_kill/job_output can resolve the job id the model saw to it.
	// Before this the shell id stayed inside this goroutine until the job
	// was already terminal, making both tools unusable for a live job.
	t.coordinator.asyncJobs.setShellID(sessionID, completion.ToolCallID, metadata.ShellID)
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
	Timeout *struct {
		Seconds int    `json:"seconds"`
		Kind    string `json:"kind"`
	} `json:"timeout,omitempty"`
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
// schema). Both new timeout{} and legacy run_command.timeout_seconds set on
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
	if parsed.Timeout != nil && parsed.TimeoutSeconds != 0 {
		return nil, fmt.Errorf("provide at most one of timeout or the legacy timeout_seconds, not both")
	}
	if parsed.Timeout == nil {
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
	t := parsed.Timeout
	if t.Seconds < timeoutSecondsFloor || t.Seconds > timeoutSecondsCeil {
		return nil, fmt.Errorf("timeout.seconds must be between %d and %d (7 days), got %d", timeoutSecondsFloor, timeoutSecondsCeil, t.Seconds)
	}
	var kind timeoutKind
	switch t.Kind {
	case "wake_only":
		kind = timeoutWakeOnly
	case "terminate_and_wake":
		kind = timeoutTerminateAndWake
	default:
		return nil, fmt.Errorf("timeout.kind must be %q or %q, got %q", "wake_only", "terminate_and_wake", t.Kind)
	}
	return &TimeoutSpec{
		Deadline: time.Now().Add(time.Duration(t.Seconds) * time.Second),
		Kind:     kind,
		Seconds:  t.Seconds,
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
