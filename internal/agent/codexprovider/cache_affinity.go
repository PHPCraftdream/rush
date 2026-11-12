package codexprovider

import (
	"context"

	"charm.land/fantasy"
	"charm.land/fantasy/providers/openai"
	"github.com/PHPCraftdream/rush/internal/agent/cliprovider"
	"github.com/PHPCraftdream/rush/internal/session"
)

func promptCacheKey(ctx context.Context, call fantasy.Call) (string, bool) {
	if options, ok := call.ProviderOptions[openai.Name].(*openai.ResponsesProviderOptions); ok && options != nil && options.PromptCacheKey != nil {
		return *options.PromptCacheKey, true
	}
	if id, ok := ctx.Value(cliprovider.SessionIDContextKey).(string); ok && id != "" {
		return session.HashID(id), true
	}
	return "", false
}
