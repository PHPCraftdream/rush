package tools

import (
	"context"
	_ "embed"
	"fmt"
	"html/template"
	"strings"

	"charm.land/fantasy"
	"github.com/PHPCraftdream/rush/internal/permission"
)

const FSFindToolName = "fs_find"

// FSFindMaxResults caps the matches one fs_find item may report. Keep
// in step with glob.go's own literal 100.
const FSFindMaxResults = 100

//go:embed fs_find.md.tpl
var fsFindDescriptionTmpl []byte

var fsFindDescriptionTpl = template.Must(
	template.New("fsFindDescription").
		Parse(string(fsFindDescriptionTmpl)),
)

type fsFindDescriptionData struct {
	MaxResults int
}

func fsFindDescription() string {
	return renderTemplate(fsFindDescriptionTpl, fsFindDescriptionData{
		MaxResults: FSFindMaxResults,
	})
}

type FSFindItem struct {
	Pattern string `json:"pattern" description:"The glob pattern to match files against"`
	Path    string `json:"path,omitempty" description:"Directory to search in, absolute or relative to the working directory (defaults to the deepest folder-scope root that grants find)"`
}

type FSFindParams struct {
	Items []FSFindItem `json:"items" description:"Searches to run. Each item is scope-checked and reported independently (max 50 per call)"`
}

// NewFSFindTool builds the scoped, batch-capable file-name search tool.
// The zero FolderScope denies everything, which is the safe default.
func NewFSFindTool(scope permission.FolderScope, workingDir string, disk DiskProvider) fantasy.AgentTool {
	disk = diskOrOS(disk)
	return fantasy.NewAgentTool(
		FSFindToolName,
		fsFindDescription(),
		func(ctx context.Context, params FSFindParams, call fantasy.ToolCall) (fantasy.ToolResponse, error) {
			items := make([]FSFindItem, len(params.Items))
			copy(items, params.Items)
			for i := range items {
				if items[i].Path == "" {
					if root, ok := fsDefaultScopeRoot(scope, permission.FileOpFind); ok {
						items[i].Path = root
					}
				}
			}

			return RunFSBatch(ctx, FSBatch[FSFindItem]{
				Tool: FSFindToolName, WorkingDir: workingDir, Scope: scope, Items: items, Disk: disk,
				PathOf:    func(item FSFindItem) string { return item.Path },
				Preflight: fsFindPreflight,
				Execute:   fsFindExecute(scope, workingDir, disk),
			})
		},
	)
}

// fsFindPreflight validates each item structurally; the pattern is the
// only required field, the path is resolved and scope-checked by the
// runner itself.
func fsFindPreflight(_ context.Context, item FSFindItem, _ int, _ string) (permission.FileOp, error) {
	if strings.TrimSpace(item.Pattern) == "" {
		return "", fmt.Errorf("pattern is required")
	}
	return permission.FileOpFind, nil
}

// fsFindExecute searches one group of items sharing a resolved
// directory, reporting one outcome per item in order.
func fsFindExecute(scope permission.FolderScope, workingDir string, disk DiskProvider) FSExecuteFunc[FSFindItem] {
	return func(ctx context.Context, group FSBatchGroup[FSFindItem]) ([]FSItemOutcome, error) {
		outcomes := make([]FSItemOutcome, len(group.Items))
		for i, member := range group.Items {
			block, err := fsFindOne(ctx, disk, scope, workingDir, group.Path, member.Item)
			if err != nil {
				outcomes[i] = FSItemOutcome{Status: FSStatusFailed, Error: err.Error()}
				continue
			}
			outcomes[i] = FSItemOutcome{Status: FSStatusOK, Block: block}
		}
		return outcomes, nil
	}
}

// fsFindOne searches one directory by pattern with policy filtering:
// disk.Find applies no scope policy, so every result path is re-checked
// and denied results are dropped before rendering.
//
// FindResult.Paths are documented as absolute matches, not canonical ones
// (fs_provider.go): a custom provider, or a future OSDisk change, may
// return an alias-spelled path (e.g. a followed directory symlink) whose
// canonical target sits under a symlink-canonicalized deny carve-out
// (fs_scope.go's CanonicalizeFolderScopeSpec). Checking the lexical
// spelling directly would let such a result dodge the carve-out — the
// same namespace-mismatch class R5-2 closed for directly REQUESTED paths,
// and R6-2 (P1 security review finding) found reopened here for RESULT
// paths, mirroring fs_list's fsListOne. Each result is therefore
// re-resolved through resolveScopedPath before Check ever sees it; an
// unresolvable result is dropped, never rendered, exactly like a denied
// one. The rendered output still shows the result's original,
// provider-returned spelling — the model needs the name it would
// actually use to reach the file again.
func fsFindOne(ctx context.Context, disk DiskProvider, scope permission.FolderScope, workingDir, searchPath string, item FSFindItem) (string, error) {
	findResult, err := disk.Find(ctx, FindRequest{Pattern: item.Pattern, Dir: searchPath, Limit: FSFindMaxResults})
	if err != nil {
		return "", fmt.Errorf("error finding files: %w", err)
	}
	files, truncated := findResult.Paths, findResult.Truncated

	// Policy filter: globFiles applies no scope policy, so every result
	// path must be resolved and re-checked, and denied or unresolvable
	// results are dropped. The RESOLVED path (not the possibly
	// alias-spelled result path) is what Check sees — see fsFindOne's
	// doc comment for why. The rendered path stays the original spelling.
	dropped := 0
	kept := make([]string, 0, len(files))
	for _, f := range files {
		resolved, err := resolveScopedPath(ctx, disk, workingDir, f)
		if err != nil {
			dropped++
			continue
		}
		if scope.Check(resolved, permission.FileOpFind) != nil {
			dropped++
			continue
		}
		kept = append(kept, f)
	}
	normalizeFilePaths(kept)

	var b strings.Builder
	if len(kept) == 0 {
		b.WriteString("No files found")
	} else {
		b.WriteString(strings.Join(kept, "\n"))
	}
	if truncated {
		b.WriteString("\n\n(Results are truncated. Consider using a more specific path or pattern.)")
	}
	if dropped > 0 {
		fmt.Fprintf(&b, "\n\n(%d results hidden by the folder scope)", dropped)
	}
	return b.String(), nil
}
