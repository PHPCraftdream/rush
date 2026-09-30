// ─── Shared types mirroring Go structs ────────────────────────────────────────

export type TodoStatus = "pending" | "in_progress" | "completed";

export interface Todo {
  content: string;
  status: TodoStatus;
  active_form?: string;
}

export interface Session {
  ID: string;
  ParentSessionID: string;
  Title: string;
  MessageCount: number;
  PromptTokens: number;
  CompletionTokens: number;
  SummaryMessageID: string;
  Cost: number;
  Todos: Todo[];
  CreatedAt: number;
  UpdatedAt: number;
  CWD: string;

  SmartModelProvider: string;
  SmartModelID: string;
  SmartModelReasoningEffort: string; // "low", "medium", "high", or "max"
  FastModelProvider: string;
  FastModelID: string;
  FastModelReasoningEffort: string; // "low", "medium", "high", or "max"

  // Optional sub-agent model slots (task #466). Empty means "inherit the
  // folder/system default" — same convention as smart/fast above.
  WorkerModelProvider: string;
  WorkerModelID: string;
  WorkerModelReasoningEffort: string;
  ReviewerModelProvider: string;
  ReviewerModelID: string;
  ReviewerModelReasoningEffort: string;

  SystemPrompt: string;
  YoloEnabled: boolean;

  // Set by the server when another live rush process holds the session
  // lock (heartbeat fresher than ~20s). When true, the UI flips the
  // session into a read-only "follow" mode: input disabled, controls
  // hidden, banner shown, polling drives live updates instead of pubsub.
  OwnedExternal?: boolean;
  OwnedByPID?: number;
  // True when the session has a running delegation row (async_jobs, a
  // sub-agent or agentic_fetch) below it, at any depth, on a host that is
  // not provably dead: the session is delegating, not done, even with no
  // session lock held.
  HasLiveDescendantWork?: boolean;
  // Descendant session IDs those running delegation rows name (deduplicated).
  LiveDescendantIDs?: string[];
  // True when the session is not finished although it holds no lock: it owns
  // a running background job (bash/run_command) on a host that is not
  // provably dead, or a live `rush run` loop drives it between turns. Does
  // not imply that a job exists.
  HasLiveOwnWork?: boolean;
}

export type MessageRole = "user" | "assistant" | "tool" | "system";

export interface TextContent {
  type: "text";
  Text: string;
}

export interface ReasoningContent {
  type: "thinking";
  Thinking: string;
}

export interface ToolCall {
  type: "tool_call";
  ID: string;
  Name: string;
  Input: string;
  Finished: boolean;
}

export interface ToolResult {
  type: "tool_result";
  ToolCallID: string;
  Name: string;
  Content: string;
  IsError: boolean;
  Metadata?: string;
}

export interface FinishPart {
  type: "finish";
  Reason: string;
  Message: string;
  Details: string;
  // True when this Finish was written by the server's auto-checkpoint
  // ticker mid-stream, not a real step-end (mirrors message.Finish.Partial
  // / PartWire.Partial). A message whose only Finish part has Partial=true
  // is still owned by an in-flight agent turn: editing it is refused
  // server-side (task #590) because the turn's next checkpoint or terminal
  // write will silently overwrite the edit.
  Partial?: boolean;
}

export type ContentPart =
  | TextContent
  | ReasoningContent
  | ToolCall
  | ToolResult
  | FinishPart;

export interface Message {
  ID: string;
  SessionID: string;
  Role: MessageRole;
  Parts: ContentPart[];
  Model: string;
  Provider: string;
  ReasoningEffort: string; // "low", "medium", "high", or "max" - for Claude models
  CreatedAt: number;
  UpdatedAt: number;
  IsSummaryMessage: boolean;
  Pinned: boolean;
  Hidden: boolean;
  AutoResumed: boolean;
  BackgroundJobNotice: boolean;
  // Entry channel the message arrived through ("cli" / "web" / "sdk").
  // Mirrors internal/server/wire.go's MessageWire.Origin (message.Origin:
  // CLI `rush run` prompts, `rush sessions inject`, SDK runs). Absent from
  // rows written before the origin column existed and from older servers —
  // both mean "unspecified", which is NOT composer-typed input, so the
  // composer recall history ($myPrompts in web/src/store.ts) requires
  // exactly "web".
  Origin?: string;
  // Mirrors message.Message.NoticeKind / MessageWire.NoticeKind: "" (or
  // absent, from an older server) for an ordinary message, otherwise the
  // kind of system notice (e.g. "supervision", "timeout_wake_only").
  NoticeKind?: string;
  // HumanTyped is computed server-side (internal/server/wire.go's
  // isHumanTyped) from structured fields ONLY -- never message text. True
  // only when a human typed this directly into the web composer. This is
  // the SOLE field the composer recall history and history dropdown
  // ($myPrompts below) filter on; absent from an older server means false
  // (unspecified is not composer-typed input either).
  HumanTyped?: boolean;
  // Per-message token accounting. Absent on messages written before
  // per-message tracking existed, and on non-assistant messages.
  Usage?: MessageUsage;
  // DeleteGeneration is the delete watermark (task #737; replaces the old
  // RowID field — internal/server/wire.go's MessageWire.DeleteGeneration
  // doc comment has the full mechanism). Only ever populated on a
  // message_deleted push's payload; absent/0 everywhere else (created/
  // updated pushes and messages_list snapshot rows never carry it).
  DeleteGeneration?: number;
}

// MessageUsage mirrors internal/server/wire.go's UsageWire.
//
// Tokens are split into three DISJOINT classes, so PromptTokens is their sum:
// InputTokens (fresh, full price), CacheReadTokens (served from the provider's
// prompt cache, much cheaper) and CacheCreationTokens (written into it).
export interface MessageUsage {
  InputTokens: number;
  OutputTokens: number;
  ReasoningTokens?: number;
  CacheCreationTokens: number;
  CacheReadTokens: number;
  PromptTokens: number;
  TotalTokens: number;
  CostUSD: number;
  Provider?: string;
  Model?: string;
  CacheSupport?: string;
  // null when the provider does not report caching. Render "n/a" — a
  // fabricated 0% is indistinguishable from a genuine cache miss.
  CacheHitRatio: number | null;
  // True when the provider sent no usage and the numbers were derived from
  // message lengths. Approximate; must not be presented as measured.
  Estimated?: boolean;
}

export interface ModelInfo {
  id: string;
  name: string;
  provider: string;
}

export interface ProviderInfo {
  name?: string;
  enabled?: boolean;
  type?: string;
  models?: { id: string; name: string; contextWindow?: number }[];
  baseUrl?: string;
  isCustom?: boolean;
  apiKeySet?: boolean;
  // Effective config scope: "local" when a workspace override shadows the
  // global entry, "global" otherwise. Mirrors server ProviderWire.Scope;
  // absent (older server) means global.
  scope?: "global" | "local";
  // Peak-hours window in local browser time ("HH:MM"). null/absent means no
  // restriction. Mirrors server PeakHoursWirePayload (lowerCamelCase keys).
  // message is an optional operator note appended to the exit guidance
  // when a run refuses/halts because of this window.
  peakHours?: { start: string; end: string; message?: string } | null;
}

export interface ConfigPayload {
  models?: Record<string, { Provider: string; Model: string }>;
  providers?: Record<string, ProviderInfo>;
  yolo?: boolean;
  debug?: boolean;
  debugLsp?: boolean;
  theme?: string;
  recentSmartModels?: Array<{ Provider: string; Model: string }>;
  recentFastModels?: Array<{ Provider: string; Model: string }>;
  recentWorkerModels?: Array<{ Provider: string; Model: string }>;
  recentReviewerModels?: Array<{ Provider: string; Model: string }>;
  contextPaths?: string[];
  skillsPaths?: string[];
  initializeAs?: string;
  version?: string;
  cwd?: string;
  // Server-resolved keep-alive preference: backend defaults nil → true and
  // always sends an explicit bool, so this is non-nullable in practice
  // (kept optional only to survive transitional reloads against an older
  // server build).
  keepAliveEnabled?: boolean;
}

export interface SkillInfo {
  name: string;
  description: string;
  path: string;
  source?: string;
  instructions?: string;
}

export interface SkillsSnapshot {
  skills: SkillInfo[];
  paths: string[];
}

export interface MCPServerInfo {
  name: string;
  status: string;
  disabled: boolean;
  toolCount: number;
  tools?: string[];
  serverType?: string;
  command?: string;
  args?: string[];
  url?: string;
  env?: Record<string, string>;
  headers?: Record<string, string>;
  source?: string;
}

export interface MCPState {
  servers: MCPServerInfo[];
}

// ─── Live work panel (task #1059) ───────────────────────────────────────────
//
// One in-flight async command (bash/run_command) or sub-agent delegation
// (agent/agentic_fetch), sourced from internal/server/protocol.go's
// LiveWorkItemWire. The server-side emitter lands in #1058 (needs the
// DB-backed readers); this task only wires the shared shape + client render.
export interface LiveWorkItem {
  toolCallID: string;
  toolName: string;
  title: string;
  // Set only for a sub-agent delegation -- the child session's own ID.
  childSessionID?: string;
  startedAt: number; // unix ms
}

// SessionLiveWorkPayload mirrors the server's SessionLiveWorkPayload: the
// `session_live_work` push and the `get_session_live_work` reply both carry
// this shape, always as a FULL snapshot -- never a delta.
export interface SessionLiveWorkPayload {
  sessionID: string;
  commands: LiveWorkItem[];
  agents: LiveWorkItem[];
}

export interface AgentBusyPayload {
  SessionID: string;
  Busy: boolean;
}

export interface SummarizeQueuedPayload {
  SessionID: string;
  Queued: boolean;
}



// ─── WebSocket protocol ────────────────────────────────────────────────────────

export interface WSMessage<T = unknown> {
  id?: string;
  type: string;
  payload?: T;
  error?: string;
}
