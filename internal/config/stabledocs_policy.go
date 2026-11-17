package config

import (
	"encoding/json"
	"fmt"

	"github.com/PHPCraftdream/rush/internal/config/io/docread"
)

func loadConfigCandidateStableForWorkingDir(configPaths []string, workingDir string) (*Config, []string, map[string]reloadFileFingerprint, []stableConfigDocument, error) {
	documents, err := docread.ReadStableConfigDocumentsWithOwner(configPaths, func(path string) (int, bool, error) {
		return docread.ConfigOwnerPolicyForPath(path, workingDir)
	})
	if err != nil {
		return nil, nil, nil, nil, err
	}
	for _, document := range documents {
		if document.Present && len(document.Data) > 0 && !json.Valid(document.Data) {
			return nil, nil, nil, nil, fmt.Errorf("invalid JSON in config file %s", document.Path)
		}
		if document.Present {
			if err := validateMCPDisabledOverlays(document.Data); err != nil {
				return nil, nil, nil, nil, fmt.Errorf("invalid MCP configuration in config file %s: %w", document.Path, err)
			}
		}
	}
	configs, loaded, fingerprints := docread.ConfigDocumentBytes(documents)
	docread.AddDocumentAliasFingerprints(fingerprints, documents)

	cfg, err := loadFromBytes(configs)
	if err != nil {
		return nil, nil, nil, nil, err
	}
	return cfg, loaded, fingerprints, documents, nil
}
