package mcp

import (
	"context"
	"io"

	modelmcp "github.com/modelcontextprotocol/go-sdk/mcp"
)

// TextAdmission streams decoded text into the caller's pre-persistence owner.
// This bounds additional renderer capture, NOT the SDK's already allocated
// CallToolResult or transport JSON. It runs only for entirely text results;
// media and structured content retain their existing conversion semantics.
type TextAdmission func(context.Context, func(io.Writer) error, bool) (string, error)

type textAdmissionKey struct{}

func WithTextAdmission(ctx context.Context, admission TextAdmission) context.Context {
	return context.WithValue(ctx, textAdmissionKey{}, admission)
}

func admitToolText(ctx context.Context, result *modelmcp.CallToolResult) (ToolResult, bool, error) {
	admit, _ := ctx.Value(textAdmissionKey{}).(TextAdmission)
	if admit == nil || result.StructuredContent != nil {
		return ToolResult{}, false, nil
	}
	for _, part := range result.Content {
		if _, ok := part.(*modelmcp.TextContent); !ok {
			return ToolResult{}, false, nil
		}
	}
	render := func(w io.Writer) error {
		for i, part := range result.Content {
			if i > 0 {
				if _, err := io.WriteString(w, "\n"); err != nil {
					return err
				}
			}
			if _, err := io.WriteString(w, part.(*modelmcp.TextContent).Text); err != nil {
				return err
			}
		}
		return nil
	}
	text, err := admit(ctx, render, result.IsError)
	return ToolResult{Type: "text", Content: text, IsError: result.IsError}, true, err
}
