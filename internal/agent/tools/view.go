package tools

import (
	"bufio"
	"context"
	_ "embed"
	"errors"
	"fmt"
	"html/template"
	"io"
	"io/fs"
	"net/http"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"unicode/utf8"

	"charm.land/fantasy"
	"github.com/PHPCraftdream/rush/internal/filepathext"
	"github.com/PHPCraftdream/rush/internal/filetracker"
	"github.com/PHPCraftdream/rush/internal/permission"
	"github.com/PHPCraftdream/rush/internal/skills"
	"github.com/PHPCraftdream/rush/internal/stringext"
)

//go:embed view.md.tpl
var viewDescriptionTmpl []byte

var viewDescriptionTpl = template.Must(
	template.New("viewDescription").
		Parse(string(viewDescriptionTmpl)),
)

type viewDescriptionData struct {
	DefaultReadLimit int
	MaxViewSizeKB    int
}

func viewDescription() string {
	return renderTemplate(viewDescriptionTpl, viewDescriptionData{
		DefaultReadLimit: DefaultReadLimit,
		MaxViewSizeKB:    MaxViewSize / 1024,
	})
}

type ViewParams struct {
	FilePath string `json:"file_path" description:"The path to the file to read"`
	Offset   int    `json:"offset,omitempty" description:"The line number to start reading from (0-based)"`
	Limit    int    `json:"limit,omitempty" description:"The number of lines to read (defaults to 500)"`
}

type ViewPermissionsParams struct {
	FilePath string `json:"file_path"`
	Offset   int    `json:"offset"`
	Limit    int    `json:"limit"`
}

type ViewResourceType string

const (
	ViewResourceUnset ViewResourceType = ""
	ViewResourceSkill ViewResourceType = "skill"
)

type viewAfterAnchorSeamKey struct{}

func withViewAfterAnchorSeam(ctx context.Context, seam func(*readAnchor)) context.Context {
	return context.WithValue(ctx, viewAfterAnchorSeamKey{}, seam)
}

type viewSkillParserKey struct{}

func withViewSkillParser(ctx context.Context, parser func([]byte) (*skills.Skill, error)) context.Context {
	return context.WithValue(ctx, viewSkillParserKey{}, parser)
}

type ViewResponseMetadata struct {
	FilePath            string           `json:"file_path"`
	Content             string           `json:"content"`
	ResourceType        ViewResourceType `json:"resource_type,omitempty"`
	ResourceName        string           `json:"resource_name,omitempty"`
	ResourceDescription string           `json:"resource_description,omitempty"`
}

const (
	ViewToolName = "view"
	MaxViewSize  = 200 * 1024 // 200KB
	// Fork merge note (origin/main 1811bec2 "fix(prompts): tweak file reads
	// to encourage more targeted reads"): upstream cut the default from
	// 2000 to 200 to push the model toward offset/limit usage. We picked
	// 500 as a compromise — small enough to discourage "read everything",
	// large enough to cover most of our .go files in one pass so
	// cliprovider subprocess agents (claude/codex/gemini) don't have to
	// round-trip for every read.
	DefaultReadLimit = 500
	MaxLineLength    = 2000
	viewReaderBuffer = 4096
	maxReadLines     = 1_000_000
)

type contentTooLargeError struct {
	Size int
	Max  int
}

func (e contentTooLargeError) Error() string {
	return fmt.Sprintf("content section is too large (%d bytes). Maximum size is %d bytes", e.Size, e.Max)
}

func NewViewTool(
	permissions permission.Service,
	filetracker filetracker.Service,
	skillTracker *skills.Tracker,
	workingDir string,
	skillsPaths ...string,
) fantasy.AgentTool {
	return fantasy.NewAgentTool(
		ViewToolName,
		viewDescription(),
		func(ctx context.Context, params ViewParams, call fantasy.ToolCall) (fantasy.ToolResponse, error) {
			if params.FilePath == "" {
				return fantasy.NewTextErrorResponse("file_path is required"), nil
			}
			if err := validateViewParams(params); err != nil {
				return fantasy.NewTextErrorResponse(err.Error()), nil
			}

			// Handle builtin skill files (rush: prefix).
			if strings.HasPrefix(params.FilePath, skills.BuiltinPrefix) {
				resp, err := readBuiltinFile(params, skillTracker)
				return resp, err
			}

			// Handle relative paths
			filePath := filepathext.SmartJoin(workingDir, params.FilePath)

			// Check if file is outside working directory and request permission if needed
			_, err := filepath.Abs(workingDir)
			if err != nil {
				// workingDir is tool wiring, not model input, so strictly
				// this failure is retry-invariant. It still answers as a
				// response, matching ls.go's identical site, because the
				// likeliest cause — the process cwd having been deleted or
				// unmounted — is something the operator can repair while
				// the session stays alive.
				return fantasy.NewTextErrorResponse(fmt.Sprintf(
					"Error resolving the working directory %q: %v. The session's working directory cannot be resolved to an absolute path — it may have been deleted or unmounted. No file was read. This is an environment problem for the operator, not a path problem.",
					workingDir, err)), nil
			}

			absFilePath, err := filepath.Abs(filePath)
			if err != nil {
				// The model's own path string made the OS refuse to even
				// resolve it; that is correctable input, so level 1.
				return fantasy.NewTextErrorResponse(fmt.Sprintf(
					"The OS rejected the file path %q: %v. The path string itself is invalid — common causes are an embedded NUL or other control character, a name too long for the filesystem, or a malformed path component. Nothing was read. Resend the path built from regular characters and valid separators.",
					params.FilePath, err)), nil
			}

			isSkillFile := isInSkillsPath(absFilePath, skillsPaths)

			sessionID := GetSessionFromContext(ctx)
			if sessionID == "" {
				// Deliberately unconditional, unlike ls.go where the check
				// lives inside the outside-workdir branch: a successful read
				// records the file in the session's file tracker at the
				// bottom of this function, so the session ID is needed on
				// the common path too, not only for the permission request.
				// A missing session ID is wiring, invariant to retry, so it
				// stays fatal (level 3).
				return fantasy.ToolResponse{}, fmt.Errorf("session ID is required for recording file reads and for accessing files outside the working directory")
			}

			// Resolve symlink aliases before any Stat/read operation follows them.
			var anchor *readAnchor
			if !isSkillFile {
				var allowed bool
				var authErr error
				anchor, allowed, authErr = authorizeWorkspaceRead(
					ctx,
					permissions,
					workingDir,
					absFilePath,
					ViewToolName,
					"read",
					call.ID,
					ViewPermissionsParams(params),
				)
				if authErr != nil {
					return fantasy.NewTextErrorResponse(fmt.Sprintf("Cannot access %s: %v", absFilePath, authErr)), nil
				}
				if !allowed {
					return NewPermissionDeniedResponse(), nil
				}
			} else {
				anchor, err = anchorExternalPath(absFilePath)
				if err != nil {
					return fantasy.NewTextErrorResponse(fmt.Sprintf("Cannot access %s: %v", absFilePath, err)), nil
				}
			}
			defer anchor.Close()
			if seam, ok := ctx.Value(viewAfterAnchorSeamKey{}).(func(*readAnchor)); ok {
				seam(anchor)
			}
			if err := ctx.Err(); err != nil {
				return fantasy.ToolResponse{}, err
			}

			// Check if file exists through the anchored root.
			fileInfo, err := anchor.root.Stat(anchor.rootPath())
			if ctxErr := ctx.Err(); ctxErr != nil {
				return fantasy.ToolResponse{}, ctxErr
			}
			if err != nil {
				if errors.Is(err, fs.ErrNotExist) {
					// Try to offer suggestions for similarly named files
					suggestions := anchoredSuggestions(anchor, filePath)
					if len(suggestions) > 0 {
						return fantasy.NewTextErrorResponse(fmt.Sprintf("File not found: %s\n\nDid you mean one of these?\n%s",
							filePath, strings.Join(suggestions, "\n"))), nil
					}
					return fantasy.NewTextErrorResponse(fmt.Sprintf("File not found: %s", filePath)), nil
				}
				if isSharingViolation(err) {
					return fantasy.NewTextErrorResponse(fmt.Sprintf("Cannot read %s: %v", filePath, err)), nil
				}
				if osFailureIsFatal(err) {
					return fantasy.ToolResponse{}, fmt.Errorf("error accessing file: %w", err)
				}
				return fantasy.NewTextErrorResponse(fmt.Sprintf(
					"Cannot access %s: %v. The OS rejected this path — common causes are no read permission somewhere along the path, a path component that is a file rather than a directory, a symlink loop, or a name too long for the filesystem. Nothing was read. Try a different path or a corrected form of this one.",
					filePath, err)), nil
			}

			// Check if it's a directory or another non-regular object before
			// opening. This keeps FIFO/device opens fail-closed.
			if fileInfo.IsDir() {
				return fantasy.NewTextErrorResponse(fmt.Sprintf("Path is a directory, not a file: %s", filePath)), nil
			}
			if !fileInfo.Mode().IsRegular() {
				return fantasy.NewTextErrorResponse(fmt.Sprintf("Path is not a regular file: %s", filePath)), nil
			}

			// Set the default line window. External skill files remain byte-bounded.
			if params.Limit <= 0 {
				if isSkillFile {
					params.Limit = maxReadLines
				} else {
					params.Limit = DefaultReadLimit
				}
			}

			isSupportedImage, mimeType := getImageMimeType(anchor.rootPath())
			file, openErr := openOwnedRegularAnchorFile(ctx, anchor)
			if openErr != nil {
				if osFailureIsFatal(openErr) {
					return fantasy.ToolResponse{}, fmt.Errorf("error opening file: %w", openErr)
				}
				return fantasy.NewTextErrorResponse(fmt.Sprintf(
					"Cannot read %s: %v. The file was found but could not be opened as a regular file. No file content was returned. Try a different path or use a corrected form of this one.",
					filePath, openErr)), nil
			}
			defer file.Close()
			if isSupportedImage {
				if !GetSupportsImagesFromContext(ctx) {
					modelName := GetModelNameFromContext(ctx)
					return fantasy.NewTextErrorResponse(fmt.Sprintf("This model (%s) does not support image data.", modelName)), nil
				}

				imageData, readErr := readBoundedBytes(ctx, file, MaxViewSize)
				if readErr != nil {
					var tooLarge contentTooLargeError
					if errors.As(readErr, &tooLarge) {
						return fantasy.NewTextErrorResponse(fmt.Sprintf("Image file is too large (%d bytes). Maximum size is %d bytes",
							tooLarge.Size, tooLarge.Max)), nil
					}
					if osFailureIsFatal(readErr) {
						return fantasy.ToolResponse{}, fmt.Errorf("error reading image file: %w", readErr)
					}
					return fantasy.NewTextErrorResponse(fmt.Sprintf(
						"Cannot read image file %s: %v. The file was found but could not be read — it may have been deleted or locked by another process since it was found, or the path lacks read permission. No image content was returned. Try viewing it again or use a different path.",
						filePath, readErr)), nil
				}

				// Some tools save files with a mismatched extension
				// (e.g. pinchtab writes JPEG bytes to a .png file).
				// Providers like Anthropic strictly validate the
				// media type against the base64 magic bytes and 400
				// on mismatch, so prefer the sniffed type whenever
				// it identifies a supported image format.
				mimeType = sniffImageMimeType(imageData, mimeType)

				return fantasy.NewImageResponse(imageData, mimeType), nil
			}

			// External skill files are model-visible OS files and keep the same
			// finite byte cap as ordinary files. Embedded builtin skills are
			// handled separately above.
			maxContentSize := MaxViewSize
			window, err := readTextFileWindowFromReader(ctx, file, params.Offset, params.Limit, maxContentSize)
			if err != nil {
				var tooLarge contentTooLargeError
				if errors.As(err, &tooLarge) {
					return fantasy.NewTextErrorResponse(fmt.Sprintf("Content section is too large (%d bytes). Maximum size is %d bytes",
						tooLarge.Size, tooLarge.Max)), nil
				}
				if osFailureIsFatal(err) {
					return fantasy.ToolResponse{}, fmt.Errorf("error reading file: %w", err)
				}
				return fantasy.NewTextErrorResponse(fmt.Sprintf(
					"Cannot read %s: %v. The file was found but could not be read — it may have been deleted or locked by another process since it was found, or the path lacks read permission. No file content was returned. Try viewing it again or use a different path.",
					filePath, err)), nil
			}
			content := window.content
			if !utf8.ValidString(content) {
				return fantasy.NewTextErrorResponse("File content is not valid UTF-8"), nil
			}

			output := "<file>\n"
			output += addLineNumbersForCount(content, params.Offset+1, window.lineCount)

			if window.hasMore {
				output += fmt.Sprintf("\n\n(File has more lines. Use 'offset' parameter to read beyond line %d)",
					params.Offset+window.lineCount)
			}
			output += "\n</file>\n"
			filetracker.RecordRead(ctx, sessionID, filePath)

			meta := ViewResponseMetadata{
				FilePath: filePath,
				Content:  content,
			}
			if isSkillFile && params.Offset == 0 {
				parseSkill := skills.ParseContent
				if injected, ok := ctx.Value(viewSkillParserKey{}).(func([]byte) (*skills.Skill, error)); ok {
					parseSkill = injected
				}
				if skill, err := parseSkill([]byte(content)); err == nil {
					meta.ResourceType = ViewResourceSkill
					meta.ResourceName = skill.Name
					meta.ResourceDescription = skill.Description
					skillTracker.MarkLoaded(skill.Name)
				}
			}

			return fantasy.WithResponseMetadata(
				fantasy.NewTextResponse(output),
				meta,
			), nil
		},
	)
}

func anchoredSuggestions(anchor *readAnchor, requestedPath string) []string {
	rootPath := anchor.rootPath()
	parent := filepath.ToSlash(filepath.Dir(filepath.FromSlash(rootPath)))
	if parent == "" {
		parent = "."
	}
	entries, err := fs.ReadDir(anchor.FS(), parent)
	if err != nil {
		return nil
	}
	base := strings.ToLower(filepath.Base(requestedPath))
	suggestions := make([]string, 0, 3)
	for _, entry := range entries {
		name := entry.Name()
		lowerName := strings.ToLower(name)
		if strings.Contains(lowerName, base) || strings.Contains(base, lowerName) {
			suggestions = append(suggestions, filepath.Join(filepath.Dir(requestedPath), name))
			if len(suggestions) >= 3 {
				break
			}
		}
	}
	return suggestions
}

func isSharingViolation(err error) bool {
	if runtime.GOOS != "windows" {
		return false
	}
	var errno syscall.Errno
	return errors.As(err, &errno) && errno == syscall.Errno(32)
}

func addLineNumbers(content string, startLine int) string {
	if content == "" {
		return ""
	}

	lines := strings.Split(content, "\n")

	var result []string
	for i, line := range lines {
		line = strings.TrimSuffix(line, "\r")

		lineNum := i + startLine
		numStr := fmt.Sprintf("%d", lineNum)

		if len(numStr) >= 6 {
			result = append(result, fmt.Sprintf("%s|%s", numStr, line))
		} else {
			paddedNum := fmt.Sprintf("%6s", numStr)
			result = append(result, fmt.Sprintf("%s|%s", paddedNum, line))
		}
	}

	return strings.Join(result, "\n")
}

func addLineNumbersForCount(content string, startLine, lineCount int) string {
	if content != "" || lineCount != 1 {
		return addLineNumbers(content, startLine)
	}
	return fmt.Sprintf("%6d|", startLine)
}

func validateViewParams(params ViewParams) error {
	if params.Offset < 0 {
		return fmt.Errorf("offset must be 0 or greater")
	}
	if params.Offset > maxReadLines {
		return fmt.Errorf("offset must be no greater than %d", maxReadLines)
	}
	if params.Limit < 0 {
		return fmt.Errorf("limit must be 0 or greater")
	}
	if params.Limit > maxReadLines {
		return fmt.Errorf("limit must be no greater than %d", maxReadLines)
	}
	return nil
}

// readTextFile keeps its exact signature for the legacy view tool: it
// always reads through the real disk. fs_read routes through
// readTextFileFrom instead, so it can honour an injected DiskProvider.
func readTextFile(filePath string, offset, limit, maxContentSize int) (string, bool, error) {
	return readTextFileFrom(context.Background(), OSDisk(), filePath, offset, limit, maxContentSize)
}

func readTextFileFromAnchor(ctx context.Context, anchor *readAnchor, offset, limit, maxContentSize int) (string, bool, error) {
	window, err := readTextFileWindowFromAnchor(ctx, anchor, offset, limit, maxContentSize)
	if err != nil {
		return "", false, err
	}
	return window.content, window.hasMore, nil
}

func readFileFromAnchor(anchor *readAnchor) ([]byte, error) {
	file, err := openOwnedRegularAnchorFile(context.Background(), anchor)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	return readBoundedBytes(context.Background(), file, MaxViewSize)
}

// readTextFileFrom is readTextFile's provider-aware core: identical
// bounded line-window logic, its one real disk call (Open) routed
// through disk instead of os.Open directly.
func readTextFileFrom(ctx context.Context, disk DiskProvider, filePath string, offset, limit, maxContentSize int) (string, bool, error) {
	window, err := readTextFileWindow(ctx, disk, filePath, offset, limit, maxContentSize)
	if err != nil {
		return "", false, err
	}
	return window.content, window.hasMore, nil
}

type textReadWindow struct {
	content   string
	hasMore   bool
	lineCount int
}

func readTextFileWindow(ctx context.Context, disk DiskProvider, filePath string, offset, limit, maxContentSize int) (textReadWindow, error) {
	if err := validateReadWindow(offset, limit, maxContentSize); err != nil {
		return textReadWindow{}, err
	}
	disk = diskOrOS(disk)
	var file io.ReadCloser
	var err error
	if IsOSDisk(disk) {
		file, err = openOwnedRegularOSFile(ctx, filePath)
	} else {
		if err = ctx.Err(); err == nil {
			file, err = disk.Open(ctx, filePath)
			if err == nil {
				file = ownReadCloser(ctx, file)
			}
		}
	}
	if err != nil {
		return textReadWindow{}, err
	}
	defer file.Close()
	if err := ctx.Err(); err != nil {
		return textReadWindow{}, err
	}
	return readTextFileWindowFromReader(ctx, file, offset, limit, maxContentSize)
}

func readTextFileWindowFromAnchor(ctx context.Context, anchor *readAnchor, offset, limit, maxContentSize int) (textReadWindow, error) {
	if err := validateReadWindow(offset, limit, maxContentSize); err != nil {
		return textReadWindow{}, err
	}
	file, err := openOwnedRegularAnchorFile(ctx, anchor)
	if err != nil {
		return textReadWindow{}, err
	}
	defer file.Close()
	return readTextFileWindowFromReader(ctx, file, offset, limit, maxContentSize)
}

func readTextFileWindowFromReader(ctx context.Context, file io.Reader, offset, limit, maxContentSize int) (textReadWindow, error) {
	if err := validateReadWindow(offset, limit, maxContentSize); err != nil {
		return textReadWindow{}, err
	}

	reader := bufio.NewReaderSize(contextReader{ctx: ctx, reader: file}, viewReaderBuffer)
	skipped := 0
	for skipped < offset {
		line, err := readViewBoundedLine(ctx, reader, false)
		if err != nil {
			if err == io.EOF {
				return textReadWindow{}, nil
			}
			return textReadWindow{}, err
		}
		if !line.terminated {
			return textReadWindow{}, nil
		}
		skipped++
	}

	lines := make([]string, 0, min(limit, DefaultReadLimit))
	contentSize := 0

	for len(lines) < limit {
		line, err := readViewBoundedLine(ctx, reader, true)
		if err != nil {
			if err == io.EOF {
				break
			}
			return textReadWindow{}, err
		}
		if !line.present {
			break
		}
		lineText := line.text
		projectedSize := contentSize + len(lineText)
		if len(lines) > 0 {
			projectedSize++
		}
		if maxContentSize > 0 && projectedSize > maxContentSize {
			return textReadWindow{}, contentTooLargeError{Size: projectedSize, Max: maxContentSize}
		}
		contentSize = projectedSize
		lines = append(lines, lineText)
		if !line.terminated {
			break
		}
	}

	// Peek one more line only when we filled the limit.
	hasMore := false
	if len(lines) == limit {
		var err error
		hasMore, err = peekViewLine(ctx, reader)
		if err != nil {
			return textReadWindow{}, err
		}
	}

	return textReadWindow{content: strings.Join(lines, "\n"), hasMore: hasMore, lineCount: len(lines)}, nil
}

func validateReadWindow(offset, limit, maxContentSize int) error {
	if offset < 0 {
		return fmt.Errorf("offset must be 0 or greater")
	}
	if offset > maxReadLines {
		return fmt.Errorf("offset must be no greater than %d", maxReadLines)
	}
	if limit < 0 {
		return fmt.Errorf("limit must be 0 or greater")
	}
	if limit > maxReadLines {
		return fmt.Errorf("limit must be no greater than %d", maxReadLines)
	}
	if maxContentSize < 0 || maxContentSize > MaxViewSize {
		return fmt.Errorf("maximum content size must be between 0 and %d", MaxViewSize)
	}
	return nil
}

type boundedLine struct {
	text       string
	present    bool
	terminated bool
}

// readViewBoundedLine consumes one line while retaining only its bounded prefix.
func readViewBoundedLine(ctx context.Context, reader *bufio.Reader, retain bool) (boundedLine, error) {
	const retainedLimit = MaxLineLength + 1

	var retained []byte
	tooLong := false
	sawContent := false
	if retain {
		retained = make([]byte, 0, retainedLimit)
	}

	for {
		if err := ctx.Err(); err != nil {
			return boundedLine{present: sawContent}, err
		}
		fragment, err := reader.ReadSlice('\n')
		if ctxErr := ctx.Err(); ctxErr != nil {
			return boundedLine{present: sawContent}, ctxErr
		}
		if len(fragment) > 0 {
			sawContent = true
		}
		terminated := len(fragment) > 0 && fragment[len(fragment)-1] == '\n'
		if terminated {
			fragment = fragment[:len(fragment)-1]
		}

		if retain {
			if remaining := retainedLimit - len(retained); remaining > 0 {
				keep := min(len(fragment), remaining)
				retained = append(retained, fragment[:keep]...)
				if keep < len(fragment) {
					tooLong = true
				}
			} else if len(fragment) > 0 {
				tooLong = true
			}
		}

		if err != nil && err != io.EOF && err != bufio.ErrBufferFull {
			return boundedLine{present: sawContent}, err
		}
		if terminated {
			return boundedLine{
				text:       finishBoundedLine(retained, tooLong),
				present:    true,
				terminated: true,
			}, nil
		}
		if err == io.EOF {
			if !sawContent {
				return boundedLine{}, io.EOF
			}
			return boundedLine{
				text:    finishBoundedLine(retained, tooLong),
				present: true,
			}, nil
		}
	}
}

func finishBoundedLine(retained []byte, tooLong bool) string {
	if !tooLong && len(retained) == MaxLineLength+1 && retained[len(retained)-1] != '\r' {
		tooLong = true
	}
	if !tooLong {
		if len(retained) > 0 && retained[len(retained)-1] == '\r' {
			retained = retained[:len(retained)-1]
		}
		return string(retained)
	}

	return stringext.Truncate(string(retained), MaxLineLength) + "..."
}

// peekViewLine reads only the first available fragment of the next line.
func peekViewLine(ctx context.Context, reader *bufio.Reader) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	fragment, err := reader.ReadSlice('\n')
	if ctxErr := ctx.Err(); ctxErr != nil {
		return len(fragment) > 0, ctxErr
	}
	if err == io.EOF {
		return len(fragment) > 0, nil
	}
	if err == bufio.ErrBufferFull {
		return true, nil
	}
	return len(fragment) > 0 || err == nil, err
}

type contextReader struct {
	ctx    context.Context
	reader io.Reader
}

// ownedReadCloser closes an opened reader when ctx is canceled and exactly
// once when normal cleanup races that cancellation. A malicious Close method
// can still block its caller; ordinary close-unblocks readers are covered.
type ownedReadCloser struct {
	reader io.ReadCloser
	stop   func() bool
	once   sync.Once
	err    error
}

func ownReadCloser(ctx context.Context, reader io.ReadCloser) *ownedReadCloser {
	owned := &ownedReadCloser{reader: reader}
	owned.stop = context.AfterFunc(ctx, func() {
		_ = owned.closeUnderlying()
	})
	return owned
}

func (r *ownedReadCloser) Read(p []byte) (int, error) {
	return r.reader.Read(p)
}

func (r *ownedReadCloser) ReadContext(ctx context.Context, p []byte) (int, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	var n int
	var err error
	if reader, ok := r.reader.(contextRead); ok {
		n, err = reader.ReadContext(ctx, p)
	} else {
		n, err = r.reader.Read(p)
	}
	if ctxErr := ctx.Err(); ctxErr != nil {
		return n, ctxErr
	}
	return n, err
}

func (r *ownedReadCloser) closeUnderlying() error {
	r.once.Do(func() { r.err = r.reader.Close() })
	return r.err
}

func (r *ownedReadCloser) Close() error {
	if r.stop != nil {
		r.stop()
	}
	return r.closeUnderlying()
}

func openOwnedRegularOSFile(ctx context.Context, path string) (io.ReadCloser, error) {
	file, err := openRegularOSFile(ctx, path)
	if err != nil {
		return nil, err
	}
	return ownReadCloser(ctx, file), nil
}

func openOwnedRegularAnchorFile(ctx context.Context, anchor *readAnchor) (io.ReadCloser, error) {
	file, err := openRegularAnchorFile(ctx, anchor)
	if err != nil {
		return nil, err
	}
	return ownReadCloser(ctx, file), nil
}

type contextRead interface {
	ReadContext(context.Context, []byte) (int, error)
}

func (r contextReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	var n int
	var err error
	if reader, ok := r.reader.(contextRead); ok {
		n, err = reader.ReadContext(r.ctx, p)
	} else {
		n, err = r.reader.Read(p)
	}
	if ctxErr := r.ctx.Err(); ctxErr != nil {
		return n, ctxErr
	}
	return n, err
}

func readBoundedBytes(ctx context.Context, reader io.Reader, max int) ([]byte, error) {
	if max < 0 || max >= int(^uint(0)>>1) {
		return nil, fmt.Errorf("maximum byte size is invalid: %d", max)
	}
	data := make([]byte, 0, min(max+1, viewReaderBuffer))
	buf := make([]byte, viewReaderBuffer)
	for len(data) <= max {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		remaining := max + 1 - len(data)
		if remaining < len(buf) {
			buf = buf[:remaining]
		}
		n, err := contextReader{ctx: ctx, reader: reader}.Read(buf)
		if n > 0 {
			data = append(data, buf[:n]...)
			if len(data) > max {
				return nil, contentTooLargeError{Size: len(data), Max: max}
			}
		}
		if err != nil {
			if err == io.EOF {
				return data, nil
			}
			return nil, err
		}
	}
	return data, nil
}

func addLineNumbersForWindow(content string, startLine, lineCount int) string {
	return addLineNumbersForCount(content, startLine, lineCount)
}

func getImageMimeType(filePath string) (bool, string) {
	ext := strings.ToLower(filepath.Ext(filePath))
	switch ext {
	case ".jpg", ".jpeg":
		return true, "image/jpeg"
	case ".png":
		return true, "image/png"
	case ".gif":
		return true, "image/gif"
	case ".webp":
		return true, "image/webp"
	default:
		return false, ""
	}
}

// sniffImageMimeType returns the content-sniffed MIME type when it identifies
// a supported image format. Otherwise it returns the provided fallback, which
// is usually the extension-derived type. Providers that validate the image
// media type against the base64 magic bytes (e.g. Anthropic) reject mismatched
// requests with a 400, so trusting the filename alone is unsafe.
func sniffImageMimeType(data []byte, fallback string) string {
	sniffed := http.DetectContentType(data)
	// http.DetectContentType may return the MIME with a ";" parameter
	// (e.g. "image/svg+xml; charset=utf-8") although current image sniffers
	// return bare types; strip defensively.
	if i := strings.IndexByte(sniffed, ';'); i >= 0 {
		sniffed = strings.TrimSpace(sniffed[:i])
	}
	switch sniffed {
	case "image/jpeg", "image/png", "image/gif", "image/webp":
		return sniffed
	}
	return fallback
}

// isInSkillsPath checks if filePath is within any of the configured skills
// directories. Returns true for files that can be read without permission
// prompts and without size limits.
//
// Note that symlinks are resolved to prevent path traversal attacks via
// symbolic links.
func isInSkillsPath(filePath string, skillsPaths []string) bool {
	if len(skillsPaths) == 0 {
		return false
	}

	absFilePath, err := filepath.Abs(filePath)
	if err != nil {
		return false
	}

	evalFilePath, err := filepath.EvalSymlinks(absFilePath)
	if err != nil {
		return false
	}

	for _, skillsPath := range skillsPaths {
		absSkillsPath, err := filepath.Abs(skillsPath)
		if err != nil {
			continue
		}

		evalSkillsPath, err := filepath.EvalSymlinks(absSkillsPath)
		if err != nil {
			continue
		}

		relPath, err := filepath.Rel(evalSkillsPath, evalFilePath)
		if err == nil && !strings.HasPrefix(relPath, "..") {
			return true
		}
	}

	return false
}

// readBuiltinFile reads a file from the embedded builtin skills filesystem.
func readBuiltinFile(params ViewParams, skillTracker *skills.Tracker) (fantasy.ToolResponse, error) {
	if err := validateViewParams(params); err != nil {
		return fantasy.NewTextErrorResponse(err.Error()), nil
	}
	embeddedPath := "builtin/" + strings.TrimPrefix(params.FilePath, skills.BuiltinPrefix)
	builtinFS := skills.BuiltinFS()

	data, err := fs.ReadFile(builtinFS, embeddedPath)
	if err != nil {
		return fantasy.NewTextErrorResponse(fmt.Sprintf("Builtin file not found: %s", params.FilePath)), nil
	}

	content := string(data)
	if !utf8.ValidString(content) {
		return fantasy.NewTextErrorResponse("File content is not valid UTF-8"), nil
	}

	limit := params.Limit
	if limit <= 0 {
		limit = maxReadLines // Embedded content is trusted and separately bounded by its asset.
	}

	lines := strings.Split(content, "\n")
	offset := min(params.Offset, len(lines))
	lines = lines[offset:]

	hasMore := len(lines) > limit
	if hasMore {
		lines = lines[:limit]
	}

	output := "<file>\n"
	output += addLineNumbersForCount(strings.Join(lines, "\n"), offset+1, len(lines))
	if hasMore {
		output += fmt.Sprintf("\n\n(File has more lines. Use 'offset' parameter to read beyond line %d)",
			offset+len(lines))
	}
	output += "\n</file>\n"

	meta := ViewResponseMetadata{
		FilePath: params.FilePath,
		Content:  strings.Join(lines, "\n"),
	}
	if skill, err := skills.ParseContent(data); err == nil {
		meta.ResourceType = ViewResourceSkill
		meta.ResourceName = skill.Name
		meta.ResourceDescription = skill.Description
		skillTracker.MarkLoaded(skill.Name)
	}

	return fantasy.WithResponseMetadata(
		fantasy.NewTextResponse(output),
		meta,
	), nil
}
