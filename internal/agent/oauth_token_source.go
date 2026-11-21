package agent

import (
	"net/http"

	"charm.land/catwalk/pkg/catwalk"
	"github.com/PHPCraftdream/rush/internal/agent/runtime/modelkit"

	"github.com/PHPCraftdream/rush/internal/agent/provider/hyper"
	"github.com/PHPCraftdream/rush/internal/agent/runtime/policy/provauth"
	"github.com/PHPCraftdream/rush/internal/config"
)

// withOAuthBearerClient leaves SDK/static credentials alone unless a live OAuth store is available.
func (c *coordinator) withOAuthBearerClient(pc config.ProviderConfig, client *http.Client) *http.Client {
	return newOAuthBearerClientComponent(c).withOAuthBearerClient(pc, client)
}

type oauthBearerClientHost interface {
	oauthTokenConfigStore() *config.ConfigStore
}

func (c *coordinator) oauthTokenConfigStore() *config.ConfigStore {
	if c == nil {
		return nil
	}
	return c.cfg
}

type oauthBearerClientComponent struct {
	host oauthBearerClientHost
}

func newOAuthBearerClientComponent(c *coordinator) *oauthBearerClientComponent {
	return &oauthBearerClientComponent{host: c}
}

func (o *oauthBearerClientComponent) withOAuthBearerClient(pc config.ProviderConfig, client *http.Client) *http.Client {
	if o.host.oauthTokenConfigStore() == nil || pc.OAuthToken == nil || (pc.ID != string(catwalk.InferenceProviderCopilot) && pc.ID != hyper.Name) {
		return client
	}
	if client == nil {
		client = http.DefaultClient
	}
	copied := *client
	base := copied.Transport
	if base == nil {
		base = http.DefaultTransport
	}
	copied.Transport = provauth.NewBearerTransport(base, provauth.NewOAuthTokenSource(pc.ID, o.host.oauthTokenConfigStore(), modelkit.ResolveProviderHTTPClient), pc.ID)
	return &copied
}
