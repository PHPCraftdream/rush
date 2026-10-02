package config

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"time"

	"charm.land/catwalk/pkg/catwalk"
)

const modelCatalogTTL = 7 * 24 * time.Hour

type cachedModelCatalog struct {
	Fingerprint string          `json:"fingerprint"`
	FetchedAt   time.Time       `json:"fetched_at"`
	Models      []catwalk.Model `json:"models"`
}

var cachedModelCatalogProviders = [...]string{"openai-codex", "stepfun", "zai"}

func modelCatalogPath(provider string) string {
	return cachePathFor("model-catalog-" + provider)
}

func modelCatalogFingerprint(provider, endpoint, identity string) string {
	value := sha256.Sum256([]byte(provider + "\x00" + endpoint + "\x00" + identity))
	return hex.EncodeToString(value[:])
}

func readModelCatalog(path, fingerprint string) (cachedModelCatalog, bool) {
	data, err := os.ReadFile(path)
	if err != nil {
		return cachedModelCatalog{}, false
	}
	var entry cachedModelCatalog
	if json.Unmarshal(data, &entry) != nil || entry.Fingerprint != fingerprint || len(entry.Models) == 0 {
		return cachedModelCatalog{}, false
	}
	return entry, true
}

func cachedProviderModels(ctx context.Context, provider, endpoint, identity string, fetch func(context.Context) ([]catwalk.Model, error)) ([]catwalk.Model, error) {
	path := modelCatalogPath(provider)
	fingerprint := modelCatalogFingerprint(provider, endpoint, identity)
	entry, exists := readModelCatalog(path, fingerprint)
	age := time.Since(entry.FetchedAt)
	if exists && age >= 0 && age < modelCatalogTTL {
		return entry.Models, nil
	}
	models, err := fetch(ctx)
	if err == nil && len(models) == 0 {
		err = fmt.Errorf("model catalog for %s returned no models", provider)
	}
	if err != nil {
		return nil, err
	}
	data, err := json.Marshal(cachedModelCatalog{Fingerprint: fingerprint, FetchedAt: time.Now(), Models: models})
	if err == nil {
		err = os.MkdirAll(filepath.Dir(path), 0o700)
	}
	if err == nil {
		err = atomicWriteFile(path, data, 0o600)
	}
	if err != nil {
		slog.Warn("Failed to cache model catalog", "provider", provider, "error", err)
	}
	return models, nil
}

// ClearModelCatalogCache removes cached provider catalogs. An empty provider
// clears every supported catalog; the next Rush process fetches them again.
func ClearModelCatalogCache(provider string) error {
	if provider == "" || provider == "all" {
		for _, id := range cachedModelCatalogProviders {
			if err := clearModelCatalog(id); err != nil {
				return err
			}
		}
		return nil
	}
	for _, id := range cachedModelCatalogProviders {
		if provider == id {
			return clearModelCatalog(id)
		}
	}
	return fmt.Errorf("unsupported model catalog %q (valid: openai-codex, stepfun, zai, all)", provider)
}

func clearModelCatalog(provider string) error {
	if err := os.Remove(modelCatalogPath(provider)); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("clear %s model catalog: %w", provider, err)
	}
	if provider == "zai" {
		return clearModelCatalog("zai-docs")
	}
	return nil
}
