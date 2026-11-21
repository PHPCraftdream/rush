package modelkit

import (
	"errors"
	"fmt"
	"maps"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"slices"
	"strings"

	"github.com/PHPCraftdream/rush/internal/agent/runtime/policy/provauth"

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

// Copilot models that use the Responses API instead of Chat Completions.
var copilotResponsesModels = map[string]bool{
	"gpt-5.2":       true,
	"gpt-5.2-codex": true,
	"gpt-5.3-codex": true,
	"gpt-5.4":       true,
	"gpt-5.4-mini":  true,
	"gpt-5.5":       true,
	"gpt-5-mini":    true,
}

// OpenCode models that use Anthropic Messages API instead of Chat Completions.
// Ported from upstream b7f4ad6c (#3040).
var opencodeMessagesModels = map[string]bool{
	"qwen3.7-max": true,
}

// zai53ModelIDs are the Z.AI model ids whose reasoning-effort contract
// matches zai53ReasoningLevels in internal/cmd/models_atoms.go (low/high/max,
// no off — see that var's comment for the verification). Registered here as
// a map rather than inline `||` comparisons so a future sibling id is a
// one-line addition; the `internal/cmd` atom registry can't be imported
// directly (it doesn't export atomRegistry), so this list is the other half
// of the three-way SYNC WARNING pairing this file already documents.
var zai53ModelIDs = map[string]bool{
	"glm-5.3":       true,
	"glm-5.3-flash": true,
}

func IsOpenCodeMessagesModel(modelID string) bool {
	return opencodeMessagesModels[modelID]
}

func IsZAI53Model(modelID string) bool {
	return zai53ModelIDs[modelID]
}

func anthropicHeaderValue(headers map[string]string, name string) (string, bool) {
	keys := make([]string, 0, len(headers))
	for key := range headers {
		if strings.EqualFold(key, name) {
			keys = append(keys, key)
		}
	}
	if len(keys) == 0 {
		return "", false
	}
	if value, ok := headers[name]; ok {
		return value, true
	}
	slices.Sort(keys)
	return headers[keys[0]], true
}

func setAnthropicHeader(headers map[string]string, name, value string) {
	for key := range headers {
		if strings.EqualFold(key, name) {
			delete(headers, key)
		}
	}
	headers[name] = value
}

func configureAnthropicAuthHeaders(headers map[string]string, apiKey, providerID string) map[string]string {
	headers = maps.Clone(headers)
	if headers == nil {
		headers = make(map[string]string)
	}
	operatorAPIKey, hasOperatorAPIKey := anthropicHeaderValue(headers, "x-api-key")
	operatorAuthorization, hasOperatorAuthorization := anthropicHeaderValue(headers, "Authorization")

	// Remove all case variants before adding one canonical value. This makes
	// auth precedence independent of map iteration order.
	setAnthropicHeader(headers, "x-api-key", "")
	setAnthropicHeader(headers, "Authorization", "")

	switch {
	case strings.HasPrefix(apiKey, "Bearer "):
		setAnthropicHeader(headers, "x-api-key", "")
		setAnthropicHeader(headers, "Authorization", apiKey)
	case providerID == string(catwalk.InferenceProviderMiniMax) || providerID == string(catwalk.InferenceProviderMiniMaxChina):
		setAnthropicHeader(headers, "x-api-key", "")
		setAnthropicHeader(headers, "Authorization", "Bearer "+apiKey)
	case apiKey != "":
		setAnthropicHeader(headers, "x-api-key", apiKey)
		if hasOperatorAuthorization {
			setAnthropicHeader(headers, "Authorization", operatorAuthorization)
		}
	default:
		if hasOperatorAPIKey {
			setAnthropicHeader(headers, "x-api-key", operatorAPIKey)
		}
		if hasOperatorAuthorization {
			setAnthropicHeader(headers, "Authorization", operatorAuthorization)
		}
	}

	return headers
}

func BuildAnthropicProvider(baseURL, apiKey string, headers map[string]string, providerID string, httpClient *http.Client) (fantasy.Provider, error) {
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

	opts = append(opts, anthropic.WithHTTPClient(provauth.WithoutEmptyAuthHeaders(httpClient)))
	return anthropic.New(opts...)
}

func BuildOpenaiProvider(baseURL, apiKey string, headers map[string]string, httpClient *http.Client) (fantasy.Provider, error) {
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

func BuildOpenrouterProvider(_ string, apiKey string, headers map[string]string, httpClient *http.Client) (fantasy.Provider, error) {
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

func BuildVercelProvider(_ string, apiKey string, headers map[string]string, httpClient *http.Client) (fantasy.Provider, error) {
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

// BuildOpenaiCompatProvider takes the SAME pinned cfg snapshot (F6) the
// caller resolved providerCfg from: its Options.Debug flag shapes the
// Copilot transport and must not come from a separately-timed live read.
func BuildOpenaiCompatProvider(cfg *config.Config, baseURL, apiKey string, headers map[string]string, extraBody map[string]any, providerID string, isSubAgent bool, httpClient *http.Client) (fantasy.Provider, error) {
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

func BuildAzureProvider(baseURL, apiKey string, headers map[string]string, options map[string]string, httpClient *http.Client) (fantasy.Provider, error) {
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

func BuildBedrockProvider(apiKey string, headers map[string]string, httpClient *http.Client, allowAmbientAuth bool) (fantasy.Provider, error) {
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

func BuildGoogleProvider(baseURL, apiKey string, headers map[string]string, httpClient *http.Client) (fantasy.Provider, error) {
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

func BuildGoogleVertexProvider(headers map[string]string, options map[string]string, httpClient *http.Client) (fantasy.Provider, error) {
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

func IsAnthropicThinking(model config.SelectedModel) bool {
	if model.Think {
		return true
	}
	opts, err := anthropic.ParseOptions(model.ProviderOptions)
	return err == nil && opts.Thinking != nil
}

func IsUnauthorized(err error) bool {
	if ResponsesWSReplayForbidden(err) {
		return false
	}
	var providerErr *fantasy.ProviderError
	return errors.As(err, &providerErr) && providerErr.StatusCode == http.StatusUnauthorized
}

// ResolveProviderHTTPClient builds the *http.Client this provider's
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
func ResolveProviderHTTPClient(cfg *config.Config, providerCfg config.ProviderConfig) (*http.Client, error) {
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

// copilotBaseTransport extracts the base round tripper from the optional
// resolved HTTP client so the Copilot initiator transport can layer on top
// of the proxy/DNS-configured transport. Nil means "nothing configured".
func copilotBaseTransport(httpClient *http.Client) http.RoundTripper {
	if httpClient == nil {
		return nil
	}
	return httpClient.Transport
}

// networkURLPattern matches scheme-qualified URL-looking tokens inside
// error text so each can be redacted before the message is persisted.
var networkURLPattern = regexp.MustCompile(`[a-zA-Z][a-zA-Z0-9+.-]*://[^\s'"<>]+`)

// trailingPunctuation is trimmed off a matched token before URL parsing:
// prose often appends sentence punctuation or a closing bracket directly
// to the URL, and a trailing "…8080)." fails to parse — which would
// leave the secrets in place unredacted. The trimmed suffix is appended
// back after redaction.
const trailingPunctuation = ".,;:!?)\\]}>"

// RedactNetworkURLs rewrites every URL-looking token in s (R2-5) so
// diagnostic strings that end up persisted in finish details or the
// transcript cannot carry URL secrets: userinfo is stripped, scheme and
// host are kept, path is kept, and query/fragment are dropped. Tokens
// that do not parse as URLs with a host are left untouched.
func RedactNetworkURLs(s string) string {
	return networkURLPattern.ReplaceAllStringFunc(s, func(token string) string {
		body := strings.TrimRight(token, trailingPunctuation)
		suffix := token[len(body):]
		u, err := url.Parse(body)
		if err != nil || u.Host == "" {
			return token
		}
		return u.Scheme + "://" + u.Host + u.Path + suffix
	})
}

// redactedNetworkError wraps err with a redacted message while keeping
// the original error reachable via Unwrap, so errors.Is/As chains that
// classify network failures keep working after redaction (R2-5).
type redactedNetworkError struct {
	msg string
	err error
}

func (e *redactedNetworkError) Error() string { return e.msg }

func (e *redactedNetworkError) Unwrap() error { return e.err }

// redactNetworkError returns err with any URL secrets in its message
// redacted (R2-5); nil passes through unchanged.
func redactNetworkError(err error) error {
	if err == nil {
		return nil
	}
	return &redactedNetworkError{msg: RedactNetworkURLs(err.Error()), err: err}
}

func ResponsesWSReplayForbidden(err error) bool {
	var guard interface{ NoReplay() bool }
	return errors.As(err, &guard) && guard.NoReplay()
}
