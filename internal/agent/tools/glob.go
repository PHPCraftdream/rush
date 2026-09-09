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
	"log/slog"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"

	"charm.land/fantasy"
	"github.com/PHPCraftdream/rush/internal/filepathext"
	"github.com/PHPCraftdream/rush/internal/fsext"
	"github.com/PHPCraftdream/rush/internal/permission"
)

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

			files, truncated, err := globFilesFS(ctx, params.Pattern, anchor, 100)
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

func globFilesFS(_ context.Context, pattern string, anchor *readAnchor, limit int) ([]string, bool, error) {
	return fsext.GlobGitignoreAwareFS(anchor.FS(), anchor.rootPath(), anchor.displayRoot(), pattern, limit)
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
