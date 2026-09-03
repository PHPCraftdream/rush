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
				Execute:   fsListExecute(scope, workingDir, lsConfig, disk),
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
func fsListExecute(scope permission.FolderScope, workingDir string, lsConfig config.ToolLs, disk DiskProvider) FSExecuteFunc[FSListItem] {
	return func(ctx context.Context, group FSBatchGroup[FSListItem]) ([]FSItemOutcome, error) {
		outcomes := make([]FSItemOutcome, len(group.Items))
		for i, member := range group.Items {
			block, err := fsListOne(ctx, disk, scope, workingDir, group.Path, member.Item, lsConfig)
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
//
// disk.List returns entries at whatever spelling the provider's traversal
// used, not necessarily canonical: OSDisk's fastwalk follows directory
// symlinks (Follow: true, internal/fsext/ls.go) while keeping the
// alias-spelled path for the tree. Checking that lexical spelling against
// a symlink-canonicalized scope (fs_scope.go's CanonicalizeFolderScopeSpec)
// would let a denied subtree reachable only through an aliased directory
// dodge a deny carve-out compiled against its real target (R6-2, P1
// security review finding) — the same class of namespace mismatch R5-2
// closed for directly REQUESTED paths, reopened here for RESULT paths.
// Each entry is therefore re-resolved through resolveScopedPath (the same
// algorithm and the same disk provider used everywhere else) before
// Check ever sees it; an entry that cannot be resolved is dropped, never
// rendered, exactly like a denied one — a path that cannot be judged safe
// is not safe. The TREE ITSELF still renders the entry's original,
// provider-returned spelling (native, alias included): createFileTree
// requires every entry to share the literal Dir prefix it was listed
// under, and the model is best served seeing the name it would actually
// use to reach the file again through this same tool.
func fsListOne(ctx context.Context, disk DiskProvider, scope permission.FolderScope, workingDir, absPath string, item FSListItem, lsConfig config.ToolLs) (string, error) {
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
	// entry must be resolved and re-checked, and denied or unresolvable
	// entries are dropped. Entries arrive forward-slashed (fastwalk's
	// ToSlash) with a trailing native separator on directories; FromSlash
	// restores native separators for the tree builder. The RESOLVED path
	// (not the possibly alias-spelled native one) is what Check sees —
	// see fsListOne's doc comment for why.
	dropped := 0
	allowed := make([]string, 0, len(files))
	for _, f := range files {
		native := filepath.FromSlash(f)
		resolved, err := resolveScopedPath(ctx, disk, workingDir, native)
		if err != nil {
			dropped++
			continue
		}
		if scope.Check(resolved, permission.FileOpList) != nil {
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
