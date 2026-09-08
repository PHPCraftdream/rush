package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"charm.land/fantasy"
	"github.com/PHPCraftdream/rush/internal/db"
	"github.com/PHPCraftdream/rush/internal/filetracker"
	"github.com/PHPCraftdream/rush/internal/permission"
	"github.com/PHPCraftdream/rush/internal/pubsub"
	"github.com/stretchr/testify/require"
)

func TestReadTextFileBoundaryCases(t *testing.T) {
	t.Parallel()

	tmpDir := t.TempDir()
	filePath := filepath.Join(tmpDir, "sample.txt")

	var allLines []string
	for i := range 5 {
		allLines = append(allLines, fmt.Sprintf("line %d", i+1))
	}
	require.NoError(t, os.WriteFile(filePath, []byte(strings.Join(allLines, "\n")), 0o644))

	tests := []struct {
		name        string
		offset      int
		limit       int
		wantContent string
		wantHasMore bool
	}{
		{
			name:        "exactly limit lines remaining",
			offset:      0,
			limit:       5,
			wantContent: "line 1\nline 2\nline 3\nline 4\nline 5",
			wantHasMore: false,
		},
		{
			name:        "limit plus one line remaining",
			offset:      0,
			limit:       4,
			wantContent: "line 1\nline 2\nline 3\nline 4",
			wantHasMore: true,
		},
		{
			name:        "offset at last line",
			offset:      4,
			limit:       3,
			wantContent: "line 5",
			wantHasMore: false,
		},
		{
			name:        "offset beyond eof",
			offset:      10,
			limit:       3,
			wantContent: "",
			wantHasMore: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			gotContent, gotHasMore, err := readTextFile(filePath, tt.offset, tt.limit, 0)
			require.NoError(t, err)
			require.Equal(t, tt.wantContent, gotContent)
			require.Equal(t, tt.wantHasMore, gotHasMore)
		})
	}
}

func TestReadTextFileTruncatesLongLines(t *testing.T) {
	t.Parallel()

	tmpDir := t.TempDir()
	filePath := filepath.Join(tmpDir, "longline.txt")

	longLine := strings.Repeat("a", MaxLineLength+10)
	require.NoError(t, os.WriteFile(filePath, []byte(longLine), 0o644))

	content, hasMore, err := readTextFile(filePath, 0, 1, 0)
	require.NoError(t, err)
	require.False(t, hasMore)
	require.Equal(t, strings.Repeat("a", MaxLineLength)+"...", content)
}

func TestReadTextFileLineExceeding1MB(t *testing.T) {
	t.Parallel()

	tmpDir := t.TempDir()
	filePath := filepath.Join(tmpDir, "huge_line.txt")

	hugeLine := strings.Repeat("A", 2*1024*1024) // 2MB — exceeds bufio.Scanner max
	require.NoError(t, os.WriteFile(filePath, []byte(hugeLine), 0o644))

	content, hasMore, err := readTextFile(filePath, 0, 1, 0)
	require.NoError(t, err)
	require.False(t, hasMore)
	require.Equal(t, strings.Repeat("A", MaxLineLength)+"...", content)
}

func TestViewToolAllowsSmallSectionsOfLargeFiles(t *testing.T) {
	t.Parallel()

	workingDir := t.TempDir()
	filePath := filepath.Join(workingDir, "large.txt")
	lines := []string{strings.Repeat("a", MaxViewSize+1), "target line", "after target"}
	require.NoError(t, os.WriteFile(filePath, []byte(strings.Join(lines, "\n")), 0o644))

	tool := newViewToolForTest(workingDir)
	ctx := context.WithValue(context.Background(), SessionIDContextKey, "test-session")
	resp := runViewTool(t, tool, ctx, ViewParams{
		FilePath: filePath,
		Offset:   1,
		Limit:    1,
	})

	require.False(t, resp.IsError)
	require.Contains(t, resp.Content, "     2|target line")
	require.NotContains(t, resp.Content, "File is too large")

	var meta ViewResponseMetadata
	require.NoError(t, json.Unmarshal([]byte(resp.Metadata), &meta))
	require.Equal(t, "target line", meta.Content)
}

func TestViewToolBlocksOversizedReturnedSections(t *testing.T) {
	t.Parallel()

	workingDir := t.TempDir()
	filePath := filepath.Join(workingDir, "large-section.txt")
	lines := make([]string, DefaultReadLimit)
	for i := range lines {
		lines[i] = strings.Repeat("a", MaxLineLength)
	}
	require.NoError(t, os.WriteFile(filePath, []byte(strings.Join(lines, "\n")), 0o644))

	tool := newViewToolForTest(workingDir)
	ctx := context.WithValue(context.Background(), SessionIDContextKey, "test-session")
	resp := runViewTool(t, tool, ctx, ViewParams{
		FilePath: filePath,
	})

	require.True(t, resp.IsError)
	require.Contains(t, resp.Content, "Content section is too large")
}

func TestViewToolBlocksOversizedImages(t *testing.T) {
	t.Parallel()

	workingDir := t.TempDir()
	filePath := filepath.Join(workingDir, "large.png")
	require.NoError(t, os.WriteFile(filePath, []byte(strings.Repeat("a", MaxViewSize+1)), 0o644))

	tool := newViewToolForTest(workingDir)
	ctx := context.WithValue(context.Background(), SessionIDContextKey, "test-session")
	ctx = context.WithValue(ctx, SupportsImagesContextKey, true)
	resp := runViewTool(t, tool, ctx, ViewParams{
		FilePath: filePath,
	})

	require.True(t, resp.IsError)
	require.Contains(t, resp.Content, "Image file is too large")
}

func TestReadTextFileEnforcesMaxContentSize(t *testing.T) {
	t.Parallel()

	workingDir := t.TempDir()
	filePath := filepath.Join(workingDir, "oversized.txt")
	lines := []string{
		strings.Repeat("a", MaxLineLength),
		strings.Repeat("b", MaxLineLength),
		"target line",
	}
	require.NoError(t, os.WriteFile(filePath, []byte(strings.Join(lines, "\n")), 0o644))

	content, hasMore, err := readTextFile(filePath, 0, len(lines), MaxLineLength)
	require.ErrorAs(t, err, &contentTooLargeError{})
	require.Empty(t, content)
	require.False(t, hasMore)

	content, hasMore, err = readTextFile(filePath, 2, 1, MaxLineLength)
	require.NoError(t, err)
	require.Equal(t, "target line", content)
	require.False(t, hasMore)
}

func TestReadTextFileAllowsExactMaxContentSize(t *testing.T) {
	t.Parallel()

	workingDir := t.TempDir()
	filePath := filepath.Join(workingDir, "exact-size.txt")
	require.NoError(t, os.WriteFile(filePath, []byte("abcd\nefgh"), 0o644))

	content, hasMore, err := readTextFile(filePath, 0, 2, len("abcd\nefgh"))
	require.NoError(t, err)
	require.Equal(t, "abcd\nefgh", content)
	require.False(t, hasMore)
}

type generatedReadSegment struct {
	literal string
	fill    byte
	count   int64
}

type generatedReadCloser struct {
	segments  []generatedReadSegment
	segment   int
	offset    int64
	readBytes int64
	maxRead   int
}

func (r *generatedReadCloser) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}

	read := 0
	for read < len(p) && r.segment < len(r.segments) {
		segment := r.segments[r.segment]
		var remaining int64
		if segment.literal != "" {
			remaining = int64(len(segment.literal)) - r.offset
		} else {
			remaining = segment.count - r.offset
		}
		if remaining <= 0 {
			r.segment++
			r.offset = 0
			continue
		}

		take := min(int64(len(p)-read), remaining)
		if segment.literal != "" {
			copy(p[read:read+int(take)], segment.literal[r.offset:r.offset+take])
		} else {
			for i := range p[read : read+int(take)] {
				p[read+i] = segment.fill
			}
		}
		read += int(take)
		r.offset += take
	}

	if read == 0 {
		return 0, io.EOF
	}
	r.readBytes += int64(read)
	if read > r.maxRead {
		r.maxRead = read
	}
	return read, nil
}

func (r *generatedReadCloser) Close() error { return nil }

type generatedReadDisk struct {
	DiskProvider
	reader io.ReadCloser
}

func (d generatedReadDisk) Open(context.Context, string) (io.ReadCloser, error) {
	return d.reader, nil
}

func TestReadTextFileFromSkipsGeneratedLongLines(t *testing.T) {
	t.Parallel()

	const skippedBytes = MaxLineLength * 64
	reader := &generatedReadCloser{segments: []generatedReadSegment{
		{fill: 's', count: skippedBytes},
		{literal: "\nselected\n"},
	}}
	disk := generatedReadDisk{DiskProvider: OSDisk(), reader: reader}

	content, hasMore, err := readTextFileFrom(t.Context(), disk, "generated", 1, 1, 0)
	require.NoError(t, err)
	require.Equal(t, "selected", content)
	require.False(t, hasMore)
	require.GreaterOrEqual(t, reader.readBytes, int64(skippedBytes))
	require.LessOrEqual(t, reader.maxRead, 4096)
}

func TestReadTextFileFromBoundsGeneratedSelectedLine(t *testing.T) {
	t.Parallel()

	reader := &generatedReadCloser{segments: []generatedReadSegment{
		{fill: 'x', count: int64(MaxLineLength*64 + 1)},
		{literal: "\r\nnext"},
	}}
	disk := generatedReadDisk{DiskProvider: OSDisk(), reader: reader}

	content, hasMore, err := readTextFileFrom(t.Context(), disk, "generated", 0, 1, 0)
	require.NoError(t, err)
	require.Equal(t, strings.Repeat("x", MaxLineLength)+"...", content)
	require.True(t, hasMore)
	require.LessOrEqual(t, len(content), MaxLineLength+3)
}

func TestReadTextFileFromPreservesLineBoundaries(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		segments []generatedReadSegment
		want     string
		more     bool
	}{
		{
			name: "line just over limit",
			segments: []generatedReadSegment{
				{fill: 'a', count: MaxLineLength + 1},
			},
			want: strings.Repeat("a", MaxLineLength) + "...",
		},
		{
			name: "carriage return is not truncated",
			segments: []generatedReadSegment{
				{fill: 'b', count: MaxLineLength},
				{literal: "\r\n"},
			},
			want: strings.Repeat("b", MaxLineLength),
		},
		{
			name: "split CRLF",
			segments: []generatedReadSegment{
				{fill: 'c', count: 4095},
				{literal: "\r\nnext"},
			},
			want: strings.Repeat("c", MaxLineLength) + "...",
			more: true,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			reader := &generatedReadCloser{segments: test.segments}
			disk := generatedReadDisk{DiskProvider: OSDisk(), reader: reader}

			content, hasMore, err := readTextFileFrom(t.Context(), disk, "generated", 0, 1, 0)
			require.NoError(t, err)
			require.Equal(t, test.want, content)
			require.Equal(t, test.more, hasMore)
		})
	}
}

func TestReadTextFileFromPeekDoesNotConsumeNextGeneratedLine(t *testing.T) {
	t.Parallel()

	const nextLineBytes = MaxLineLength * 64
	reader := &generatedReadCloser{segments: []generatedReadSegment{
		{literal: "first\n"},
		{fill: 'p', count: nextLineBytes},
		{literal: "\n"},
	}}
	disk := generatedReadDisk{DiskProvider: OSDisk(), reader: reader}

	content, hasMore, err := readTextFileFrom(t.Context(), disk, "generated", 0, 1, 0)
	require.NoError(t, err)
	require.Equal(t, "first", content)
	require.True(t, hasMore)
	require.LessOrEqual(t, reader.readBytes, int64(8192), "peek should inspect one buffered fragment")
}

type mockViewPermissionService struct {
	*pubsub.Broker[permission.PermissionRequest]
}

func (m *mockViewPermissionService) Request(ctx context.Context, req permission.CreatePermissionRequest) (bool, error) {
	return true, nil
}

func (m *mockViewPermissionService) Grant(req permission.PermissionRequest) {}

func (m *mockViewPermissionService) Deny(req permission.PermissionRequest) {}

func (m *mockViewPermissionService) GrantPersistent(req permission.PermissionRequest) {}

func (m *mockViewPermissionService) AutoApproveSession(sessionID string) {}

func (m *mockViewPermissionService) InheritSessionAutoApprove(parentID, childID string) {}

func (m *mockViewPermissionService) SetSkipRequests(skip bool) {}

func (m *mockViewPermissionService) SetRunAllowlist(allowlist permission.RunAllowlist) {}

func (m *mockViewPermissionService) SkipRequests() bool {
	return false
}

func (m *mockViewPermissionService) SubscribeNotifications(ctx context.Context) <-chan pubsub.Event[permission.PermissionNotification] {
	return make(<-chan pubsub.Event[permission.PermissionNotification])
}

// Fork merge note: the next three methods satisfy the fork-extended Service
// interface that exposes the persistent-permission CRUD to the web UI. The
// mock implementations are intentionally no-ops because none of the view-tool
// paths touch persisted rules.
func (m *mockViewPermissionService) ListSessionPermissions(ctx context.Context, sessionID string) ([]db.SessionPermission, error) {
	return nil, nil
}

func (m *mockViewPermissionService) UpdatePermissionEnabled(ctx context.Context, ruleID string, enabled bool) error {
	return nil
}

func (m *mockViewPermissionService) DeletePermission(ctx context.Context, ruleID string) error {
	return nil
}

type mockFileTracker struct{}

func (m mockFileTracker) RecordRead(ctx context.Context, sessionID, path string) {}

func (m mockFileTracker) LastReadTime(ctx context.Context, sessionID, path string) time.Time {
	return time.Time{}
}

func (m mockFileTracker) ListReadFiles(ctx context.Context, sessionID string) ([]string, error) {
	return nil, nil
}

func newViewToolForTest(workingDir string) fantasy.AgentTool {
	permissions := &mockViewPermissionService{Broker: pubsub.NewBroker[permission.PermissionRequest]()}
	return NewViewTool(permissions, mockFileTracker{}, nil, workingDir)
}

func runViewTool(t *testing.T, tool fantasy.AgentTool, ctx context.Context, params ViewParams) fantasy.ToolResponse {
	t.Helper()

	input, err := json.Marshal(params)
	require.NoError(t, err)

	call := fantasy.ToolCall{
		ID:    "test-call",
		Name:  ViewToolName,
		Input: string(input),
	}

	resp, err := tool.Run(ctx, call)
	require.NoError(t, err)
	return resp
}

var _ filetracker.Service = mockFileTracker{}

func TestReadBuiltinFile(t *testing.T) {
	t.Parallel()

	t.Run("reads rush-config skill", func(t *testing.T) {
		t.Parallel()

		resp, err := readBuiltinFile(ViewParams{
			FilePath: "rush://skills/rush-config/SKILL.md",
		}, nil)
		require.NoError(t, err)
		require.NotEmpty(t, resp.Content)
		require.Contains(t, resp.Content, "Rush Configuration")
	})

	t.Run("not found", func(t *testing.T) {
		t.Parallel()

		resp, err := readBuiltinFile(ViewParams{
			FilePath: "rush://skills/nonexistent/SKILL.md",
		}, nil)
		require.NoError(t, err)
		require.True(t, resp.IsError)
	})

	t.Run("metadata has skill info", func(t *testing.T) {
		t.Parallel()

		resp, err := readBuiltinFile(ViewParams{
			FilePath: "rush://skills/rush-config/SKILL.md",
		}, nil)
		require.NoError(t, err)

		var meta ViewResponseMetadata
		require.NoError(t, json.Unmarshal([]byte(resp.Metadata), &meta))
		require.Equal(t, ViewResourceSkill, meta.ResourceType)
		require.Equal(t, "rush-config", meta.ResourceName)
		require.NotEmpty(t, meta.ResourceDescription)
	})

	t.Run("respects offset", func(t *testing.T) {
		t.Parallel()

		resp, err := readBuiltinFile(ViewParams{
			FilePath: "rush://skills/rush-config/SKILL.md",
			Offset:   5,
		}, nil)
		require.NoError(t, err)
		require.NotContains(t, resp.Content, "     1|")
	})
}

func TestSniffImageMimeType(t *testing.T) {
	t.Parallel()

	jpegMagic := []byte{0xff, 0xd8, 0xff, 0xe0, 0x00, 0x10, 'J', 'F', 'I', 'F'}
	pngMagic := []byte{0x89, 'P', 'N', 'G', 0x0d, 0x0a, 0x1a, 0x0a}
	gifMagic := []byte("GIF89a")
	// Minimal RIFF/WEBP header.
	webpMagic := append([]byte("RIFF\x00\x00\x00\x00WEBPVP8 "), make([]byte, 16)...)
	random := []byte("not an image at all, just text")

	cases := []struct {
		name     string
		data     []byte
		fallback string
		want     string
	}{
		{"jpeg bytes in .png file uses sniffed", jpegMagic, "image/png", "image/jpeg"},
		{"png bytes in .jpg file uses sniffed", pngMagic, "image/jpeg", "image/png"},
		{"gif bytes uses sniffed", gifMagic, "image/png", "image/gif"},
		{"webp bytes uses sniffed", webpMagic, "image/png", "image/webp"},
		{"matching extension and content keeps sniffed", pngMagic, "image/png", "image/png"},
		{"unsniffable content falls back", random, "image/png", "image/png"},
		{"empty content falls back", nil, "image/jpeg", "image/jpeg"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			require.Equal(t, tc.want, sniffImageMimeType(tc.data, tc.fallback))
		})
	}
}
