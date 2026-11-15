// Prompt-shaping tests: formatPrompt conversation formatting, saveFileParts
// attachment extraction (including the filename-collision regressions), and
// extractText.

package cliprovider

import (
	"os"
	"path/filepath"
	"testing"

	"charm.land/fantasy"
)

func TestFormatPrompt(t *testing.T) {
	tests := []struct {
		name   string
		prompt fantasy.Prompt
		want   string
	}{
		{
			name:   "empty",
			prompt: nil,
			want:   "",
		},
		{
			name: "system message",
			prompt: fantasy.Prompt{
				fantasy.NewSystemMessage("You are helpful."),
			},
			want: "<system>\nYou are helpful.\n</system>",
		},
		{
			name: "user message",
			prompt: fantasy.Prompt{
				fantasy.NewUserMessage("Hello"),
			},
			want: "User: Hello",
		},
		{
			name: "full conversation",
			prompt: fantasy.Prompt{
				fantasy.NewSystemMessage("Be helpful."),
				fantasy.NewUserMessage("Hi"),
				{Role: fantasy.MessageRoleAssistant, Content: []fantasy.MessagePart{fantasy.TextPart{Text: "Hello!"}}},
				fantasy.NewUserMessage("How are you?"),
			},
			want: "<system>\nBe helpful.\n</system>\n\nUser: Hi\n\nAssistant: Hello!\n\nUser: How are you?",
		},
		{
			name: "tool role included",
			prompt: fantasy.Prompt{
				{Role: fantasy.MessageRoleTool, Content: []fantasy.MessagePart{fantasy.TextPart{Text: "result: ok"}}},
			},
			want: "Tool: result: ok",
		},
		{
			name: "non-text parts skipped",
			prompt: fantasy.Prompt{
				{Role: fantasy.MessageRoleUser, Content: []fantasy.MessagePart{
					fantasy.TextPart{Text: "Look at this"},
					fantasy.FilePart{Filename: "test.png", Data: []byte("fake"), MediaType: "image/png"},
				}},
			},
			want: "User: Look at this",
		},
		{
			name: "message with only non-text parts skipped entirely",
			prompt: fantasy.Prompt{
				{Role: fantasy.MessageRoleUser, Content: []fantasy.MessagePart{
					fantasy.FilePart{Filename: "test.png", Data: []byte("fake"), MediaType: "image/png"},
				}},
			},
			want: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := formatPrompt(tt.prompt, nil)
			if got != tt.want {
				t.Errorf("formatPrompt() =\n%q\nwant:\n%q", got, tt.want)
			}
		})
	}
}

func TestFormatPromptWithFilePaths(t *testing.T) {
	msgs := fantasy.Prompt{
		fantasy.NewUserMessage("Look at this"),
	}
	paths := map[int][]string{
		0: {"/tmp/image.png"},
	}
	got := formatPrompt(msgs, paths)
	want := "User: Look at this\n[Attached file: /tmp/image.png]"
	if got != want {
		t.Errorf("formatPrompt() =\n%q\nwant:\n%q", got, want)
	}
}

func TestFormatPromptFileOnlyMessage(t *testing.T) {
	msgs := fantasy.Prompt{
		{Role: fantasy.MessageRoleUser, Content: []fantasy.MessagePart{
			fantasy.FilePart{Filename: "test.png", Data: []byte("fake"), MediaType: "image/png"},
		}},
	}
	paths := map[int][]string{
		0: {"/tmp/test.png"},
	}
	got := formatPrompt(msgs, paths)
	want := "User: \n[Attached file: /tmp/test.png]"
	if got != want {
		t.Errorf("formatPrompt() =\n%q\nwant:\n%q", got, want)
	}
}

func TestSaveFileParts(t *testing.T) {
	msgs := fantasy.Prompt{
		{Role: fantasy.MessageRoleUser, Content: []fantasy.MessagePart{
			fantasy.TextPart{Text: "Check this"},
			fantasy.FilePart{Filename: "screenshot.png", Data: []byte("PNG_DATA"), MediaType: "image/png"},
		}},
		{Role: fantasy.MessageRoleUser, Content: []fantasy.MessagePart{
			fantasy.FilePart{Filename: "", Data: []byte("JPEG_DATA"), MediaType: "image/jpeg"},
		}},
	}

	tmpDir, paths, err := saveFileParts(msgs)
	if err != nil {
		t.Fatalf("saveFileParts() error: %v", err)
	}
	defer os.RemoveAll(tmpDir)

	if len(paths[0]) != 1 {
		t.Fatalf("expected 1 file for msg 0, got %d", len(paths[0]))
	}
	if len(paths[1]) != 1 {
		t.Fatalf("expected 1 file for msg 1, got %d", len(paths[1]))
	}

	// Check file contents.
	data, err := os.ReadFile(paths[0][0])
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "PNG_DATA" {
		t.Errorf("file content = %q, want %q", data, "PNG_DATA")
	}
	if filepath.Base(paths[0][0]) != "screenshot.png" {
		t.Errorf("filename = %q, want screenshot.png", filepath.Base(paths[0][0]))
	}

	// Second file: auto-generated name from MIME type.
	data2, err := os.ReadFile(paths[1][0])
	if err != nil {
		t.Fatal(err)
	}
	if string(data2) != "JPEG_DATA" {
		t.Errorf("file content = %q, want %q", data2, "JPEG_DATA")
	}
}

// TestSaveFilePartsDisambiguatesCollidingFilenames is a BUG-3 regression
// test (found by a full-project @rush --role reviewer audit, 2026-08-11):
// two FileParts sharing the same base filename (e.g. two "image.png"
// attachments from different messages) used to silently write to the same
// path — the second os.WriteFile overwrote the first, but both entries'
// filePaths still pointed at that one path, now containing only the last
// write's content.
//
// REVERT CHECK PROCEDURE:
//  1. In provider.go's saveFileParts, remove the usedNames-based
//     disambiguation (use `name` directly as `finalName` unconditionally).
//  2. Run: go test ./internal/agent/cliprovider -run TestSaveFilePartsDisambiguatesCollidingFilenames -v
//  3. FAIL: both paths point at the same file, containing only the second
//     write's content.
//  4. Restore the disambiguation and PASS.
func TestSaveFilePartsDisambiguatesCollidingFilenames(t *testing.T) {
	msgs := fantasy.Prompt{
		{Role: fantasy.MessageRoleUser, Content: []fantasy.MessagePart{
			fantasy.FilePart{Filename: "image.png", Data: []byte("FIRST_IMAGE"), MediaType: "image/png"},
		}},
		{Role: fantasy.MessageRoleUser, Content: []fantasy.MessagePart{
			fantasy.FilePart{Filename: "image.png", Data: []byte("SECOND_IMAGE"), MediaType: "image/png"},
		}},
	}

	tmpDir, paths, err := saveFileParts(msgs)
	if err != nil {
		t.Fatalf("saveFileParts() error: %v", err)
	}
	defer os.RemoveAll(tmpDir)

	if len(paths[0]) != 1 || len(paths[1]) != 1 {
		t.Fatalf("expected 1 file each for msg 0 and msg 1, got %v", paths)
	}
	path1, path2 := paths[0][0], paths[1][0]
	if path1 == path2 {
		t.Fatalf("colliding filenames must be written to DIFFERENT paths, both resolved to %q", path1)
	}

	data1, err := os.ReadFile(path1)
	if err != nil {
		t.Fatal(err)
	}
	if string(data1) != "FIRST_IMAGE" {
		t.Errorf("first file content = %q, want %q — the second write must not have overwritten it", data1, "FIRST_IMAGE")
	}

	data2, err := os.ReadFile(path2)
	if err != nil {
		t.Fatal(err)
	}
	if string(data2) != "SECOND_IMAGE" {
		t.Errorf("second file content = %q, want %q", data2, "SECOND_IMAGE")
	}
}

// TestSaveFilePartsDisambiguatesMixedCollidingFilenames is the regression
// test for a gap an independent @oh review found in the first version of
// the collision fix above: usedNames tracked a per-ORIGINAL-name counter,
// so a mixed input where a later part's literal filename happened to match
// an earlier collision's GENERATED name ("image.png", "image.png",
// "image-1.png") still silently overwrote the second entry — the first
// "image.png" collision disambiguated to "image-1.png", and the third,
// literal "image-1.png" then reused that exact generated name. Fixed by
// tracking the set of final on-disk names actually handed out instead of a
// counter keyed by the original name.
func TestSaveFilePartsDisambiguatesMixedCollidingFilenames(t *testing.T) {
	msgs := fantasy.Prompt{
		{Role: fantasy.MessageRoleUser, Content: []fantasy.MessagePart{
			fantasy.FilePart{Filename: "image.png", Data: []byte("FIRST"), MediaType: "image/png"},
		}},
		{Role: fantasy.MessageRoleUser, Content: []fantasy.MessagePart{
			fantasy.FilePart{Filename: "image.png", Data: []byte("SECOND"), MediaType: "image/png"},
		}},
		{Role: fantasy.MessageRoleUser, Content: []fantasy.MessagePart{
			fantasy.FilePart{Filename: "image-1.png", Data: []byte("THIRD"), MediaType: "image/png"},
		}},
	}

	tmpDir, paths, err := saveFileParts(msgs)
	if err != nil {
		t.Fatalf("saveFileParts() error: %v", err)
	}
	defer os.RemoveAll(tmpDir)

	if len(paths[0]) != 1 || len(paths[1]) != 1 || len(paths[2]) != 1 {
		t.Fatalf("expected 1 file each for msg 0, 1, and 2, got %v", paths)
	}
	p0, p1, p2 := paths[0][0], paths[1][0], paths[2][0]
	if p0 == p1 || p0 == p2 || p1 == p2 {
		t.Fatalf("all three colliding filenames must resolve to DISTINCT paths, got %q %q %q", p0, p1, p2)
	}

	want := map[string]string{p0: "FIRST", p1: "SECOND", p2: "THIRD"}
	for path, wantContent := range want {
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if string(data) != wantContent {
			t.Errorf("content of %q = %q, want %q — a later entry must not have overwritten an earlier one", path, data, wantContent)
		}
	}
}

func TestSaveFilePartsNoFiles(t *testing.T) {
	msgs := fantasy.Prompt{
		fantasy.NewUserMessage("just text"),
	}
	tmpDir, paths, err := saveFileParts(msgs)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if tmpDir != "" || paths != nil {
		t.Errorf("expected no temp dir for text-only prompt, got dir=%q paths=%v", tmpDir, paths)
	}
}

func TestExtractText(t *testing.T) {
	msg := fantasy.Message{
		Role: fantasy.MessageRoleUser,
		Content: []fantasy.MessagePart{
			fantasy.TextPart{Text: "hello "},
			fantasy.TextPart{Text: "world"},
		},
	}
	got := extractText(msg)
	if got != "hello world" {
		t.Errorf("extractText() = %q, want %q", got, "hello world")
	}
}

func TestExtractTextNonText(t *testing.T) {
	msg := fantasy.Message{
		Role: fantasy.MessageRoleUser,
		Content: []fantasy.MessagePart{
			fantasy.TextPart{Text: "text"},
			fantasy.ToolCallPart{ToolCallID: "1", ToolName: "bash", Input: `{}`},
		},
	}
	got := extractText(msg)
	if got != "text" {
		t.Errorf("extractText() = %q, want %q", got, "text")
	}
}
