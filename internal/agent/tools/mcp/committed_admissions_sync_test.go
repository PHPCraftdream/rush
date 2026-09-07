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
