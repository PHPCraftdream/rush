package tools

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/url"
	"strconv"
	"strings"
	"unicode/utf8"

	"charm.land/fantasy"
	"github.com/PHPCraftdream/rush/internal/permission"
	"github.com/PHPCraftdream/rush/internal/stringext"
)

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
