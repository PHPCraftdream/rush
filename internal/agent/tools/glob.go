package tools

import (
	"bufio"
	"cmp"
	"container/heap"
	"context"
	_ "embed"
	"errors"
	"fmt"
	"html/template"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"

	"charm.land/fantasy"
	"github.com/PHPCraftdream/rush/internal/agent/safety/permission"
	"github.com/PHPCraftdream/rush/internal/config"
	"github.com/PHPCraftdream/rush/internal/kit/fs/filepathext"
	"github.com/PHPCraftdream/rush/internal/kit/fs/fsext"
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
	resolvedCache := make(map[string]string, len(files))
	for _, f := range files {
		native := filepath.FromSlash(f)
		resolved, ok := resolvedCache[native]
		if !ok {
			var err error
			resolved, err = resolveScopedPath(ctx, disk, workingDir, native)
			if err != nil {
				resolvedCache[native] = ""
				dropped++
				continue
			}
			resolvedCache[native] = resolved
		}
		if resolved == "" {
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

const GlobToolName = "glob"

var (
	// ErrGlobRipgrepOutput identifies a malformed or otherwise incomplete
	// NUL-delimited rg stream. Such output is not safe to replace with a
	// second search engine because records may have been lost.
	ErrGlobRipgrepOutput = errors.New("ripgrep glob output error")
	// ErrGlobRipgrepRecordTooLong identifies one NUL record over the bound.
	ErrGlobRipgrepRecordTooLong = errors.New("ripgrep glob record too long")
	// ErrGlobRipgrepIncompleteRecord identifies a final record without NUL.
	ErrGlobRipgrepIncompleteRecord = errors.New("ripgrep glob output has an incomplete record")
)

const (
	maxRipgrepGlobRecordBytes = 1 * 1024 * 1024
)

// GlobRipgrepError preserves the kind of a bounded rg stream failure after
// the child has been drained and reaped.
type GlobRipgrepError struct {
	err        error
	diagnostic string
}

func (e *GlobRipgrepError) Error() string {
	if e.diagnostic == "" {
		return e.err.Error()
	}
	return fmt.Sprintf("%v: %s", e.err, e.diagnostic)
}

func (e *GlobRipgrepError) Unwrap() error { return e.err }

func (e *GlobRipgrepError) Is(target error) bool {
	return target == ErrGlobRipgrepOutput || errors.Is(e.err, target)
}

type globMatch struct {
	path string
	seq  int64
}

// globRipgrepRecordHookKey is a test-only context seam.
type globRipgrepRecordHookKey struct{}

// boundedGlobHeap keeps the longest paths at its root. A later path of the
// same length is also worse, preserving rg's stable path-length ordering.
type boundedGlobHeap []globMatch

func (h boundedGlobHeap) Len() int { return len(h) }
func (h boundedGlobHeap) Less(i, j int) bool {
	if len(h[i].path) != len(h[j].path) {
		return len(h[i].path) > len(h[j].path)
	}
	return h[i].seq > h[j].seq
}
func (h boundedGlobHeap) Swap(i, j int) { h[i], h[j] = h[j], h[i] }
func (h *boundedGlobHeap) Push(x any)   { *h = append(*h, x.(globMatch)) }
func (h *boundedGlobHeap) Pop() any {
	old := *h
	n := len(old)
	x := old[n-1]
	*h = old[:n-1]
	return x
}

func globMatchIsWorse(a, b globMatch) bool {
	if len(a.path) != len(b.path) {
		return len(a.path) > len(b.path)
	}
	return a.seq > b.seq
}

//go:embed glob.md.tpl
var globDescriptionTmpl []byte

var globDescriptionTpl = template.Must(
	template.New("globDescription").
		Parse(string(globDescriptionTmpl)),
)

type globDescriptionData struct {
	MaxResults int
}

func globDescription() string {
	return renderTemplate(globDescriptionTpl, globDescriptionData{
		MaxResults: 100,
	})
}

type GlobParams struct {
	Pattern string `json:"pattern" description:"The glob pattern to match files against"`
	Path    string `json:"path,omitempty" description:"The directory to search in. Defaults to the current working directory."`
}

type GlobResponseMetadata struct {
	NumberOfFiles int  `json:"number_of_files"`
	Truncated     bool `json:"truncated"`
}

func NewGlobTool(workingDir string, permissionServices ...permission.Service) fantasy.AgentTool {
	var permissions permission.Service
	if len(permissionServices) > 0 {
		permissions = permissionServices[0]
	}
	return fantasy.NewAgentTool(
		GlobToolName,
		globDescription(),
		func(ctx context.Context, params GlobParams, call fantasy.ToolCall) (fantasy.ToolResponse, error) {
			if params.Pattern == "" {
				return fantasy.NewTextErrorResponse("pattern is required"), nil
			}

			searchPath := filepathext.SmartJoin(workingDir, cmp.Or(params.Path, workingDir))
			anchor, allowed, err := authorizeWorkspaceRead(
				ctx,
				permissions,
				workingDir,
				searchPath,
				GlobToolName,
				"read",
				call.ID,
				params,
			)
			if err != nil {
				return fantasy.NewTextErrorResponse(fmt.Sprintf("error authorizing search path: %v", err)), nil
			}
			if !allowed {
				return NewPermissionDeniedResponse(), nil
			}
			defer anchor.Close()

			files, truncated, note, err := globFilesFS(ctx, params.Pattern, anchor.FS(), anchor.rootPath(), anchor.displayRoot(), 100)
			if err != nil {
				return fantasy.NewTextErrorResponse(fmt.Sprintf("error finding files: %v", err)), nil
			}

			var output string
			if len(files) == 0 {
				output = "No files found"
			} else {
				normalizeFilePaths(files)
				output = strings.Join(files, "\n")
				if truncated {
					output += "\n\n(Results are truncated. Consider using a more specific path or pattern.)"
				}
			}
			if note != "" {
				output += "\n\n" + note
			}

			return fantasy.WithResponseMetadata(
				fantasy.NewTextResponse(output),
				GlobResponseMetadata{
					NumberOfFiles: len(files),
					Truncated:     truncated,
				},
			), nil
		},
	)
}

// globFiles searches with the non-FS ripgrep path when available; that path
// is ctx-bounded but NOT entry-budgeted.
func globFiles(ctx context.Context, pattern, searchPath string, limit int) ([]string, bool, error) {
	cmdRg := getRgCmd(ctx, pattern)
	if cmdRg != nil {
		cmdRg.Dir = searchPath
		matches, truncated, err := runRipgrepBounded(ctx, cmdRg, searchPath, limit)
		if err == nil {
			return matches, truncated, nil
		}
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, false, ctxErr
		}
		var streamErr *GlobRipgrepError
		if errors.As(err, &streamErr) {
			return nil, false, err
		}
		slog.Warn("Ripgrep execution failed, falling back to doublestar", "error", err)
	}

	return fsext.GlobGitignoreAwareNoFollow(pattern, searchPath, limit)
}

// globFilesFS is the anchored, budget-bounded glob over an fs.FS.
func globFilesFS(ctx context.Context, pattern string, fsys fs.FS, rootPath, displayRoot string, limit int) ([]string, bool, string, error) {
	return fsext.GlobGitignoreAwareFSBounded(ctx, fsys, rootPath, displayRoot, pattern, limit)
}

func runRipgrep(cmd *exec.Cmd, searchRoot string, limit int) ([]string, error) {
	matches, _, err := runRipgrepBounded(context.Background(), cmd, searchRoot, limit)
	return matches, err
}

// runRipgrepBounded streams NUL-delimited --files output. The heap retains
// only the shortest paths; sorting it by length and sequence exactly matches
// the old stable sort followed by truncation.
func runRipgrepBounded(ctx context.Context, cmd *exec.Cmd, searchRoot string, limit int) ([]string, bool, error) {
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, false, err
	}
	var diagnostic boundedRipgrepDiagnostic
	cmd.Stderr = &diagnostic
	if err := cmd.Start(); err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, false, ctxErr
		}
		return nil, false, fmt.Errorf("ripgrep: %w", err)
	}

	capacity := limit
	if capacity < 1 {
		capacity = 0
	}
	h := make(boundedGlobHeap, 0, capacity)
	var total int64
	var seq int64
	reader := bufio.NewReader(stdout)
	var parseErr error
	for {
		record, readErr := readBoundedNULRecord(reader, maxRipgrepGlobRecordBytes)
		if readErr == io.EOF {
			break
		}
		if readErr != nil {
			parseErr = &GlobRipgrepError{err: readErr}
			break
		}
		if len(record) == 0 {
			continue
		}
		absPath := filepathext.SmartJoin(searchRoot, string(record))
		if fsext.SkipHidden(absPath) {
			continue
		}
		if hook, ok := ctx.Value(globRipgrepRecordHookKey{}).(func(string)); ok {
			hook(absPath)
		}
		total++
		seq++
		candidate := globMatch{path: absPath, seq: seq}
		if capacity == 0 {
			// A zero limit is the documented unlimited mode. It cannot use a
			// bounded result set because the returned result itself is unlimited.
			h = append(h, candidate)
		} else if len(h) < capacity {
			heap.Push(&h, candidate)
		} else if !globMatchIsWorse(candidate, h[0]) {
			h[0] = candidate
			heap.Fix(&h, 0)
		}
	}

	if parseErr != nil || ctx.Err() != nil {
		_, _ = io.Copy(io.Discard, stdout)
	}
	waitErr := cmd.Wait()
	if ctxErr := ctx.Err(); ctxErr != nil {
		return nil, false, ctxErr
	}
	if parseErr != nil {
		var streamErr *GlobRipgrepError
		if errors.As(parseErr, &streamErr) {
			streamErr.diagnostic = diagnostic.String()
		}
		return nil, false, parseErr
	}
	if waitErr != nil {
		if ee, ok := waitErr.(*exec.ExitError); ok && ee.ExitCode() == 1 {
			return nil, false, nil
		}
		if diagnostic.String() != "" {
			waitErr = fmt.Errorf("%w: %s", waitErr, diagnostic.String())
		}
		return nil, false, fmt.Errorf("ripgrep: %w", waitErr)
	}

	matches := make([]string, len(h))
	sort.Slice(h, func(i, j int) bool {
		if len(h[i].path) != len(h[j].path) {
			return len(h[i].path) < len(h[j].path)
		}
		return h[i].seq < h[j].seq
	})
	for i, match := range h {
		matches[i] = match.path
	}
	return matches, limit > 0 && total > int64(limit), nil
}

func readBoundedNULRecord(reader *bufio.Reader, maxBytes int) ([]byte, error) {
	var record []byte
	for {
		part, err := reader.ReadSlice(0)
		if len(part) > 0 {
			if len(record)+len(part) > maxBytes+1 {
				return nil, fmt.Errorf("%w: record exceeds %d bytes", ErrGlobRipgrepRecordTooLong, maxBytes)
			}
			record = append(record, part...)
		}
		if err == nil {
			return record[:len(record)-1], nil
		}
		if errors.Is(err, bufio.ErrBufferFull) {
			continue
		}
		if errors.Is(err, io.EOF) {
			if len(record) == 0 {
				return nil, io.EOF
			}
			if len(record) > maxBytes {
				return nil, fmt.Errorf("%w: record exceeds %d bytes", ErrGlobRipgrepRecordTooLong, maxBytes)
			}
			return nil, ErrGlobRipgrepIncompleteRecord
		}
		return nil, err
	}
}

func normalizeFilePaths(paths []string) {
	for i, p := range paths {
		paths[i] = filepath.ToSlash(p)
	}
}

type LSParams struct {
	Path   string   `json:"path,omitempty" description:"The path to the directory to list (defaults to current working directory)"`
	Ignore []string `json:"ignore,omitempty" description:"List of glob patterns to ignore"`
	Depth  int      `json:"depth,omitempty" description:"The maximum depth to traverse"`
}

type LSPermissionsParams struct {
	Path   string   `json:"path"`
	Ignore []string `json:"ignore"`
	Depth  int      `json:"depth"`
}

type NodeType string

const (
	NodeTypeFile      NodeType = "file"
	NodeTypeDirectory NodeType = "directory"
)

type TreeNode struct {
	Name     string      `json:"name"`
	Path     string      `json:"path"`
	Type     NodeType    `json:"type"`
	Children []*TreeNode `json:"children,omitempty"`
}

type LSResponseMetadata struct {
	NumberOfFiles int  `json:"number_of_files"`
	Truncated     bool `json:"truncated"`
}

const (
	LSToolName = "ls"
	maxLSFiles = 1000
)

//go:embed ls.md.tpl
var lsDescriptionTmpl []byte

var lsDescriptionTpl = template.Must(
	template.New("lsDescription").
		Parse(string(lsDescriptionTmpl)),
)

type lsDescriptionData struct {
	MaxFiles int
}

func lsDescription() string {
	return renderTemplate(lsDescriptionTpl, lsDescriptionData{
		MaxFiles: maxLSFiles,
	})
}

func NewLsTool(permissions permission.Service, workingDir string, lsConfig config.ToolLs) fantasy.AgentTool {
	return fantasy.NewAgentTool(
		LSToolName,
		lsDescription(),
		func(ctx context.Context, params LSParams, call fantasy.ToolCall) (fantasy.ToolResponse, error) {
			searchPath, err := fsext.Expand(cmp.Or(params.Path, workingDir))
			if err != nil {
				return fantasy.NewTextErrorResponse(fmt.Sprintf("error expanding path: %v", err)), nil
			}

			searchPath = filepathext.SmartJoin(workingDir, searchPath)

			anchor, allowed, authErr := authorizeWorkspaceRead(
				ctx,
				permissions,
				workingDir,
				searchPath,
				LSToolName,
				"list",
				call.ID,
				LSPermissionsParams(params),
			)
			if authErr != nil {
				return fantasy.NewTextErrorResponse(fmt.Sprintf("error authorizing search path: %v", authErr)), nil
			}
			if !allowed {
				return NewPermissionDeniedResponse(), nil
			}
			defer anchor.Close()

			output, metadata, err := listDirectoryTreeFS(ctx, anchor.FS(), anchor.displayRoot(), anchor.rootPath(), params, lsConfig)
			if err != nil {
				return fantasy.NewTextErrorResponse(err.Error()), nil
			}

			return fantasy.WithResponseMetadata(
				fantasy.NewTextResponse(output),
				metadata,
			), nil
		},
	)
}

func ListDirectoryTree(searchPath string, params LSParams, lsConfig config.ToolLs) (string, LSResponseMetadata, error) {
	if _, err := os.Stat(searchPath); os.IsNotExist(err) {
		return "", LSResponseMetadata{}, fmt.Errorf("path does not exist: %s", searchPath)
	}

	depth, limit := lsConfig.Limits()
	maxFiles := cmp.Or(limit, maxLSFiles)
	files, truncated, err := fsext.ListDirectoryNoFollow(
		searchPath,
		params.Ignore,
		cmp.Or(params.Depth, depth),
		maxFiles,
	)
	if err != nil {
		return "", LSResponseMetadata{}, fmt.Errorf("error listing directory: %w", err)
	}

	metadata := LSResponseMetadata{
		NumberOfFiles: len(files),
		Truncated:     truncated,
	}
	tree := createFileTree(files, searchPath)

	var output string
	if truncated {
		output = fmt.Sprintf("There are more than %d files in the directory. Use a more specific path or use the Glob tool to find specific files. The first %[1]d files and directories are included below.\n", maxFiles)
	}
	if depth > 0 {
		output = fmt.Sprintf("The directory tree is shown up to a depth of %d. Use a higher depth and a specific path to see more levels.\n", cmp.Or(params.Depth, depth))
	}
	return output + "\n" + printTree(tree, searchPath), metadata, nil
}

func listDirectoryTreeFS(ctx context.Context, fsys fs.FS, displayRoot string, start string, params LSParams, lsConfig config.ToolLs) (string, LSResponseMetadata, error) {
	if _, err := fs.Stat(fsys, start); errors.Is(err, fs.ErrNotExist) {
		return "", LSResponseMetadata{}, fmt.Errorf("path does not exist: %s", displayRoot)
	}
	depth, limit := lsConfig.Limits()
	maxFiles := cmp.Or(limit, maxLSFiles)
	files, truncated, note, err := fsext.ListDirectoryFSBounded(ctx, fsys, start, displayRoot, params.Ignore, cmp.Or(params.Depth, depth), maxFiles)
	if err != nil {
		return "", LSResponseMetadata{}, fmt.Errorf("error listing directory: %w", err)
	}
	metadata := LSResponseMetadata{NumberOfFiles: len(files), Truncated: truncated}
	tree := createFileTree(files, displayRoot)
	var output string
	if truncated {
		output = fmt.Sprintf("There are more than %d files in the directory. Use a more specific path or use the Glob tool to find specific files. The first %[1]d files and directories are included below.\n", maxFiles)
	}
	if depth > 0 {
		output = fmt.Sprintf("The directory tree is shown up to a depth of %d. Use a higher depth and a specific path to see more levels.\n", cmp.Or(params.Depth, depth))
	}
	if note != "" {
		output += "\n" + note
	}
	return output + "\n" + printTree(tree, displayRoot), metadata, nil
}

func createFileTree(sortedPaths []string, rootPath string) []*TreeNode {
	root := []*TreeNode{}
	pathMap := make(map[string]*TreeNode)

	for _, path := range sortedPaths {
		relativePath := strings.TrimPrefix(path, rootPath)
		parts := strings.Split(relativePath, string(filepath.Separator))
		currentPath := ""
		var parentPath string

		var cleanParts []string
		for _, part := range parts {
			if part != "" {
				cleanParts = append(cleanParts, part)
			}
		}
		parts = cleanParts

		if len(parts) == 0 {
			continue
		}

		for i, part := range parts {
			if currentPath == "" {
				currentPath = part
			} else {
				currentPath = filepath.Join(currentPath, part)
			}

			if _, exists := pathMap[currentPath]; exists {
				parentPath = currentPath
				continue
			}

			isLastPart := i == len(parts)-1
			isDir := !isLastPart || strings.HasSuffix(relativePath, string(filepath.Separator))
			nodeType := NodeTypeFile
			if isDir {
				nodeType = NodeTypeDirectory
			}
			newNode := &TreeNode{
				Name:     part,
				Path:     currentPath,
				Type:     nodeType,
				Children: []*TreeNode{},
			}

			pathMap[currentPath] = newNode

			if i > 0 && parentPath != "" {
				if parent, ok := pathMap[parentPath]; ok {
					parent.Children = append(parent.Children, newNode)
				}
			} else {
				root = append(root, newNode)
			}

			parentPath = currentPath
		}
	}

	return root
}

func printTree(tree []*TreeNode, rootPath string) string {
	var result strings.Builder

	result.WriteString("- ")
	result.WriteString(filepath.ToSlash(rootPath))
	if rootPath[len(rootPath)-1] != '/' {
		result.WriteByte('/')
	}
	result.WriteByte('\n')

	for _, node := range tree {
		printNode(&result, node, 1)
	}

	return result.String()
}

func printNode(builder *strings.Builder, node *TreeNode, level int) {
	indent := strings.Repeat("  ", level)

	nodeName := node.Name
	if node.Type == NodeTypeDirectory {
		nodeName = nodeName + "/"
	}

	fmt.Fprintf(builder, "%s- %s\n", indent, nodeName)

	if node.Type == NodeTypeDirectory && len(node.Children) > 0 {
		for _, child := range node.Children {
			printNode(builder, child, level+1)
		}
	}
}
