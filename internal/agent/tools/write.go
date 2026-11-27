package tools

import (
	"context"
	_ "embed"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"charm.land/fantasy"
	"github.com/PHPCraftdream/rush/internal/agent/safety/permission"
	"github.com/PHPCraftdream/rush/internal/kit/fs/filepathext"
	"github.com/PHPCraftdream/rush/internal/kit/fs/fsext"
	"github.com/PHPCraftdream/rush/internal/kit/text/diff"
	"github.com/PHPCraftdream/rush/internal/store/filetracker"
	"github.com/PHPCraftdream/rush/internal/store/history"
)

// Fork patch (concurrency): both file writes in this file
// (processMultiEditWithCreation, processMultiEditExistingFile) use
// fsext.AtomicWriteFile (write-to-tmp + rename) instead of
// os.WriteFile so a kill -9 / OOM mid-write cannot leave the user's
// file half-truncated. See CHANGELOG.fork.md (Section 4.I).

type MultiEditOperation struct {
	OldString  string `json:"old_string" description:"The text to replace"`
	NewString  string `json:"new_string" description:"The text to replace it with"`
	ReplaceAll bool   `json:"replace_all,omitempty" description:"Replace all occurrences of old_string (default false)."`
}

type MultiEditParams struct {
	FilePath string               `json:"file_path" description:"The absolute path to the file to modify"`
	Edits    []MultiEditOperation `json:"edits" description:"Array of edit operations to perform sequentially on the file"`
}

type MultiEditPermissionsParams struct {
	FilePath   string `json:"file_path"`
	OldContent string `json:"old_content,omitempty"`
	NewContent string `json:"new_content,omitempty"`
}

type FailedEdit struct {
	Index int                `json:"index"`
	Error string             `json:"error"`
	Edit  MultiEditOperation `json:"edit"`
}

type MultiEditResponseMetadata struct {
	Additions    int          `json:"additions"`
	Removals     int          `json:"removals"`
	OldContent   string       `json:"old_content,omitempty"`
	NewContent   string       `json:"new_content,omitempty"`
	EditsApplied int          `json:"edits_applied"`
	EditsFailed  []FailedEdit `json:"edits_failed,omitempty"`
}

const MultiEditToolName = "multiedit"

//go:embed multiedit.md
var multieditDescription string

func NewMultiEditTool(
	permissions permission.Service,
	files history.Service,
	filetracker filetracker.Service,
	workingDir string,
) fantasy.AgentTool {
	return fantasy.NewAgentTool(
		MultiEditToolName,
		multieditDescription,
		func(ctx context.Context, params MultiEditParams, call fantasy.ToolCall) (fantasy.ToolResponse, error) {
			if params.FilePath == "" {
				return fantasy.NewTextErrorResponse("file_path is required"), nil
			}

			if len(params.Edits) == 0 {
				return fantasy.NewTextErrorResponse("at least one edit operation is required"), nil
			}

			params.FilePath = filepathext.SmartJoin(workingDir, params.FilePath)

			if err := CheckForbiddenWrite(params.FilePath); err != nil {
				return fantasy.NewTextErrorResponse(err.Error()), nil
			}

			// Validate all edits before applying any
			if err := validateEdits(params.Edits); err != nil {
				return fantasy.NewTextErrorResponse(err.Error()), nil
			}

			var response fantasy.ToolResponse
			var err error

			editCtx := editContext{ctx, permissions, files, filetracker, workingDir, nil}
			// Handle file creation case (first edit has empty old_string)
			if len(params.Edits) > 0 && params.Edits[0].OldString == "" {
				response, err = processMultiEditWithCreation(editCtx, params, call)
			} else {
				response, err = processMultiEditExistingFile(editCtx, params, call)
			}

			if err != nil {
				return response, err
			}

			if response.IsError {
				return response, nil
			}

			response.Content = fmt.Sprintf("<result>\n%s\n</result>\n", response.Content)
			return response, nil
		},
	)
}

func validateEdits(edits []MultiEditOperation) error {
	for i, edit := range edits {
		// Only the first edit can have empty old_string (for file creation)
		if i > 0 && edit.OldString == "" {
			return fmt.Errorf("edit %d: only the first edit can have empty old_string (for file creation)", i+1)
		}
	}
	return nil
}

// applyEditsToContent applies edits sequentially to currentContent, tracking
// any failures. startIndex is the 0-based index of edits[0] within the
// original params.Edits slice, used to report accurate failure indices when
// only a sub-slice of edits is passed in (e.g. file-creation mode skips the
// first edit, which was already consumed to seed the content).
func applyEditsToContent(currentContent string, edits []MultiEditOperation, startIndex int) (string, []FailedEdit) {
	var failedEdits []FailedEdit
	for i, edit := range edits {
		newContent, err := applyEditToContent(currentContent, edit)
		if err != nil {
			failedEdits = append(failedEdits, FailedEdit{
				Index: startIndex + i + 1,
				Error: err.Error(),
				Edit:  edit,
			})
			continue
		}
		currentContent = newContent
	}
	return currentContent, failedEdits
}

func processMultiEditWithCreation(edit editContext, params MultiEditParams, call fantasy.ToolCall) (fantasy.ToolResponse, error) {
	// First edit creates the file
	firstEdit := params.Edits[0]
	if firstEdit.OldString != "" {
		return fantasy.NewTextErrorResponse("first edit must have empty old_string for file creation"), nil
	}

	// Check if file already exists
	if _, err := os.Stat(params.FilePath); err == nil {
		return fantasy.NewTextErrorResponse(fmt.Sprintf("file already exists: %s", params.FilePath)), nil
	} else if !os.IsNotExist(err) {
		if osFailureIsFatal(err) {
			return fantasy.ToolResponse{}, fmt.Errorf("failed to access file: %w", err)
		}
		return fantasy.NewTextErrorResponse(fmt.Sprintf(
			"Cannot access %s: %v. The OS rejected this path — common causes are no permission somewhere along the path, a path component that is a file rather than a directory, a name too long for the filesystem, or an invalid character in the name. The file was not created and nothing was written. Try a different path or a corrected form of this one.",
			params.FilePath, err)), nil
	}

	// Create parent directories
	dir := filepath.Dir(params.FilePath)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		if osFailureIsFatal(err) {
			return fantasy.ToolResponse{}, fmt.Errorf("failed to create parent directories: %w", err)
		}
		return fantasy.NewTextErrorResponse(fmt.Sprintf(
			"Cannot create the parent directory for %s: %v. A path component is a file rather than a directory, the target exists with different permissions, or the OS refused to create it. The file was not created and nothing was written. Try a different path or a corrected form of this one.",
			params.FilePath, err)), nil
	}

	// Start with the content from the first edit, then apply the remaining
	// edits, tracking failures.
	currentContent, failedEdits := applyEditsToContent(firstEdit.NewString, params.Edits[1:], 1)

	// Get session and message IDs
	sessionID := GetSessionFromContext(edit.ctx)
	if sessionID == "" {
		return fantasy.ToolResponse{}, fmt.Errorf("session ID is required for creating a new file")
	}

	// Check permissions
	_, additions, removals := diff.GenerateDiff("", currentContent, strings.TrimPrefix(params.FilePath, edit.workingDir))

	editsApplied := len(params.Edits) - len(failedEdits)
	var description string
	if len(failedEdits) > 0 {
		description = fmt.Sprintf("Create file %s with %d of %d edits (%d failed)", params.FilePath, editsApplied, len(params.Edits), len(failedEdits))
	} else {
		description = fmt.Sprintf("Create file %s with %d edits", params.FilePath, editsApplied)
	}
	p, err := edit.permissions.Request(edit.ctx, permission.CreatePermissionRequest{
		SessionID:   sessionID,
		Path:        fsext.PathOrPrefix(params.FilePath, edit.workingDir),
		ToolCallID:  call.ID,
		ToolName:    MultiEditToolName,
		Action:      "write",
		Description: description,
		Params: MultiEditPermissionsParams{
			FilePath:   params.FilePath,
			OldContent: "",
			NewContent: currentContent,
		},
	})
	if err != nil {
		return fantasy.ToolResponse{}, err
	}
	if !p {
		resp := NewPermissionDeniedResponse()
		resp = fantasy.WithResponseMetadata(resp, MultiEditResponseMetadata{
			OldContent:   "",
			NewContent:   currentContent,
			Additions:    additions,
			Removals:     removals,
			EditsApplied: editsApplied,
			EditsFailed:  failedEdits,
		})
		return resp, nil
	}

	// Write the file
	if err := CheckForbiddenWriteResolved(params.FilePath); err != nil {
		return fantasy.NewTextErrorResponse(err.Error()), nil
	}
	err = fsext.AtomicWriteFile(params.FilePath, []byte(currentContent), 0o644)
	if err != nil {
		if osFailureIsFatal(err) {
			return fantasy.ToolResponse{}, fmt.Errorf("failed to write file: %w", err)
		}
		return fantasy.NewTextErrorResponse(fmt.Sprintf(
			"Cannot write %s: %v. The OS refused the final write or rename — the file or a directory along its path is read-only, the target is a directory, or another process holds it. Nothing was changed and the temporary file was cleaned up. Try a different path, or fix the permissions and retry.",
			params.FilePath, err)), nil
	}

	// Update file history
	_, err = edit.files.Create(edit.ctx, sessionID, params.FilePath, "")
	if err != nil {
		return fantasy.ToolResponse{}, fmt.Errorf("error creating file history: %w", err)
	}

	_, err = edit.files.CreateVersion(edit.ctx, sessionID, params.FilePath, currentContent)
	if err != nil {
		slog.Error("Error creating file history version", "error", err)
	}

	edit.filetracker.RecordRead(edit.ctx, sessionID, params.FilePath)

	var message string
	if len(failedEdits) > 0 {
		message = fmt.Sprintf("File created with %d of %d edits: %s (%d edit(s) failed)", editsApplied, len(params.Edits), params.FilePath, len(failedEdits))
	} else {
		message = fmt.Sprintf("File created with %d edits: %s", len(params.Edits), params.FilePath)
	}

	return fantasy.WithResponseMetadata(
		fantasy.NewTextResponse(message),
		MultiEditResponseMetadata{
			OldContent:   "",
			NewContent:   currentContent,
			Additions:    additions,
			Removals:     removals,
			EditsApplied: editsApplied,
			EditsFailed:  failedEdits,
		},
	), nil
}

func processMultiEditExistingFile(edit editContext, params MultiEditParams, call fantasy.ToolCall) (fantasy.ToolResponse, error) {
	sessionID, oldContent, isCrlf, readRefreshed, resp, err := loadExistingFileWithReadRefresh(edit, params.FilePath, "session ID is required for editing a file")
	if err != nil {
		return fantasy.ToolResponse{}, err
	}
	if resp.Content != "" || resp.IsError {
		return resp, nil
	}

	currentContent, failedEdits := applyEditsToContent(oldContent, params.Edits, 0)

	// Check if content actually changed
	if oldContent == currentContent {
		metadata := struct {
			MultiEditResponseMetadata
			*EditEffectMetadata
		}{
			MultiEditResponseMetadata: MultiEditResponseMetadata{
				EditsApplied: 0,
				EditsFailed:  failedEdits,
			},
		}
		if !readRefreshed {
			metadata.EditEffectMetadata = &EditEffectMetadata{}
			metadata.EditEffect.NoWrite = true
			metadata.EditEffect.Reason = "unchanged_content"
		}
		return fantasy.WithResponseMetadata(
			fantasy.NewTextErrorResponse(fmt.Sprintf("no changes made; %d edit operation(s) failed", len(failedEdits))),
			metadata,
		), nil
	}

	// Generate diff and check permissions
	_, additions, removals := diff.GenerateDiff(oldContent, currentContent, strings.TrimPrefix(params.FilePath, edit.workingDir))

	editsApplied := len(params.Edits) - len(failedEdits)
	var description string
	if len(failedEdits) > 0 {
		description = fmt.Sprintf("Apply %d of %d edits to file %s (%d failed)", editsApplied, len(params.Edits), params.FilePath, len(failedEdits))
	} else {
		description = fmt.Sprintf("Apply %d edits to file %s", editsApplied, params.FilePath)
	}
	p, err := edit.permissions.Request(edit.ctx, permission.CreatePermissionRequest{
		SessionID:   sessionID,
		Path:        fsext.PathOrPrefix(params.FilePath, edit.workingDir),
		ToolCallID:  call.ID,
		ToolName:    MultiEditToolName,
		Action:      "write",
		Description: description,
		Params: MultiEditPermissionsParams{
			FilePath:   params.FilePath,
			OldContent: oldContent,
			NewContent: currentContent,
		},
	})
	if err != nil {
		return fantasy.ToolResponse{}, err
	}
	if !p {
		resp := NewPermissionDeniedResponse()
		resp = fantasy.WithResponseMetadata(resp, MultiEditResponseMetadata{
			OldContent:   oldContent,
			NewContent:   currentContent,
			Additions:    additions,
			Removals:     removals,
			EditsApplied: editsApplied,
			EditsFailed:  failedEdits,
		})
		return resp, nil
	}

	writeContent := currentContent
	if isCrlf {
		writeContent, _ = fsext.ToWindowsLineEndings(writeContent)
	}

	if resp, err := commitFileChange(edit, sessionID, params.FilePath, oldContent, writeContent); err != nil {
		return fantasy.ToolResponse{}, err
	} else if resp.IsError {
		return resp, nil
	}

	var message string
	if len(failedEdits) > 0 {
		message = fmt.Sprintf("Applied %d of %d edits to file: %s (%d edit(s) failed)", editsApplied, len(params.Edits), params.FilePath, len(failedEdits))
	} else {
		message = fmt.Sprintf("Applied %d edits to file: %s", len(params.Edits), params.FilePath)
	}

	return fantasy.WithResponseMetadata(
		fantasy.NewTextResponse(message),
		MultiEditResponseMetadata{
			OldContent:   oldContent,
			NewContent:   writeContent,
			Additions:    additions,
			Removals:     removals,
			EditsApplied: editsApplied,
			EditsFailed:  failedEdits,
		},
	), nil
}

func applyEditToContent(content string, edit MultiEditOperation) (string, error) {
	if edit.OldString == "" && edit.NewString == "" {
		return content, nil
	}

	if edit.OldString == "" {
		return "", fmt.Errorf("old_string cannot be empty for content replacement")
	}

	return findAndReplace(content, edit.OldString, edit.NewString, edit.ReplaceAll)
}

// Fork patch (concurrency): the file write at the bottom of NewWriteTool
// uses fsext.AtomicWriteFile (write-to-tmp + rename) instead of
// os.WriteFile so a kill -9 / OOM mid-write cannot leave the user's
// file half-truncated. See CHANGELOG.fork.md (Section 4.I).

//go:embed write.md
var writeDescription string

type WriteParams struct {
	FilePath    string `json:"file_path" description:"The path to the file to write"`
	Content     string `json:"content" description:"The content to write to the file"`
	AllowShrink bool   `json:"allow_shrink,omitempty" description:"Set true only to deliberately replace a large existing file with much smaller content (write is refused otherwise: use edit for partial changes)"`
}

type WritePermissionsParams struct {
	FilePath   string `json:"file_path"`
	OldContent string `json:"old_content,omitempty"`
	NewContent string `json:"new_content,omitempty"`
}

type WriteResponseMetadata struct {
	Diff      string `json:"diff"`
	Additions int    `json:"additions"`
	Removals  int    `json:"removals"`
}

const WriteToolName = "write"

func NewWriteTool(
	permissions permission.Service,
	files history.Service,
	filetracker filetracker.Service,
	workingDir string,
) fantasy.AgentTool {
	return fantasy.NewAgentTool(
		WriteToolName,
		writeDescription,
		func(ctx context.Context, params WriteParams, call fantasy.ToolCall) (fantasy.ToolResponse, error) {
			if params.FilePath == "" {
				return fantasy.NewTextErrorResponse("file_path is required"), nil
			}

			sessionID := GetSessionFromContext(ctx)
			if sessionID == "" {
				return fantasy.ToolResponse{}, fmt.Errorf("session_id is required")
			}

			filePath := filepathext.SmartJoin(workingDir, params.FilePath)

			if err := CheckForbiddenWrite(filePath); err != nil {
				return fantasy.NewTextErrorResponse(err.Error()), nil
			}

			fileInfo, err := os.Stat(filePath)
			if err == nil {
				if fileInfo.IsDir() {
					return fantasy.NewTextErrorResponse(fmt.Sprintf("Path is a directory, not a file: %s", filePath)), nil
				}
				if resp, tooLarge := checkEditFileSize(filePath, fileInfo.Size()); tooLarge {
					return resp, nil
				}

				modTime := fileInfo.ModTime().Truncate(time.Second)
				lastRead := filetracker.LastReadTime(ctx, sessionID, filePath)
				if modTime.After(lastRead) {
					// File was modified externally since last read, update the read time to allow the write
					slog.Warn("File was modified externally since last read, proceeding with write", "file", filePath, "mod_time", modTime.Format(time.RFC3339), "last_read", lastRead.Format(time.RFC3339))
					filetracker.RecordRead(ctx, sessionID, filePath)
				}

				oldContent, readErr := os.ReadFile(filePath)
				if readErr == nil && string(oldContent) == params.Content {
					return fantasy.NewTextErrorResponse(fmt.Sprintf("File %s already contains the exact content. No changes made.", filePath)), nil
				}
				if readErr == nil && !params.AllowShrink && writeWouldShrinkFile(len(oldContent), len(params.Content)) {
					return fantasy.NewTextErrorResponse(writeShrinkRefusal(WriteToolName, "edit or multiedit", filePath, len(oldContent), len(params.Content))), nil
				}
			} else if !os.IsNotExist(err) {
				if osFailureIsFatal(err) {
					return fantasy.ToolResponse{}, fmt.Errorf("error checking file: %w", err)
				}
				return fantasy.NewTextErrorResponse(fmt.Sprintf(
					"Cannot access %s: %v. The OS rejected this path — common causes are no permission somewhere along the path, a path component that is a file rather than a directory, or an invalid character in the name. Nothing was written. Try a different path or a corrected form of this one.",
					filePath, err)), nil
			}

			dir := filepath.Dir(filePath)

			oldContent := ""
			if fileInfo != nil && !fileInfo.IsDir() {
				oldBytes, readErr := os.ReadFile(filePath)
				if readErr == nil {
					oldContent = string(oldBytes)
				}
			}

			diff, additions, removals := diff.GenerateDiff(
				oldContent,
				params.Content,
				strings.TrimPrefix(filePath, workingDir),
			)

			p, err := permissions.Request(
				ctx,
				permission.CreatePermissionRequest{
					SessionID:   sessionID,
					Path:        fsext.PathOrPrefix(filePath, workingDir),
					ToolCallID:  call.ID,
					ToolName:    WriteToolName,
					Action:      "write",
					Description: fmt.Sprintf("Create file %s", filePath),
					Params: WritePermissionsParams{
						FilePath:   filePath,
						OldContent: oldContent,
						NewContent: params.Content,
					},
				},
			)
			if err != nil {
				return fantasy.ToolResponse{}, err
			}
			if !p {
				resp := NewPermissionDeniedResponse()
				resp = fantasy.WithResponseMetadata(resp, WriteResponseMetadata{
					Diff:      diff,
					Additions: additions,
					Removals:  removals,
				})
				return resp, nil
			}

			if err = os.MkdirAll(dir, 0o755); err != nil {
				if osFailureIsFatal(err) {
					return fantasy.ToolResponse{}, fmt.Errorf("error creating directory: %w", err)
				}
				return fantasy.NewTextErrorResponse(fmt.Sprintf(
					"Cannot create the parent directory for %s: %v. A path component is a file rather than a directory, the target exists with different permissions, or the OS refused to create it. Nothing was written. Try a different path or a corrected form of this one.",
					filePath, err)), nil
			}

			if err = CheckForbiddenWriteResolved(filePath); err != nil {
				return fantasy.NewTextErrorResponse(err.Error()), nil
			}
			err = fsext.AtomicWriteFile(filePath, []byte(params.Content), 0o644)
			if err != nil {
				if osFailureIsFatal(err) {
					return fantasy.ToolResponse{}, fmt.Errorf("error writing file: %w", err)
				}
				return fantasy.NewTextErrorResponse(fmt.Sprintf(
					"Cannot write %s: %v. The OS refused the final write or rename — the file or a directory along its path is read-only, the target is a directory, or another process holds it. Nothing was changed and the temporary file was cleaned up. Try a different path, or fix the permissions and retry.",
					filePath, err)), nil
			}

			// Check if file exists in history
			file, err := files.GetByPathAndSession(ctx, filePath, sessionID)
			if err != nil {
				_, err = files.Create(ctx, sessionID, filePath, oldContent)
				if err != nil {
					// Log error but don't fail the operation
					return fantasy.ToolResponse{}, fmt.Errorf("error creating file history: %w", err)
				}
			}
			if file.Content != oldContent {
				// User manually changed the content; store an intermediate version
				_, err = files.CreateVersion(ctx, sessionID, filePath, oldContent)
				if err != nil {
					slog.Error("Error creating file history version", "error", err)
				}
			}
			// Store the new version
			_, err = files.CreateVersion(ctx, sessionID, filePath, params.Content)
			if err != nil {
				slog.Error("Error creating file history version", "error", err)
			}

			filetracker.RecordRead(ctx, sessionID, filePath)

			result := fmt.Sprintf("File successfully written: %s", filePath)
			result = fmt.Sprintf("<result>\n%s\n</result>", result)
			return fantasy.WithResponseMetadata(
				fantasy.NewTextResponse(result),
				WriteResponseMetadata{
					Diff:      diff,
					Additions: additions,
					Removals:  removals,
				},
			), nil
		},
	)
}

// A worker replaced a 100-line registry document with one table row through
// write, silently (session wbgfix1, 2026-10-05): write has no "partial" mode, so
// a model that means a surgical change but sends only the changed part destroys
// the file. A write that would shrink an existing non-trivial file to a
// fraction of its size is refused unless the caller says it is deliberate.
const (
	// writeShrinkMinOldBytes: smaller files are cheap to recreate, never guarded.
	writeShrinkMinOldBytes = 2048
	// writeShrinkRatio: the new content must be at least 1/writeShrinkRatio of the
	// old size (a 75% reduction trips the guard).
	writeShrinkRatio = 4
)

func writeWouldShrinkFile(oldBytes, newBytes int) bool {
	return oldBytes >= writeShrinkMinOldBytes && newBytes*writeShrinkRatio < oldBytes
}

// writeShrinkRefusal is the refusal text; partialTools names the partial-edit
// tools of the toolset the caller is in (write's differ from fs_write's).
func writeShrinkRefusal(tool, partialTools, path string, oldBytes, newBytes int) string {
	return fmt.Sprintf(
		"Refusing to replace %s (%d bytes) with %d bytes of content: %s overwrites the WHOLE file, so sending only the changed part would delete the rest. "+
			"For a partial change use %s; if you really mean to replace the file with this much smaller content, repeat the call with allow_shrink=true. Nothing was written.",
		path, oldBytes, newBytes, tool, partialTools)
}
