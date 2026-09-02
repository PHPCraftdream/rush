package tools

import (
	"cmp"
	"context"
	_ "embed"
	"fmt"
	"html/template"
	"path/filepath"
	"strings"

	"charm.land/fantasy"
	"github.com/PHPCraftdream/rush/internal/config"
	"github.com/PHPCraftdream/rush/internal/permission"
)

const FSListToolName = "fs_list"

//go:embed fs_list.md.tpl
var fsListDescriptionTmpl []byte

var fsListDescriptionTpl = template.Must(
	template.New("fsListDescription").
		Parse(string(fsListDescriptionTmpl)),
)

type fsListDescriptionData struct {
	MaxFiles int
}

func fsListDescription() string {
	return renderTemplate(fsListDescriptionTpl, fsListDescriptionData{
		MaxFiles: maxLSFiles,
	})
}

type FSListItem struct {
	Path   string   `json:"path,omitempty" description:"Directory to list, absolute or relative to the working directory (defaults to the deepest folder-scope root that grants list)"`
	Ignore []string `json:"ignore,omitempty" description:"List of glob patterns to ignore"`
	Depth  int      `json:"depth,omitempty" description:"The maximum depth to traverse"`
}

type FSListParams struct {
	Items []FSListItem `json:"items" description:"Directories to list. Each item is scope-checked and reported independently (max 50 per call)"`
}

// NewFSListTool builds the scoped, batch-capable directory-listing tool.
// The zero FolderScope denies everything, which is the safe default.
func NewFSListTool(scope permission.FolderScope, workingDir string, lsConfig config.ToolLs, disk DiskProvider) fantasy.AgentTool {
	disk = diskOrOS(disk)
	return fantasy.NewAgentTool(
		FSListToolName,
		fsListDescription(),
		func(ctx context.Context, params FSListParams, call fantasy.ToolCall) (fantasy.ToolResponse, error) {
			items := make([]FSListItem, len(params.Items))
			copy(items, params.Items)
			for i := range items {
				if items[i].Path == "" {
					if root, ok := fsDefaultScopeRoot(scope, permission.FileOpList); ok {
						items[i].Path = root
					}
				}
			}

			return RunFSBatch(ctx, FSBatch[FSListItem]{
				Tool: FSListToolName, WorkingDir: workingDir, Scope: scope, Items: items, Disk: disk,
				PathOf:    func(item FSListItem) string { return item.Path },
				Preflight: fsListPreflight,
				Execute:   fsListExecute(scope, lsConfig, disk),
			})
		},
	)
}

// fsDefaultScopeRoot returns the scope's first (deepest) root granting
// op, for items that omit an explicit path.
func fsDefaultScopeRoot(scope permission.FolderScope, op permission.FileOp) (string, bool) {
	roots := scope.Roots(op)
	if len(roots) == 0 {
		return "", false
	}
	return roots[0], true
}

// fsListPreflight resolves each item's operation and rejects an empty
// path before the runner's scope check can misjudge it.
func fsListPreflight(_ context.Context, item FSListItem, _ int, _ string) (permission.FileOp, error) {
	if strings.TrimSpace(item.Path) == "" {
		return "", fmt.Errorf("path is required (no folder-scope root grants list to default to)")
	}
	return permission.FileOpList, nil
}

// fsListExecute lists one group of items sharing a resolved directory,
// reporting one outcome per item in order.
func fsListExecute(scope permission.FolderScope, lsConfig config.ToolLs, disk DiskProvider) FSExecuteFunc[FSListItem] {
	return func(ctx context.Context, group FSBatchGroup[FSListItem]) ([]FSItemOutcome, error) {
		outcomes := make([]FSItemOutcome, len(group.Items))
		for i, member := range group.Items {
			block, err := fsListOne(ctx, disk, scope, group.Path, member.Item, lsConfig)
			if err != nil {
				outcomes[i] = FSItemOutcome{Status: FSStatusFailed, Error: err.Error()}
				continue
			}
			outcomes[i] = FSItemOutcome{Status: FSStatusOK, Block: block}
		}
		return outcomes, nil
	}
}

// fsListOne lists one directory with policy filtering: disk.List does no
// scope checking, so every entry is re-checked and denied entries are
// dropped before the tree is rendered.
func fsListOne(ctx context.Context, disk DiskProvider, scope permission.FolderScope, absPath string, item FSListItem, lsConfig config.ToolLs) (string, error) {
	info, err := disk.Stat(ctx, absPath)
	if err != nil {
		return "", fmt.Errorf("path does not exist: %s", absPath)
	}
	if !info.IsDir() {
		return "", fmt.Errorf("not a directory: %s", absPath)
	}
	cfgDepth, cfgLimit := lsConfig.Limits()
	maxFiles := cmp.Or(cfgLimit, maxLSFiles)
	listResult, err := disk.List(ctx, ListRequest{
		Dir:            absPath,
		IgnorePatterns: item.Ignore,
		Depth:          cmp.Or(item.Depth, cfgDepth),
		Limit:          maxFiles,
	})
	if err != nil {
		return "", fmt.Errorf("error listing directory: %w", err)
	}
	files, truncated := listResult.Entries, listResult.Truncated

	// Policy filter: ListDirectory applies no scope policy, so every
	// entry must pass Check with FileOpList; denied entries are
	// dropped. Entries arrive forward-slashed (fastwalk's ToSlash) with
	// a trailing native separator on directories; FromSlash restores
	// native separators for both the scope match and the tree builder.
	dropped := 0
	allowed := make([]string, 0, len(files))
	for _, f := range files {
		native := filepath.FromSlash(f)
		if scope.Check(filepath.Clean(native), permission.FileOpList) != nil {
			dropped++
			continue
		}
		allowed = append(allowed, native)
	}

	tree := createFileTree(allowed, absPath)
	var output string
	if truncated {
		output = fmt.Sprintf(
			"There are more than %d files in the directory. Use a more specific path or a higher depth.\n", maxFiles)
	}
	if dropped > 0 {
		output += fmt.Sprintf("(%d entries hidden by the folder scope)\n", dropped)
	}
	return output + "\n" + printTree(tree, absPath), nil
}
