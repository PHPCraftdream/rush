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
	if err := t.coordinator.asyncJobs.start(sessionID, call.ID, origin == message.OriginCLI, cancel); err != nil {
		cancel()
		return fantasy.NewTextErrorResponse(err.Error()), nil
	}
	if childSessionID != "" && t.coordinator.permissions != nil {
		t.coordinator.permissions.InheritSessionAutoApprove(sessionID, childSessionID)
		if mgr, ok := t.coordinator.permissions.(permission.SessionRunAllowlistManager); ok {
			mgr.InheritSessionRunAllowlist(sessionID, childSessionID)
		}
	}
	go t.run(jobCtx, cancel, sessionID, childSessionID, call)
	content := fmt.Sprintf("Async %s job %s started. Its result will arrive as a new session message; continue independent work.", t.name, call.ID)
	return fantasy.WithResponseMetadata(fantasy.NewTextResponse(content), asyncToolMetadata{
		Async: true, JobID: call.ID, ChildSessionID: childSessionID, Status: "running",
	}), nil
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
		t.coordinator.asyncJobs.finish(completion)
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

func (t *asyncTool) awaitShell(ctx context.Context, sessionID string, response fantasy.ToolResponse, completion *AsyncCompletion) {
	var metadata tools.BashResponseMetadata
	if json.Unmarshal([]byte(response.Metadata), &metadata) != nil || metadata.ShellID == "" {
		return
	}
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
