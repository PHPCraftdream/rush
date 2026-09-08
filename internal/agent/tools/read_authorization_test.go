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
	"github.com/PHPCraftdream/rush/internal/config"
	"github.com/PHPCraftdream/rush/internal/fsext"
	"github.com/PHPCraftdream/rush/internal/permission"
	"github.com/stretchr/testify/require"
)

type readAuthPermissionService struct {
	mockViewPermissionService
	granted  bool
	requests []permission.CreatePermissionRequest
}

func (s *readAuthPermissionService) Request(_ context.Context, req permission.CreatePermissionRequest) (bool, error) {
	s.requests = append(s.requests, req)
	return s.granted, nil
}

func newReadAuthPermissionService(granted bool) *readAuthPermissionService {
	return &readAuthPermissionService{mockViewPermissionService: mockViewPermissionService{}, granted: granted}
}

func runReadAuthTool(t *testing.T, tool fantasy.AgentTool, name string, params any) fantasy.ToolResponse {
	t.Helper()
	input, err := json.Marshal(params)
	require.NoError(t, err)
	ctx := context.WithValue(t.Context(), SessionIDContextKey, "read-auth-session")
	return runReadAuthToolWithContext(t, tool, ctx, name, string(input))
}

func runReadAuthToolWithContext(t *testing.T, tool fantasy.AgentTool, ctx context.Context, name, input string) fantasy.ToolResponse {
	t.Helper()
	resp, err := tool.Run(ctx, fantasy.ToolCall{ID: "read-auth-call", Name: name, Input: string(input)})
	require.NoError(t, err)
	return resp
}

func TestLegacyReadToolsAuthorizeOutsideRootsAndRejectSymlinks(t *testing.T) {
	t.Parallel()

	workspace := t.TempDir()
	outside := t.TempDir()
	secret := "outside-read-secret"
	outsideFile := filepath.Join(outside, "secret.txt")
	require.NoError(t, os.WriteFile(outsideFile, []byte(secret), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(workspace, "inside.txt"), []byte("inside"), 0o600))

	deny := newReadAuthPermissionService(false)
	view := NewViewTool(deny, mockFileTracker{}, nil, workspace)
	grep := NewGrepTool(workspace, config.ToolGrep{}, deny)
	glob := NewGlobTool(workspace, deny)
	ls := NewLsTool(deny, workspace, config.ToolLs{})

	for _, test := range []struct {
		name   string
		tool   fantasy.AgentTool
		params any
	}{
		{"view", view, ViewParams{FilePath: outsideFile}},
		{"grep", grep, GrepParams{Pattern: secret, Path: outside}},
		{"glob", glob, GlobParams{Pattern: "*", Path: outside}},
		{"ls", ls, LSParams{Path: outside}},
	} {
		t.Run(test.name, func(t *testing.T) {
			resp := runReadAuthTool(t, test.tool, test.name, test.params)
			require.True(t, resp.IsError)
			require.NotContains(t, resp.Content, secret)
		})
	}
	require.Len(t, deny.requests, 4)
	for _, req := range deny.requests {
		require.Equal(t, "read-auth-session", req.SessionID)
		require.Contains(t, req.Path, filepath.Base(outside))
		switch req.ToolName {
		case ViewToolName, GrepToolName, GlobToolName:
			require.Equal(t, "read", req.Action)
		case LSToolName:
			require.Equal(t, "list", req.Action)
		default:
			t.Fatalf("unexpected permission tool %q", req.ToolName)
		}
	}

	allow := newReadAuthPermissionService(true)
	resp := runReadAuthTool(t, NewViewTool(allow, mockFileTracker{}, nil, workspace), ViewToolName, ViewParams{FilePath: outsideFile})
	require.Contains(t, resp.Content, secret)
	resp = runReadAuthTool(t, NewGrepTool(workspace, config.ToolGrep{}, allow), GrepToolName, GrepParams{Pattern: secret, Path: outside})
	require.Contains(t, resp.Content, secret)
	resp = runReadAuthTool(t, NewGlobTool(workspace, allow), GlobToolName, GlobParams{Pattern: "*", Path: outside})
	require.Contains(t, resp.Content, "secret.txt")
	resp = runReadAuthTool(t, NewLsTool(allow, workspace, config.ToolLs{}), LSToolName, LSParams{Path: outside})
	require.Contains(t, resp.Content, "secret.txt")

	linkFile := filepath.Join(workspace, "linked.txt")
	if err := os.Symlink(outsideFile, linkFile); err != nil {
		t.Logf("file symlinks unavailable: %v", err)
		fallbackPermissions := newReadAuthPermissionService(false)
		_, allowed, authErr := authorizeWorkspaceRead(
			context.WithValue(t.Context(), SessionIDContextKey, "read-auth-session"),
			fallbackPermissions, workspace, outsideFile, ViewToolName, "read", "fallback", ViewParams{FilePath: outsideFile},
		)
		require.NoError(t, authErr)
		require.False(t, allowed)
		require.Len(t, fallbackPermissions.requests, 1)
		require.Equal(t, outsideFile, fallbackPermissions.requests[0].Path)
		return
	}
	linkDir := filepath.Join(workspace, "linked-dir")
	if err := os.Symlink(outside, linkDir); err != nil {
		t.Logf("directory symlinks unavailable: %v", err)
		fallbackPermissions := newReadAuthPermissionService(false)
		_, allowed, authErr := authorizeWorkspaceRead(
			context.WithValue(t.Context(), SessionIDContextKey, "read-auth-session"),
			fallbackPermissions, workspace, outside, LSToolName, "list", "fallback", LSParams{Path: outside},
		)
		require.NoError(t, authErr)
		require.False(t, allowed)
		require.Len(t, fallbackPermissions.requests, 1)
		require.Equal(t, outside, fallbackPermissions.requests[0].Path)
		return
	}

	denySymlink := newReadAuthPermissionService(false)
	for _, test := range []struct {
		name   string
		tool   fantasy.AgentTool
		params any
	}{
		{"view-symlink", NewViewTool(denySymlink, mockFileTracker{}, nil, workspace), ViewParams{FilePath: linkFile}},
		{"grep-symlink", NewGrepTool(workspace, config.ToolGrep{}, denySymlink), GrepParams{Pattern: secret, Path: linkDir}},
		{"glob-symlink", NewGlobTool(workspace, denySymlink), GlobParams{Pattern: "*", Path: linkDir}},
		{"ls-symlink", NewLsTool(denySymlink, workspace, config.ToolLs{}), LSParams{Path: linkDir}},
	} {
		t.Run(test.name, func(t *testing.T) {
			resp := runReadAuthTool(t, test.tool, strings.TrimSuffix(test.name, "-symlink"), test.params)
			require.True(t, resp.IsError)
			require.NotContains(t, resp.Content, secret)
		})
	}
	require.Len(t, denySymlink.requests, 4)
	for _, req := range denySymlink.requests {
		require.Equal(t, "read-auth-session", req.SessionID)
		require.Contains(t, req.Path, filepath.Base(outside))
		switch req.ToolName {
		case ViewToolName, GrepToolName, GlobToolName:
			require.Equal(t, "read", req.Action)
		case LSToolName:
			require.Equal(t, "list", req.Action)
		default:
			t.Fatalf("unexpected symlink permission tool %q", req.ToolName)
		}
	}

	for _, test := range []struct {
		name   string
		tool   fantasy.AgentTool
		params any
		want   string
	}{
		{"view-authorized-symlink", NewViewTool(allow, mockFileTracker{}, nil, workspace), ViewParams{FilePath: linkFile}, secret},
		{"grep-authorized-symlink", NewGrepTool(workspace, config.ToolGrep{}, allow), GrepParams{Pattern: secret, Path: linkDir}, secret},
		{"glob-authorized-symlink", NewGlobTool(workspace, allow), GlobParams{Pattern: "*", Path: linkDir}, "secret.txt"},
		{"ls-authorized-symlink", NewLsTool(allow, workspace, config.ToolLs{}), LSParams{Path: linkDir}, "secret.txt"},
	} {
		t.Run(test.name, func(t *testing.T) {
			resp := runReadAuthTool(t, test.tool, strings.SplitN(test.name, "-", 2)[0], test.params)
			require.Contains(t, resp.Content, test.want)
		})
	}
}

var _ permission.Service = (*readAuthPermissionService)(nil)

func TestAnchoredReadRootSurvivesWorkspacePathRetarget(t *testing.T) {
	t.Parallel()

	workspace := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(workspace, "inside.txt"), []byte("inside-bytes"), 0o600))
	root, err := os.OpenRoot(workspace)
	require.NoError(t, err)
	defer root.Close()

	moved := workspace + "-moved"
	renameErr := os.Rename(workspace, moved)
	retargeted := renameErr == nil
	if retargeted {
		require.NoError(t, os.Mkdir(workspace, 0o700))
		require.NoError(t, os.WriteFile(filepath.Join(workspace, "secret.txt"), []byte("external-bytes"), 0o600))
	} else {
		// Windows keeps the anchored directory handle open and rejects a
		// replacement rename. That is itself the structural no-retarget
		// guarantee; continue with the live root and assert its contents.
	}

	displayRoot := workspace
	if retargeted {
		displayRoot = moved
	}
	data, err := readFileFromAnchor(&readAnchor{root: root, rel: "inside.txt", path: filepath.Join(displayRoot, "inside.txt")})
	require.NoError(t, err)
	require.Equal(t, "inside-bytes", string(data))

	globbed, _, err := fsext.GlobGitignoreAwareFS(root.FS(), ".", displayRoot, "*", 100)
	require.NoError(t, err)
	require.NotContains(t, strings.Join(globbed, "\n"), "secret.txt")
	listed, _, err := fsext.ListDirectoryFS(root.FS(), ".", displayRoot, nil, -1, 100)
	require.NoError(t, err)
	require.NotContains(t, strings.Join(listed, "\n"), "secret.txt")
}

func TestAnchoredReadConsumesOpenedHandleAfterPathReplacement(t *testing.T) {
	t.Parallel()

	workspace := t.TempDir()
	path := filepath.Join(workspace, "file.txt")
	require.NoError(t, os.WriteFile(path, []byte("old-content"), 0o600))
	root, err := os.OpenRoot(workspace)
	require.NoError(t, err)
	defer root.Close()

	file, err := openRegularAnchorFile(t.Context(), &readAnchor{root: root, rel: "file.txt", path: path})
	require.NoError(t, err)
	defer file.Close()

	replaced := filepath.Join(workspace, "replacement.txt")
	if err := os.Rename(path, replaced); err != nil {
		t.Skipf("path replacement is not supported while a file is open: %v", err)
	}
	require.NoError(t, os.WriteFile(path, []byte("new-content"), 0o600))
	data, err := readBoundedBytes(t.Context(), file, MaxViewSize)
	require.NoError(t, err)
	require.Equal(t, "old-content", string(data))
}

func TestOpenedHandleIdentityIsStructural(t *testing.T) {
	t.Parallel()

	type source struct{ content string }
	open := func(s *source) io.ReadCloser {
		return io.NopCloser(strings.NewReader(s.content))
	}

	logical := &source{content: "opened-A"}
	opened := open(logical)
	logical.content = "replacement-B"
	window, err := readTextFileWindowFromReader(t.Context(), opened, 0, 1, MaxViewSize)
	require.NoError(t, err)
	require.Equal(t, "opened-A", window.content)
}

func TestAnchoredGrepKeepsNewestMatchesAfterEarlyWalk(t *testing.T) {
	t.Parallel()

	workspace := t.TempDir()
	old := time.Now().Add(-time.Hour)
	newer := time.Now()
	for i := 0; i < 120; i++ {
		oldPath := filepath.Join(workspace, fmt.Sprintf("a-%03d.txt", i))
		newPath := filepath.Join(workspace, fmt.Sprintf("z-%03d.txt", i))
		require.NoError(t, os.WriteFile(oldPath, []byte("needle\n"), 0o600))
		require.NoError(t, os.WriteFile(newPath, []byte("needle\n"), 0o600))
		require.NoError(t, os.Chtimes(oldPath, old, old))
		require.NoError(t, os.Chtimes(newPath, newer, newer))
	}
	root, err := os.OpenRoot(workspace)
	require.NoError(t, err)
	defer root.Close()
	anchor := &readAnchor{root: root, rel: ".", path: workspace}
	matches, truncated, err := searchFilesFS(t.Context(), "needle", anchor, "", 100)
	require.NoError(t, err)
	require.True(t, truncated)
	require.Len(t, matches, 100)
	for _, match := range matches {
		require.Contains(t, filepath.Base(match.path), "z-")
	}
}

func TestAnchoredGlobMatchesRegularGlob(t *testing.T) {
	t.Parallel()

	workspace := t.TempDir()
	require.NoError(t, os.Mkdir(filepath.Join(workspace, "nested"), 0o700))
	for _, name := range []string{"a.txt", filepath.Join("nested", "b.txt"), "c.go"} {
		require.NoError(t, os.WriteFile(filepath.Join(workspace, name), []byte(name), 0o600))
	}
	regular, regularTruncated, err := fsext.GlobGitignoreAware("**/*.txt", workspace, 100)
	require.NoError(t, err)
	root, err := os.OpenRoot(workspace)
	require.NoError(t, err)
	defer root.Close()
	anchored, anchoredTruncated, err := fsext.GlobGitignoreAwareFS(root.FS(), ".", workspace, "**/*.txt", 100)
	require.NoError(t, err)
	require.Equal(t, regularTruncated, anchoredTruncated)
	require.ElementsMatch(t, regular, anchored)
}

func TestViewMissingSuggestionUsesAnchoredRootAfterRetarget(t *testing.T) {
	workspace := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(workspace, "needle.txt"), []byte("inside"), 0o600))
	oldRoot := workspace + "-moved"
	seam := func(_ *readAnchor) {
		if err := os.Rename(workspace, oldRoot); err == nil {
			if err := os.Mkdir(workspace, 0o700); err != nil {
				t.Fatalf("replacement mkdir: %v", err)
			}
			if err := os.WriteFile(filepath.Join(workspace, "external-name.txt"), []byte("outside"), 0o600); err != nil {
				t.Fatalf("replacement file: %v", err)
			}
		}
	}
	ctx := withViewAfterAnchorSeam(context.WithValue(t.Context(), SessionIDContextKey, "read-auth-session"), seam)
	resp := runReadAuthToolWithContext(t, NewViewTool(newReadAuthPermissionService(false), mockFileTracker{}, nil, workspace), ctx, ViewToolName, `{"file_path":"needl"}`)
	require.Contains(t, resp.Content, "needle.txt")
	require.NotContains(t, resp.Content, "external-name.txt")
}

func TestLegacyReadToolsAcceptCanonicalWorkingDirectoryAlias(t *testing.T) {
	t.Parallel()

	parent := t.TempDir()
	realWorkspace := filepath.Join(parent, "real")
	require.NoError(t, os.Mkdir(realWorkspace, 0o700))
	content := "canonical-workspace-content"
	require.NoError(t, os.WriteFile(filepath.Join(realWorkspace, "inside.txt"), []byte(content), 0o600))
	alias := filepath.Join(parent, "alias")
	if err := os.Symlink(realWorkspace, alias); err != nil {
		// Mandatory non-symlink fallback: exercise the same canonical
		// containment and anchored-open branch without skipping the oracle.
		canonicalRoot, canonicalErr := canonicalizePath(realWorkspace)
		require.NoError(t, canonicalErr)
		canonicalFile, canonicalErr := canonicalizePath(filepath.Join(realWorkspace, "inside.txt"))
		require.NoError(t, canonicalErr)
		require.True(t, pathWithin(canonicalRoot, canonicalFile))
		root, rootErr := os.OpenRoot(realWorkspace)
		require.NoError(t, rootErr)
		defer root.Close()
		anchor, anchorErr := anchorWithinRoot(root, canonicalRoot, canonicalFile)
		require.NoError(t, anchorErr)
		data, readErr := readFileFromAnchor(anchor)
		require.NoError(t, readErr)
		require.Equal(t, content, string(data))
		return
	}

	permissions := newReadAuthPermissionService(false)
	view := NewViewTool(permissions, mockFileTracker{}, nil, alias)
	grep := NewGrepTool(alias, config.ToolGrep{}, permissions)
	glob := NewGlobTool(alias, permissions)
	ls := NewLsTool(permissions, alias, config.ToolLs{})
	resp := runReadAuthTool(t, view, ViewToolName, ViewParams{FilePath: "inside.txt"})
	require.Contains(t, resp.Content, content)
	resp = runReadAuthTool(t, grep, GrepToolName, GrepParams{Pattern: content})
	require.Contains(t, resp.Content, content)
	resp = runReadAuthTool(t, glob, GlobToolName, GlobParams{Pattern: "*"})
	require.Contains(t, resp.Content, "inside.txt")
	resp = runReadAuthTool(t, ls, LSToolName, LSParams{})
	require.Contains(t, resp.Content, "inside.txt")
	require.Empty(t, permissions.requests, "canonical in-workspace alias must not request outside permission")
}

func TestSyntheticCanonicalAliasOutsideRoutesToPermission(t *testing.T) {
	t.Parallel()

	workspace := t.TempDir()
	outside := t.TempDir()
	outsideFile := filepath.Join(outside, "outside.txt")
	require.NoError(t, os.WriteFile(outsideFile, []byte("outside"), 0o600))
	workingAbs, err := filepath.Abs(workspace)
	require.NoError(t, err)
	lexicalAlias := filepath.Join(workspace, "alias", "outside.txt")
	permissions := newReadAuthPermissionService(false)
	ctx := withReadCanonicalizer(
		context.WithValue(t.Context(), SessionIDContextKey, "synthetic-session"),
		func(path string) (string, error) {
			if filepath.Clean(path) == filepath.Clean(workingAbs) {
				return workingAbs, nil
			}
			return outsideFile, nil
		},
	)
	_, allowed, err := authorizeWorkspaceRead(ctx, permissions, workspace, lexicalAlias, ViewToolName, "read", "synthetic-call", ViewParams{FilePath: lexicalAlias})
	require.NoError(t, err)
	require.False(t, allowed)
	require.Len(t, permissions.requests, 1)
	require.Equal(t, outsideFile, permissions.requests[0].Path)
	require.Equal(t, ViewToolName, permissions.requests[0].ToolName)
	require.Equal(t, "read", permissions.requests[0].Action)
}

func TestWorkspaceRootIdentityBindingRejectsWrongOpenedRoot(t *testing.T) {
	t.Parallel()

	workspaceA := t.TempDir()
	workspaceB := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(workspaceA, "file.txt"), []byte("A"), 0o600))
	require.NoError(t, os.Mkdir(filepath.Join(workspaceA, "dir"), 0o700))
	for _, path := range []string{filepath.Join(workspaceA, "file.txt"), filepath.Join(workspaceA, "dir")} {
		permissions := newReadAuthPermissionService(false)
		ctx := withReadRootOpener(t.Context(), func(string) (*os.Root, error) {
			return os.OpenRoot(workspaceB)
		})
		anchor, allowed, err := authorizeWorkspaceRead(ctx, permissions, workspaceA, path, ViewToolName, "read", "identity-mismatch", ViewParams{FilePath: path})
		require.Error(t, err, "root B must never be used for canonical root A")
		require.False(t, allowed)
		require.Nil(t, anchor)
		require.Empty(t, permissions.requests)
	}
}
