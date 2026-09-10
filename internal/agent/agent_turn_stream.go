package agent

import (
	"context"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"charm.land/fantasy"
	"charm.land/fantasy/providers/anthropic"
	"charm.land/fantasy/providers/google"
	"charm.land/fantasy/providers/openai"

	"github.com/PHPCraftdream/rush/internal/message"
	"github.com/PHPCraftdream/rush/internal/session"
)

// compositionPhase replaces runTurn's original bare bool sawToolBoundary.
// The two states are exactly the two the bool encoded; this only makes the
// callback code that flips it self-documenting at each of its five call
// sites (OnTextDelta, OnToolInputStart, OnToolCall, OnToolResult,
// OnStepFinish — one more than an earlier pass over this function counted,
// since OnToolInputStart is also a boundary).
type compositionPhase int

const (
	// phaseToolBoundary means the stream just crossed a tool call/result
	// boundary, or a step just finished: the NEXT text delta starts a new
	// "final composition" span and should log its start.
	phaseToolBoundary compositionPhase = iota
	// phaseComposing means the current span's start was already logged;
	// further text deltas append silently.
	phaseComposing
)

// turnStreamConfig groups newTurnStream's inputs. Several are identically
// typed func() callbacks (bumpActivity/toolStarted/toolFinished/
// startCheckpoint/stopCheckpoint); a plain struct literal with field names
// prevents them from being silently transposed the way same-typed
// constructor positional arguments could be.
type turnStreamConfig struct {
	a      *sessionAgent
	ctx    context.Context
	genCtx context.Context
	cancel context.CancelFunc
	call   SessionAgentCall
	genID  uint64

	smartModel     Model
	promptPrefix   string
	historyIDs     map[string]struct{}
	currentSession session.Session

	bumpActivity    func()
	toolStarted     func()
	toolFinished    func()
	startCheckpoint func()
	stopCheckpoint  func()
	notifyUI        func() error
	drainPendingUI  func()

	setPeakHoursAbortErr func(error) bool

	wd               streamWatchdog
	watchdogCauseVal *atomic.Int32
	toolMaxDuration  time.Duration
	timeoutHardCap   time.Duration
	idleTimeout      time.Duration
}

// turnStream is runTurn's fantasy.AgentStreamCall callback state, all in one
// type because the callbacks do not divide along topic (streaming vs. tools
// vs. step-lifecycle) the way their names suggest — they divide along
// OWNERSHIP instead. An earlier plan gave each theme its own type
// (streamRouter/toolsRouter/stepRouter); it was dropped because the theme
// boundary does not match the ownership boundary — e.g. compositionPhase
// above is written from five callbacks spanning all three proposed themes.
//
// Fields fall into three groups:
//
//   - callback-sequence only, no lock: fantasy invokes every callback for
//     one agent.Stream call sequentially from a single loop, never
//     concurrently with each other, so state touched only from inside
//     these methods needs no lock. currentSession belongs here too — it
//     looks turn-wide-shared, but nothing outside this callback sequence
//     ever reads or writes it (the ticker goroutines below only touch
//     currentAssistant), so the original code never locked it either.
//   - shared with this turn's ticker goroutines (turnCheckpointWriter,
//     turnUINotifier, peakHoursWatcher) and so always touched under mu:
//     currentAssistant.
//   - stepMessages/stepTools: written under mu in prepareStep but read
//     without mu in onStepFinish, preserved exactly as the original
//     runTurn had it. This asymmetry is a separate, already-tracked
//     finding (task #941), not something this mechanical split changes.
type turnStream struct {
	a      *sessionAgent
	ctx    context.Context // outer ctx: survives genCtx's cancellation, used where a write must land even mid-cancel
	genCtx context.Context // this turn's cancelable ctx
	cancel context.CancelFunc
	call   SessionAgentCall
	genID  uint64

	smartModel   Model
	promptPrefix string

	historyIDs map[string]struct{}

	bumpActivity    func()
	toolStarted     func()
	toolFinished    func()
	startCheckpoint func()
	stopCheckpoint  func()
	notifyUI        func() error
	drainPendingUI  func()

	setPeakHoursAbortErr func(error) bool

	wd               streamWatchdog
	watchdogCauseVal *atomic.Int32
	toolMaxDuration  time.Duration
	timeoutHardCap   time.Duration
	idleTimeout      time.Duration

	// callback-sequence-only state — see doc above.
	phase               compositionPhase
	stepHistory         []fantasy.StepResult
	loopDetected        bool
	loopDetail          loopDetail
	sanitizedToolCalls  map[string]bool
	shouldSummarize     bool
	silentCompactNeeded bool
	currentSession      session.Session

	// currentAssistant is shared with the ticker goroutines; every touch,
	// from ANY goroutine including these callbacks, holds mu.
	mu               sync.Mutex
	currentAssistant *message.Message

	// See the struct doc's third bullet: asymmetric locking preserved as-is.
	stepMessages []fantasy.Message
	stepTools    []fantasy.AgentTool
}

func newTurnStream(cfg turnStreamConfig) *turnStream {
	return &turnStream{
		a:                    cfg.a,
		ctx:                  cfg.ctx,
		genCtx:               cfg.genCtx,
		cancel:               cfg.cancel,
		call:                 cfg.call,
		genID:                cfg.genID,
		smartModel:           cfg.smartModel,
		promptPrefix:         cfg.promptPrefix,
		historyIDs:           cfg.historyIDs,
		currentSession:       cfg.currentSession,
		bumpActivity:         cfg.bumpActivity,
		toolStarted:          cfg.toolStarted,
		toolFinished:         cfg.toolFinished,
		startCheckpoint:      cfg.startCheckpoint,
		stopCheckpoint:       cfg.stopCheckpoint,
		notifyUI:             cfg.notifyUI,
		drainPendingUI:       cfg.drainPendingUI,
		setPeakHoursAbortErr: cfg.setPeakHoursAbortErr,
		wd:                   cfg.wd,
		watchdogCauseVal:     cfg.watchdogCauseVal,
		toolMaxDuration:      cfg.toolMaxDuration,
		timeoutHardCap:       cfg.timeoutHardCap,
		idleTimeout:          cfg.idleTimeout,
		phase:                phaseToolBoundary,
		sanitizedToolCalls:   make(map[string]bool),
	}
}

// streamCall builds the fantasy.AgentStreamCall for this turn's agent.Stream
// invocation. Once every callback is a turnStream method value instead of a
// closure, this is the whole "router": one flat struct literal, no branching.
func (ts *turnStream) streamCall(history []fantasy.Message, files []fantasy.FilePart, maxOutputTokens *int64) fantasy.AgentStreamCall {
	return fantasy.AgentStreamCall{
		Prompt:           message.PromptWithTextAttachments(ts.call.Prompt, ts.call.Attachments),
		Files:            files,
		Messages:         history,
		Headers:          sessionHeaders(ts.call.SessionID),
		ProviderOptions:  ts.call.ProviderOptions,
		MaxOutputTokens:  maxOutputTokens,
		TopP:             ts.call.TopP,
		Temperature:      ts.call.Temperature,
		PresencePenalty:  ts.call.PresencePenalty,
		TopK:             ts.call.TopK,
		FrequencyPenalty: ts.call.FrequencyPenalty,
		PrepareStep:      ts.prepareStep,
		OnReasoningStart: ts.onReasoningStart,
		OnReasoningDelta: ts.onReasoningDelta,
		OnReasoningEnd:   ts.onReasoningEnd,
		OnTextDelta:      ts.onTextDelta,
		OnToolInputStart: ts.onToolInputStart,
		OnToolInputDelta: ts.onToolInputDelta,
		OnToolInputEnd:   ts.onToolInputEnd,
		OnRetry:          ts.onRetry,
		OnWarnings:       ts.onWarnings,
		OnToolCall:       ts.onToolCall,
		OnToolResult:     ts.onToolResult,
		OnStepFinish:     ts.onStepFinish,
		StopWhen:         ts.stopConditions(),
	}
}

func (ts *turnStream) onReasoningStart(id string, reasoning fantasy.ReasoningContent) error {
	ts.bumpActivity()
	slog.Debug("agent: OnReasoningStart called", "id", id)
	ts.mu.Lock()
	ts.currentAssistant.AppendReasoningContent(reasoning.Text)
	snap := ts.currentAssistant.Clone()
	ts.mu.Unlock()
	return ts.a.messages.Update(ts.genCtx, snap)
}

func (ts *turnStream) onReasoningDelta(id string, text string) error {
	ts.bumpActivity()
	slog.Debug("agent: OnReasoningDelta called", "len", len(text))
	ts.mu.Lock()
	ts.currentAssistant.AppendReasoningContent(text)
	ts.mu.Unlock()
	return ts.notifyUI()
}

func (ts *turnStream) onReasoningEnd(id string, reasoning fantasy.ReasoningContent) error {
	ts.bumpActivity()
	ts.mu.Lock()
	// handle anthropic signature
	if anthropicData, ok := reasoning.ProviderMetadata[anthropic.Name]; ok {
		if reasoning, ok := anthropicData.(*anthropic.ReasoningOptionMetadata); ok {
			ts.currentAssistant.AppendReasoningSignature(reasoning.Signature)
		}
	}
	if googleData, ok := reasoning.ProviderMetadata[google.Name]; ok {
		if reasoning, ok := googleData.(*google.ReasoningMetadata); ok {
			ts.currentAssistant.AppendThoughtSignature(reasoning.Signature, reasoning.ToolID)
		}
	}
	if openaiData, ok := reasoning.ProviderMetadata[openai.Name]; ok {
		if reasoning, ok := openaiData.(*openai.ResponsesReasoningMetadata); ok {
			ts.currentAssistant.SetReasoningResponsesData(reasoning)
		}
	}
	ts.currentAssistant.FinishThinking()
	snap := ts.currentAssistant.Clone()
	ts.mu.Unlock()
	return ts.a.messages.Update(ts.genCtx, snap)
}

func (ts *turnStream) onTextDelta(id string, text string) error {
	ts.bumpActivity()
	// Fork patch: batch 8 — start the checkpoint ticker on the
	// first text delta of this step (lazily, once only).
	ts.startCheckpoint()
	ts.mu.Lock()
	// Fork patch: batch 8 — emit final-composition log at most
	// once per step, on the first text delta after a tool boundary.
	if ts.phase == phaseToolBoundary && ts.currentAssistant != nil {
		ts.phase = phaseComposing
		slog.Info(
			"agent: final composition started",
			"session_id", ts.call.SessionID,
			"message_id", ts.currentAssistant.ID,
			"chars_in_message_so_far", len(ts.currentAssistant.FullText()),
		)
	}
	// Strip leading newline from initial text content. This is is
	// particularly important in non-interactive mode where leading
	// newlines are very visible.
	if len(ts.currentAssistant.Parts) == 0 {
		text = strings.TrimPrefix(text, "\n")
	}

	ts.currentAssistant.AppendContent(text)
	ts.mu.Unlock()
	return ts.notifyUI()
}

func (ts *turnStream) onToolInputStart(id string, toolName string) error {
	ts.bumpActivity()
	ts.phase = phaseToolBoundary // Fork patch: batch 8
	toolCall := message.ToolCall{
		ID:               id,
		Name:             toolName,
		ProviderExecuted: false,
		Finished:         false,
	}
	ts.mu.Lock()
	ts.currentAssistant.AddToolCall(toolCall)
	snap := ts.currentAssistant.Clone()
	ts.mu.Unlock()
	// Use parent ctx instead of genCtx to ensure the update succeeds
	// even if the request is canceled mid-stream
	return ts.a.messages.Update(ts.ctx, snap)
}

func (ts *turnStream) onToolInputDelta(id string, delta string) error {
	ts.bumpActivity()
	ts.mu.Lock()
	ts.currentAssistant.AppendToolCallInput(id, delta)
	ts.mu.Unlock()
	return nil // don't spam DB on every delta; ToolInputEnd will persist
}

func (ts *turnStream) onToolInputEnd(id string) error {
	ts.bumpActivity()
	ts.mu.Lock()
	ts.currentAssistant.FinishToolCall(id)
	snap := ts.currentAssistant.Clone()
	ts.mu.Unlock()
	return ts.a.messages.Update(ts.genCtx, snap)
}

func (ts *turnStream) onRetry(err *fantasy.ProviderError, delay time.Duration) {
	ts.bumpActivity()
	slog.Warn("Provider request failed, retrying", providerRetryLogFields(err, delay)...)
}

func (ts *turnStream) onWarnings(warnings []fantasy.CallWarning) error {
	for _, w := range warnings {
		slog.Warn("Provider warning", "type", w.Type, "message", w.Message)
	}
	return nil
}

func (ts *turnStream) onToolCall(tc fantasy.ToolCallContent) error {
	ts.bumpActivity()
	// A tool is about to execute — pause the stall watchdog until its
	// result arrives (OnToolResult). fantasy fires every OnToolCall
	// for a step before executing any tool, so the counter brackets
	// the whole executeTools window. The same toolMaxDuration cap
	// bounds every tool, including a sub-agent delegation (the
	// `agent` tool) — see toolExecutionMaxDefault's doc in agent.go.
	ts.toolStarted()
	ts.phase = phaseToolBoundary // Fork patch: batch 8
	input, wasSanitized := sanitizeToolInput(tc.ToolName, tc.ToolCallID, tc.Input)
	if wasSanitized {
		ts.sanitizedToolCalls[tc.ToolCallID] = true
	}
	toolCall := message.ToolCall{
		ID:               tc.ToolCallID,
		Name:             tc.ToolName,
		Input:            input,
		ProviderExecuted: false,
		Finished:         true,
	}
	ts.mu.Lock()
	ts.currentAssistant.AddToolCall(toolCall)
	snap := ts.currentAssistant.Clone()
	ts.mu.Unlock()
	// Use parent ctx instead of genCtx to ensure the update succeeds
	// even if the request is canceled mid-stream
	return ts.a.messages.Update(ts.ctx, snap)
}

func (ts *turnStream) onToolResult(result fantasy.ToolResultContent) error {
	ts.bumpActivity()
	// Tool finished — resume the stall watchdog (and restart its idle
	// window so the tool's runtime isn't counted against the provider).
	ts.toolFinished()
	ts.phase = phaseToolBoundary // Fork patch: batch 8
	toolResult := ts.a.convertToToolResult(result)
	if ts.sanitizedToolCalls[result.ToolCallID] {
		toolResult.Content = "Tool call failed: arguments were not valid JSON. Please check your tool call format and try again."
		toolResult.IsError = true
	}
	ts.mu.Lock()
	sessionID := ts.currentAssistant.SessionID
	ts.mu.Unlock()
	// Use parent ctx instead of genCtx to ensure the message is created
	// even if the request is canceled mid-stream
	_, createMsgErr := ts.a.messages.Create(ts.ctx, sessionID, message.CreateMessageParams{
		Role: message.Tool,
		Parts: []message.ContentPart{
			toolResult,
		},
	})
	return createMsgErr
}
