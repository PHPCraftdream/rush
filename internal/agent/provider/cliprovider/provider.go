// Package cliprovider implements a fantasy.Provider that invokes local CLI tools.
// Each CLISpec describes one hardcoded model: which binary to run and how to
// build its arguments from the prompt text and the yolo flag.
//
// To add a new CLI model, append a new CLISpec to the [All] slice.
package cliprovider

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"

	"charm.land/fantasy"
	"github.com/PHPCraftdream/rush/internal/agent/provider/cliprovider/clipmcp"
	"github.com/PHPCraftdream/rush/internal/agent/provider/cliprovider/clipspec"
	"github.com/PHPCraftdream/rush/internal/agent/provider/cliprovider/clipstream"
	"github.com/PHPCraftdream/rush/internal/agent/provider/cliprovider/cliptool"
	"github.com/PHPCraftdream/rush/internal/agent/safety/permission"
	"github.com/PHPCraftdream/rush/internal/config/providers/catalog"
	"github.com/PHPCraftdream/rush/internal/kit/sync/csync"
	"github.com/PHPCraftdream/rush/internal/kit/sys/shell"
	session "github.com/PHPCraftdream/rush/internal/session/service"
)

// CL-B facade aliases for the clipmcp seam (design §3.4 S4). The legacy
// unqualified names below are never reassigned.
type (
	rushMCPServer = clipmcp.RushMCPServer
	mcpToolEvent  = clipmcp.MCPToolEvent
)

var newRushMCPServer = clipmcp.NewRushMCPServer

// CL-A facade aliases: the declarations live in clipspec (design §3.5/§4.1);
// these names are never reassigned, so value aliases preserve identity.
var (
	SessionIDContextKey       = clipspec.SessionIDContextKey
	TitleGenerationContextKey = clipspec.TitleGenerationContextKey
	ReasoningEffortContextKey = clipspec.ReasoningEffortContextKey
	ServiceTierContextKey     = clipspec.ServiceTierContextKey
	NonInteractiveContextKey  = clipspec.NonInteractiveContextKey
)

// CL-A facade aliases for the spec catalog and CLI spec type (design §3.5).
var All = clipspec.All

type CLISpec = clipspec.CLISpec

// streamEvent is the Claude CLI stream-json envelope; moved to clipspec in CL-A.
type streamEvent = clipspec.StreamEvent

// CL-C facade alias: StreamResult moved to clipstream (design §3.3 S3).
type StreamResult = clipstream.StreamResult

// ProviderType is the catwalk.Type value used for CLI providers.
const ProviderType = catalog.LocalCLIProviderType

// ProviderID is the catwalk.InferenceProvider ID for the built-in CLI provider.
const ProviderID = catalog.LocalCLIProviderID

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
// character ceiling. Found via a smoke test of a real `rush run`
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

// resolveBinary resolves name (a bare command like "claude" or "bash") to
// the executable that will actually run, going through the same
// WSL-launcher-aware PATH lookup internal/shell uses for #!/bin/bash
// shebang dispatch (see shell.LookPathSkippingWSL).
//
// This matters specifically for Binary: "bash": plain exec.LookPath (and
// the implicit lookup os/exec.Cmd performs on a bare name) can resolve to
// %SystemRoot%\System32\bash.exe — the WSL launcher — if it happens to sit
// ahead of Git Bash/MSYS bash on PATH. The WSL launcher expects Linux-style
// paths (/mnt/c/...) and cannot run a script given a Windows-style path or
// working directory, so silently handing it back here would launch a
// process that appears to start but fails in confusing ways downstream.
// Both call sites in Stream (PTY branch and pipe-fallback branch) resolve
// through this helper rather than trusting exec.LookPath/os/exec's default
// resolution directly.
//
// On non-Windows this is a thin wrapper around exec.LookPath.
func resolveBinary(name string) (string, error) {
	return shell.LookPathSkippingWSL(name)
}

// AvailableFunc returns the locally-installed CLI specs. It is a package
// var so tests can stub CLI detection deterministically — otherwise the set
// depends on whatever binaries (claude, gemini, npx, …) happen to be on the
// runner's PATH, which makes provider-count assertions environment-dependent.
var AvailableFunc = detectAvailable

// Available reports which CLI model specs are usable on this machine.
func Available() []CLISpec { return AvailableFunc() }

// AvailableCLIModels projects the locally-available CLI specs onto the
// execution-free [catalog.LocalCLIModel] descriptor config synthesizes the
// local-cli provider from. It follows [AvailableFunc], so stubbing that var
// in tests also controls this projection.
func AvailableCLIModels() []catalog.LocalCLIModel {
	specs := Available()
	models := make([]catalog.LocalCLIModel, 0, len(specs))
	for _, spec := range specs {
		models = append(models, catalog.LocalCLIModel{
			ID:            spec.ModelID,
			Name:          spec.ModelName,
			ContextWindow: spec.ContextWindow,
		})
	}
	return models
}

// detectAvailableCache memoizes detectAvailable's PATH scan, keyed on the
// PATH (and, on Windows, PATHEXT) value the scan was performed against.
//
// Why this cache exists: detectAvailable resolves one binary per distinct
// CLISpec.Binary (claude/gemini/codex/qwen) through exec.LookPath, and
// exec.LookPath does no caching of its own — every call re-walks every PATH
// entry, on Windows multiplied by every PATHEXT suffix. Measured on a
// developer workstation with 71 PATH entries and 11 PATHEXT suffixes, one
// detectAvailable() call costs ~360ms (~90ms per binary); a runner where
// none of the four CLIs are installed pays the full unsuccessful walk for
// each.
//
// That would be a once-per-process cost if Available() were called once, but
// it is called from config.configureProviders, which runs on the initial
// config Load AND on every buildAndPublishReload — i.e. once per config
// field write. A single `rush models use ...` invocation triggers ~5 of
// them, so ~1.8s of the command's runtime was spent re-answering "is
// claude on PATH?" with an answer that had not changed. In the test suite
// the same multiplier applied per test: internal/cmd's models_* tests each
// paid ~2s of PATH scanning out of a ~2.8s total runtime.
//
// Keying on PATH/PATHEXT rather than caching unconditionally keeps the
// tests that install a fake binary into a temp dir and point PATH at it
// (internal/agent/cliprovider/provider_windows_test.go, internal/shell's
// dispatch tests) working unchanged: changing PATH invalidates the entry.
// The remaining behavioral difference from an uncached scan is that
// installing a CLI into an already-on-PATH directory while a long-lived
// rush process is running is no longer picked up by a config reload; it
// needs a restart. That is the same trade the fork already makes for the
// Catwalk/Hyper provider catalogs (see internal/config/catwalk.go's
// providerCacheTTL) and is why detection is only ever consulted to
// synthesize the local-cli provider list, never on the hot path of actually
// launching a CLI (Stream resolves its binary directly through
// resolveBinary every time).
var detectAvailableCache struct {
	mu     sync.Mutex
	key    string
	specs  []CLISpec
	loaded bool
}

// detectAvailablePathKey returns the cache key identifying the lookup
// environment detectAvailable would resolve against. PATHEXT only affects
// resolution on Windows, but including it unconditionally costs nothing and
// keeps the key definition platform-independent.
func detectAvailablePathKey() string {
	return os.Getenv("PATH") + "\x00" + os.Getenv("PATHEXT")
}

func detectAvailable() []CLISpec {
	key := detectAvailablePathKey()

	detectAvailableCache.mu.Lock()
	defer detectAvailableCache.mu.Unlock()
	if detectAvailableCache.loaded && detectAvailableCache.key == key {
		// Hand back a copy: callers (config.configureProviders) range over
		// the result and a shared backing array would let one caller's
		// append alias another's.
		return slices.Clone(detectAvailableCache.specs)
	}

	seen := make(map[string]bool)
	var result []CLISpec
	for _, spec := range All {
		if !seen[spec.Binary] {
			_, err := resolveBinary(spec.Binary)
			seen[spec.Binary] = err == nil
		}
		if seen[spec.Binary] {
			result = append(result, spec)
		}
	}

	detectAvailableCache.key = key
	detectAvailableCache.specs = result
	detectAvailableCache.loaded = true
	return slices.Clone(result)
}

type cliProvider struct {
	workingDir  string
	dataDir     string
	yoloFn      func() bool
	perms       permission.Service
	sessions    session.Service
	mcpProxy    ExternalMCPProxy
	specs       map[string]CLISpec
	cliSessions *csync.Map[string, clipstream.CLISessionEntry] // rush session key → CLI session entry
}

// CL-B facade aliases for the cliptool seam (design §3.4/§3.5, §4.1:
// bridgeFallback lives in cliptool; ExternalMCPTool/ExternalMCPProxy are
// physically defined in cliptool because cliptool's policy layer consumes
// them, with clipmcp aliasing them). All names below are never reassigned.
type (
	ToolBridge       = cliptool.ToolBridge
	BridgeTool       = cliptool.BridgeTool
	ToolPolicy       = cliptool.ToolPolicy
	ExternalMCPTool  = cliptool.ExternalMCPTool
	ExternalMCPProxy = cliptool.ExternalMCPProxy
)

var (
	WithToolBridge     = cliptool.WithToolBridge
	WithToolPolicy     = cliptool.WithToolPolicy
	ToolPolicyFrom     = cliptool.ToolPolicyFrom
	BuiltinBridgeTools = cliptool.BuiltinBridgeTools
	FilterBridgeRead   = cliptool.FilterBridgeRead
	SetBridgeFallback  = cliptool.SetBridgeFallback
)

// New creates a CLI provider that runs all specs from [All].
// workingDir is set as the working directory for every CLI invocation.
// dataDir is the session data directory (holds .rush/locks/*) -- used
// only to durably register CLI-provider child process groups so
// `rush sessions kill` can reach them cross-process (see
// internal/session/childgroup_registry_unix.go); pass "" to disable that
// registration (e.g. from a caller, like `rush ping`, that has no
// meaningful per-session data directory context).
// yoloFn is called at request time to decide whether to pass the auto-accept flag.
// perms is used to show rush's permission dialog when UseRushMCP specs are invoked.
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
		cliSessions: csync.NewMap[string, clipstream.CLISessionEntry](),
	}
}

func (p *cliProvider) Name() string { return ProviderID }

func (p *cliProvider) LanguageModel(_ context.Context, modelID string) (fantasy.LanguageModel, error) {
	spec, ok := p.specs[modelID]
	if !ok {
		return nil, fmt.Errorf("unknown CLI model: %q", modelID)
	}
	// Delegates to clipstream (design §3.3 S3): cliModel lives there.
	return clipstream.NewModel(spec, p.workingDir, p.dataDir, p.yoloFn, p.perms, p.sessions, p.mcpProxy, p.cliSessions), nil
}

// LogRawPromptEnabled delegates to clipstream (design §3.3 S3), where the
// raw-prompt logging helpers physically live; consumed by
// internal/agent/agent_ownership.go without changes.
func LogRawPromptEnabled() bool { return clipstream.LogRawPromptEnabled() }
