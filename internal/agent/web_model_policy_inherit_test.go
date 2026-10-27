package agent

import (
	"context"
	"testing"

	"github.com/PHPCraftdream/rush/internal/config"
	"github.com/stretchr/testify/require"
)

func TestWebModelPolicyReviewerIsolation(t *testing.T) {
	ctx := WithCallOptions(context.Background(), &CallOptions{
		WebTurnOptIn: true,
		ModelRole:    config.SelectedModelTypeReviewer,
	})
	require.False(t, WebModelPolicy(ctx), "reviewer calls must not inherit web session preferences")
	require.Nil(t, inheritWebPolicyCallOptions(ctx), "reviewer child calls must not inherit web policy")
}

func TestInheritWebPolicyCallOptions(t *testing.T) {
	plain := WithCallOptions(context.Background(), &CallOptions{ModelRole: config.SelectedModelTypeReviewer})
	require.Nil(t, inheritWebPolicyCallOptions(plain), "reviewer/default calls do not inherit web policy")

	web := WithCallOptions(context.Background(), &CallOptions{WebTurnOptIn: true, ModelRole: config.SelectedModelTypeSmart})
	childOptions := inheritWebPolicyCallOptions(web)
	require.NotNil(t, childOptions)
	require.True(t, childOptions.WebTurnOptIn)
	require.Empty(t, childOptions.ModelRole, "only the web policy marker crosses the child boundary")
}
