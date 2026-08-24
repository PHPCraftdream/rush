// Prompt shaping: attachment extraction to temp files, conversation
// formatting for CLI consumption, and the prefix-hash / latest-message
// helpers backing --resume.

package cliprovider

import (
	"fmt"
	"hash/fnv"
	"log/slog"
	"mime"
	"os"
	"path/filepath"
	"strings"

	"charm.land/fantasy"
)

// saveFileParts extracts FilePart entries from messages, writes them to a temp
// directory on disk, and returns the directory path and a per-message list of
// saved file paths. The caller must os.RemoveAll(tempDir) when done.
// Returns ("", nil, nil) if no file parts are found.
func saveFileParts(msgs fantasy.Prompt) (tempDir string, filePaths map[int][]string, err error) {
	// Collect file parts with their message indices.
	type entry struct {
		msgIdx int
		fp     fantasy.FilePart
	}
	var entries []entry
	for i, msg := range msgs {
		for _, part := range msg.Content {
			if fp, ok := fantasy.AsMessagePart[fantasy.FilePart](part); ok {
				slog.Debug("cliprovider: found FilePart", "msgIdx", i, "filename", fp.Filename, "mediaType", fp.MediaType, "dataLen", len(fp.Data))
				entries = append(entries, entry{msgIdx: i, fp: fp})
			}
		}
	}
	slog.Debug("cliprovider: saveFileParts scan", "totalMessages", len(msgs), "filePartsFound", len(entries))
	if len(entries) == 0 {
		return "", nil, nil
	}

	tempDir, err = os.MkdirTemp("", "rush-attachments-*")
	if err != nil {
		return "", nil, fmt.Errorf("create attachment temp dir: %w", err)
	}

	filePaths = make(map[int][]string)
	usedNames := make(map[string]bool)
	for seq, e := range entries {
		name := e.fp.Filename
		if name == "" {
			ext := ".bin"
			if exts, _ := mime.ExtensionsByType(e.fp.MediaType); len(exts) > 0 {
				ext = exts[0]
			}
			name = fmt.Sprintf("attachment-%d%s", seq, ext)
		}
		// Sanitize: keep only the base name.
		name = filepath.Base(name)

		// Disambiguate on collision (found by a full-project @crush
		// --role reviewer audit): two FileParts sharing the same base
		// filename (e.g. two "image.png" attachments from different
		// messages) used to silently write to the same path — the
		// second os.WriteFile overwrote the first, but both entries'
		// filePaths still pointed at that one path, now containing only
		// the last write's content. Preserve the original name in the
		// common (non-colliding) case; append a "-N" suffix only when
		// needed, mirroring saveAttachmentToDisk's own reasoning in
		// internal/server/handlers.go.
		//
		// usedNames tracks the FINAL on-disk names actually handed out,
		// not the original requested names — an independent review of
		// the first version of this fix (which kept a per-original-name
		// counter) found it still collided on a mixed set like
		// ["image.png", "image.png", "image-1.png"]: the second
		// "image.png" disambiguated to "image-1.png", and the literal
		// third entry then silently reused (overwrote) that same
		// generated name. Looping against the used-name set instead of a
		// single counter closes that gap for any input ordering.
		finalName := name
		if usedNames[finalName] {
			ext := filepath.Ext(name)
			base := strings.TrimSuffix(name, ext)
			for n := 1; ; n++ {
				candidate := fmt.Sprintf("%s-%d%s", base, n, ext)
				if !usedNames[candidate] {
					finalName = candidate
					break
				}
			}
		}
		usedNames[finalName] = true

		path := filepath.Join(tempDir, finalName)
		if werr := os.WriteFile(path, e.fp.Data, 0o644); werr != nil {
			slog.Warn("cliprovider: failed to write attachment", "path", path, "err", werr)
			continue
		}
		filePaths[e.msgIdx] = append(filePaths[e.msgIdx], path)
	}
	return tempDir, filePaths, nil
}

// hashPromptPrefix returns a hash of all messages except the last user message.
// Used to detect conversation edits/deletes that would make a CLI session stale.
func hashPromptPrefix(msgs fantasy.Prompt) uint64 {
	h := fnv.New64a()
	// Find the last user message index.
	lastUser := -1
	for i := len(msgs) - 1; i >= 0; i-- {
		if msgs[i].Role == fantasy.MessageRoleUser {
			lastUser = i
			break
		}
	}
	// Hash everything before the last user message.
	for i := 0; i < len(msgs); i++ {
		if i == lastUser {
			break
		}
		fmt.Fprintf(h, "%d:%s:%s\n", i, msgs[i].Role, extractText(msgs[i]))
	}
	return h.Sum64()
}

// extractLatestUserMessage returns only the text of the last user message
// from the prompt, for use with --resume where the CLI already has history.
func extractLatestUserMessage(msgs fantasy.Prompt, filePaths map[int][]string) string {
	for i := len(msgs) - 1; i >= 0; i-- {
		if msgs[i].Role == fantasy.MessageRoleUser {
			text := extractText(msgs[i])
			files := filePaths[i]
			if len(files) > 0 {
				var sb strings.Builder
				sb.WriteString(text)
				for _, f := range files {
					sb.WriteString("\n[Attached file: ")
					sb.WriteString(f)
					sb.WriteString("]")
				}
				return sb.String()
			}
			return text
		}
	}
	// Fallback: full prompt if no user message found.
	return formatPrompt(msgs, filePaths)
}

// formatPrompt converts a fantasy.Prompt into a single text string for the CLI.
// The full conversation (system prompt + message history) is formatted so the
// CLI model receives as much context as possible.
// filePaths maps message indices to on-disk file paths for attached files;
// nil means no files were attached.
func formatPrompt(msgs fantasy.Prompt, filePaths map[int][]string) string {
	var sb strings.Builder
	for i, msg := range msgs {
		text := extractText(msg)
		files := filePaths[i]
		if text == "" && len(files) == 0 {
			continue
		}
		switch msg.Role {
		case fantasy.MessageRoleSystem:
			sb.WriteString("<system>\n")
			sb.WriteString(text)
			sb.WriteString("\n</system>\n\n")
		case fantasy.MessageRoleUser:
			sb.WriteString("User: ")
			sb.WriteString(text)
			for _, f := range files {
				sb.WriteString("\n[Attached file: ")
				sb.WriteString(f)
				sb.WriteString("]")
			}
			sb.WriteString("\n\n")
		case fantasy.MessageRoleAssistant:
			sb.WriteString("Assistant: ")
			sb.WriteString(text)
			sb.WriteString("\n\n")
		case fantasy.MessageRoleTool:
			sb.WriteString("Tool: ")
			sb.WriteString(text)
			sb.WriteString("\n\n")
		default:
			slog.Warn("cliprovider: unknown message role, skipping", "role", msg.Role)
		}
	}
	return strings.TrimSpace(sb.String())
}

// extractText collects all TextPart strings from a message's content.
// Non-text parts (tool calls, files, etc.) are silently skipped with a debug log.
func extractText(msg fantasy.Message) string {
	var sb strings.Builder
	for _, part := range msg.Content {
		if tp, ok := fantasy.AsMessagePart[fantasy.TextPart](part); ok {
			sb.WriteString(tp.Text)
		} else {
			slog.Debug("cliprovider: skipping non-text content part", "type", part.GetType(), "model_role", msg.Role)
		}
	}
	return sb.String()
}
