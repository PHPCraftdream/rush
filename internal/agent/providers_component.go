package agent

import (
	"errors"
	"fmt"
	"net/http"
	"os"
	"strings"

	"charm.land/catwalk/pkg/catwalk"
	"charm.land/fantasy"
	"charm.land/fantasy/providers/anthropic"
	"charm.land/fantasy/providers/azure"
	"charm.land/fantasy/providers/bedrock"
	"charm.land/fantasy/providers/google"
	"charm.land/fantasy/providers/openai"
	"charm.land/fantasy/providers/openaicompat"
	"charm.land/fantasy/providers/openrouter"
	"charm.land/fantasy/providers/vercel"
	"github.com/PHPCraftdream/rush/internal/agent/provider/nettransport"
	"github.com/PHPCraftdream/rush/internal/config"
	"github.com/PHPCraftdream/rush/internal/config/domains/oauth/copilot"
	"github.com/PHPCraftdream/rush/internal/kit/diag/log"
	openaisdk "github.com/charmbracelet/openai-go/option"
)

type providerHost interface{}

type (
	providerComponent       struct{ host providerHost }
	coordinatorProviderHost struct{ coordinator *coordinator }
)

func newProviderComponent(c *coordinator) *providerComponent {
	return &providerComponent{host: coordinatorProviderHost{coordinator: c}}
}

func (p *providerComponent) buildAnthropicProvider(baseURL, apiKey string, headers map[string]string, providerID string, httpClient *http.Client) (fantasy.Provider, error) {
	var opts []anthropic.Option
	headers = configureAnthropicAuthHeaders(headers, apiKey, providerID)

	if apiKey != "" &&
		!strings.HasPrefix(apiKey, "Bearer ") &&
		providerID != string(catwalk.InferenceProviderMiniMax) &&
		providerID != string(catwalk.InferenceProviderMiniMaxChina) {
		// X-Api-Key header.
		opts = append(opts, anthropic.WithAPIKey(apiKey))
	}

	if len(headers) > 0 {
		opts = append(opts, anthropic.WithHeaders(headers))
	}

	if baseURL != "" {
		opts = append(opts, anthropic.WithBaseURL(baseURL))
	}

	opts = append(opts, anthropic.WithHTTPClient(withoutEmptyAuthHeaders(httpClient)))
	return anthropic.New(opts...)
}

func (p *providerComponent) buildOpenaiProvider(baseURL, apiKey string, headers map[string]string, httpClient *http.Client) (fantasy.Provider, error) {
	opts := []openai.Option{
		openai.WithAPIKey(apiKey),
		openai.WithUseResponsesAPI(),
	}
	if httpClient != nil {
		opts = append(opts, openai.WithHTTPClient(httpClient))
	}
	if len(headers) > 0 {
		opts = append(opts, openai.WithHeaders(headers))
	}
	if baseURL != "" {
		opts = append(opts, openai.WithBaseURL(baseURL))
	}
	return openai.New(opts...)
}

func (p *providerComponent) buildOpenrouterProvider(_ string, apiKey string, headers map[string]string, httpClient *http.Client) (fantasy.Provider, error) {
	opts := []openrouter.Option{
		openrouter.WithAPIKey(apiKey),
	}
	if httpClient != nil {
		opts = append(opts, openrouter.WithHTTPClient(httpClient))
	}
	if len(headers) > 0 {
		opts = append(opts, openrouter.WithHeaders(headers))
	}
	return openrouter.New(opts...)
}

func (p *providerComponent) buildVercelProvider(_ string, apiKey string, headers map[string]string, httpClient *http.Client) (fantasy.Provider, error) {
	opts := []vercel.Option{
		vercel.WithAPIKey(apiKey),
	}
	if httpClient != nil {
		opts = append(opts, vercel.WithHTTPClient(httpClient))
	}
	if len(headers) > 0 {
		opts = append(opts, vercel.WithHeaders(headers))
	}
	return vercel.New(opts...)
}

// buildOpenaiCompatProvider takes the SAME pinned cfg snapshot (F6) the
// caller resolved providerCfg from: its Options.Debug flag shapes the
// Copilot transport and must not come from a separately-timed live read.
func (p *providerComponent) buildOpenaiCompatProvider(cfg *config.Config, baseURL, apiKey string, headers map[string]string, extraBody map[string]any, providerID string, isSubAgent bool, httpClient *http.Client) (fantasy.Provider, error) {
	opts := []openaicompat.Option{
		openaicompat.WithBaseURL(baseURL),
		openaicompat.WithAPIKey(apiKey),
	}

	// Set HTTP client based on provider and debug mode. Copilot builds its
	// own initiator-transport client, layered on top of the resolved
	// network transport; everything else uses the resolved client as-is.
	switch providerID {
	case string(catwalk.InferenceProviderCopilot):
		opts = append(
			opts,
			openaicompat.WithUseResponsesAPI(),
			openaicompat.WithResponsesAPIFunc(func(modelID string) bool {
				return copilotResponsesModels[modelID]
			}),
		)
		opts = append(opts, openaicompat.WithHTTPClient(copilot.NewClientWithTransport(
			isSubAgent, cfg.Options.Debug, copilotBaseTransport(httpClient))))
	default:
		if httpClient != nil {
			opts = append(opts, openaicompat.WithHTTPClient(httpClient))
		}
	}

	if len(headers) > 0 {
		opts = append(opts, openaicompat.WithHeaders(headers))
	}

	for extraKey, extraValue := range extraBody {
		opts = append(opts, openaicompat.WithSDKOptions(openaisdk.WithJSONSet(extraKey, extraValue)))
	}

	return openaicompat.New(opts...)
}

func (p *providerComponent) buildAzureProvider(baseURL, apiKey string, headers map[string]string, options map[string]string, httpClient *http.Client) (fantasy.Provider, error) {
	opts := []azure.Option{
		azure.WithBaseURL(baseURL),
		azure.WithAPIKey(apiKey),
		azure.WithUseResponsesAPI(),
	}
	if httpClient != nil {
		opts = append(opts, azure.WithHTTPClient(httpClient))
	}
	if options == nil {
		options = make(map[string]string)
	}
	if apiVersion, ok := options["apiVersion"]; ok {
		opts = append(opts, azure.WithAPIVersion(apiVersion))
	}
	if len(headers) > 0 {
		opts = append(opts, azure.WithHeaders(headers))
	}

	return azure.New(opts...)
}

func (p *providerComponent) buildBedrockProvider(apiKey string, headers map[string]string, httpClient *http.Client, allowAmbientAuth bool) (fantasy.Provider, error) {
	var opts []bedrock.Option
	if httpClient != nil {
		opts = append(opts, bedrock.WithHTTPClient(httpClient))
	}
	if len(headers) > 0 {
		opts = append(opts, bedrock.WithHeaders(headers))
	}
	switch {
	case apiKey != "":
		opts = append(opts, bedrock.WithAPIKey(apiKey))
	case apiKey == "" && allowAmbientAuth && os.Getenv("AWS_BEARER_TOKEN_BEDROCK") != "":
		opts = append(opts, bedrock.WithAPIKey(os.Getenv("AWS_BEARER_TOKEN_BEDROCK")))
	default:
		// Skip, let the SDK do authentication.
	}
	return bedrock.New(opts...)
}

func (p *providerComponent) buildGoogleProvider(baseURL, apiKey string, headers map[string]string, httpClient *http.Client) (fantasy.Provider, error) {
	opts := []google.Option{
		google.WithBaseURL(baseURL),
		google.WithGeminiAPIKey(apiKey),
	}
	if httpClient != nil {
		opts = append(opts, google.WithHTTPClient(httpClient))
	}
	if len(headers) > 0 {
		opts = append(opts, google.WithHeaders(headers))
	}
	return google.New(opts...)
}

func (p *providerComponent) buildGoogleVertexProvider(headers map[string]string, options map[string]string, httpClient *http.Client) (fantasy.Provider, error) {
	opts := []google.Option{}
	if httpClient != nil {
		opts = append(opts, google.WithHTTPClient(httpClient))
	}
	if len(headers) > 0 {
		opts = append(opts, google.WithHeaders(headers))
	}

	project := options["project"]
	location := options["location"]

	opts = append(opts, google.WithVertex(project, location))

	return google.New(opts...)
}

func (p *providerComponent) isAnthropicThinking(model config.SelectedModel) bool {
	if model.Think {
		return true
	}
	opts, err := anthropic.ParseOptions(model.ProviderOptions)
	return err == nil && opts.Thinking != nil
}

func (p *providerComponent) isUnauthorized(err error) bool {
	if responsesWSReplayForbidden(err) {
		return false
	}
	var providerErr *fantasy.ProviderError
	return errors.As(err, &providerErr) && providerErr.StatusCode == http.StatusUnauthorized
}

// resolveProviderHTTPClient builds the *http.Client this provider's
// outbound connections should use, composing the resolved proxy/DNS/DoH
// network config with the existing Options.Debug request/response
// logging. Returns nil when NEITHER is configured, so callers fall back
// to the provider SDK's own default client unchanged.
//
// cfg MUST be the SAME pinned *config.Config snapshot the caller used to
// resolve providerCfg (F6): this used to read the global network defaults
// via its own separate, later c.cfg.Config() call, so a reload landing
// between the caller's snapshot and this read paired a provider built
// from config generation A with network policy from generation B — a
// mixed pairing that then persisted for the rest of the call. Threading
// the caller's snapshot down makes both reads one atomic read.
func (p *providerComponent) resolveProviderHTTPClient(cfg *config.Config, providerCfg config.ProviderConfig) (*http.Client, error) {
	resolved := nettransport.ResolveNetworkConfig(cfg.Options.Network, providerCfg.Network)
	networkClient, err := nettransport.BuildHTTPClient(resolved)
	if err != nil {
		// R2-5: the inner error text can embed the raw proxy/DoH URL
		// from config (userinfo, query secrets included); redact before
		// this message is wrapped and persisted. Unwrap still reaches
		// the original error so errors.Is/As classification is intact.
		return nil, fmt.Errorf("provider %q: build network client: %w", providerCfg.ID, redactNetworkError(err))
	}
	if !cfg.Options.Debug {
		return networkClient, nil
	}
	base := http.RoundTripper(http.DefaultTransport)
	if networkClient != nil {
		base = networkClient.Transport
	}
	return log.NewHTTPClientWithTransport(base), nil
}
