package clipstream

import (
	"bufio"
	"context"
	"io"
	"regexp"
	"strings"
	"sync"

	"charm.land/fantasy"
	"charm.land/fantasy/object"
	"github.com/PHPCraftdream/rush/internal/agent/provider/cliprovider/clipmcp"
	"github.com/PHPCraftdream/rush/internal/agent/provider/cliprovider/clipspec"
	"github.com/PHPCraftdream/rush/internal/agent/provider/cliprovider/cliptool"
	"github.com/PHPCraftdream/rush/internal/agent/safety/permission"
	"github.com/PHPCraftdream/rush/internal/config/catalog"
	"github.com/PHPCraftdream/rush/internal/kit/sync/csync"
	session "github.com/PHPCraftdream/rush/internal/session/service"
)

// CLISessionEntry stores a CLI session ID along with a hash of the conversation
// prefix (all messages except the last), so we can detect edits/deletes that
// would make the CLI session's history stale. Exported (renamed from the
// facade's cliSessionEntry) because the facade's cliProvider owns the shared
// cliSessions map and passes it into NewModel (design §3.3 S3).
type CLISessionEntry struct {
	CLISessionID string
	PrefixHash   uint64
}

// CLISpec aliases the spec type from clipspec; the stream cluster touches it
// everywhere (design §3.4 S4).
type CLISpec = clipspec.CLISpec

// streamEvent is the Claude CLI stream-json envelope (moved from the facade
// alias to its clipspec home).
type streamEvent = clipspec.StreamEvent

// ── seam aliases copied verbatim from the facade (design §3.4 S4): the moved
// Stream code and its tests use the legacy unqualified names; every one is a
// never-reassigned type/value alias. ──
type (
	rushMCPServer = clipmcp.RushMCPServer
	mcpToolEvent  = clipmcp.MCPToolEvent
	ToolPolicy    = cliptool.ToolPolicy
	BridgeTool    = cliptool.BridgeTool
)

var (
	newRushMCPServer          = clipmcp.NewRushMCPServer
	sessionMCPName            = clipmcp.SessionMCPName
	registerQwenMCP           = clipmcp.RegisterQwenMCP
	deregisterQwenMCP         = clipmcp.DeregisterQwenMCP
	registerGeminiMCP         = clipmcp.RegisterGeminiMCP
	deregisterGeminiMCP       = clipmcp.DeregisterGeminiMCP
	rushMCPToolArgsWithPolicy = clipmcp.RushMCPToolArgsWithPolicy

	WithToolBridge          = cliptool.WithToolBridge
	ToolPolicyFrom          = cliptool.ToolPolicyFrom
	BuiltinBridgeTools      = cliptool.BuiltinBridgeTools
	applyRestrictedToolArgs = cliptool.ApplyRestrictedToolArgs
	policyBridgeTools       = cliptool.PolicyBridgeTools
	invocationToolBridge    = cliptool.InvocationToolBridge

	SessionIDContextKey       = clipspec.SessionIDContextKey
	TitleGenerationContextKey = clipspec.TitleGenerationContextKey
	ReasoningEffortContextKey = clipspec.ReasoningEffortContextKey
	NonInteractiveContextKey  = clipspec.NonInteractiveContextKey
)

// ansiEscape matches ANSI/VT escape sequences injected by PTY drivers:
//   - CSI sequences: ESC [ <params> <letter>  (e.g. \x1b[2J, \x1b[?25h)
//   - OSC sequences: ESC ] <text> BEL         (e.g. \x1b]0;title\a)
//   - other two-char escapes: ESC <char>
//
// Also strips bare \r so JSON lines from PTY output parse cleanly.
//
// Moved verbatim from the facade (design §4.1: consumers are the stream loop
// and quota diagnostics).
var ansiEscape = regexp.MustCompile(`\x1b(?:\[[0-9;?]*[a-zA-Z]|\][^\x07]*\x07|[^\[])|\r`)

// testDisablePTY, when true, forces pipe mode regardless of spec. The
// clipstream test suite sets it on Windows, where go-pty's ConPTY path has
// an internal data race the -race detector flags. Always false in production.
//
// Moved verbatim from the facade's provider_detection.go: its only reader is
// the Stream PTY branch and its only writers are clipstream tests.
var testDisablePTY bool

// providerID is a local copy of the facade's ProviderID value
// (catalog.LocalCLIProviderID); cliModel.Provider reports it verbatim.
const providerID = catalog.LocalCLIProviderID

type procHandle struct {
	stdout    io.Reader
	usingPTY  bool
	kill      func()
	wait      func() (string, error)
	closeIO   func()
	drained   func()
	stderrRaw func() []byte
	cleanup   func()
}

type scanResult struct {
	raw  []byte
	done bool
	err  error
}

type StreamResult struct {
	Stderr  string
	Err     error
	ScanErr error
}

const (
	maxRetainedOutputBytes = 1024 * 1024
	outputTruncationMarker = "\n...(output truncated; retained output capped)\n"
)

type StreamSession struct {
	stream          fantasy.StreamResponse
	done            chan struct{}
	notify          chan struct{}
	mu              sync.Mutex
	lines           []scanResult
	output          []byte
	outputTruncated bool
	result          StreamResult
	stopOnce        sync.Once
	stopDone        chan struct{}
	proc            procHandle
}

func (s *StreamSession) Stream() fantasy.StreamResponse { return s.stream }
func (s *StreamSession) Done() <-chan struct{}          { return s.done }
func (s *StreamSession) Output() []byte {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]byte(nil), s.output...)
}

func (s *StreamSession) Wait(ctx context.Context) (StreamResult, error) {
	select {
	case <-s.done:
		return s.result, nil
	case <-ctx.Done():
		return StreamResult{}, ctx.Err()
	}
}

func (s *StreamSession) Stop(ctx context.Context) error {
	s.stopOnce.Do(func() {
		go func() { s.proc.kill(); close(s.stopDone) }()
	})
	select {
	case <-s.stopDone:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (s *StreamSession) kill() {
	s.stopOnce.Do(func() { go func() { s.proc.kill(); close(s.stopDone) }() })
}

func (s *StreamSession) legacyKill() {
	s.kill()
	<-s.stopDone
}

func (s *StreamSession) ready(index int) <-chan struct{} {
	s.mu.Lock()
	defer s.mu.Unlock()
	if index < len(s.lines) {
		ready := make(chan struct{})
		close(ready)
		return ready
	}
	return s.notify
}

func (s *StreamSession) next(ctx context.Context, index int) (scanResult, bool) {
	for {
		s.mu.Lock()
		if index < len(s.lines) {
			line := s.lines[index]
			s.mu.Unlock()
			return line, true
		}
		notify := s.notify
		s.mu.Unlock()
		select {
		case <-notify:
		case <-ctx.Done():
			return scanResult{}, false
		}
	}
}

func newStreamSession(proc procHandle) *StreamSession {
	s := &StreamSession{done: make(chan struct{}), notify: make(chan struct{}), stopDone: make(chan struct{}), proc: proc}
	go func() {
		scanner := bufio.NewScanner(io.TeeReader(proc.stdout, streamCapture{s}))
		scanner.Buffer(make([]byte, 64*1024), 4*1024*1024)
		for scanner.Scan() {
			raw := append([]byte(nil), scanner.Bytes()...)
			s.mu.Lock()
			s.lines = append(s.lines, scanResult{raw: raw})
			close(s.notify)
			s.notify = make(chan struct{})
			s.mu.Unlock()
		}
		s.mu.Lock()
		s.lines = append(s.lines, scanResult{done: true, err: scanner.Err()})
		close(s.notify)
		s.notify = make(chan struct{})
		s.mu.Unlock()
		if scanner.Err() != nil && !proc.usingPTY {
			s.kill()
			if closer, ok := proc.stdout.(io.Closer); ok {
				_ = closer.Close()
			}
		}
		if proc.drained != nil {
			proc.drained()
		}
		stderr, err := proc.wait()
		if proc.stderrRaw != nil {
			s.mu.Lock()
			s.appendOutput(proc.stderrRaw())
			s.mu.Unlock()
		}
		s.result = StreamResult{Stderr: stderr, Err: err, ScanErr: scanner.Err()}
		if proc.cleanup != nil {
			proc.cleanup()
		}
		close(s.done)
	}()
	return s
}

type streamCapture struct{ session *StreamSession }

func (s *StreamSession) appendOutput(p []byte) {
	if s.outputTruncated || len(p) == 0 {
		return
	}
	marker := []byte(outputTruncationMarker)
	remaining := maxRetainedOutputBytes - len(s.output)
	if len(p) <= remaining {
		s.output = append(s.output, p...)
		return
	}

	// Reserve marker space, but retain as much as possible from this final
	// write rather than discarding the entire chunk that crossed the cap.
	keep := maxRetainedOutputBytes - len(marker)
	if len(s.output) > keep {
		s.output = s.output[:keep]
	}
	available := keep - len(s.output)
	if available > len(p) {
		available = len(p)
	}
	s.output = append(s.output, p[:available]...)
	s.output = append(s.output, marker...)
	s.outputTruncated = true
}

func (c streamCapture) Write(p []byte) (int, error) {
	c.session.mu.Lock()
	c.session.appendOutput(p)
	c.session.mu.Unlock()
	return len(p), nil
}

// ── moved verbatim from the facade (design §3.3 S3): cliModel and its
// non-Stream methods; Go forbids methods of one type in two packages. ──

type cliModel struct {
	streamProcess        func(bool, []string) procHandle
	streamVisibleCleanup func()
	spec                 CLISpec
	mcpProxy             cliptool.ExternalMCPProxy
	cliSessions          *csync.Map[string, CLISessionEntry]
	workingDir           string
	dataDir              string
	yoloFn               func() bool
	perms                permission.Service
	sessions             session.Service
}

func (m *cliModel) Provider() string { return providerID }
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

// NewModel builds the clipstream CLI model for one spec (design §3.3 S3).
// The facade's LanguageModel delegates here; cliSessions is the provider-wide
// session map the facade constructs in New (construction order unchanged:
// spec -> ExternalMCPProxy -> cliSessions map).
func NewModel(spec clipspec.CLISpec, workingDir, dataDir string, yoloFn func() bool, perms permission.Service, sessions session.Service, mcpProxy cliptool.ExternalMCPProxy, cliSessions *csync.Map[string, CLISessionEntry]) fantasy.LanguageModel {
	return &cliModel{
		spec:        spec,
		workingDir:  workingDir,
		dataDir:     dataDir,
		yoloFn:      yoloFn,
		perms:       perms,
		sessions:    sessions,
		mcpProxy:    mcpProxy,
		cliSessions: cliSessions,
	}
}
