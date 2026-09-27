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
	if origin != message.OriginCLI && origin != message.OriginWeb {
		return t.inner.Run(ctx, call)
	}
	sessionID := tools.GetSessionFromContext(ctx)
	if sessionID == "" || call.ID == "" {
		return fantasy.ToolResponse{}, fmt.Errorf("async %s requires session and tool call IDs", t.name)
	}
	childSessionID, err := t.childSessionID(ctx, sessionID, call)
	if err != nil {
		return fantasy.NewTextErrorResponse(err.Error()), nil
	}
	jobCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	job, existing, err := t.coordinator.asyncJobs.Start(sessionID, call.ID, call.Input, t.name, childSessionID, origin == message.OriginCLI, cancel)
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
		return t.startedResponse(call.ID, job.childSession), nil
	}
	if childSessionID != "" && t.coordinator.permissions != nil {
		t.coordinator.permissions.InheritSessionAutoApprove(sessionID, childSessionID)
		if mgr, ok := t.coordinator.permissions.(permission.SessionRunAllowlistManager); ok {
			mgr.InheritSessionRunAllowlist(sessionID, childSessionID)
		}
	}
	go t.run(jobCtx, cancel, sessionID, childSessionID, call)
	return t.startedResponse(call.ID, childSessionID), nil
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

func (t *asyncTool) run(ctx context.Context, cancel context.CancelFunc, sessionID, childSessionID string, call fantasy.ToolCall) {
	defer cancel()
	if childSessionID != "" && t.coordinator.permissions != nil {
		if mgr, ok := t.coordinator.permissions.(permission.SessionRunAllowlistManager); ok {
			defer mgr.ClearSessionRunAllowlist(childSessionID)
		}
	}
	completion := AsyncCompletion{SessionID: sessionID, ToolCallID: call.ID, ToolName: t.name}
	defer func() {
		if recovered := recover(); recovered != nil {
			completion.IsError = true
			completion.Content = fmt.Sprintf("async %s panicked: %v", t.name, recovered)
		}
		t.finalize(ctx, sessionID, childSessionID, completion)
	}()
	if t.name == tools.BashToolName {
		ctx = tools.WithoutBackgroundCallback(ctx)
		var params map[string]json.RawMessage
		if json.Unmarshal([]byte(call.Input), &params) == nil && params != nil {
			params["run_in_background"] = json.RawMessage("true")
			if input, err := json.Marshal(params); err == nil {
				call.Input = string(input)
			}
		}
	}
	response, err := t.inner.Run(ctx, call)
	if err != nil {
		completion.IsError = true
		completion.Content = err.Error()
		return
	}
	completion.IsError = response.IsError
	completion.Content = response.Content
	if t.name == tools.BashToolName {
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
	result := jobResult{content: completion.Content, isError: completion.IsError}
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
