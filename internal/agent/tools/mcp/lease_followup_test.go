package mcp

import (
	"context"
	"errors"
	"fmt"
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

func TestRetireMCPClientAndWaitHandlesClosedAndRetiredSessions(t *testing.T) {
	closed := &ClientSession{}
	require.NoError(t, closed.Close())
	retireMCPClientAndWait("already-closed", closed)
	select {
	case <-closed.closedDone():
	default:
		t.Fatal("already-closed session did not signal closedDone")
	}

	retired := &ClientSession{}
	operationCtx, releaseOperation, usable := retired.acquireOperation(context.Background())
	require.True(t, usable)
	defer releaseOperation()
	select {
	case <-operationCtx.Done():
		t.Fatal("operation was canceled before retirement")
	default:
	}
	require.False(t, retired.retire())
	waitDone := make(chan struct{})
	go func() {
		retireMCPClientAndWait("already-retired", retired)
		close(waitDone)
	}()
	releaseOperation()
	select {
	case <-waitDone:
	case <-time.After(time.Second):
		t.Fatal("retireMCPClientAndWait did not receive the final close handoff")
	}
}
