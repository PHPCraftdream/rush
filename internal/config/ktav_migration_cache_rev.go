// cache_rev handling for the legacy JSON -> Ktav migration conversion.
package config

import (
	"fmt"

	"github.com/PHPCraftdream/rush/internal/ktavcodec"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// applyKtavMigrationModelsIntegrity carries the models-integrity cache_rev
// across the migration conversion. It runs on the CONVERTED Ktav bytes, just
// before they are staged for publication, and classifies the legacy
// document's cache_rev exactly once:
//
//   - A MISSING cache_rev stays missing: nothing is added (the Ktav bytes
//     already carry the document without a signature).
//   - An INVALID cache_rev (present but not the valid signature over the
//     legacy models value) stays a mismatch: the old value is copied through
//     unchanged so a pre-existing direct edit is never blessed by the
//     migration. The Ktav bytes already carry it verbatim.
//   - A VALID cache_rev is RECOMPUTED over the models value as it will read
//     back from the Ktav document: Ktav normalizes number lexemes on disk
//     (1.50 becomes 1.5) and canonicalModelsJSON hashes number spellings
//     verbatim, so the legacy signature would no longer verify after
//     migration. The signature is computed from the models value of the
//     Ktav->JSON round trip (the same decode path load uses), fed through
//     the same canonicalModelsJSON canonical form, and written back before
//     the document is re-encoded to Ktav.
//
// sync_rev is never touched here; it is copied by the conversion as-is and
// is read without the lock password.
func applyKtavMigrationModelsIntegrity(source, ktavBytes []byte) ([]byte, error) {
	canonical, present, err := canonicalModelsJSON(gjson.Get(string(source), "models").Raw)
	if err != nil || !present {
		// Nothing hashable in the legacy document: leave the converted
		// bytes (and whatever cache_rev they carry) untouched.
		return ktavBytes, nil
	}
	rev := gjson.Get(string(source), cacheRevKey)
	if !rev.Exists() {
		// (d) A missing signature stays missing.
		return ktavBytes, nil
	}
	if rev.Type != gjson.String || rev.String() == "" || rev.String() != modelsIntegritySignature(canonical) {
		// (a)/(c) An existing mismatch stays a mismatch: copy the OLD
		// cache_rev unchanged, never recompute over a tampered models
		// block. The conversion already carried it over verbatim.
		return ktavBytes, nil
	}
	// (b) A valid signature: recompute over the models value AS IT WILL
	// READ BACK from Ktav. Decode the converted bytes back to JSON with
	// the same path the load-time check uses, so the lexeme normalization
	// Ktav applies on disk is exactly what the new signature covers.
	tree, err := ktavcodec.DecodeKtav(ktavBytes)
	if err != nil {
		return nil, fmt.Errorf("ktav round-trip failed while recomputing cache_rev: %w", err)
	}
	jsonBytes, err := ktavcodec.ToJSON(tree)
	if err != nil {
		return nil, fmt.Errorf("ktav round-trip failed while recomputing cache_rev: %w", err)
	}
	normalized, present, err := canonicalModelsJSON(gjson.GetBytes(jsonBytes, "models").Raw)
	if err != nil {
		return nil, fmt.Errorf("failed to canonicalize model settings: %w", err)
	}
	if !present {
		return ktavBytes, nil
	}
	doc, err := sjson.Set(string(source), cacheRevKey, modelsIntegritySignature(normalized))
	if err != nil {
		return nil, fmt.Errorf("failed to update cache_rev during ktav migration: %w", err)
	}
	converted, err := ktavcodec.ConvertJSONToKtav([]byte(doc))
	if err != nil {
		return nil, fmt.Errorf("ktav conversion failed after cache_rev recompute: %w", err)
	}
	return converted, nil
}
