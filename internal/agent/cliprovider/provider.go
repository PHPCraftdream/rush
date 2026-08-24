// Package cliprovider implements a fantasy.Provider that invokes local CLI tools.
// Each CLISpec describes one hardcoded model: which binary to run and how to
// build its arguments from the prompt text and the yolo flag.
//
// To add a new CLI model, append a new CLISpec to the [All] slice.
package cliprovider

import (
	"context"
	"fmt"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"

	"charm.land/fantasy"
	"charm.land/fantasy/object"
	"github.com/PHPCraftdream/rush/internal/csync"
	"github.com/PHPCraftdream/rush/internal/permission"
	"github.com/PHPCraftdream/rush/internal/session"
)

// sessionIDContextKey is a private key type so it cannot collide with other packages.
type sessionIDContextKey struct{}

// SessionIDContextKey is the context key for the session ID, set by the agent
// before calling Stream so the MCP todos tool knows which session to update.
var SessionIDContextKey = sessionIDContextKey{}

// reasoningEffortContextKey is a private key type for the reasoning effort value.
type reasoningEffortContextKey struct{}

// ReasoningEffortContextKey is the context key for the reasoning effort level
// (e.g. "low", "medium", "high", "max"), set by the agent before calling
// Stream so CLI models can inject the --effort flag dynamically.
var ReasoningEffortContextKey = reasoningEffortContextKey{}

// Fork patch: batch 14 — non-interactive context propagation.
//
// nonInteractiveContextKey marks the request as coming from `crush run` (a
// non-interactive entry point with no human at the keyboard). When set, the
// CLI sub-process is launched with its own bypass-permissions flag
// (claude --dangerously-skip-permissions, codex --approval-mode yolo,
// gemini --yolo) regardless of the runtime yoloFn. Otherwise the inner
// CLI would block waiting for an interactive permission prompt that
// nobody is there to answer, and `crush run` would hang silently.
type nonInteractiveContextKey struct{}

// NonInteractiveContextKey is set by app.RunNonInteractive on the agent
// context so cliprovider.Stream can force bypass-permissions for the inner
// CLI when there is provably no human to confirm.
var NonInteractiveContextKey = nonInteractiveContextKey{}

// ProviderType is the catwalk.Type value used for CLI providers.
const ProviderType = "cli"

// ProviderID is the catwalk.InferenceProvider ID for the built-in CLI provider.
const ProviderID = "local-cli"

// maxPromptArgLen is the maximum prompt length (in bytes) that will be passed
// as a CLI argument when the target binary is executed directly (a native
// .exe on Windows, or any binary on Unix). Longer prompts are piped via
// stdin to avoid OS limits. Calibrated for the real argv ceiling in that
// case (Windows CreateProcess ~32767 chars, Linux ARG_MAX / macOS typically
// far larger) — see maxPromptArgLenWindowsCmdShim below for the much lower
// threshold used when that assumption does not hold.
const maxPromptArgLen = 30 * 1024

// maxPromptArgLenWindowsCmdShim is the threshold used instead of
// maxPromptArgLen when, on Windows, the resolved binary is a .cmd/.bat
// shim (see isWindowsCmdShim) rather than a native .exe.
//
// Windows cannot exec a .cmd/.bat file directly the way it execs a .exe:
// CreateProcess always routes it through `cmd.exe /c <shim> <args...>`, and
// cmd.exe enforces its OWN command-line length ceiling — about 8191
// characters, per Microsoft's own documented limit for CMD.EXE — entirely
// independent of, and far below, the ~32767-character CreateProcess limit
// maxPromptArgLen above was calibrated for.
//
// This is not a corner case: npm installs every Node-based CLI on Windows
// this way (claude.cmd, gemini.cmd, qwen.cmd, grok.cmd, ...), so it is the
// default shape of every CLI provider's binary on a Windows machine with
// these tools installed via npm. A real system prompt is routinely ~10-15KB
// once skills, MCP tool descriptions, and env/git context are folded in —
// comfortably under maxPromptArgLen's 30KB, but well past cmd.exe's ~8191
// character ceiling. Found via a smoke test of a real `crush run`
// invocation on Windows: the underlying claude.cmd process exited with
// status 1 and its only PTY output was cmd.exe's own
// "The command line is too long." — before this fix, maxPromptArgLen's 30KB
// threshold never triggered the stdin fallback because the ~12KB prompt in
// question looked "short enough", when in fact it was already 50% past the
// real, much lower limit for this specific binary shape.
//
// Set well under 8191 to leave headroom for the rest of the command line
// this length check does not itself measure: the resolved binary's own
// (possibly long) path, plus every other flag already on the argument list
// by this point (--mcp-config's temp file path, --allowedTools's tool
// list, etc.), which together can run to a few hundred bytes on their own.
const maxPromptArgLenWindowsCmdShim = 4 * 1024

// isWindowsCmdShim reports whether resolvedBinary is a Windows .cmd/.bat
// script — the shape npm installs Node-based CLI wrappers as on Windows.
// Always false on non-Windows platforms, where this distinction does not
// apply (exec.Command execs the target directly, no intermediate shell).
func isWindowsCmdShim(resolvedBinary string) bool {
	if runtime.GOOS != "windows" {
		return false
	}
	switch strings.ToLower(filepath.Ext(resolvedBinary)) {
	case ".cmd", ".bat":
		return true
	default:
		return false
	}
}

// effectiveMaxPromptArgLen returns the byte threshold above which the
// prompt is piped via stdin instead of passed as a CLI argument, for the
// binary resolveBinary(rawBinary) resolves to. See maxPromptArgLen and
// maxPromptArgLenWindowsCmdShim for why these two thresholds differ.
func effectiveMaxPromptArgLen(rawBinary string) int {
	resolved := rawBinary
	if r, err := resolveBinary(rawBinary); err == nil {
		resolved = r
	}
	if isWindowsCmdShim(resolved) {
		return maxPromptArgLenWindowsCmdShim
	}
	return maxPromptArgLen
}

// ansiEscape matches ANSI/VT escape sequences injected by PTY drivers:
//   - CSI sequences: ESC [ <params> <letter>  (e.g. \x1b[2J, \x1b[?25h)
//   - OSC sequences: ESC ] <text> BEL         (e.g. \x1b]0;title\a)
//   - other two-char escapes: ESC <char>
//
// Also strips bare \r so JSON lines from PTY output parse cleanly.
var ansiEscape = regexp.MustCompile(`\x1b(?:\[[0-9;?]*[a-zA-Z]|\][^\x07]*\x07|[^\[])|\r`)

// cliSessionEntry stores a CLI session ID along with a hash of the conversation
// prefix (all messages except the last), so we can detect edits/deletes that
// would make the CLI session's history stale.
type cliSessionEntry struct {
	CLISessionID string
	PrefixHash   uint64
}

type cliProvider struct {
	workingDir  string
	dataDir     string
	yoloFn      func() bool
	perms       permission.Service
	sessions    session.Service
	mcpProxy    ExternalMCPProxy
	specs       map[string]CLISpec
	cliSessions *csync.Map[string, cliSessionEntry] // crush session key → CLI session entry
}

// ExternalMCPTool describes an external MCP tool to expose through the crush MCP bridge.
type ExternalMCPTool struct {
	ServerName  string
	Name        string
	Description string
	InputSchema any // JSON schema
}

// ExternalMCPProxy provides access to external MCP tools and the ability
// to call them. Implemented by the coordinator to avoid circular imports.
type ExternalMCPProxy interface {
	// ListTools returns all enabled external MCP tools.
	ListTools() []ExternalMCPTool
	// CallTool invokes a tool on the named MCP server and returns the text result.
	CallTool(ctx context.Context, serverName, toolName, inputJSON string) (string, error)
}

// New creates a CLI provider that runs all specs from [All].
// workingDir is set as the working directory for every CLI invocation.
// dataDir is the session data directory (holds .crush/locks/*) -- used
// only to durably register CLI-provider child process groups so
// `crush sessions kill` can reach them cross-process (see
// internal/session/childgroup_registry_unix.go); pass "" to disable that
// registration (e.g. from a caller, like `crush ping`, that has no
// meaningful per-session data directory context).
// yoloFn is called at request time to decide whether to pass the auto-accept flag.
// perms is used to show crush's permission dialog when UseCrushMCP specs are invoked.
// sessions is used by the todos MCP tool to persist task lists.
// mcpProxy, if non-nil, is used for proxying external MCP tools to CLI models.
func New(workingDir, dataDir string, yoloFn func() bool, perms permission.Service, sessions session.Service, mcpProxy ExternalMCPProxy) fantasy.Provider {
	specs := make(map[string]CLISpec, len(All))
	for _, s := range All {
		specs[s.ModelID] = s
	}
	return &cliProvider{
		workingDir:  workingDir,
		dataDir:     dataDir,
		yoloFn:      yoloFn,
		perms:       perms,
		sessions:    sessions,
		mcpProxy:    mcpProxy,
		specs:       specs,
		cliSessions: csync.NewMap[string, cliSessionEntry](),
	}
}

func (p *cliProvider) Name() string { return ProviderID }

func (p *cliProvider) LanguageModel(_ context.Context, modelID string) (fantasy.LanguageModel, error) {
	spec, ok := p.specs[modelID]
	if !ok {
		return nil, fmt.Errorf("unknown CLI model: %q", modelID)
	}
	return &cliModel{spec: spec, workingDir: p.workingDir, dataDir: p.dataDir, yoloFn: p.yoloFn, perms: p.perms, sessions: p.sessions, mcpProxy: p.mcpProxy, cliSessions: p.cliSessions}, nil
}

type cliModel struct {
	spec        CLISpec
	mcpProxy    ExternalMCPProxy
	cliSessions *csync.Map[string, cliSessionEntry]
	workingDir  string
	dataDir     string
	yoloFn      func() bool
	perms       permission.Service
	sessions    session.Service
}

func (m *cliModel) Provider() string { return ProviderID }
func (m *cliModel) Model() string    { return m.spec.ModelID }

func (m *cliModel) GenerateObject(ctx context.Context, call fantasy.ObjectCall) (*fantasy.ObjectResponse, error) {
	return object.GenerateWithTool(ctx, m, call)
}

func (m *cliModel) StreamObject(ctx context.Context, call fantasy.ObjectCall) (fantasy.ObjectStreamResponse, error) {
	return object.StreamWithTool(ctx, m, call)
}

func (m *cliModel) Generate(ctx context.Context, call fantasy.Call) (*fantasy.Response, error) {
	var text strings.Builder
	var usage fantasy.Usage
	stream, err := m.Stream(ctx, call)
	if err != nil {
		return nil, err
	}
	for part := range stream {
		if part.Type == fantasy.StreamPartTypeError {
			return nil, part.Error
		}
		if part.Type == fantasy.StreamPartTypeTextDelta {
			text.WriteString(part.Delta)
		}
		if part.Type == fantasy.StreamPartTypeFinish {
			usage = part.Usage
		}
	}
	return &fantasy.Response{
		Content:      fantasy.ResponseContent{fantasy.TextContent{Text: text.String()}},
		FinishReason: fantasy.FinishReasonStop,
		Usage:        usage,
	}, nil
}
