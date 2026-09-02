package tools

import (
	"context"
	_ "embed"
	"errors"
	"fmt"
	"html/template"
	"strings"
	"unicode/utf8"

	"charm.land/fantasy"
	"github.com/PHPCraftdream/rush/internal/filetracker"
	"github.com/PHPCraftdream/rush/internal/permission"
)

const FSReadToolName = "fs_read"

// fsReadFullFileLimit is the line count handed to readTextFile for
// full-file and open-ended reads; readTextFile's maxContentSize byte cap
// (MaxViewSize) is what actually bounds the result.
const fsReadFullFileLimit = 1_000_000

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
		if item.EndLine != 0 && item.EndLine < item.StartLine {
			return fsReadWindow{}, fmt.Errorf("end_line (%d) is before start_line (%d)", item.EndLine, item.StartLine)
		}
		limit := fsReadFullFileLimit
		if item.EndLine != 0 {
			limit = item.EndLine - item.StartLine + 1
		}
		return fsReadWindow{offset: item.StartLine - 1, limit: limit, firstLine: item.StartLine}, nil
	case hasRadius:
		if item.Line < 1 {
			return fsReadWindow{}, fmt.Errorf("line must be 1 or greater")
		}
		if item.Radius < 0 {
			return fsReadWindow{}, fmt.Errorf("radius must be 0 or greater")
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
	content, hasMore, err := readTextFileFrom(ctx, disk, absPath, win.offset, win.limit, MaxViewSize)
	if err != nil {
		var tooLarge contentTooLargeError
		if errors.As(err, &tooLarge) {
			return "", fmt.Errorf("file section is too large (%d bytes, maximum %d); use start_line/end_line to read a range", tooLarge.Size, tooLarge.Max)
		}
		return "", fmt.Errorf("error reading file: %w", err)
	}
	if !utf8.ValidString(content) {
		return "", fmt.Errorf("file content is not valid UTF-8")
	}

	lastLine := win.firstLine - 1
	if content != "" {
		lastLine = win.firstLine + strings.Count(content, "\n")
	}

	var b strings.Builder
	fmt.Fprintf(&b, "<file path=%q lines=%q status=\"ok\">\n", rawPath, fmt.Sprintf("%d-%d", win.firstLine, lastLine))
	b.WriteString(addLineNumbers(content, win.firstLine))
	if hasMore {
		fmt.Fprintf(&b, "\n\n(File has more lines. Use start_line/end_line to read beyond line %d)", lastLine)
	}
	b.WriteString("\n</file>\n")
	return b.String(), nil
}
