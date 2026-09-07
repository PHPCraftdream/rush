package mcp

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"github.com/PHPCraftdream/rush/internal/config"
	"github.com/stretchr/testify/require"
)

func TestCanceledAddAfterRetainReclaimsUniqueLeaseReferences(t *testing.T) {
	store := isolatedMCPStore(t)
	owner, err := Acquire()
	require.NoError(t, err)
	defer func() { require.NoError(t, owner.Close(context.Background())) }()

	addAdmissionHooks.Lock()
	previous := addAdmissionHooks.afterRetainFn
	var retained int
	addAdmissionHooks.afterRetainFn = func(admission *serverAdmission) {
		retained++
		admission.cancel()
	}
	addAdmissionHooks.Unlock()
	defer func() {
		addAdmissionHooks.Lock()
		addAdmissionHooks.afterRetainFn = previous
		addAdmissionHooks.Unlock()
	}()

	for i := range 256 {
		name := fmt.Sprintf("canceled-add-%d", i)
		err := addServerWithInitializer(
			context.Background(), store, name,
			config.MCPConfig{Type: config.MCPStdio, Command: name},
			func(context.Context, *config.ConfigStore, string, config.MCPConfig, config.VariableResolver, *serverAdmission) error {
				return errors.New("initializer must not run after immediate cancellation")
			},
		)
		require.ErrorIs(t, err, ErrOwnerBusy)
		require.Zero(t, leases.Len(), "canceled Add %d leaked its retained lease", i)
	}
	require.Equal(t, 256, retained)
	leases.mu.Lock()
	require.Empty(t, leases.entries)
	leases.mu.Unlock()
}

func TestRetireMCPClientQueuesCloseAfterFinalRelease(t *testing.T) {
	owner, err := Acquire()
	require.NoError(t, err)
	defer func() { require.NoError(t, owner.Close(context.Background())) }()

	closeStarted := make(chan struct{})
	releaseClose := make(chan struct{})
	var closeCalls atomic.Int32
	retired := &ClientSession{terminal: func() {
		closeCalls.Add(1)
		close(closeStarted)
		<-releaseClose
	}}
	lifecycleMu.Lock()
	owner.trackSessionLocked(retired, "retirement-handoff")
	lifecycleMu.Unlock()
	operationCtx, releaseOperation, usable := retired.acquireOperation(context.Background())
	require.True(t, usable)
	select {
	case <-operationCtx.Done():
		t.Fatal("operation was canceled before retirement")
	default:
	}
	require.False(t, retired.retire())
	retireDone := make(chan struct{})
	go func() {
		retireMCPClient("retirement-handoff", retired)
		close(retireDone)
	}()
	select {
	case <-retireDone:
	case <-time.After(time.Second):
		t.Fatal("retirement blocked while an operation was pinned")
	}
	select {
	case <-closeStarted:
		t.Fatal("close began while an operation was still pinned")
	case <-retired.closedDone():
		t.Fatal("retirement completed before final release")
	default:
	}

	releaseDone := make(chan struct{})
	go func() {
		releaseOperation()
		close(releaseDone)
	}()
	select {
	case <-releaseDone:
	case <-time.After(time.Second):
		t.Fatal("final release waited for blocked Close")
	}
	<-closeStarted
	require.Equal(t, int32(1), closeCalls.Load())
	select {
	case <-retired.closedDone():
		t.Fatal("blocked Close completed before barrier release")
	default:
	}
	close(releaseClose)
	select {
	case <-retired.closedDone():
	case <-time.After(time.Second):
		t.Fatal("retired Close did not finish after barrier release")
	}
	require.Equal(t, int32(1), closeCalls.Load())
}
