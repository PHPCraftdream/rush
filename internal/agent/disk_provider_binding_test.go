package agent

import (
	"context"
	"errors"
	"io"
	"io/fs"
	"testing"

	"github.com/PHPCraftdream/rush/internal/agent/tools"
	"github.com/stretchr/testify/require"
)

type bindingTestNonComparable struct {
	tools.DiskProvider
	tags []string
}
type bindingTestNilProvider struct{}

func (*bindingTestNilProvider) Stat(context.Context, string) (fs.FileInfo, error) { return nil, nil }

func (*bindingTestNilProvider) EvalSymlinks(context.Context, string) (string, error) { return "", nil }

func (*bindingTestNilProvider) Open(context.Context, string) (io.ReadCloser, error) { return nil, nil }

func (*bindingTestNilProvider) ReadFile(context.Context, string) ([]byte, error)    { return nil, nil }
func (*bindingTestNilProvider) MkdirAll(context.Context, string, fs.FileMode) error { return nil }
func (*bindingTestNilProvider) WriteFile(context.Context, string, []byte, fs.FileMode) error {
	return nil
}
func (*bindingTestNilProvider) Remove(context.Context, string) error { return nil }
func (*bindingTestNilProvider) List(context.Context, tools.ListRequest) (tools.ListResult, error) {
	return tools.ListResult{}, nil
}

func (*bindingTestNilProvider) Find(context.Context, tools.FindRequest) (tools.FindResult, error) {
	return tools.FindResult{}, nil
}

func (*bindingTestNilProvider) Search(context.Context, tools.SearchRequest) (tools.DiskSearchResult, error) {
	return tools.DiskSearchResult{}, nil
}

func TestDiskProviderRegistryBindingLifecycle(t *testing.T) {
	reg1, reg2 := NewDiskProviderRegistry(), NewDiskProviderRegistry()
	provider := bindingTestNonComparable{DiskProvider: tools.OSDisk(), tags: []string{"non-comparable"}}
	id, err := reg1.RegisterProvider(provider)
	require.NoError(t, err)
	got, err := reg1.Lookup(id)
	require.NoError(t, err)
	require.Equal(t, provider, got)
	_, err = reg2.Lookup(id)
	require.ErrorIs(t, err, ErrDiskProviderUnavailable)

	var typedNil *bindingTestNilProvider
	_, err = reg1.RegisterProvider(typedNil)
	require.Error(t, err)
	_, err = reg1.RegisterProvider(tools.OSDisk())
	require.Error(t, err)
	require.NotErrorIs(t, err, ErrDiskProviderUnavailable)

	reg1.ReleaseProvider(id)
	_, err = reg1.Lookup(id)
	require.ErrorIs(t, err, ErrDiskProviderUnavailable)
	_, _, err = reg1.AcquireProvider(id)
	require.ErrorIs(t, err, ErrDiskProviderUnavailable)
}

func TestDiskProviderRegistryReferenceRetainsOwner(t *testing.T) {
	reg := NewDiskProviderRegistry()
	id, err := reg.RegisterProvider(bindingTestNonComparable{DiskProvider: tools.OSDisk(), tags: []string{"retained"}})
	require.NoError(t, err)
	_, release, err := reg.AcquireProvider(id)
	require.NoError(t, err)
	reg.ReleaseProvider(id)
	_, err = reg.Lookup(id)
	require.NoError(t, err)
	release()
	_, err = reg.Lookup(id)
	require.ErrorIs(t, err, ErrDiskProviderUnavailable)
}

func TestDiskProviderRegistryNilInstanceAndUnavailableSentinel(t *testing.T) {
	var nilRegistry *DiskProviderRegistry
	_, err := nilRegistry.RegisterProvider(bindingTestNonComparable{DiskProvider: tools.OSDisk()})
	require.ErrorIs(t, err, ErrNilDiskProviderRegistry)
	_, err = NewDiskProviderRegistry().Lookup("missing")
	require.True(t, errors.Is(err, ErrDiskProviderUnavailable))
}
