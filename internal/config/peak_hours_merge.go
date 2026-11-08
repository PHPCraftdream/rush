package config

import (
	"bytes"
	"encoding/json"
	"fmt"
	"sort"
)

// peakHoursOverlay is the peak_hours payload one config document
// declares for one provider, in document order.
type peakHoursOverlay struct {
	doc     int
	payload json.RawMessage // nil = key absent; "null" = explicit off
}

// stripPeakHoursOverlays removes every providers.<id>.peak_hours key
// from each document and returns the per-provider overlay lists plus
// the stripped documents. It errors when a peak_hours value is not an
// object or null. Provider entries that become empty objects stay in
// place so the merge still sees them.
func stripPeakHoursOverlays(configs [][]byte) ([][]byte, map[string][]peakHoursOverlay, error) {
	overlays := make(map[string][]peakHoursOverlay)
	stripped := make([][]byte, len(configs))
	for i, data := range configs {
		var root map[string]json.RawMessage
		if err := json.Unmarshal(data, &root); err != nil || root == nil {
			stripped[i] = data
			continue
		}
		providersRaw, ok := root["providers"]
		if !ok {
			stripped[i] = data
			continue
		}
		var providers map[string]json.RawMessage
		if err := json.Unmarshal(providersRaw, &providers); err != nil || providers == nil {
			stripped[i] = data
			continue
		}
		changed := false
		for id, providerRaw := range providers {
			var fields map[string]json.RawMessage
			if err := json.Unmarshal(providerRaw, &fields); err != nil || fields == nil {
				continue
			}
			payload, ok := fields["peak_hours"]
			if !ok {
				continue
			}
			trimmed := bytes.TrimSpace(payload)
			if !bytes.Equal(trimmed, []byte("null")) {
				var value map[string]json.RawMessage
				if err := json.Unmarshal(trimmed, &value); err != nil || value == nil {
					return nil, nil, fmt.Errorf("provider %s: peak_hours must be an object or null", id)
				}
			}
			overlays[id] = append(overlays[id], peakHoursOverlay{doc: i, payload: json.RawMessage(trimmed)})
			delete(fields, "peak_hours")
			merged, err := json.Marshal(fields)
			if err != nil {
				return nil, nil, err
			}
			providers[id] = merged
			changed = true
		}
		if !changed {
			stripped[i] = data
			continue
		}
		providersBytes, err := json.Marshal(providers)
		if err != nil {
			return nil, nil, err
		}
		root["providers"] = providersBytes
		rootBytes, err := json.Marshal(root)
		if err != nil {
			return nil, nil, err
		}
		stripped[i] = rootBytes
	}
	return stripped, overlays, nil
}

// resolvePeakHoursOverlays folds one provider's overlays in document
// order (doc 0 = lowest priority) and returns the winning raw JSON or
// nil (absent/inherit). Legacy fragments field-merge into legacy
// state; the weekly policy form replaces state entirely; explicit
// null turns the feature off; an incomplete legacy fragment over a
// policy is a hard error rather than a guess.
func resolvePeakHoursOverlays(provider string, overlays []peakHoursOverlay) (json.RawMessage, error) {
	var state json.RawMessage // nil = absent, "null" = explicit off, else object
	for _, overlay := range overlays {
		if overlay.payload == nil {
			continue
		}
		trimmed := bytes.TrimSpace(overlay.payload)
		if bytes.Equal(trimmed, []byte("null")) {
			state = json.RawMessage("null")
			continue
		}
		var entry ProviderPeakHours
		if err := json.Unmarshal(trimmed, &entry); err != nil {
			return nil, fmt.Errorf("provider %s: invalid peak_hours payload: %w", provider, err)
		}
		var fields map[string]json.RawMessage
		if err := json.Unmarshal(trimmed, &fields); err != nil {
			return nil, fmt.Errorf("provider %s: invalid peak_hours payload: %w", provider, err)
		}
		_, hasStart := fields["start"]
		_, hasEnd := fields["end"]
		policyForm := peakHoursFieldsArePolicy(fields)
		if policyForm {
			// A non-empty policy object replaces the state entirely.
			state = json.RawMessage(append(json.RawMessage(nil), trimmed...))
			continue
		}
		// Legacy-form document (possibly the empty {} or a
		// message-only object).
		if hasStart && hasEnd {
			if peakHoursStateIsPolicy(state) {
				// A complete window replaces a policy wholesale.
				state = json.RawMessage(append(json.RawMessage(nil), trimmed...))
				continue
			}
			merged, err := mergePeakHoursObjects(state, trimmed)
			if err != nil {
				return nil, err
			}
			state = merged
			continue
		}
		if !hasStart && !hasEnd {
			// Empty {} explicitly disables a policy. Message-only
			// fragments may merge only within the legacy format.
			if len(fields) == 0 {
				if peakHoursStateIsPolicy(state) {
					state = json.RawMessage(append(json.RawMessage(nil), trimmed...))
					continue
				}
				merged, err := mergePeakHoursObjects(state, trimmed)
				if err != nil {
					return nil, err
				}
				state = merged
				continue
			}
			if _, ok := fields["message"]; ok {
				if peakHoursStateIsPolicy(state) {
					return nil, fmt.Errorf("provider %s: incomplete legacy peak_hours fragment cannot be applied to the weekly windows form; write a complete window or policy", provider)
				}
				merged, err := mergePeakHoursMessage(state, trimmed)
				if err != nil {
					return nil, err
				}
				state = merged
				continue
			}
			// Unknown non-policy keys without start/end: replace,
			// matching the policy replacement behaviour.
			state = json.RawMessage(append(json.RawMessage(nil), trimmed...))
			continue
		}
		// Exactly one of start/end: a partial legacy fragment.
		if peakHoursStateIsPolicy(state) {
			return nil, fmt.Errorf(
				"provider %s: incomplete legacy peak_hours fragment (only one of start/end) cannot be applied to the weekly windows form",
				provider)
		}
		merged, err := mergePeakHoursObjects(state, trimmed)
		if err != nil {
			return nil, err
		}
		state = merged
	}
	return state, nil
}

// peakHoursFieldsArePolicy reports whether a decoded peak_hours object
// uses the weekly policy form (any policy-only key present).
func peakHoursFieldsArePolicy(fields map[string]json.RawMessage) bool {
	for _, key := range peakHoursPolicyKeys {
		if _, ok := fields[key]; ok {
			return true
		}
	}
	return false
}

// peakHoursStateIsPolicy reports whether the current resolved state is
// a policy-form object (nil or "null" are not).
func peakHoursStateIsPolicy(state json.RawMessage) bool {
	if len(state) == 0 || bytes.Equal(bytes.TrimSpace(state), []byte("null")) {
		return false
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(state, &fields); err != nil {
		return false
	}
	return peakHoursFieldsArePolicy(fields)
}

// mergePeakHoursObjects field-merges the later JSON object over the
// current one (later wins per key). A nil or null current yields the
// later object verbatim.
func mergePeakHoursObjects(current, later json.RawMessage) (json.RawMessage, error) {
	trimmedCurrent := bytes.TrimSpace(current)
	if len(current) == 0 || bytes.Equal(trimmedCurrent, []byte("null")) {
		return json.RawMessage(append(json.RawMessage(nil), bytes.TrimSpace(later)...)), nil
	}
	var base map[string]json.RawMessage
	if err := json.Unmarshal(trimmedCurrent, &base); err != nil {
		return nil, err
	}
	var overlay map[string]json.RawMessage
	if err := json.Unmarshal(bytes.TrimSpace(later), &overlay); err != nil {
		return nil, err
	}
	for key, value := range overlay {
		base[key] = value
	}
	return json.Marshal(base)
}

// mergePeakHoursMessage merges only the message key of the later
// legacy document onto the current state (legacy or policy alike).
func mergePeakHoursMessage(current, later json.RawMessage) (json.RawMessage, error) {
	var overlay map[string]json.RawMessage
	if err := json.Unmarshal(bytes.TrimSpace(later), &overlay); err != nil {
		return nil, err
	}
	messageRaw, ok := overlay["message"]
	if !ok {
		return json.RawMessage(append(json.RawMessage(nil), bytes.TrimSpace(later)...)), nil
	}
	if len(current) == 0 {
		return json.Marshal(map[string]json.RawMessage{"message": messageRaw})
	}
	trimmedCurrent := bytes.TrimSpace(current)
	if bytes.Equal(trimmedCurrent, []byte("null")) {
		return json.Marshal(map[string]json.RawMessage{"message": messageRaw})
	}
	var base map[string]json.RawMessage
	if err := json.Unmarshal(trimmedCurrent, &base); err != nil {
		return nil, err
	}
	if base == nil {
		base = make(map[string]json.RawMessage)
	}
	base["message"] = messageRaw
	return json.Marshal(base)
}

// applyPeakHoursOverlays injects each provider's resolved peak_hours
// back into the merged document bytes. nil results (absent) are
// skipped; explicit null is injected as JSON null.
func applyPeakHoursOverlays(data []byte, resolved map[string]json.RawMessage) ([]byte, error) {
	if len(resolved) == 0 {
		return data, nil
	}
	var root map[string]json.RawMessage
	if err := json.Unmarshal(data, &root); err != nil || root == nil {
		return data, nil
	}
	providersRaw, ok := root["providers"]
	if !ok {
		providersRaw = json.RawMessage("{}")
	}
	var providers map[string]json.RawMessage
	if err := json.Unmarshal(providersRaw, &providers); err != nil || providers == nil {
		providers = make(map[string]json.RawMessage)
	}
	for _, id := range sortedPeakHoursProviderIDs(resolved) {
		value := resolved[id]
		if value == nil {
			continue
		}
		providerRaw, ok := providers[id]
		if !ok {
			providers[id] = json.RawMessage([]byte(`{"peak_hours":` + string(value) + `}`))
			continue
		}
		var fields map[string]json.RawMessage
		if err := json.Unmarshal(providerRaw, &fields); err != nil || fields == nil {
			fields = make(map[string]json.RawMessage)
		}
		fields["peak_hours"] = value
		merged, err := json.Marshal(fields)
		if err != nil {
			return nil, err
		}
		providers[id] = merged
	}
	providersBytes, err := json.Marshal(providers)
	if err != nil {
		return nil, err
	}
	root["providers"] = providersBytes
	return json.Marshal(root)
}

// sortedPeakHoursProviderIDs returns the resolved provider ids in a
// stable order so injection is deterministic.
func sortedPeakHoursProviderIDs(resolved map[string]json.RawMessage) []string {
	ids := make([]string, 0, len(resolved))
	for id := range resolved {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}
