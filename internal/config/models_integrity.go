package config

import cfgmodel "github.com/PHPCraftdream/rush/internal/config/model"

const cacheRevKey = cfgmodel.CacheRevKey

func applyModelsIntegrity(doc string) (string, error) {
	return cfgmodel.ApplyModelsIntegrity(doc)
}

func touchesModelsKey(keys []string) bool {
	return cfgmodel.TouchesModelsKey(keys)
}

func modelsIntegrityOf(data []byte, lockExists bool) error {
	return cfgmodel.ModelsIntegrityOf(data, lockExists)
}
