package tools

import "context"

type suppressBackgroundCallbackKey struct{}

// WithoutBackgroundCallback delegates completion delivery to the caller.
func WithoutBackgroundCallback(ctx context.Context) context.Context {
	return context.WithValue(ctx, suppressBackgroundCallbackKey{}, true)
}

func suppressBackgroundCallback(ctx context.Context) bool {
	return ctx.Value(suppressBackgroundCallbackKey{}) == true
}
