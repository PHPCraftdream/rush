package agent

import (
	"context"
	"fmt"
	"testing"

	"charm.land/catwalk/pkg/catwalk"
	"github.com/PHPCraftdream/rush/internal/config"
	"github.com/PHPCraftdream/rush/internal/message"
	"github.com/PHPCraftdream/rush/internal/session"
	"github.com/stretchr/testify/require"
)

func TestDrainWebModelPolicyUnfencedUsesLastUserOrigin(t *testing.T) {
	for _, tc := range []struct {
		name    string
		origins []message.Origin
		want    string
	}{
		{name: "web-after-cli", origins: []message.Origin{message.OriginCLI, message.OriginWeb}, want: "session-smart-model"},
		{name: "cli-after-web", origins: []message.Origin{message.OriginWeb, message.OriginCLI}, want: "probe"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			f := newAttemptFixture(t, "drain-origin-unfenced-"+tc.name, attemptFixtureOpts{
				noIdle: true, noDriver: true, oauthProvider: oauthTestProvider,
			})
			provider := oauthTestProvider
			pc, ok := f.coord.cfg.Config().Providers.Get(provider)
			require.True(t, ok)
			pc.Models = append(pc.Models, catwalk.Model{ID: "session-smart-model"})
			f.coord.cfg.SetProviderRuntimeConfig(provider, pc)
			f.coord.cfg.Config().Models[config.SelectedModelTypeSmart] = config.SelectedModel{Provider: provider, Model: "probe"}
			f.coord.cfg.Config().Models[config.SelectedModelTypeFast] = config.SelectedModel{Provider: provider, Model: "probe"}
			require.NoError(t, f.env.sessions.UpdateModels(ctx, f.sessID,
				&session.ModelSlotUpdate{Provider: provider, Model: "session-smart-model"}, nil))
			for i, origin := range tc.origins {
				_, err := f.env.messages.Create(ctx, f.sessID, message.CreateMessageParams{
					Role: message.User, Parts: []message.ContentPart{message.TextContent{Text: fmt.Sprintf("message-%d", i)}}, Origin: origin,
				})
				require.NoError(t, err)
			}
			call, prep, err := f.coord.drainCallFor(ctx, f.sessID)
			require.NoError(t, err)
			require.Nil(t, prep, "unfenced drain has no prepared continuation")
			require.Equal(t, tc.want, call.SmartModel.ModelCfg.Model)
		})
	}
}

func TestDrainWebModelPolicyUsesLastUserOrigin(t *testing.T) {
	for _, tc := range []struct {
		name    string
		origins []message.Origin
		want    string
	}{
		{name: "web-after-cli", origins: []message.Origin{message.OriginCLI, message.OriginWeb}, want: "session-smart-model"},
		{name: "cli-after-web", origins: []message.Origin{message.OriginWeb, message.OriginCLI}, want: "probe"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			f := newAttemptFixture(t, "drain-origin-"+tc.name, attemptFixtureOpts{
				noIdle: true, noDriver: true, oauthProvider: oauthTestProvider,
			})
			provider := oauthTestProvider
			pc, ok := f.coord.cfg.Config().Providers.Get(provider)
			require.True(t, ok)
			pc.Models = append(pc.Models, catwalk.Model{ID: "session-smart-model"})
			f.coord.cfg.SetProviderRuntimeConfig(provider, pc)
			f.coord.cfg.Config().Models[config.SelectedModelTypeSmart] = config.SelectedModel{Provider: provider, Model: "probe"}
			f.coord.cfg.Config().Models[config.SelectedModelTypeFast] = config.SelectedModel{Provider: provider, Model: "probe"}
			require.NoError(t, f.env.sessions.UpdateModels(ctx, f.sessID,
				&session.ModelSlotUpdate{Provider: provider, Model: "session-smart-model"}, nil))
			for i, origin := range tc.origins {
				_, err := f.env.messages.Create(ctx, f.sessID, message.CreateMessageParams{
					Role: message.User, Parts: []message.ContentPart{message.TextContent{Text: fmt.Sprintf("message-%d", i)}}, Origin: origin,
				})
				require.NoError(t, err)
			}
			_, _ = wireRootDrainFence(t, f)

			call, prep, err := f.coord.drainCallFor(ctx, f.sessID)
			require.NoError(t, err)
			require.NotNil(t, prep)
			defer prep.release()
			require.Equal(t, tc.want, call.SmartModel.ModelCfg.Model)
		})
	}
}

func TestDrainWebModelPolicyUnfencedPreservesCallOptionsAndExcludesReviewer(t *testing.T) {
	ctx := WithCallOptions(context.Background(), &CallOptions{
		DisableSubAgents: true,
		ModelRole:        config.SelectedModelTypeReviewer,
	})
	f := newAttemptFixture(t, "drain-origin-unfenced-reviewer", attemptFixtureOpts{
		noIdle: true, noDriver: true, oauthProvider: oauthTestProvider,
	})
	provider := oauthTestProvider
	pc, ok := f.coord.cfg.Config().Providers.Get(provider)
	require.True(t, ok)
	pc.Models = append(pc.Models, catwalk.Model{ID: "session-smart-model"})
	f.coord.cfg.SetProviderRuntimeConfig(provider, pc)
	f.coord.cfg.Config().Models[config.SelectedModelTypeSmart] = config.SelectedModel{Provider: provider, Model: "probe"}
	f.coord.cfg.Config().Models[config.SelectedModelTypeFast] = config.SelectedModel{Provider: provider, Model: "probe"}
	require.NoError(t, f.env.sessions.UpdateModels(ctx, f.sessID,
		&session.ModelSlotUpdate{Provider: provider, Model: "session-smart-model"}, nil))
	_, err := f.env.messages.Create(ctx, f.sessID, message.CreateMessageParams{
		Role: message.User, Parts: []message.ContentPart{message.TextContent{Text: "web"}}, Origin: message.OriginWeb,
	})
	require.NoError(t, err)

	call, prep, err := f.coord.drainCallFor(ctx, f.sessID)
	require.NoError(t, err)
	require.Nil(t, prep)
	require.NotEqual(t, "session-smart-model", call.SmartModel.ModelCfg.Model, "reviewer must not inherit web session preferences")
	require.NotNil(t, call.CallOptions)
	require.True(t, call.CallOptions.DisableSubAgents, "unfenced setup must preserve unrelated CallOptions")
	require.Equal(t, config.SelectedModelTypeReviewer, call.CallOptions.ModelRole)
	require.False(t, call.CallOptions.WebTurnOptIn)
}

func TestDrainWebModelPolicyReviewerExclusion(t *testing.T) {
	ctx := WithCallOptions(context.Background(), &CallOptions{
		WebTurnOptIn: true,
		ModelRole:    config.SelectedModelTypeReviewer,
	})
	require.False(t, WebModelPolicy(ctx), "reviewer calls must not inherit web session preferences")
}
