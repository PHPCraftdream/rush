package server

import (
	"bytes"
	"encoding/json"
	"fmt"
)

// MarshalJSON preserves the policy discriminator even for an empty window list.
func (w PeakHoursWirePayload) MarshalJSON() ([]byte, error) {
	type plain PeakHoursWirePayload
	if w.Windows == nil && w.Timezone == "" && w.PausedUntil == "" {
		return json.Marshal(plain(w))
	}
	windows := w.Windows
	if windows == nil {
		windows = []PeakHoursWindowWire{}
	}
	return json.Marshal(struct {
		plain
		Windows []PeakHoursWindowWire `json:"windows"`
	}{plain: plain(w), Windows: windows})
}

// UnmarshalJSON rejects unknown fields before handlers can write configuration.
func (w *PeakHoursWirePayload) UnmarshalJSON(data []byte) error {
	type plain PeakHoursWirePayload
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil {
		return err
	}
	var value plain
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&value); err != nil {
		return err
	}
	_, windowsPresent := fields["windows"]
	_, timezonePresent := fields["timezone"]
	_, pausePresent := fields["pausedUntil"]
	policyForm := windowsPresent || timezonePresent || pausePresent
	if policyForm {
		if _, present := fields["start"]; present {
			return fmt.Errorf("peakHours mixes legacy start/end with the weekly windows form")
		}
		if _, present := fields["end"]; present {
			return fmt.Errorf("peakHours mixes legacy start/end with the weekly windows form")
		}
		if !windowsPresent && value.Timezone != "" && value.PausedUntil == "" {
			return fmt.Errorf("peakHours timezone requires an explicit windows field")
		}
		if value.Windows == nil {
			value.Windows = []PeakHoursWindowWire{}
		}
	}
	*w = PeakHoursWirePayload(value)
	return nil
}
