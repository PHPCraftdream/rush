package mcp

import (
	"context"
	"testing"

	"github.com/PHPCraftdream/rush/internal/config"
	"github.com/stretchr/testify/require"
)

func TestStartFallbackDoesNotReplaceNewerSession(t *testing.T) {
	const name = "fallback-stale-config"
	fallback := config.MCPConfig{
		Type:    config.MCPHttp,
		URL:     "http://fallback.invalid",
		Headers: map[string]string{"X-Config": "F"},
	}
	winner := config.MCPConfig{
		Type:    config.MCPHttp,
		URL:     "http://winner.invalid",
		Headers: map[string]string{"X-Config": "G"},
	}
	store := config.NewTestStore(&config.Config{MCP: config.MCPs{name: winner}})
	owner, err := Acquire()
	require.NoError(t, err)
	defer func() { require.NoError(t, owner.Close(context.Background())) }()

	winnerSession := &ClientSession{}
	sessions.Set(name, winnerSession)
	setState(name, StateConnected, nil, winnerSession, Counts{})

	startFallback(context.Background(), store, name, fallback, owner)

	current, ok := sessions.Get(name)
	require.True(t, ok)
	require.Same(t, winnerSession, current)
	state, ok := GetState(name)
	require.True(t, ok)
	require.Same(t, winnerSession, state.Client)
}

func TestStartFallbackLetsQueuedMutationWinBeforeStaleResult(t *testing.T) {
	const name = "fallback-queued-mutation"
	fallback := config.MCPConfig{Type: config.MCPHttp, URL: "http://fallback.invalid"}
	winner := config.MCPConfig{Type: config.MCPHttp, URL: "http://winner.invalid"}
	store := config.NewTestStore(&config.Config{MCP: config.MCPs{name: fallback}})
	owner, err := Acquire()
	require.NoError(t, err)
	defer func() { require.NoError(t, owner.Close(context.Background())) }()

	winnerSession := &ClientSession{}
	lease := serverLeaseFor(name)
	lease.Lock()
	gAcquired := make(chan struct{})
	releaseG := make(chan struct{})
	go func() {
		lease.Lock()
		_, _ = store.UpdateMCP(name, func(mcpConfig *config.MCPConfig) { *mcpConfig = winner })
		sessions.Set(name, winnerSession)
		setState(name, StateConnected, nil, winnerSession, Counts{})
		close(gAcquired)
		<-releaseG
		lease.Unlock()
	}()
	lease.Unlock()
	<-gAcquired

	fallbackDone := make(chan struct{})
	go func() {
		startFallback(context.Background(), store, name, fallback, owner)
		close(fallbackDone)
	}()
	close(releaseG)
	<-fallbackDone

	current, ok := sessions.Get(name)
	require.True(t, ok)
	require.Same(t, winnerSession, current)
	state, ok := GetState(name)
	require.True(t, ok)
	require.Equal(t, StateConnected, state.State)
}
