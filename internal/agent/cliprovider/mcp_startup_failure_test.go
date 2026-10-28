package cliprovider

import (
	"context"
	"io"
	"os"
	"strings"
	"testing"

	"charm.land/fantasy"
	"github.com/stretchr/testify/require"
)

func TestStartStreamRequiredMCPFailureFailsClosed(t *testing.T) {
	self, err := os.Executable()
	require.NoError(t, err)
	m := &cliModel{
		spec: CLISpec{
			ModelID:    "required-mcp",
			Binary:     self,
			UseRushMCP: true,
			BuildArgs:  func(bool) []string { return nil },
		},
		perms: &recordingMCPPermissionService{},
		streamProcess: func(bool) procHandle {
			t.Fatal("process started without required MCP")
			return procHandle{}
		},
	}
	_, err = m.StartStream(context.Background(), fantasy.Call{})
	require.ErrorContains(t, err, "required MCP server failed to start")
}

func TestStartStreamRequiredMCPStartsNormally(t *testing.T) {
	self, err := os.Executable()
	require.NoError(t, err)
	started := false
	m := &cliModel{
		spec: CLISpec{
			ModelID:    "required-mcp",
			Binary:     self,
			UseRushMCP: true,
			BuildArgs:  func(bool) []string { return nil },
		},
		perms: &recordingMCPPermissionService{},
		streamProcess: func(bool) procHandle {
			started = true
			return procHandle{
				stdout: strings.NewReader("done\n"),
				kill:   func() {},
				wait:   func() (string, error) { return "", nil },
			}
		},
	}
	ctx := context.WithValue(context.Background(), SessionIDContextKey, "session")
	stream, err := m.Stream(ctx, fantasy.Call{})
	require.NoError(t, err)
	for range stream {
	}
	require.True(t, started)
}

func TestStartStreamWithoutRushMCPRemainsAvailable(t *testing.T) {
	self, err := os.Executable()
	require.NoError(t, err)
	started := false
	m := &cliModel{
		spec: CLISpec{ModelID: "no-tools", Binary: self, UseRushMCP: true, BuildArgs: func(bool) []string { return nil }},
		streamProcess: func(bool) procHandle {
			started = true
			return procHandle{
				stdout: io.NopCloser(strings.NewReader("done\n")),
				kill:   func() {},
				wait:   func() (string, error) { return "", nil },
			}
		},
	}
	stream, err := m.Stream(context.Background(), fantasy.Call{})
	require.NoError(t, err)
	for range stream {
	}
	require.True(t, started)
}
