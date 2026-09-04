package sdk_test

import (
	"context"
	"io"
	"io/fs"
	"path/filepath"
	"strings"
	"testing"

	"github.com/PHPCraftdream/rush/sdk"
	"github.com/stretchr/testify/require"
)

// r16_6ValueDisk is deliberately a value-type provider with map and slice
// fields. All methods have value receivers, proving that the public
// DiskProvider contract does not require a comparable dynamic type.
type r16_6ValueDisk struct {
	files map[string]string
	dirs  map[string]bool
	tags  []string
}

var _ sdk.DiskProvider = r16_6ValueDisk{}

func newR16_6ValueDisk() r16_6ValueDisk {
	disk := r16_6ValueDisk{
		files: make(map[string]string),
		dirs:  make(map[string]bool),
		tags:  []string{"non-comparable"},
	}
	for dir := filepath.Clean(sdk.LibraryVirtualRoot()); ; {
		disk.dirs[dir] = true
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	return disk
}

func (d r16_6ValueDisk) Stat(_ context.Context, name string) (fs.FileInfo, error) {
	name = filepath.Clean(name)
	if _, ok := d.files[name]; ok {
		return fakeSDKFileInfo{name: filepath.Base(name)}, nil
	}
	if d.dirs[name] {
		return fakeSDKFileInfo{name: filepath.Base(name), isDir: true}, nil
	}
	return nil, fs.ErrNotExist
}

func (d r16_6ValueDisk) EvalSymlinks(_ context.Context, name string) (string, error) {
	return filepath.Clean(name), nil
}

func (d r16_6ValueDisk) Open(_ context.Context, name string) (io.ReadCloser, error) {
	content, ok := d.files[filepath.Clean(name)]
	if !ok {
		return nil, fs.ErrNotExist
	}
	return io.NopCloser(strings.NewReader(content)), nil
}

func (d r16_6ValueDisk) ReadFile(_ context.Context, name string) ([]byte, error) {
	content, ok := d.files[filepath.Clean(name)]
	if !ok {
		return nil, fs.ErrNotExist
	}
	return []byte(content), nil
}

func (d r16_6ValueDisk) MkdirAll(_ context.Context, dir string, _ fs.FileMode) error {
	for dir = filepath.Clean(dir); ; dir = filepath.Dir(dir) {
		d.dirs[dir] = true
		parent := filepath.Dir(dir)
		if parent == dir {
			return nil
		}
	}
}

func (d r16_6ValueDisk) WriteFile(_ context.Context, name string, data []byte, _ fs.FileMode) error {
	d.files[filepath.Clean(name)] = string(data)
	return nil
}

func (d r16_6ValueDisk) Remove(_ context.Context, name string) error {
	delete(d.files, filepath.Clean(name))
	return nil
}

func (d r16_6ValueDisk) List(_ context.Context, _ sdk.DiskListRequest) (sdk.DiskListResult, error) {
	return sdk.DiskListResult{}, nil
}

func (d r16_6ValueDisk) Find(_ context.Context, _ sdk.DiskFindRequest) (sdk.DiskFindResult, error) {
	return sdk.DiskFindResult{}, nil
}

func (d r16_6ValueDisk) Search(_ context.Context, _ sdk.DiskSearchRequest) (sdk.DiskSearchResult, error) {
	return sdk.DiskSearchResult{}, nil
}

// TestSDKLibraryModeFolderScopesAcceptNonComparableValueDiskProvider proves
// that the documented FolderScopes + DiskProvider opt-in reaches the scoped
// fs_* tools without comparing the provider interface. The provider value
// contains a map and slice, so the precondition, no-real-workspace floor,
// and virtual-root guard all exercise the non-comparable case.
func TestSDKLibraryModeFolderScopesAcceptNonComparableValueDiskProvider(t *testing.T) {
	isolateGlobalConfigForWorkdirTest(t)

	const (
		relPath = "scoped/virtual.txt"
		content = "R16_6_NON_COMPARABLE_CONTENT"
		marker  = "R16_6_NON_COMPARABLE_OK"
	)
	disk := newR16_6ValueDisk()
	srv := libraryVirtualRootRoundTripServer(t, relPath, content, marker)

	client, err := sdk.Open(context.Background(), sdk.Options{
		Mode:          sdk.ModeLibrary,
		LibraryConfig: libraryConfigFor(srv.URL, "sk-library-secret"),
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = client.Close() })

	res, err := client.Run(context.Background(), sdk.RunRequest{
		Prompt:            "write then read against the non-comparable virtual disk",
		Mode:              sdk.RunModeJSON,
		ContinueSessionID: "sdk-r16-6-non-comparable",
		HideSpinner:       true,
		Overrides: sdk.RunOverrides{
			FolderScopes: []sdk.FolderScope{
				{Dir: "scoped", Ops: []sdk.FileOp{sdk.FileOpCreate, sdk.FileOpRead}},
			},
			DiskProvider: disk,
		},
	})
	require.NoError(t, err)
	require.NotNil(t, res)
	require.Equal(t, marker, res.FinalText)

	msgs, err := client.Messages(context.Background(), "sdk-r16-6-non-comparable")
	require.NoError(t, err)
	writeResult := fsToolResultOf(t, msgs, "fs_write")
	require.False(t, writeResult.IsError, "content %q", writeResult.Content)
	readResult := fsToolResultOf(t, msgs, "fs_read")
	require.False(t, readResult.IsError, "content %q", readResult.Content)
	require.Contains(t, readResult.Content, content)

	expected := filepath.Clean(filepath.Join(sdk.LibraryVirtualRoot(), relPath))
	require.Equal(t, content, disk.files[expected])
}
