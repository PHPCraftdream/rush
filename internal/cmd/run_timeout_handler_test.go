package cmd

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"charm.land/fang/v2"
	"github.com/PHPCraftdream/rush/internal/agent"
	"github.com/PHPCraftdream/rush/internal/app"
	"github.com/stretchr/testify/require"
)

// Revert-check: commandErrorHandler (installed by Execute) pins plain timeout rendering and default delegation.
func TestCommandErrorHandlerTimeoutAndDefault(t *testing.T) {
	var styles fang.Styles
	timeoutErr := &app.RunTimeoutError{Cause: &agent.RunTimeoutCause{Duration: time.Second, Source: "--timeout"}, ResumeCommand: "rush run --role fast --session s1"}
	var got bytes.Buffer
	commandErrorHandler(&got, styles, fmt.Errorf("wrapped: %w", timeoutErr))
	require.Equal(t, "rush: run timeout 1s exceeded (source: --timeout); resume: rush run --role fast --session s1; stored work remains in the session (rush sessions show/last)\n", got.String())
	require.NotContains(t, got.String(), "ERROR")
	require.ErrorIs(t, timeoutErr, context.DeadlineExceeded)
	got.Reset()
	var want bytes.Buffer
	generic := errors.New("ordinary failure")
	fang.DefaultErrorHandler(&want, styles, generic)
	commandErrorHandler(&got, styles, generic)
	require.Equal(t, want.String(), got.String())
}
