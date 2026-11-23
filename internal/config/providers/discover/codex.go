package discover

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"charm.land/catwalk/pkg/catwalk"
)

const (
	// CodexBaseURL is the authenticated ChatGPT backend root used for Codex.
	CodexBaseURL = "https://chatgpt.com/backend-api"
	// DefaultCodexClientVersion is the Codex CLI version Rush presents when rush.json
	// does not override it (providers.openai-codex.extra_headers.version). The backend
	// lists a model generation only for clients at least as new as the Codex CLI
	// release that introduced it (0.159.0 hid gpt-6.2-*: model_not_found), so a new
	// generation needs only a config edit, not a Rush release.
	DefaultCodexClientVersion = "0.169.0"
	codexMaxResponseBytes     = 10 << 20
)

var codexClientVersion atomic.Pointer[string]

// SetCodexClientVersion installs the configured Codex client version; blank restores the default.
func SetCodexClientVersion(v string) {
	if v = strings.TrimSpace(v); v == "" {
		codexClientVersion.Store(nil)
		return
	}
	codexClientVersion.Store(&v)
}

// ApplyCodexConfig installs the version override from the provider's extra_headers
// ("version"; blank restores the default) and returns the Codex backend base URL.
func ApplyCodexConfig(extraHeaders map[string]string) string {
	SetCodexClientVersion(extraHeaders["version"])
	return CodexBaseURL
}

// CodexCatalogIdentity keys the cached account catalog by the effective client version: the
// backend lists different models per version, and a catalog fetched under an older default
// (0.159.0 hid gpt-6.2-*) must not be served after the default moves.
func CodexCatalogIdentity(accountID, accessToken string) string {
	return cmp.Or(accountID, accessToken) + "@" + CodexClientVersion()
}

// CodexClientVersion is the version sent as client_version and the version header: the configured
// (or default) version, or the version learned from the backend when that is newer. A learned
// version is a floor, so a config reload that still carries the old value cannot lower it.
func CodexClientVersion() string {
	base := DefaultCodexClientVersion
	if v := codexClientVersion.Load(); v != nil {
		base = *v
	}
	if l := codexLearnedVersion.Load(); l != nil && compareSemver(*l, base) > 0 {
		return *l
	}
	return base
}

var (
	codexLearnedVersion   atomic.Pointer[string]
	codexUpgradeMu        sync.Mutex
	codexLastReleaseCheck time.Time
	codexPersister        func(oldVersion, newVersion string)

	// codexLatestReleaseURL is where the latest Codex CLI release is read from (tests replace it).
	codexLatestReleaseURL = "https://api.github.com/repos/openai/codex/releases/latest"
	codexReleaseCooldown  = 10 * time.Minute
	codexSemverRE         = regexp.MustCompile(`\b(\d+)\.(\d+)\.(\d+)\b`)
	// codexTagRE finds the version in a release tag such as "rust-v0.169.0" (no word boundary before the digits).
	codexTagRE = regexp.MustCompile(`(\d+\.\d+\.\d+)`)
)

// ErrCodexUpgradeCooldown is returned when the latest-release lookup already ran recently.
var ErrCodexUpgradeCooldown = errors.New("codex client version: latest release was checked recently")

// SetCodexClientVersionPersister installs the callback that stores a raised version (the app writes
// it to the config; it may fail, for example under a settings lock, which only costs persistence).
func SetCodexClientVersionPersister(fn func(oldVersion, newVersion string)) {
	codexUpgradeMu.Lock()
	codexPersister = fn
	codexUpgradeMu.Unlock()
}

// ResetCodexClientVersionLearning forgets a version learned from the backend (tests, and a deliberate
// return to the configured version).
func ResetCodexClientVersionLearning() {
	codexUpgradeMu.Lock()
	codexLearnedVersion.Store(nil)
	codexLastReleaseCheck = time.Time{}
	codexUpgradeMu.Unlock()
}

// CodexNeedsNewerClient reports whether a 4xx backend answer means "the client is too old": a
// named minimum version in the body, error text asking for a newer client, or the model_not_found
// that version gating produces. 401, 429 and usage limits never qualify. minVersion is the version
// the backend named, or "" when it only implied one.
func CodexNeedsNewerClient(status int, body []byte) (minVersion string, ok bool) {
	if status < 400 || status >= 500 || status == http.StatusUnauthorized || status == http.StatusTooManyRequests {
		return "", false
	}
	var doc any
	if json.Unmarshal(body, &doc) == nil {
		if v := codexMinVersionField(doc); v != "" {
			return v, true
		}
	}
	low := strings.ToLower(string(body))
	if strings.Contains(low, "usage limit") {
		return "", false
	}
	for _, marker := range []string{"newer client", "update codex", "upgrade codex", "client version", "outdated", "model_not_found"} {
		if !strings.Contains(low, marker) {
			continue
		}
		if marker == "model_not_found" && !strings.Contains(low, "when using codex") {
			continue
		}
		return codexSemverRE.FindString(string(body)), true
	}
	return "", false
}

func codexMinVersionField(v any) string {
	switch t := v.(type) {
	case map[string]any:
		for key, val := range t {
			switch strings.ToLower(key) {
			case "minimal_version", "min_client_version", "minimum_client_version", "required_version", "min_version":
				if s := codexVersionValue(val); s != "" {
					return s
				}
			}
		}
		for _, val := range t {
			if s := codexMinVersionField(val); s != "" {
				return s
			}
		}
	case []any:
		for _, val := range t {
			if s := codexMinVersionField(val); s != "" {
				return s
			}
		}
	}
	return ""
}

// codexVersionValue reads a version given as "X.Y.Z" or as [X,Y,Z].
func codexVersionValue(v any) string {
	switch t := v.(type) {
	case string:
		return codexSemverRE.FindString(t)
	case []any:
		if len(t) == 3 {
			parts := make([]string, 0, 3)
			for _, p := range t {
				n, ok := p.(float64)
				if !ok || n < 0 {
					return ""
				}
				parts = append(parts, strconv.Itoa(int(n)))
			}
			return strings.Join(parts, ".")
		}
	}
	return ""
}

func parseSemver(s string) (v [3]int, ok bool) {
	m := codexSemverRE.FindStringSubmatch(s)
	if m == nil {
		return v, false
	}
	for i := range v {
		v[i], _ = strconv.Atoi(m[i+1])
	}
	return v, true
}

// compareSemver orders two X.Y.Z strings; an unparsable one sorts lowest.
func compareSemver(a, b string) int {
	av, aok := parseSemver(a)
	bv, bok := parseSemver(b)
	switch {
	case !aok && !bok:
		return 0
	case !aok:
		return -1
	case !bok:
		return 1
	}
	for i := range av {
		if av[i] != bv[i] {
			return cmp.Compare(av[i], bv[i])
		}
	}
	return 0
}

// RaiseCodexClientVersion lifts the Codex client version after the backend refused an old client:
// to the version it named (hint) or, when it named none, to the latest Codex CLI release (looked up
// at most once per 10 minutes). It only raises, one raise at a time, and hands the new version to
// the persister. raised is false when nothing newer exists.
func RaiseCodexClientVersion(ctx context.Context, client *http.Client, hint string) (version string, raised bool, err error) {
	codexUpgradeMu.Lock()
	defer codexUpgradeMu.Unlock()
	current := CodexClientVersion()
	target := codexSemverRE.FindString(hint)
	if target == "" {
		if time.Since(codexLastReleaseCheck) < codexReleaseCooldown {
			return current, false, ErrCodexUpgradeCooldown
		}
		codexLastReleaseCheck = time.Now()
		if target, err = fetchLatestCodexRelease(ctx, clientOrDefault(client)); err != nil {
			return current, false, err
		}
	}
	if compareSemver(target, current) <= 0 {
		return current, false, nil
	}
	codexLearnedVersion.Store(&target)
	if codexPersister != nil {
		codexPersister(current, target)
	}
	return target, true, nil
}

func fetchLatestCodexRelease(ctx context.Context, client *http.Client) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, codexLatestReleaseURL, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("codex latest release: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("codex latest release: HTTP %d", resp.StatusCode)
	}
	var release struct {
		TagName string `json:"tag_name"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&release); err != nil {
		return "", fmt.Errorf("codex latest release: %w", err)
	}
	if v := codexTagRE.FindString(release.TagName); v != "" {
		return v, nil
	}
	return "", fmt.Errorf("codex latest release: no version in tag %q", release.TagName)
}

// DiscoverCodexModels reads the authenticated account's ChatGPT Codex model
// catalog. The access token is Rush's own OAuth credential, not a Codex CLI
// token. Callers should provide a bounded context.
func DiscoverCodexModels(ctx context.Context, client *http.Client, accessToken, accountID string) ([]catwalk.Model, error) {
	return discoverCodexModels(ctx, clientOrDefault(client), CodexBaseURL, accessToken, accountID)
}

func discoverCodexModels(ctx context.Context, client *http.Client, baseURL, accessToken, accountID string) ([]catwalk.Model, error) {
	var lastErr error
	for _, path := range [...]string{"/codex/models", "/models"} {
		u, err := url.Parse(strings.TrimRight(baseURL, "/") + path)
		if err != nil {
			return nil, fmt.Errorf("build Codex model-list URL: %w", err)
		}
		query := u.Query()
		query.Set("client_version", CodexClientVersion())
		u.RawQuery = query.Encode()

		req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
		if err != nil {
			return nil, fmt.Errorf("build Codex model-list request: %w", err)
		}
		req.Header.Set("Authorization", "Bearer "+accessToken)
		if accountID = strings.TrimSpace(accountID); accountID != "" {
			req.Header.Set("chatgpt-account-id", accountID)
		}
		req.Header.Set("OpenAI-Beta", "responses=experimental")
		req.Header.Set("originator", "omp")
		req.Header.Set("version", CodexClientVersion())
		req.Header.Set("Accept", "application/json")

		resp, err := client.Do(req)
		if err != nil {
			lastErr = fmt.Errorf("request Codex model catalog: %w", err)
			continue
		}
		body, readErr := io.ReadAll(io.LimitReader(resp.Body, codexMaxResponseBytes))
		closeErr := resp.Body.Close()
		if readErr != nil {
			lastErr = fmt.Errorf("read Codex model catalog: %w", readErr)
			continue
		}
		if len(body) == codexMaxResponseBytes {
			lastErr = fmt.Errorf("codex model catalog exceeds %d bytes", codexMaxResponseBytes)
			continue
		}
		if closeErr != nil {
			lastErr = fmt.Errorf("close Codex model catalog response: %w", closeErr)
			continue
		}
		if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
			return nil, fmt.Errorf("codex model catalog rejected the account (HTTP %d); check that this ChatGPT account has Codex access", resp.StatusCode)
		}
		if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
			lastErr = fmt.Errorf("codex model catalog returned HTTP %d", resp.StatusCode)
			continue
		}

		models, valid, err := parseCodexModelCatalog(body)
		if err != nil {
			lastErr = err
			continue
		}
		if valid {
			return models, nil
		}
		lastErr = fmt.Errorf("codex model catalog response has no models or data array")
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("codex model catalog is unavailable")
	}
	return nil, lastErr
}

func parseCodexModelCatalog(body []byte) ([]catwalk.Model, bool, error) {
	var payload struct {
		Models *[]json.RawMessage `json:"models"`
		Data   *[]json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return nil, false, fmt.Errorf("decode Codex model catalog: %w", err)
	}
	entries := payload.Models
	if entries == nil {
		entries = payload.Data
	}
	if entries == nil {
		return nil, false, nil
	}

	models := make([]catwalk.Model, 0, len(*entries))
	for _, raw := range *entries {
		model, ok := parseCodexModel(raw)
		if ok {
			models = append(models, model)
		}
	}
	return models, true, nil
}

func parseCodexModel(raw json.RawMessage) (catwalk.Model, bool) {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil || fields == nil {
		return catwalk.Model{}, false
	}
	id := codexString(fields["slug"])
	if id == "" {
		id = codexString(fields["id"])
	}
	if id == "" {
		return catwalk.Model{}, false
	}
	// Superseded families (pre-6 GPT, o3, codex-mini) never enter the
	// account catalog; explicitly configured selections keep working
	// without it.
	if !ModelVisible("openai-codex", id) {
		return catwalk.Model{}, false
	}
	visibility := strings.ToLower(codexString(fields["visibility"]))
	if visibility == "hide" || visibility == "hidden" {
		return catwalk.Model{}, false
	}

	// Raw values only: context-window and max-token facts come from the
	// documented-facts table (ApplyModelFacts at config load), never from
	// parsing.
	contextWindow := codexPositiveInt(fields["context_window"])
	maxTokens := int64(0)

	defaultEffort := strings.ToLower(codexString(fields["default_reasoning_level"]))
	reasoningLevels := codexReasoningLevels(fields["supported_reasoning_levels"])
	canReason := defaultEffort != "" && defaultEffort != "none" || len(reasoningLevels) > 0

	supportsImages := true
	if rawModalities, ok := fields["input_modalities"]; ok {
		var modalities []string
		if json.Unmarshal(rawModalities, &modalities) == nil {
			supportsImages = false
			for _, modality := range modalities {
				if strings.EqualFold(modality, "image") {
					supportsImages = true
					break
				}
			}
		}
	}

	name := codexString(fields["display_name"])
	if name == "" {
		name = id
	}
	return catwalk.Model{
		ID:                     id,
		Name:                   name,
		ContextWindow:          contextWindow,
		DefaultMaxTokens:       maxTokens,
		CanReason:              canReason,
		ReasoningLevels:        reasoningLevels,
		DefaultReasoningEffort: defaultEffort,
		SupportsImages:         supportsImages,
	}, true
}

// codexGPT6Documented reports whether id is a model of the documented GPT-6
// family: gpt-6 or a later gpt-6.N release of astra, sol or luna.
func codexGPT6Documented(id string) bool {
	if !strings.HasPrefix(id, "gpt-6") {
		return false
	}
	for _, tier := range []string{"-astra", "-sol", "-luna"} {
		if strings.HasSuffix(id, tier) {
			return true
		}
	}
	return false
}

func codexString(raw json.RawMessage) string {
	var value string
	if len(raw) == 0 || json.Unmarshal(raw, &value) != nil {
		return ""
	}
	return strings.TrimSpace(value)
}

func codexPositiveInt(raw json.RawMessage) int64 {
	var number json.Number
	if len(raw) == 0 || json.Unmarshal(raw, &number) != nil {
		return 0
	}
	value, err := strconv.ParseFloat(number.String(), 64)
	if err != nil || value <= 0 || value >= float64(uint64(1)<<63) {
		return 0
	}
	return int64(value)
}

func codexReasoningLevels(raw json.RawMessage) []string {
	var entries []json.RawMessage
	if len(raw) == 0 || json.Unmarshal(raw, &entries) != nil {
		return nil
	}
	levels := make([]string, 0, len(entries))
	seen := make(map[string]struct{}, len(entries))
	for _, entry := range entries {
		var effort string
		if json.Unmarshal(entry, &effort) != nil {
			var item struct {
				Effort string `json:"effort"`
			}
			if json.Unmarshal(entry, &item) != nil {
				continue
			}
			effort = item.Effort
		}
		effort = strings.ToLower(strings.TrimSpace(effort))
		if effort == "" {
			continue
		}
		if _, ok := seen[effort]; ok {
			continue
		}
		seen[effort] = struct{}{}
		levels = append(levels, effort)
	}
	return levels
}
