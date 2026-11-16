package clipspec

// Context keys shared by the agent (writer) and the CLI provider stream
// (reader). Moved verbatim from the cliprovider facade by CL-A; the facade
// keeps var aliases (never reassigned — design §3.5/§4.1).

// sessionIDContextKey is a private key type so it cannot collide with other packages.
type sessionIDContextKey struct{}

// SessionIDContextKey is the context key for the session ID, set by the agent
// before calling Stream so MCP tools and CLI resume can identify the session.
var SessionIDContextKey = sessionIDContextKey{}

type titleGenerationContextKey struct{}

// TitleGenerationContextKey marks a provider request as a cosmetic session-title call.
var TitleGenerationContextKey = titleGenerationContextKey{}

// reasoningEffortContextKey is a private key type for the reasoning effort value.
type reasoningEffortContextKey struct{}

// ReasoningEffortContextKey is the context key for the reasoning effort level
// (e.g. "low", "medium", "high", "max"), set by the agent before calling
// Stream so CLI models can inject the --effort flag dynamically.
var ReasoningEffortContextKey = reasoningEffortContextKey{}

// Fork patch: batch 14 — non-interactive context propagation.
//
// nonInteractiveContextKey marks the request as coming from `rush run` (a
// non-interactive entry point with no human at the keyboard). When set, the
// CLI sub-process is launched with its own bypass-permissions flag
// (claude --dangerously-skip-permissions, codex --approval-mode yolo,
// gemini --yolo) regardless of the runtime yoloFn. Otherwise the inner
// CLI would block waiting for an interactive permission prompt that
// nobody is there to answer, and `rush run` would hang silently.
type nonInteractiveContextKey struct{}

// NonInteractiveContextKey is set by app.RunNonInteractive on the agent
// context so cliprovider.Stream can force bypass-permissions for the inner
// CLI when there is provably no human to confirm.
var NonInteractiveContextKey = nonInteractiveContextKey{}
