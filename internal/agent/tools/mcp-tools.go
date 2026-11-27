package tools

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/url"
	"slices"
	"strconv"
	"strings"
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
