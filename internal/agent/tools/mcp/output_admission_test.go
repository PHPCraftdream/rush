package mcp

import (
	"context"
	"io"
	"strings"
	"testing"

	modelmcp "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/require"
)

func TestMCPTextAdmissionStreamsFragmentsAndPreservesNonText(t *testing.T) {
	calls := 0
	ctx := WithTextAdmission(t.Context(), func(_ context.Context, render func(io.Writer) error, isError bool) (string, error) {
		calls++
		require.True(t, isError)
		var got strings.Builder
		require.NoError(t, render(&got))
		require.Equal(t, "first\nsecond", got.String())
		return "bounded retained preview", nil
	})
	result := &modelmcp.CallToolResult{IsError: true, Content: []modelmcp.Content{&modelmcp.TextContent{Text: "first"}, &modelmcp.TextContent{Text: "second"}}}
	admitted, handled, err := admitToolText(ctx, result)
	require.NoError(t, err)
	require.True(t, handled)
	require.True(t, admitted.IsError)
	require.Equal(t, "bounded retained preview", admitted.Content)
	require.Equal(t, 1, calls)
	result.StructuredContent = map[string]any{"control": "keep unchanged"}
	_, handled, err = admitToolText(ctx, result)
	require.NoError(t, err)
	require.False(t, handled)
	result.StructuredContent = nil
	result.Content = append(result.Content, &modelmcp.ImageContent{Data: []byte{1}, MIMEType: "image/png"})
	_, handled, err = admitToolText(ctx, result)
	require.NoError(t, err)
	require.False(t, handled)
	require.Equal(t, 1, calls, "media and structured results must not enter text retention")
	_, handled, err = admitToolText(t.Context(), &modelmcp.CallToolResult{})
	require.NoError(t, err)
	require.False(t, handled, "legacy calls must never truncate without an admitted store")
}
