package codexprovider

import (
	"encoding/json"

	"charm.land/fantasy"
)

// UsageMetadata preserves presence from the native Responses usage decoder.
// A nil counter is unknown; only an actual cached_tokens field can prove zero.
// It carries no payload, credentials, affinity key or provider capability guess.
type UsageMetadata struct {
	CachedTokens *int64 `json:"cached_tokens,omitempty"`
}

var _ fantasy.ProviderOptionsData = (*UsageMetadata)(nil)

func (*UsageMetadata) Options() {}

func (m *UsageMetadata) MarshalJSON() ([]byte, error) {
	type wire UsageMetadata
	return json.Marshal((*wire)(m))
}

func (m *UsageMetadata) UnmarshalJSON(data []byte) error {
	type wire UsageMetadata
	var parsed wire
	if err := json.Unmarshal(data, &parsed); err != nil {
		return err
	}
	*m = UsageMetadata(parsed)
	return nil
}
