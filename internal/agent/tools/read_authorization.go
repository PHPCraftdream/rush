package tools

import (
	"cmp"
	"context"
	_ "embed"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	"charm.land/fantasy"
	"github.com/PHPCraftdream/rush/internal/agent/safety/permission"
	"github.com/PHPCraftdream/rush/internal/agent/tools/mcp"
	"github.com/PHPCraftdream/rush/internal/config"
	"github.com/PHPCraftdream/rush/internal/kit/fs/filepathext"
)

type readAnchor struct {
	root *os.Root
	rel  string
	path string
}

type readCanonicalizerKey struct{}

type readRootOpenerKey struct{}

func withReadCanonicalizer(ctx context.Context, canonicalizer func(string) (string, error)) context.Context {
	return context.WithValue(ctx, readCanonicalizerKey{}, canonicalizer)
}

func canonicalizeReadPath(ctx context.Context, path string) (string, error) {
	if canonicalizer, ok := ctx.Value(readCanonicalizerKey{}).(func(string) (string, error)); ok {
		return canonicalizer(path)
	}
	return canonicalizePath(path)
}

func withReadRootOpener(ctx context.Context, opener func(string) (*os.Root, error)) context.Context {
	return context.WithValue(ctx, readRootOpenerKey{}, opener)
}

func openBoundedWorkspaceRoot(ctx context.Context, canonicalPath string) (*os.Root, error) {
	opener := os.OpenRoot
	if injected, ok := ctx.Value(readRootOpenerKey{}).(func(string) (*os.Root, error)); ok {
		opener = injected
	}
	for attempt := 0; attempt < 3; attempt++ {
		canonicalInfo, err := os.Stat(canonicalPath)
		if err != nil {
			return nil, err
		}
		root, err := opener(canonicalPath)
		if err != nil {
			continue
		}
		rootInfo, err := root.Stat(".")
		if err == nil && os.SameFile(canonicalInfo, rootInfo) {
			return root, nil
		}
		_ = root.Close()
	}
	return nil, fmt.Errorf("workspace root changed while opening: %s", canonicalPath)
}

func (a *readAnchor) Close() error { return a.root.Close() }

func (a *readAnchor) FS() fs.FS { return a.root.FS() }

func (a *readAnchor) rootPath() string {
	if a.rel == "." {
		return "."
	}
	return filepath.ToSlash(a.rel)
}

func (a *readAnchor) displayRoot() string {
	return a.path
}

func anchorWithinRoot(root *os.Root, rootPath, path string) (*readAnchor, error) {
	rel, err := filepath.Rel(rootPath, path)
	if err != nil || filepath.IsAbs(rel) || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return nil, fmt.Errorf("path %q is outside anchored root %q", path, rootPath)
	}
	return &readAnchor{root: root, rel: rel, path: path}, nil
}

func anchorExternalPath(path string) (*readAnchor, error) {
	info, err := os.Stat(path)
	if err == nil && info.IsDir() {
		root, err := os.OpenRoot(path)
		if err != nil {
			return nil, err
		}
		return &readAnchor{root: root, rel: ".", path: path}, nil
	}
	parent := filepath.Dir(path)
	root, err := os.OpenRoot(parent)
	if err != nil {
		return nil, err
	}
	return &readAnchor{root: root, rel: filepath.Base(path), path: path}, nil
}

// authorizeWorkspaceRead applies the legacy tools' common read boundary.
// Symlink aliases are resolved before use; a target outside the workspace
// still requires the normal outside-workspace permission.
func authorizeWorkspaceRead(
	ctx context.Context,
	permissions permission.Service,
	workingDir string,
	path string,
	toolName string,
	action string,
	callID string,
	params any,
) (*readAnchor, bool, error) {
	absWorkingDir, err := filepath.Abs(workingDir)
	if err != nil {
		return nil, false, fmt.Errorf("error resolving working directory: %w", err)
	}
	absPath, err := filepath.Abs(path)
	if err != nil {
		return nil, false, fmt.Errorf("error resolving search path: %w", err)
	}
	canonicalWorkingDir, err := canonicalizeReadPath(ctx, absWorkingDir)
	if err != nil {
		return nil, false, fmt.Errorf("error resolving working directory root: %w", err)
	}
	canonicalPath, err := canonicalizeReadPath(ctx, absPath)
	if err != nil {
		return nil, false, fmt.Errorf("error resolving read path: %w", err)
	}
	workspaceRoot, err := openBoundedWorkspaceRoot(ctx, canonicalWorkingDir)
	if err != nil {
		return nil, false, fmt.Errorf("error opening working directory root: %w", err)
	}

	closeWorkspace := true
	defer func() {
		if closeWorkspace {
			_ = workspaceRoot.Close()
		}
	}()

	readPath := canonicalPath

	if pathWithin(canonicalWorkingDir, readPath) {
		anchor, err := anchorWithinRoot(workspaceRoot, canonicalWorkingDir, readPath)
		if err != nil {
			return nil, false, err
		}
		closeWorkspace = false
		return anchor, true, nil
	}
	if permissions == nil {
		return nil, false, nil
	}

	sessionID := GetSessionFromContext(ctx)
	if sessionID == "" {
		return nil, false, fmt.Errorf("session ID is required for accessing paths outside working directory")
	}
	permissionPath := readPath
	if pathWithin(canonicalWorkingDir, readPath) {
		permissionPath = absPath
	}
	granted, err := permissions.Request(ctx, permission.CreatePermissionRequest{
		SessionID:   sessionID,
		ToolCallID:  callID,
		ToolName:    toolName,
		Action:      action,
		Description: fmt.Sprintf("Read path outside working directory: %s", permissionPath),
		Params:      params,
		Path:        permissionPath,
	})
	if err != nil {
		return nil, false, err
	}
	if !granted {
		return nil, false, nil
	}
	anchor, err := anchorExternalPath(readPath)
	if err != nil {
		return nil, false, fmt.Errorf("error opening authorized read root: %w", err)
	}
	return anchor, true, nil
}

func canonicalizePath(path string) (string, error) {
	path = filepath.Clean(path)
	var suffix []string
	for {
		resolved, err := filepath.EvalSymlinks(path)
		if err == nil {
			resolved, err = filepath.Abs(resolved)
			if err != nil {
				return "", err
			}
			for i := len(suffix) - 1; i >= 0; i-- {
				resolved = filepath.Join(resolved, suffix[i])
			}
			return filepath.Clean(resolved), nil
		}
		if !os.IsNotExist(err) {
			return "", err
		}
		parent := filepath.Dir(path)
		if parent == path {
			return path, nil
		}
		suffix = append(suffix, filepath.Base(path))
		path = parent
	}
}

func pathWithin(root, path string) bool {
	rel, err := filepath.Rel(root, path)
	if err != nil {
		return false
	}
	return rel == "." || (rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) && !filepath.IsAbs(rel))
}

type ReadMCPResourceParams struct {
	MCPName string `json:"mcp_name" description:"The MCP server name"`
	URI     string `json:"uri" description:"The resource URI to read"`
}

type ReadMCPResourcePermissionsParams struct {
	MCPName string `json:"mcp_name"`
	URI     string `json:"uri"`
}

const ReadMCPResourceToolName = "read_mcp_resource"

//go:embed read_mcp_resource.md
var readMCPResourceDescription string

func NewReadMCPResourceTool(cfg *config.ConfigStore, permissions permission.Service, owner *mcp.Owner, output ...*MCPOutputAccess) fantasy.AgentTool {
	var access *MCPOutputAccess
	if len(output) > 0 {
		access = output[0]
	}
	return fantasy.NewParallelAgentTool(
		ReadMCPResourceToolName,
		readMCPResourceDescription,
		func(ctx context.Context, params ReadMCPResourceParams, call fantasy.ToolCall) (fantasy.ToolResponse, error) {
			params.MCPName = strings.TrimSpace(params.MCPName)
			params.URI = strings.TrimSpace(params.URI)
			if params.MCPName == "" {
				return fantasy.NewTextErrorResponse("mcp_name parameter is required"), nil
			}
			if params.URI == "" {
				return fantasy.NewTextErrorResponse("uri parameter is required"), nil
			}

			sessionID := GetSessionFromContext(ctx)
			if sessionID == "" {
				return fantasy.ToolResponse{}, fmt.Errorf("session ID is required for reading MCP resources")
			}

			relPath := filepathext.SmartJoin(cfg.WorkingDir(), cmp.Or(params.URI, "mcp-resource"))
			p, err := permissions.Request(
				ctx,
				permission.CreatePermissionRequest{
					SessionID:   sessionID,
					Path:        relPath,
					ToolCallID:  call.ID,
					ToolName:    ReadMCPResourceToolName,
					Action:      "read",
					Description: fmt.Sprintf("Read MCP resource from %s", params.MCPName),
					Params:      ReadMCPResourcePermissionsParams(params),
				},
			)
			if err != nil {
				return fantasy.ToolResponse{}, err
			}
			if !p {
				return NewPermissionDeniedResponse(), nil
			}

			if response, local := access.readArtifact(ctx, params.MCPName, params.URI, call.ID); local {
				return response, nil
			}

			contents, err := owner.ReadResource(ctx, cfg, params.MCPName, params.URI)
			if err != nil {
				return fantasy.NewTextErrorResponse(err.Error()), nil
			}
			if len(contents) == 0 {
				return fantasy.NewTextResponse(""), nil
			}

			var textParts []string
			for _, content := range contents {
				if content == nil {
					continue
				}
				if content.Text != "" {
					textParts = append(textParts, content.Text)
					continue
				}
				if len(content.Blob) > 0 {
					textParts = append(textParts, string(content.Blob))
					continue
				}
				slog.Debug("MCP resource content missing text/blob", "uri", content.URI)
			}

			if len(textParts) == 0 {
				return fantasy.NewTextResponse(""), nil
			}

			return fantasy.NewTextResponse(strings.Join(textParts, "\n")), nil
		},
	)
}
