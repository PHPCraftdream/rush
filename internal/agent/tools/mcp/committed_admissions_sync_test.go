package mcp

import (
	"sync"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestCommittedAdmissionsDifferentNameDetachIsSynchronized(t *testing.T) {
	owner, err := Acquire()
	require.NoError(t, err)
	defer func() { require.NoError(t, owner.Close(t.Context())) }()

	const iterations = 2000
	left := "committed-admissions-left"
	right := "committed-admissions-right"
	start := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		<-start
		for range iterations {
			lease := serverLeaseFor(left)
			lease.Lock()
			detachSessionLocked(left)
			lease.Unlock()
		}
	}()
	go func() {
		defer wg.Done()
		<-start
		for range iterations {
			lifecycleMu.Lock()
			owner.committedAdmissions[right] = &serverAdmission{owner: owner, name: right}
			lifecycleMu.Unlock()
		}
	}()
	close(start)
	wg.Wait()

	lifecycleMu.Lock()
	delete(owner.committedAdmissions, right)
	lifecycleMu.Unlock()
}

func TestCommittedAdmissionsDetachStructuralOracle(t *testing.T) {
	owner, err := Acquire()
	require.NoError(t, err)
	defer func() { require.NoError(t, owner.Close(t.Context())) }()

	const iterations = 10000
	for i := range iterations {
		name := "committed-admissions-structural"
		if i%2 == 0 {
			name += "-even"
		} else {
			name += "-odd"
		}
		lifecycleMu.Lock()
		owner.committedAdmissions[name] = &serverAdmission{owner: owner, name: name}
		lifecycleMu.Unlock()
		lease := serverLeaseFor(name)
		lease.Lock()
		detachSessionLocked(name)
		lease.Unlock()
	}

	lifecycleMu.Lock()
	defer lifecycleMu.Unlock()
	require.Empty(t, owner.committedAdmissions)
}

// TestCommittedAdmissionsDisableAndPublishCrossServer drives the
// disable/remove detach path (per-server lease only), the publish/renew
// insert path (lifecycleMu), and a reload-fence reader (lease plus
// lifecycleMu) against different server names concurrently. The plain
// committedAdmissions map must never see two of these unsynchronized,
// and the final oracle asserts the published admission survived while
// the disabled name stayed absent.
func TestCommittedAdmissionsDisableAndPublishCrossServer(t *testing.T) {
	owner, err := Acquire()
	require.NoError(t, err)
	defer func() { require.NoError(t, owner.Close(t.Context())) }()

	const (
		iterations = 2000
		disabled   = "committed-admissions-cross-disabled"
		published  = "committed-admissions-cross-published"
	)
	publishedAdmission := &serverAdmission{owner: owner, name: published}
	start := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(3)
	go func() {
		defer wg.Done()
		<-start
		for range iterations {
			lease := serverLeaseFor(disabled)
			lease.Lock()
			detachSessionLocked(disabled)
			lease.Unlock()
		}
	}()
	go func() {
		defer wg.Done()
		<-start
		for range iterations {
			lifecycleMu.Lock()
			owner.committedAdmissions[published] = publishedAdmission
			lifecycleMu.Unlock()
		}
	}()
	go func() {
		defer wg.Done()
		<-start
		for range iterations {
			lease := serverLeaseFor(disabled)
			lease.Lock()
			lifecycleMu.Lock()
			_, _ = owner.committedAdmissions[disabled]
			lifecycleMu.Unlock()
			lease.Unlock()
		}
	}()
	close(start)
	wg.Wait()

	lifecycleMu.Lock()
	got := owner.committedAdmissions[published]
	_, hadDisabled := owner.committedAdmissions[disabled]
	length := len(owner.committedAdmissions)
	lifecycleMu.Unlock()
	require.Same(t, publishedAdmission, got)
	require.False(t, hadDisabled)
	require.Equal(t, 1, length)
}
