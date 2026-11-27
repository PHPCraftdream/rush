package tools

import (
	"cmp"
	"context"
	_ "embed"
	"errors"
	"fmt"
	"html/template"
	"io/fs"
	"path/filepath"
	"sort"
	"strings"
	"unicode/utf8"

	"charm.land/fantasy"
	"github.com/PHPCraftdream/rush/internal/agent/safety/permission"
	"github.com/PHPCraftdream/rush/internal/agent/tools/mcp"
	"github.com/PHPCraftdream/rush/internal/config"
	"github.com/PHPCraftdream/rush/internal/kit/fs/filepathext"
	"github.com/PHPCraftdream/rush/internal/store/filetracker"
)

// fs_delete is the batch-capable delete member of the scoped fs_* family:
// N files, one permission request per target, per-item outcomes via RunFSBatch.
// Deleting is not a content write, so unlike fs_write/fs_replace/
// fs_write_lines it keeps no history and reads no filetracker state:
// each item is a stat-then-remove on the resolved path.

//go:embed fs_delete.md
var fsDeleteDescription string

type FSDeleteItem struct {
	Path string `json:"path" description:"File path, absolute or relative to the working directory"`
}

type FSDeleteParams struct {
	Items []FSDeleteItem `json:"items" description:"Files to delete. Files only: directories and anything that is not a regular file are refused per item; there is no recursion"`
}

type FSDeletePermissionsParams struct {
	Paths []string `json:"paths"`
}

const FSDeleteToolName = "fs_delete"

func NewFSDeleteTool(scope permission.FolderScope, permissions permission.Service, workingDir string, disk DiskProvider) fantasy.AgentTool {
	disk = diskOrOS(disk)
	return fantasy.NewAgentTool(
		FSDeleteToolName,
		fsDeleteDescription,
		func(ctx context.Context, params FSDeleteParams, call fantasy.ToolCall) (fantasy.ToolResponse, error) {
			if len(params.Items) == 0 {
				return fantasy.NewTextErrorResponse("at least one item is required"), nil
			}
			if len(params.Items) > FSBatchMaxItems {
				return fantasy.NewTextErrorResponse(fmt.Sprintf(
					"too many items: %d (maximum %d per call)", len(params.Items), FSBatchMaxItems)), nil
			}

			sessionID := GetSessionFromContext(ctx)
			if sessionID == "" {
				return fantasy.ToolResponse{}, fmt.Errorf("session_id is required")
			}

			joinedPaths := make([]string, len(params.Items))
			for i, item := range params.Items {
				if item.Path == "" {
					return fantasy.NewTextErrorResponse(fmt.Sprintf("items[%d]: path is required", i)), nil
				}
				joinedPaths[i] = filepathext.SmartJoin(workingDir, item.Path)
			}

			// Every target path is authorized before any batch work, so a denial
			// ends the turn before the runner touches the filesystem.
			p, err := requestFSBatchPermissions(ctx, permissions, sessionID, call.ID, FSDeleteToolName, "delete", fmt.Sprintf("delete %d files", len(params.Items)), joinedPaths, workingDir, FSDeletePermissionsParams{Paths: joinedPaths})
			if err != nil {
				return fantasy.ToolResponse{}, err
			}
			if !p {
				return NewPermissionDeniedResponse(), nil
			}

			resp, err := RunFSBatch(ctx, FSBatch[FSDeleteItem]{
				Tool:       FSDeleteToolName,
				WorkingDir: workingDir,
				Scope:      scope,
				Items:      params.Items,
				Disk:       disk,
				PathOf: func(i FSDeleteItem) string {
					return i.Path
				},
				Preflight:   fsDeletePreflight,
				ResolvePath: fsDeleteResolvePath,
				Execute: func(ctx context.Context, group FSBatchGroup[FSDeleteItem]) ([]FSItemOutcome, error) {
					return fsDeleteExecuteGroup(ctx, disk, group)
				},
			})
			return resp, err
		},
	)
}

// fsDeleteResolvePath resolves parent components (including parent symlinks)
// while retaining the final entry's own name. Deleting a symlink acts on the
// link itself, so its target must not affect containment.
func fsDeleteResolvePath(ctx context.Context, disk DiskProvider, workingDir, raw string) (string, error) {
	joined := filepath.Clean(filepathext.SmartJoin(workingDir, raw))
	parent, err := resolveScopedPath(ctx, disk, workingDir, filepath.Dir(joined))
	if err != nil {
		return "", err
	}
	return filepath.Join(parent, filepath.Base(joined)), nil
}

// fsDeletePreflight grants delete unconditionally: existence and type are
// verified during execution, where the lstat check happens anyway.
func fsDeletePreflight(_ context.Context, item FSDeleteItem, _ int, _ string) (permission.FileOp, error) {
	return permission.FileOpDelete, nil
}

// fsDeleteExecuteGroup removes each item of one same-path group in
// order, independently: the first removes the file, later duplicates
// honestly report "file not found". There is no history write, no
// filetracker read and no diff — deletion is not a content write.
func fsDeleteExecuteGroup(ctx context.Context, disk DiskProvider, group FSBatchGroup[FSDeleteItem]) ([]FSItemOutcome, error) {
	disk = diskOrOS(disk)
	outcomes := make([]FSItemOutcome, len(group.Items))

	for i := range group.Items {
		info, err := disk.Stat(ctx, group.Path)
		if lstat, ok := disk.(interface {
			Lstat(context.Context, string) (fs.FileInfo, error)
		}); ok {
			info, err = lstat.Lstat(ctx, group.Path)
		}
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				outcomes[i] = FSItemOutcome{
					Status: FSStatusFailed,
					Error:  "file not found: " + group.Path,
				}
			} else {
				outcomes[i] = FSItemOutcome{
					Status: FSStatusFailed,
					Error:  fmt.Sprintf("cannot access %s: %v", group.Path, err),
				}
			}
			continue
		}
		if info.IsDir() {
			outcomes[i] = FSItemOutcome{
				Status: FSStatusFailed,
				Error:  fmt.Sprintf("%s is a directory — fs_delete removes regular files only and does not recurse", group.Path),
			}
			continue
		}
		if !info.Mode().IsRegular() && info.Mode()&fs.ModeSymlink == 0 {
			outcomes[i] = FSItemOutcome{
				Status: FSStatusFailed,
				Error:  fmt.Sprintf("%s is not a regular file", group.Path),
			}
			continue
		}
		if err := disk.Remove(ctx, group.Path); err != nil {
			outcomes[i] = FSItemOutcome{
				Status: FSStatusFailed,
				Error:  fmt.Sprintf("cannot delete %s: %v", group.Path, err),
			}
			continue
		}
		outcomes[i] = FSItemOutcome{Status: FSStatusOK}
	}

	return outcomes, nil
}

const FSReadToolName = "fs_read"

// fsReadFullFileLimit is the line count handed to readTextFile for
// full-file and open-ended reads; readTextFile's maxContentSize byte cap
// (MaxViewSize) is what actually bounds the result.
const fsReadFullFileLimit = 1_000_000

const fsReadMaxWindowLines = 1_000_000

//go:embed fs_read.md.tpl
var fsReadDescriptionTmpl []byte

var fsReadDescriptionTpl = template.Must(
	template.New("fsReadDescription").
		Parse(string(fsReadDescriptionTmpl)),
)

type fsReadDescriptionData struct {
	MaxViewSizeKB int
}

func fsReadDescription() string {
	return renderTemplate(fsReadDescriptionTpl, fsReadDescriptionData{
		MaxViewSizeKB: MaxViewSize / 1024,
	})
}

type FSReadItem struct {
	Path      string `json:"path" description:"File to read, absolute or relative to the working directory"`
	StartLine int    `json:"start_line,omitempty" description:"First line to read (1-based, inclusive); pair with end_line, not with line/radius"`
	EndLine   int    `json:"end_line,omitempty" description:"Last line to read (1-based, inclusive); omit to read to end of file"`
	Line      int    `json:"line,omitempty" description:"Center line for a radius read (1-based); pair with radius, not with start_line/end_line"`
	Radius    int    `json:"radius,omitempty" description:"Lines of context on each side of line"`
}

type FSReadParams struct {
	Items []FSReadItem `json:"items" description:"Reads to perform. Each item is scope-checked and reported independently (max 50 per call)"`
}

// NewFSReadTool builds the scoped, batch-capable file-reading tool. The
// zero FolderScope denies everything, which is the safe default. Unlike
// fs_list, a read always requires an explicit path: there is no
// default-scope-root fallback.
//
// A successful read records itself via filetracker.RecordRead, the same
// as the legacy view tool: fs_replace/fs_write_lines both require a
// non-zero LastReadTime before they will act, and fs_read is the only
// read tool that survives into a folder-scoped toolset (the legacy view
// tool is stripped), so without this a scoped run could never satisfy
// the read-before-write gate on a file it only ever read.
func NewFSReadTool(scope permission.FolderScope, filetracker filetracker.Service, workingDir string, disk DiskProvider) fantasy.AgentTool {
	disk = diskOrOS(disk)
	return fantasy.NewAgentTool(
		FSReadToolName,
		fsReadDescription(),
		func(ctx context.Context, params FSReadParams, call fantasy.ToolCall) (fantasy.ToolResponse, error) {
			return RunFSBatch(ctx, FSBatch[FSReadItem]{
				Tool: FSReadToolName, WorkingDir: workingDir, Scope: scope, Items: params.Items, Disk: disk,
				PathOf:    func(item FSReadItem) string { return item.Path },
				Preflight: fsReadPreflight,
				Execute:   fsReadExecute(disk, filetracker),
			})
		},
	)
}

// fsReadWindow translates one item's addressing mode into view.go's
// 0-based offset and line count, plus the 1-based number of the first
// line of the result.
type fsReadWindow struct {
	offset    int
	limit     int
	firstLine int
}

// fsReadWindowOf resolves one item's addressing mode. An item must use
// exactly one of the three modes: full file (path only), a 1-based
// inclusive range (start_line/end_line), or a center line with a radius
// (line/radius). The empty-path check comes first so an item without a
// path fails even when it also carries range or radius fields.
func fsReadWindowOf(item FSReadItem) (fsReadWindow, error) {
	if strings.TrimSpace(item.Path) == "" {
		return fsReadWindow{}, fmt.Errorf("path is required")
	}
	hasRange := item.StartLine != 0 || item.EndLine != 0
	hasRadius := item.Line != 0 || item.Radius != 0
	switch {
	case hasRange && hasRadius:
		return fsReadWindow{}, fmt.Errorf("specify either start_line/end_line or line/radius, not both")
	case hasRange:
		if item.StartLine < 1 {
			return fsReadWindow{}, fmt.Errorf("start_line must be 1 or greater")
		}
		if item.StartLine > fsReadMaxWindowLines {
			return fsReadWindow{}, fmt.Errorf("start_line must be no greater than %d", fsReadMaxWindowLines)
		}
		if item.EndLine != 0 && item.EndLine < item.StartLine {
			return fsReadWindow{}, fmt.Errorf("end_line (%d) is before start_line (%d)", item.EndLine, item.StartLine)
		}
		if item.EndLine > fsReadMaxWindowLines {
			return fsReadWindow{}, fmt.Errorf("end_line must be no greater than %d", fsReadMaxWindowLines)
		}
		limit := fsReadFullFileLimit
		if item.EndLine != 0 {
			limit = item.EndLine - item.StartLine + 1
		}
		if limit < 1 || limit > fsReadMaxWindowLines {
			return fsReadWindow{}, fmt.Errorf("line range is too large (maximum %d lines)", fsReadMaxWindowLines)
		}
		return fsReadWindow{offset: item.StartLine - 1, limit: limit, firstLine: item.StartLine}, nil
	case hasRadius:
		if item.Line < 1 {
			return fsReadWindow{}, fmt.Errorf("line must be 1 or greater")
		}
		if item.Line > fsReadMaxWindowLines {
			return fsReadWindow{}, fmt.Errorf("line must be no greater than %d", fsReadMaxWindowLines)
		}
		if item.Radius < 0 {
			return fsReadWindow{}, fmt.Errorf("radius must be 0 or greater")
		}
		if item.Radius > (fsReadMaxWindowLines-1)/2 {
			return fsReadWindow{}, fmt.Errorf("radius is too large (maximum %d)", (fsReadMaxWindowLines-1)/2)
		}
		first := max(item.Line-item.Radius, 1)
		return fsReadWindow{offset: first - 1, limit: 2*item.Radius + 1, firstLine: first}, nil
	default:
		return fsReadWindow{offset: 0, limit: fsReadFullFileLimit, firstLine: 1}, nil
	}
}

// fsReadPreflight rejects structurally invalid addressing before any
// execution; the runner applies the scope check itself.
func fsReadPreflight(_ context.Context, item FSReadItem, _ int, _ string) (permission.FileOp, error) {
	_, err := fsReadWindowOf(item)
	if err != nil {
		return "", err
	}
	return permission.FileOpRead, nil
}

// fsReadExecute reads one group of items sharing a resolved file,
// reporting one outcome per item in order. Scope checks already happened
// in the preflight, so no permission work happens here. Any successful
// read records the group's file once in filetracker, when a session ID
// is available, so a later fs_replace/fs_write_lines on the same file
// can pass its read-before-write check.
func fsReadExecute(disk DiskProvider, tracker filetracker.Service) FSExecuteFunc[FSReadItem] {
	return func(ctx context.Context, group FSBatchGroup[FSReadItem]) ([]FSItemOutcome, error) {
		outcomes := make([]FSItemOutcome, len(group.Items))
		anyOK := false
		for i, member := range group.Items {
			block, err := fsReadOne(ctx, disk, group.Path, member.RawPath, member.Item)
			if err != nil {
				outcomes[i] = FSItemOutcome{Status: FSStatusFailed, Error: err.Error()}
				continue
			}
			outcomes[i] = FSItemOutcome{Status: FSStatusOK, Block: block}
			anyOK = true
		}
		if anyOK {
			if sessionID := GetSessionFromContext(ctx); sessionID != "" {
				tracker.RecordRead(ctx, sessionID, group.Path)
			}
		}
		return outcomes, nil
	}
}

// fsReadOne reads one window of one file and renders it as a <file>
// block in view.go's shape, extended with path, lines and status
// attributes. absPath is the resolved path from the execution group;
// rawPath is echoed exactly as the model sent it.
func fsReadOne(ctx context.Context, disk DiskProvider, absPath string, rawPath string, item FSReadItem) (string, error) {
	win, err := fsReadWindowOf(item) // validated by preflight; recompute defensively
	if err != nil {
		return "", err
	}
	window, err := readTextFileWindow(ctx, disk, absPath, win.offset, win.limit, MaxViewSize)
	if err != nil {
		var tooLarge contentTooLargeError
		if errors.As(err, &tooLarge) {
			return "", fmt.Errorf("file section is too large (%d bytes, maximum %d); use start_line/end_line to read a range", tooLarge.Size, tooLarge.Max)
		}
		return "", fmt.Errorf("error reading file: %w", err)
	}
	content := window.content
	if !utf8.ValidString(content) {
		return "", fmt.Errorf("file content is not valid UTF-8")
	}

	var lastLine int
	firstLine := win.firstLine
	if window.lineCount > 0 {
		lastLine = win.firstLine + window.lineCount - 1
	} else {
		firstLine, lastLine = 0, 0
	}

	var b strings.Builder
	fmt.Fprintf(&b, "<file path=%q lines=%q status=\"ok\">\n", rawPath, fmt.Sprintf("%d-%d", firstLine, lastLine))
	b.WriteString(addLineNumbersForWindow(content, win.firstLine, window.lineCount))
	if window.hasMore {
		fmt.Fprintf(&b, "\n\n(File has more lines. Use start_line/end_line to read beyond line %d)", lastLine)
	}
	b.WriteString("\n</file>\n")
	return b.String(), nil
}

type ListMCPResourcesParams struct {
	MCPName string `json:"mcp_name" description:"The MCP server name"`
}

type ListMCPResourcesPermissionsParams struct {
	MCPName string `json:"mcp_name"`
}

const ListMCPResourcesToolName = "list_mcp_resources"

//go:embed list_mcp_resources.md
var listMCPResourcesDescription string

func NewListMCPResourcesTool(cfg *config.ConfigStore, permissions permission.Service, owner *mcp.Owner) fantasy.AgentTool {
	return fantasy.NewParallelAgentTool(
		ListMCPResourcesToolName,
		listMCPResourcesDescription,
		func(ctx context.Context, params ListMCPResourcesParams, call fantasy.ToolCall) (fantasy.ToolResponse, error) {
			params.MCPName = strings.TrimSpace(params.MCPName)
			if params.MCPName == "" {
				return fantasy.NewTextErrorResponse("mcp_name parameter is required"), nil
			}

			sessionID := GetSessionFromContext(ctx)
			if sessionID == "" {
				return fantasy.ToolResponse{}, fmt.Errorf("session ID is required for listing MCP resources")
			}

			relPath := filepathext.SmartJoin(cfg.WorkingDir(), params.MCPName)
			p, err := permissions.Request(
				ctx,
				permission.CreatePermissionRequest{
					SessionID:   sessionID,
					Path:        relPath,
					ToolCallID:  call.ID,
					ToolName:    ListMCPResourcesToolName,
					Action:      "list",
					Description: fmt.Sprintf("List MCP resources from %s", params.MCPName),
					Params:      ListMCPResourcesPermissionsParams(params),
				},
			)
			if err != nil {
				return fantasy.ToolResponse{}, err
			}
			if !p {
				return NewPermissionDeniedResponse(), nil
			}

			resources, err := owner.ListResources(ctx, cfg, params.MCPName)
			if err != nil {
				return fantasy.NewTextErrorResponse(err.Error()), nil
			}
			if len(resources) == 0 {
				return fantasy.NewTextResponse("No resources found"), nil
			}

			lines := make([]string, 0, len(resources))
			for _, resource := range resources {
				if resource == nil {
					continue
				}
				title := cmp.Or(resource.Title, resource.Name, resource.URI)
				line := fmt.Sprintf("- %s", title)
				if resource.URI != "" {
					line = fmt.Sprintf("%s (%s)", line, resource.URI)
				}
				if resource.Description != "" {
					line = fmt.Sprintf("%s: %s", line, resource.Description)
				}
				if resource.MIMEType != "" {
					line = fmt.Sprintf("%s [mime: %s]", line, resource.MIMEType)
				}
				if resource.Size > 0 {
					line = fmt.Sprintf("%s [size: %d]", line, resource.Size)
				}
				lines = append(lines, line)
			}

			sort.Strings(lines)
			return fantasy.NewTextResponse(strings.Join(lines, "\n")), nil
		},
	)
}
