package agent

import (
	"context"
	"errors"
	"io"
	"testing"
	"time"

	"github.com/PHPCraftdream/rush/internal/agent/tools"
	"github.com/PHPCraftdream/rush/internal/jobbatch"
	"github.com/stretchr/testify/require"
)

func TestBatchRootLeaseProvisionalFailureAndCallOwnerRelease(t *testing.T) {
	providerRegistry := NewDiskProviderRegistry()
	provider := bindingTestNonComparable{DiskProvider: tools.OSDisk(), tags: []string{"provisional"}}
	binding, err := providerRegistry.RegisterProvider(provider)
	require.NoError(t, err)
	registries := NewBatchLeafRegistryStore()
	registryID := "root-lease-provisional"
	rootRegistry := &policyLeafRegistry{entries: map[string]policyLeafEntry{}}
	require.NoError(t, registries.Register(registryID, rootRegistry))
	refs := newBatchLeafRegistryLeaseStore(registries)
	store := newBatchRootLeaseStore(providerRegistry, refs)
	failed, err := store.acquire(binding, registryID)
	require.NoError(t, err)
	require.NotNil(t, failed)
	_, err = providerRegistry.Lookup(binding)
	require.NoError(t, err) // Provisional ref holds alongside call owner.
	failed.release()
	_, err = providerRegistry.Lookup(binding)
	require.NoError(t, err) // Failed-submit release leaves call owner intact.
	providerRegistry.ReleaseProvider(binding)
	_, err = providerRegistry.Lookup(binding)
	require.ErrorIs(t, err, ErrDiskProviderUnavailable)
	_, err = registries.Resolve(registryID)
	require.Error(t, err) // Failed provisional owner did not leak a registry ref.
}

func TestBatchRootLeaseAdoptsClosedDoneAndReleasesOnlyRootOwner(t *testing.T) {
	providerRegistry := NewDiskProviderRegistry()
	binding, err := providerRegistry.RegisterProvider(bindingTestNonComparable{DiskProvider: tools.OSDisk(), tags: []string{"root"}})
	require.NoError(t, err)
	registries := NewBatchLeafRegistryStore()
	registryID := "root-lease-accepted"
	rootRegistry := &policyLeafRegistry{entries: map[string]policyLeafEntry{}}
	require.NoError(t, registries.Register(registryID, rootRegistry))
	refs := newBatchLeafRegistryLeaseStore(registries)
	store := newBatchRootLeaseStore(providerRegistry, refs)
	root := jobbatch.ID("root-accepted")

	pending, err := store.acquire(binding, registryID)
	require.NoError(t, err)
	providerRegistry.ReleaseProvider(binding) // Simulate call-owner defer.
	_, err = providerRegistry.Lookup(binding)
	require.NoError(t, err) // Root lease bridges call owner to settlement.
	done := make(chan struct{})
	close(done) // Settlement racing ahead of adopt is safe.
	require.NoError(t, store.adopt(root, done, pending))

	_, err = providerRegistry.Lookup(binding)
	require.NoError(t, err)
	resolved, err := registries.Resolve(registryID)
	require.NoError(t, err)
	require.Same(t, rootRegistry, resolved)
}

func TestBatchRootLeaseDuplicateAndMismatchAdoption(t *testing.T) {
	providerRegistry := NewDiskProviderRegistry()
	binding1, err := providerRegistry.RegisterProvider(bindingTestNonComparable{DiskProvider: tools.OSDisk(), tags: []string{"one"}})
	require.NoError(t, err)
	binding2, err := providerRegistry.RegisterProvider(bindingTestNonComparable{DiskProvider: tools.OSDisk(), tags: []string{"two"}})
	require.NoError(t, err)
	registries := NewBatchLeafRegistryStore()
	registryID := "root-lease-duplicate"
	rootRegistry := &policyLeafRegistry{entries: map[string]policyLeafEntry{}}
	require.NoError(t, registries.Register(registryID, rootRegistry))
	refs := newBatchLeafRegistryLeaseStore(registries)
	store := newBatchRootLeaseStore(providerRegistry, refs)
	root := jobbatch.ID("root-shared")
	done := make(chan struct{})
	first, err := store.acquire(binding1, registryID)
	require.NoError(t, err)
	require.NoError(t, store.adopt(root, done, first))
	duplicate, err := store.acquire(binding1, registryID)
	require.NoError(t, err)
	require.NoError(t, store.adopt(root, done, duplicate))
	_, err = providerRegistry.Lookup(binding1)
	require.NoError(t, err) // Duplicate adoption released only its extra ref.
	mismatch, err := store.acquire(binding2, registryID)
	require.NoError(t, err)
	require.ErrorIs(t, store.adopt(root, done, mismatch), ErrDiskProviderUnavailable)
	require.True(t, store.matches(root, binding1, registryID))
	close(done)
	require.Eventually(t, func() bool {
		_, lookupErr := providerRegistry.Lookup(binding)
		return errors.Is(lookupErr, ErrDiskProviderUnavailable)
	}, time.Second, time.Millisecond)
	resolved, err := registries.Resolve(registryID)
	require.NoError(t, err)
	require.Same(t, rootRegistry, resolved) // Registry owner survives root lease.
}

func TestBatchExecutionLeaseIsRetainedUntilDispose(t *testing.T) {
	providerRegistry := NewDiskProviderRegistry()
	binding, err := providerRegistry.RegisterProvider(bindingTestNonComparable{DiskProvider: tools.OSDisk(), tags: []string{"execution"}})
	require.NoError(t, err)
	_, release, err := providerRegistry.Acquire(binding)
	require.NoError(t, err)
	providerRegistry.ReleaseProvider(binding) // Drop the call owner.
	wrapped := newBatchBindingExecution(testBatchLeaseExecution{}, release)
	_, err = providerRegistry.Lookup(binding)
	require.NoError(t, err)
	_, _, err := providerRegistry.Acquire(binding)
	require.NoError(t, err)
	require.NoError(t, wrapped.(jobbatch.Disposer).Dispose(context.Background()))
	_, err = providerRegistry.Lookup(binding)
	require.ErrorIs(t, err, ErrDiskProviderUnavailable)
}

type testBatchLeaseExecution struct{}

func (testBatchLeaseExecution) Stop(context.Context) error    { return nil }
func (testBatchLeaseExecution) Dispose(context.Context) error { return nil }

var (
	_ jobbatch.Execution = testBatchLeaseExecution{}
	_ jobbatch.Disposer  = testBatchLeaseExecution{}
	_ tools.DiskProvider = (*bindingTestNilProvider)(nil)
	_ io.ReadCloser      = io.NopCloser(nil)
)
