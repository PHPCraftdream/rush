package tools

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"html/template"
	"io"
	"io/fs"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"charm.land/fantasy"
	"github.com/PHPCraftdream/rush/internal/agent/safety/permission"
	"github.com/PHPCraftdream/rush/internal/agent/tools/mcp"
	"github.com/PHPCraftdream/rush/internal/config"
	"github.com/PHPCraftdream/rush/internal/kit/text/stringext"
)

// whitelistDockerTools contains Docker MCP tools that don't require permission.
var whitelistDockerTools = []string{
	"mcp_docker_mcp-find",
	"mcp_docker_mcp-add",
	"mcp_docker_mcp-remove",
	"mcp_docker_mcp-config-set",
	"mcp_docker_code-mode",
}

// GetMCPTools gets all the currently available MCP tools owned by cfg.
//
// The schemas are deliberately live, not pinned to any particular config
// generation: this enumerates the package-level mcp.Tools() registry
// (internal/agent/tools/mcp), which holds live MCP client connections and
// their current tool schemas, then keeps only server names present in cfg.
// That registry is refreshed asynchronously --
// by each server's own ToolListChangedHandler, by EnableServer/
// DisableServer/RemoveServer, and by startup Initialize -- independently of
// any single agent build. A caller (see coordinator.buildTools) that has
// otherwise pinned one *config.Config snapshot for the rest of an agent
// build (task #576/P1-3) will still get whichever MCP tool generation is
// live at the moment this is called, which can differ from that pinned
// snapshot if a reload lands mid-build (task #591/P2-1). This is
// intentional: the registry holds live network/process state (connections,
// reconnection, server-pushed schema changes), not derived config data, so
// pinning it to an older generation would mean serving stale tool schemas
// or fighting the reconnection logic in the mcp package, not closing a
// torn read. cfg is passed through to each returned *Tool for use at Run
// time (permission requests, mcp.RunTool's getOrRenewClient) and stays live
// for the same reason. owner is this config's MCP lifecycle owner (task
// #923); nil keeps the legacy process-current-owner resolution.
func GetMCPTools(permissions permission.Service, cfg *config.ConfigStore, wd string, owner *mcp.Owner, output ...*MCPOutputAccess) []*Tool {
	var access *MCPOutputAccess
	if len(output) > 0 {
		access = output[0]
	}
	var result []*Tool
	for mcpName, tools := range mcp.Tools() {
		if !mcp.IsConfigured(cfg, mcpName) {
			continue
		}
		for _, tool := range tools {
			result = append(result, &Tool{
				mcpName:     mcpName,
				tool:        tool,
				permissions: permissions,
				workingDir:  wd,
				cfg:         cfg,
				owner:       owner,
				output:      access,
			})
		}
	}
	return result
}

type batchManagedCallKey struct{}

// WithBatchManagedCall marks calls whose lifetime is owned by a batch execution.
func WithBatchManagedCall(ctx context.Context) context.Context {
	return context.WithValue(ctx, batchManagedCallKey{}, true)
}

// IsBatchManagedCall reports whether cancellation must reach the batch owner.
func IsBatchManagedCall(ctx context.Context) bool {
	managed, _ := ctx.Value(batchManagedCallKey{}).(bool)
	return managed
}

// Tool is a tool from a MCP.
type Tool struct {
	mcpName     string
	tool        *mcp.Tool
	cfg         *config.ConfigStore
	permissions permission.Service
	workingDir  string
	// owner is this config's MCP lifecycle owner; nil keeps the legacy
	// process-current-owner resolution (nil-safe methods).
	owner           *mcp.Owner
	providerOptions fantasy.ProviderOptions
	output          *MCPOutputAccess
}

func (m *Tool) SetProviderOptions(opts fantasy.ProviderOptions) {
	m.providerOptions = opts
}

func (m *Tool) ProviderOptions() fantasy.ProviderOptions {
	return m.providerOptions
}

func (m *Tool) Name() string {
	return fmt.Sprintf("mcp_%s_%s", m.mcpName, m.tool.Name)
}

func (m *Tool) MCP() string {
	return m.mcpName
}

func (m *Tool) MCPToolName() string {
	return m.tool.Name
}

// RestrictedRunAction identifies the permission action used by the MCP
// tool's own Request call. It lets central dispatch preserve tool:action
// allowlists even when the MCP name is dynamic.
func (m *Tool) RestrictedRunAction() string { return "execute" }

func (m *Tool) Info() fantasy.ToolInfo {
	parameters := make(map[string]any)
	required := make([]string, 0)

	if input, ok := m.tool.InputSchema.(map[string]any); ok {
		if props, ok := input["properties"].(map[string]any); ok {
			parameters = props
		}
		if req, ok := input["required"].([]any); ok {
			// Convert []any -> []string when elements are strings
			for _, v := range req {
				if s, ok := v.(string); ok {
					required = append(required, s)
				}
			}
		} else if reqStr, ok := input["required"].([]string); ok {
			// Handle case where it's already []string
			required = reqStr
		}
	}

	return fantasy.ToolInfo{
		Name:        m.Name(),
		Description: m.tool.Description,
		Parameters:  parameters,
		Required:    required,
	}
}

func (m *Tool) Run(ctx context.Context, params fantasy.ToolCall) (fantasy.ToolResponse, error) {
	sessionID := GetSessionFromContext(ctx)
	if sessionID == "" {
		return fantasy.ToolResponse{}, fmt.Errorf("session ID is required for creating a new file")
	}

	// Skip permission for whitelisted Docker MCP tools.
	if !slices.Contains(whitelistDockerTools, params.Name) {
		permissionDescription := fmt.Sprintf("execute %s with the following parameters:", m.Info().Name)
		p, err := m.permissions.Request(
			ctx,
			permission.CreatePermissionRequest{
				SessionID:   sessionID,
				ToolCallID:  params.ID,
				Path:        m.workingDir,
				ToolName:    m.Info().Name,
				Action:      "execute",
				Description: permissionDescription,
				Params:      params.Input,
			},
		)
		if err != nil {
			return fantasy.ToolResponse{}, err
		}
		if !p {
			return NewPermissionDeniedResponse(), nil
		}
	}

	if m.output != nil && m.output.Store != nil {
		ctx = mcp.WithTextAdmission(ctx, func(ctx context.Context, render func(io.Writer) error, isError bool) (string, error) {
			return m.output.admit(ctx, m.mcpName, m.Name(), params.ID, render, isError)
		})
	}
	result, err := m.owner.RunTool(ctx, m.cfg, m.mcpName, m.tool.Name, params.Input)
	if err != nil {
		text := err.Error()
		if m.output != nil && m.output.Store != nil {
			admitted, retainErr := m.output.admit(ctx, m.mcpName, m.Name(), params.ID, func(w io.Writer) error {
				_, writeErr := io.WriteString(w, text)
				return writeErr
			}, true)
			if retainErr != nil {
				text = retainErr.Error()
			} else {
				text = admitted
			}
		}
		response := fantasy.NewTextErrorResponse(text)
		if IsBatchManagedCall(ctx) && (errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)) {
			return response, err
		}
		return response, nil
	}

	switch result.Type {
	case "image", "media":
		if !GetSupportsImagesFromContext(ctx) {
			modelName := GetModelNameFromContext(ctx)
			return fantasy.NewTextErrorResponse(fmt.Sprintf("This model (%s) does not support image data.", modelName)), nil
		}

		var response fantasy.ToolResponse
		if result.Type == "image" {
			response = fantasy.NewImageResponse(result.Data, result.MediaType)
		} else {
			response = fantasy.NewMediaResponse(result.Data, result.MediaType)
		}
		response.Content = result.Content
		response.IsError = result.IsError
		return response, nil
	default:
		response := fantasy.NewTextResponse(result.Content)
		response.IsError = result.IsError
		return response, nil
	}
}

const mcpOutputScheme = "rush-mcp-output"

// MCPOutputStore is session-owned durable retention, not a filesystem escape.
// Its implementation must require trusted producer/read admission and commit
// the complete artifact and quota reservation atomically before returning a ref.
type MCPOutputStore interface {
	RetainMCPOutput(context.Context, string, string, string, bool, int64, func(io.Writer) error) (string, error)
	ReadMCPOutput(context.Context, string, string, int64, int) (string, bool, int64, []byte, error)
}

// MCPOutputAccess is built once alongside the final admitted MCP tool slice.
// A nil Store leaves legacy rendering unchanged; it must not truncate output.
// Activation awaits the finalization-aware session service's trusted adapter.
type MCPOutputAccess struct {
	Store       MCPOutputStore
	Allowed     map[string]bool
	Permissions permission.Service
	WorkingDir  string
}

func formatTruncatedOutput(start, end string, lines int) string {
	return fmt.Sprintf("%s\n\n... [%d lines truncated] ...\n\n%s", start, lines, end)
}

// mcpOutputPreview retains at most 2*MaxOutputLength bytes. WriteString avoids
// io.WriteString converting a huge SDK string into an equally huge []byte.
type mcpOutputPreview struct {
	head, tail []byte
	tailNext   int
	bytes      int64
	newlines   int
}

func (p *mcpOutputPreview) WriteString(s string) (int, error) {
	if p.head == nil {
		p.head = make([]byte, 0, MaxOutputLength)
		p.tail = make([]byte, 0, MaxOutputLength)
	}
	n := len(s)
	p.bytes += int64(n)
	p.newlines += strings.Count(s, "\n")
	if len(p.head) < MaxOutputLength {
		take := min(len(s), MaxOutputLength-len(p.head))
		p.head = append(p.head, s[:take]...)
	}
	if len(s) >= MaxOutputLength {
		p.tail = append(p.tail[:0], s[len(s)-MaxOutputLength:]...)
		p.tailNext = 0
	} else {
		if len(p.tail) < MaxOutputLength {
			take := min(len(s), MaxOutputLength-len(p.tail))
			p.tail = append(p.tail, s[:take]...)
			s = s[take:]
		}
		if len(s) > 0 {
			take := min(len(s), MaxOutputLength-p.tailNext)
			copy(p.tail[p.tailNext:], s[:take])
			copy(p.tail, s[take:])
			p.tailNext = (p.tailNext + len(s)) % MaxOutputLength
		}
	}
	return n, nil
}

func (p *mcpOutputPreview) Write(data []byte) (int, error) {
	n := len(data)
	for len(data) > 0 {
		take := min(len(data), MaxOutputLength)
		_, _ = p.WriteString(string(data[:take]))
		data = data[take:]
	}
	return n, nil
}

func (p *mcpOutputPreview) text() string {
	if p.bytes <= MaxOutputLength {
		return string(p.head)
	}
	head, _ := stringext.TruncateAt(string(p.head), MaxOutputLength/2)
	tailBytes := make([]byte, len(p.tail))
	copied := copy(tailBytes, p.tail[p.tailNext:])
	copy(tailBytes[copied:], p.tail[:p.tailNext])
	tail, _ := stringext.TruncateEndAt(string(tailBytes), MaxOutputLength/2)
	lines := p.newlines - strings.Count(head, "\n") - strings.Count(tail, "\n") + 1
	return formatTruncatedOutput(head, tail, lines)
}

func (a *MCPOutputAccess) admit(ctx context.Context, server, tool, callID string, render func(io.Writer) error, isError bool) (string, error) {
	preview := &mcpOutputPreview{}
	if err := render(preview); err != nil {
		return "", err
	}
	if preview.bytes <= MaxOutputLength {
		return preview.text(), nil
	}
	owner := GetSessionFromContext(ctx)
	if a == nil || a.Store == nil || !a.Allowed[tool] {
		return "", errors.New("RETENTION FAILED after MCP tool ran: output was not admitted; do not automatically retry the original side effect")
	}
	ref, err := a.Store.RetainMCPOutput(ctx, owner, tool, callID, isError, preview.bytes, render)
	if err != nil {
		return "", fmt.Errorf("RETENTION FAILED after MCP tool ran: full output was not retained; do not automatically retry the original side effect: %w", err)
	}
	uri := (&url.URL{Scheme: mcpOutputScheme, Host: ref}).String()
	return preview.text() + fmt.Sprintf("\n\n[Full MCP output retained: %d bytes. If read_mcp_resource is granted, read mcp_name=%q uri=%q; follow next_uri for byte pages. Retention does not grant tool access.]", preview.bytes, server, uri), nil
}

// readArtifact is a reserved LOCAL URI dispatch. It never forwards malformed,
// forbidden or missing local artifacts to an untrusted MCP server.
func (a *MCPOutputAccess) readArtifact(ctx context.Context, server, rawURI, callID string) (fantasy.ToolResponse, bool) {
	u, err := url.Parse(rawURI)
	if err != nil || !strings.EqualFold(u.Scheme, mcpOutputScheme) {
		if strings.HasPrefix(strings.ToLower(rawURI), mcpOutputScheme+":") {
			return fantasy.NewTextErrorResponse("invalid local MCP output URI"), true
		}
		return fantasy.ToolResponse{}, false
	}
	fail := func(text string) (fantasy.ToolResponse, bool) { return fantasy.NewTextErrorResponse(text), true }
	if a == nil || a.Store == nil || u.User != nil || u.Host == "" || u.Path != "" || u.Fragment != "" {
		return fail("local MCP output is unavailable or its URI is invalid")
	}
	query, err := url.ParseQuery(u.RawQuery)
	if err != nil || len(query) > 1 || len(query["offset"]) > 1 {
		return fail("invalid local MCP output byte page")
	}
	for key := range query {
		if key != "offset" {
			return fail("invalid local MCP output byte page")
		}
	}
	offset := int64(0)
	if value := query.Get("offset"); value != "" {
		offset, err = strconv.ParseInt(value, 10, 64)
		if err != nil || offset < 0 {
			return fail("invalid local MCP output byte offset")
		}
	}
	tool, sourceError, total, page, err := a.Store.ReadMCPOutput(ctx, GetSessionFromContext(ctx), u.Host, offset, MaxOutputLength)
	if err != nil {
		return fail("local MCP output is unavailable for this session")
	}
	if !a.Allowed[tool] || !strings.HasPrefix(tool, "mcp_"+server+"_") {
		return fail("the original MCP source tool is not admitted")
	}
	if a.Permissions == nil {
		return fail("MCP source permission is unavailable")
	}
	// Approval of the resource read is not approval of a different source tool.
	sourceCtx := permission.WithHookApproval(ctx, "")
	request := permission.CreatePermissionRequest{
		SessionID: GetSessionFromContext(ctx), ToolCallID: callID,
		ToolName: tool, Action: "execute", Path: a.WorkingDir,
		Description: "Read retained output of the admitted MCP source tool",
		Params:      map[string]string{"retained_output": u.Host},
	}
	if gate, ok := a.Permissions.(permission.RestrictedRunDispatchAuthorizer); ok {
		if restricted, allowed := gate.AuthorizeRestrictedDispatch(sourceCtx, request); restricted && !allowed {
			return NewPermissionDeniedResponse(), true
		}
	} else if gate, ok := a.Permissions.(permission.RestrictedRunToolAuthorizer); ok {
		if restricted, allowed := gate.AuthorizeRestrictedTool(request); restricted && !allowed {
			return NewPermissionDeniedResponse(), true
		}
	}
	allowed, err := a.Permissions.Request(sourceCtx, request)
	if err != nil || !allowed {
		return NewPermissionDeniedResponse(), true
	}
	if len(page) > 0 && !utf8.RuneStart(page[0]) {
		return fail("local MCP output offset is not a UTF-8 boundary")
	}
	if offset+int64(len(page)) < total {
		for trim := 0; trim < utf8.UTFMax && len(page) > 0 && !utf8.Valid(page); trim++ {
			page = page[:len(page)-1]
		}
	}
	if !utf8.Valid(page) || (len(page) == 0 && offset < total) {
		return fail("local MCP output contains invalid UTF-8")
	}
	next := offset + int64(len(page))
	nextURI := ""
	if next < total {
		u.RawQuery = url.Values{"offset": {strconv.FormatInt(next, 10)}}.Encode()
		nextURI = u.String()
	}
	text := string(page) + fmt.Sprintf("\n\n[Retained output page: bytes %d..%d of %d; source_is_error=%t; next_uri=%q]", offset, next, total, sourceError, nextURI)
	return fantasy.WithResponseMetadata(fantasy.NewTextResponse(text), map[string]any{
		"output_ref": u.Host, "offset": offset, "next_offset": next,
		"total_bytes": total, "eof": next == total, "next_uri": nextURI, "source_is_error": sourceError,
	}), true
}

// ForbiddenWritesEnv is the env var orchestrators set to blacklist
// specific paths from the write / edit / multiedit tools. Comma-separated
// list of absolute or working-dir-relative paths. Designed to be set by
// a parent process before exec'ing `rush run`, so a wrapper like
// `rush run --json > /tmp/out.json` can guarantee the model can't
// collide with its own stdout-redirect target — the failure mode where
// the JSON envelope ended up on line 1 and write-tool content on lines
// 2+ in the same file, observed in the shamir-db .tmp-audit-D.json case.
const ForbiddenWritesEnv = "RUSH_FORBID_WRITES"

// CheckForbiddenWrite returns an error if path is on the comma-separated
// RUSH_FORBID_WRITES env-var blacklist. Comparison uses absolute, cleaned
// paths; case-insensitive on Windows. Empty env var means no
// restrictions.
func CheckForbiddenWrite(path string) error {
	return checkForbiddenWrite(path, normalizeForbiddenPath)
}

// CheckForbiddenWriteResolved checks the target and blacklist entries after
// resolving their longest existing path prefix through symlinks. Call this
// immediately before the filesystem mutation.
func CheckForbiddenWriteResolved(path string) error {
	raw := os.Getenv(ForbiddenWritesEnv)
	if raw == "" {
		return nil
	}
	target, err := normalizeResolvedForbiddenPath(path)
	if err != nil {
		return err
	}
	for _, entry := range strings.Split(raw, ",") {
		entry = strings.TrimSpace(entry)
		if entry == "" {
			continue
		}
		forbidden, err := normalizeResolvedForbiddenPath(entry)
		if err != nil {
			return fmt.Errorf("cannot resolve forbidden write path %q: %w", entry, err)
		}
		if forbidden == target {
			return forbiddenWriteError(path)
		}
	}
	return nil
}

func checkForbiddenWrite(path string, normalize func(string) string) error {
	raw := os.Getenv(ForbiddenWritesEnv)
	if raw == "" {
		return nil
	}
	target := normalize(path)
	for _, entry := range strings.Split(raw, ",") {
		entry = strings.TrimSpace(entry)
		if entry != "" && normalize(entry) == target {
			return forbiddenWriteError(path)
		}
	}
	return nil
}

func forbiddenWriteError(path string) error {
	return fmt.Errorf(
		"write to %q forbidden by %s (this path is on the orchestrator's blacklist — typically a shell stdout-redirect target). Return the content as your tool output or in final_text, or write to a different path",
		path, ForbiddenWritesEnv,
	)
}

func normalizeResolvedForbiddenPath(p string) (string, error) {
	abs, err := filepath.Abs(p)
	if err != nil {
		return "", err
	}
	abs = filepath.Clean(abs)
	prefix := abs
	for {
		resolved, resolveErr := filepath.EvalSymlinks(prefix)
		if resolveErr == nil {
			tail, relErr := filepath.Rel(prefix, abs)
			if relErr != nil {
				return "", relErr
			}
			return normalizeForbiddenPath(filepath.Join(resolved, tail)), nil
		}
		if !os.IsNotExist(resolveErr) {
			return "", resolveErr
		}
		parent := filepath.Dir(prefix)
		if parent == prefix {
			return "", resolveErr
		}
		prefix = parent
	}
}

func normalizeForbiddenPath(p string) string {
	abs, err := filepath.Abs(p)
	if err != nil {
		abs = p
	}
	abs = filepath.Clean(abs)
	if runtime.GOOS == "windows" {
		return strings.ToLower(abs)
	}
	return abs
}

type (
	sessionIDContextKey string
	messageIDContextKey string
	supportsImagesKey   string
	modelNameKey        string
)

const (
	// SessionIDContextKey is the key for the session ID in the context.
	SessionIDContextKey sessionIDContextKey = "session_id"
	// MessageIDContextKey is the key for the message ID in the context.
	MessageIDContextKey messageIDContextKey = "message_id"
	// SupportsImagesContextKey is the key for the model's image support capability.
	SupportsImagesContextKey supportsImagesKey = "supports_images"
	// ModelNameContextKey is the key for the model name in the context.
	ModelNameContextKey modelNameKey = "model_name"
)

// getContextValue is a generic helper that retrieves a typed value from context.
// If the value is not found or has the wrong type, it returns the default value.
func getContextValue[T any](ctx context.Context, key any, defaultValue T) T {
	value := ctx.Value(key)
	if value == nil {
		return defaultValue
	}
	if typedValue, ok := value.(T); ok {
		return typedValue
	}
	return defaultValue
}

// GetSessionFromContext retrieves the session ID from the context.
func GetSessionFromContext(ctx context.Context) string {
	return getContextValue(ctx, SessionIDContextKey, "")
}

// GetMessageFromContext retrieves the message ID from the context.
func GetMessageFromContext(ctx context.Context) string {
	return getContextValue(ctx, MessageIDContextKey, "")
}

// GetSupportsImagesFromContext retrieves whether the model supports images from the context.
func GetSupportsImagesFromContext(ctx context.Context) bool {
	return getContextValue(ctx, SupportsImagesContextKey, false)
}

// GetModelNameFromContext retrieves the model name from the context.
func GetModelNameFromContext(ctx context.Context) string {
	return getContextValue(ctx, ModelNameContextKey, "")
}

// Tool error contract: the three ways a tool can fail, and when to use
// each.
//
// A fantasy.AgentTool has three failure channels, and picking the wrong
// one is the difference between "the model corrects itself and carries
// on" and "the whole run dies on the spot":
//
//   1. Recoverable — `return fantasy.NewTextErrorResponse(msg), nil`.
//      The model sees the message as an ordinary tool result and can fix
//      its input next turn. Already in use: file-not-found (with
//      spelling suggestions) and path-is-a-directory in view.go, the
//      invalid todo status in todos.go (f51baaca), non-200 HTTP status
//      in fetch/download/sourcegraph, search errors in glob/grep.
//
//   2. Recoverable, turn-ending — a level-1 response with
//      `resp.StopTurn = true`. The turn ends cleanly: the session stays
//      alive, history stays intact, and the reason is visible to the
//      operator. Already in use: permission denials
//      (NewPermissionDeniedResponse directly below), the hook veto in
//      hooked_tool.go.
//
//   3. Fatal — `return fantasy.ToolResponse{}, err`. The whole run
//      dies; see below for the mechanism.
//
// The mechanism that makes level 3 catastrophic: in fantasy v0.25.2
// (agent.go), a non-nil error from a tool's Run makes executeSingleTool
// return its second value — named isCriticalError at the call site in
// executeTools — as true, and executeTools then does
// `return nil, errorResult.Error`, unwinding the agent loop then and
// there. The model never sees the message; the run simply ends. A
// response carrying IsError returns false there instead and is fed back
// to the model as an ordinary tool result it can react to. The two forms
// look nearly identical in Go — `return resp, nil` vs `return resp, err`
// is a reflex-level edit — which is exactly why this contract exists:
// do not "simplify" a response into a returned error, and do not add new
// level-3 sites for model-input failures.
//
// Choosing a level — the retry-invariance criterion. Return level 3 if
// and only if the failure is INVARIANT TO RETRY in this session: no
// input the model could send next, and no passage of time, would make
// the call succeed. A missing session ID is fatal — the next call fails
// identically, so continuing merely loops. A failed DB read or write is
// fatal for the same reason. An unresolvable DNS name is NOT fatal —
// the next call may succeed, and the model can equally pick a different
// URL. A malformed URL, a value outside the legal set, a path the OS
// rejects, a negative offset: all model input, all correctable, all
// level 1 or 2. This criterion is checkable by reading the code, unlike
// "could the model figure out its mistake", which would require
// modelling someone else's mind.
//
// Why the default is recoverable — the radius asymmetry. An error made
// recoverable by mistake costs one extra model turn. An error made fatal
// by mistake costs the entire session — potentially many minutes of work
// on a large prompt (a single mistyped todo status did exactly that, 42
// seconds into a 75k-character run; see f51baaca). When in doubt, return
// a response, not an error.
//
// The counter-argument, so this does not read one-sided: a fatal error
// reaches the OPERATOR — it surfaces through the run's JSON envelope and
// exit status, and cannot be ignored. A NewTextErrorResponse reaches the
// MODEL, which may swallow it and cheerfully continue, masking an
// infrastructure failure as ordinary tool chatter. Blanket-demoting
// every error to level 1 is therefore just as wrong as blanket-promoting
// to level 3. Level 2 exists for precisely this middle: end the turn,
// keep the session, make the refusal loud. Infrastructure that is broken
// for good within this session (missing session ID, unusable DB) stays
// at level 3 so a human finds out.
//
// error_contract_test.go enforces the model-input half of this contract.
// It names every tool that still returns a level-3 error for model input,
// so its failure output doubles as the outstanding work list.

// NewPermissionDeniedResponse returns a tool response indicating the user
// denied permission, with StopTurn set so the agent loop does not retry.
func NewPermissionDeniedResponse() fantasy.ToolResponse {
	resp := fantasy.NewTextErrorResponse("User denied permission")
	resp.StopTurn = true
	return resp
}

// ghAvailable indicates whether the `gh` CLI is available on PATH.
var ghAvailable = func() bool {
	if testing.Testing() {
		return false
	}
	_, err := exec.LookPath("gh")
	return err == nil
}()

// toolDescriptionData is the common data structure for tool description templates.
type toolDescriptionData struct {
	GhAvailable bool
}

// renderToolDescription renders a tool description template with the given data.
func renderToolDescription(tmpl *template.Template) string {
	data := toolDescriptionData{
		GhAvailable: ghAvailable,
	}
	var out bytes.Buffer
	if err := tmpl.Execute(&out, data); err != nil {
		panic("failed to execute tool description template: " + err.Error())
	}
	return out.String()
}

// renderTemplate renders a Go template with the given data.
func renderTemplate(tmpl *template.Template, data any) string {
	var out bytes.Buffer
	if err := tmpl.Execute(&out, data); err != nil {
		panic("failed to execute tool description template: " + err.Error())
	}
	return out.String()
}

// anchoredSuggestionMaxEntries caps how many directory entries are inspected
// when offering "did you mean" suggestions for a missing file.
const anchoredSuggestionMaxEntries = 2000

type suggestionClockContextKey struct{}

// boundedReadDirNames reads up to maxEntries entries from dir in batches,
// checking ctx between batches. It returns nil if the directory cannot be
// opened or is not readable as a directory. Entries are NOT sorted; callers
// only scan them for fuzzy name matches.
func boundedReadDirNames(ctx context.Context, fsys fs.FS, dir string, maxEntries int) []fs.DirEntry {
	now := time.Now
	if override, ok := ctx.Value(suggestionClockContextKey{}).(func() time.Time); ok {
		now = override
	}
	return boundedReadDirNamesWithClock(ctx, fsys, dir, maxEntries, now)
}

func boundedReadDirNamesWithClock(ctx context.Context, fsys fs.FS, dir string, maxEntries int, now func() time.Time) []fs.DirEntry {
	deadline := now().Add(100 * time.Millisecond)
	f, err := fsys.Open(dir)
	if err != nil {
		return nil
	}
	defer f.Close()
	rdf, ok := f.(fs.ReadDirFile)
	if !ok {
		return nil
	}
	var entries []fs.DirEntry
	for len(entries) < maxEntries {
		if ctx.Err() != nil || now().After(deadline) {
			return entries
		}
		n := 256
		if remaining := maxEntries - len(entries); remaining < n {
			n = remaining
		}
		batch, err := rdf.ReadDir(n)
		entries = append(entries, batch...)
		if err != nil {
			// io.EOF is the normal end of the directory; any other error is
			// still best-effort for suggestions, so return what we have.
			return entries
		}
		if len(batch) == 0 {
			return entries
		}
	}
	return entries
}
