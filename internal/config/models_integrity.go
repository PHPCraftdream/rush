// Model-integrity primitives: canonicalization of the models and roles
// JSON, the cache_rev signature, and the per-document integrity check.
// Pure functions over bytes; they move to the model package in D2-1
// (docs/plans/2026-11-15-sprint0-split-config.md §2.3).
package config

import (
	"fmt"
	"strings"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

const cacheRevKey = "cache_rev"

// applyModelsIntegrity signs built-in models and custom role definitions.
func applyModelsIntegrity(doc string) (string, error) {
	canonical, present, err := canonicalModelsAndRolesJSON([]byte(doc))
	if err != nil {
		return "", fmt.Errorf("failed to canonicalize model settings: %w", err)
	}
	if present {
		return sjson.Set(doc, cacheRevKey, modelsIntegritySignature(canonical))
	}
	return sjson.Delete(doc, cacheRevKey)
}

// touchesModelsKey includes custom roles in the model-integrity boundary.
func touchesModelsKey(keys []string) bool {
	for _, key := range keys {
		if component := settingsKeyFirstComponent(key); component == "models" || strings.EqualFold(component, "roles") {
			return true
		}
	}
	return false
}

// modelsIntegrityOf checks one config document's JSON-shaped bytes against the
// models-integrity rule: a models block must carry a valid cache_rev
// signature. A models block with no cache_rev at all is a legacy file,
// accepted only when no settings lock exists. data unreadable (nil or
// garbage) fails closed. Reload reuses this on the candidate's stable
// bytes, so a hand-edited models block never reaches a running process.
//
// data is the decoded JSON for .ktav documents (decodeConfigDocument ->
// ktavcodec.DecodeKtav + ToJSON), so no extra number normalization is
// needed before canonicalModelsJSON: the codec re-tokenizes every float
// through strconv.FormatFloat('g', -1, 64) and integers through
// strconv.FormatInt, and ToJSON emits those normalized lexemes, so a
// cache_rev computed over Ktav-normalized values at migration time
// verifies here unchanged.
func modelsIntegrityOf(data []byte, lockExists bool) error {
	canonical, present, err := canonicalModelsAndRolesJSON(data)
	if err != nil {
		return ErrModelsEditedDirectly
	}
	if !present {
		return nil
	}
	rev := gjson.GetBytes(data, cacheRevKey)
	if !rev.Exists() {
		// Genuinely absent: legacy file. Accepted only when unlocked.
		if lockExists {
			return ErrModelsEditedDirectly
		}
		return nil
	}
	// Present but not a valid signature string: a mismatch, locked or not.
	if rev.Type != gjson.String || rev.String() == "" {
		return ErrModelsEditedDirectly
	}
	if rev.String() != modelsIntegritySignature(canonical) {
		return ErrModelsEditedDirectly
	}
	return nil
}
