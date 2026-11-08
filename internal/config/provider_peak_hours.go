package config

import (
	"bytes"
	"encoding/json"
	"fmt"
	"time"
)

// ProviderPeakHours holds exactly one of the two peak-hours forms under
// the same providers.<id>.peak_hours JSON key: the legacy single-window
// form (Legacy) or the weekly policy form (Policy). Nil pointer or both
// legs nil = feature off.
type ProviderPeakHours struct {
	Legacy *PeakHoursWindow `json:"-"`
	Policy *PeakHoursPolicy `json:"-"`
}

// peakHoursPolicyKeys are the JSON keys unique to the weekly policy
// form; any of them present selects policy decoding.
var peakHoursPolicyKeys = [3]string{"windows", "timezone", "paused_until"}

// UnmarshalJSON decodes either the legacy single-window form or the
// weekly policy form. Presence of any policy-only key selects the
// policy form; a document that also carries legacy start/end is
// rejected. Message-only documents stay legacy to preserve old
// semantics (empty window = off, message kept for round-trip).
func (p *ProviderPeakHours) UnmarshalJSON(data []byte) error {
	trimmed := bytes.TrimSpace(data)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		p.Legacy = nil
		p.Policy = nil
		return nil
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(trimmed, &fields); err != nil {
		return err
	}
	policyForm := false
	for _, key := range peakHoursPolicyKeys {
		if _, ok := fields[key]; ok {
			policyForm = true
			break
		}
	}
	if !policyForm {
		var legacy PeakHoursWindow
		decoder := json.NewDecoder(bytes.NewReader(trimmed))
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&legacy); err != nil {
			return err
		}
		p.Legacy = &legacy
		p.Policy = nil
		return nil
	}
	if _, ok := fields["start"]; ok {
		return fmt.Errorf("peak_hours mixes legacy start/end with the weekly windows form")
	}
	if _, ok := fields["end"]; ok {
		return fmt.Errorf("peak_hours mixes legacy start/end with the weekly windows form")
	}
	var policy PeakHoursPolicy
	decoder := json.NewDecoder(bytes.NewReader(trimmed))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&policy); err != nil {
		return err
	}
	if _, windowsPresent := fields["windows"]; !windowsPresent && policy.Timezone != "" && policy.PausedUntil == "" {
		return fmt.Errorf("peak_hours timezone requires an explicit windows field")
	}
	p.Legacy = nil
	p.Policy = &policy
	return nil
}

// MarshalJSON emits whichever leg is present; both nil marshals as
// JSON null. The legacy shape keeps its existing field tags so
// existing files do not churn.
func (p ProviderPeakHours) MarshalJSON() ([]byte, error) {
	switch {
	case p.Legacy != nil:
		return json.Marshal(p.Legacy)
	case p.Policy != nil:
		return json.Marshal(p.Policy)
	default:
		return []byte("null"), nil
	}
}

// IsOff reports whether the value represents "feature off" as read
// from configuration. For the legacy form that is the old "empty
// window" reading; for the policy form it is simply an empty windows
// list — a paused-only or message/timezone-only policy still counts as
// off here, because IsOff describes the *window* feature for merge and
// edit normalization, not whether requests are refused.
func (p *ProviderPeakHours) IsOff() bool {
	if p == nil {
		return true
	}
	switch {
	case p.Legacy != nil && p.Policy == nil:
		return p.Legacy.Start == "" && p.Legacy.End == ""
	case p.Policy != nil && p.Legacy == nil:
		return len(p.Policy.Windows) == 0
	default:
		return true
	}
}

// Validate dispatches to whichever leg is present; a nil receiver is
// valid (feature off).
func (p *ProviderPeakHours) Validate() error {
	if p == nil {
		return nil
	}
	if p.Legacy != nil {
		return p.Legacy.Validate()
	}
	if p.Policy != nil {
		return p.Policy.Validate()
	}
	return nil
}

// InPeak reports whether now is inside the peak window under the
// present form. The legacy leg keeps its machine-local clock
// semantics unchanged.
func (p *ProviderPeakHours) InPeak(now time.Time) bool {
	if p == nil {
		return false
	}
	if p.Legacy != nil {
		return p.Legacy.InPeakHours(now)
	}
	if p.Policy != nil {
		return p.Policy.InPeak(now)
	}
	return false
}

// EffectiveMessage returns the present leg's custom refusal note, or
// the empty string when off.
func (p *ProviderPeakHours) EffectiveMessage() string {
	if p == nil {
		return ""
	}
	if p.Legacy != nil {
		return p.Legacy.Message
	}
	if p.Policy != nil {
		return p.Policy.Message
	}
	return ""
}

// Status evaluates the full peak-hours state at now for whichever leg
// is present. The legacy leg reports a machine-local, single-window
// status with no timezone label.
func (p *ProviderPeakHours) Status(now time.Time) PeakHoursStatus {
	if p == nil {
		return PeakHoursStatus{CheckedAt: now}
	}
	if p.Legacy != nil {
		w := p.Legacy
		inPeak := w.InPeakHours(now)
		status := PeakHoursStatus{
			CheckedAt: now,
			InPeak:    inPeak,
			Blocked:   inPeak,
		}
		if inPeak {
			status.ReopensAt = w.EndTimeToday(now)
		}
		return status
	}
	if p.Policy != nil {
		status := p.Policy.Evaluate(now)
		status.CheckedAt = now
		return status
	}
	return PeakHoursStatus{CheckedAt: now}
}

// AsPolicy returns the weekly-policy view of the value: the Policy
// leg verbatim, or the legacy window converted into one whole-week
// window on every day in the machine-local clock. Legacy start/end
// carry over unchanged (legacy overnight start > end keeps its wrap
// semantics; legacy clocks never exceed 23:59). Returns nil when the
// value represents off — an empty legacy window or one with equal
// start and end (both normalize to off on write) — so callers keep
// composing a fresh policy in that case instead of writing a
// zero-length window. Policy-form values round-trip without loss.
func (p *ProviderPeakHours) AsPolicy() *PeakHoursPolicy {
	if p == nil {
		return nil
	}
	if p.Policy != nil {
		pol := *p.Policy
		pol.Windows = append([]PeakHoursWindowSpec(nil), p.Policy.Windows...)
		return &pol
	}
	if p.Legacy == nil {
		return nil
	}
	w := p.Legacy
	if (w.Start == "" && w.End == "") || w.Start == w.End {
		return nil
	}
	return &PeakHoursPolicy{Windows: []PeakHoursWindowSpec{{Days: "", Start: w.Start, End: w.End}}, Message: w.Message}
}

// NormalizeForWrite collapses values that mean "off" to nil so callers
// can remove the field instead of writing an empty shell. Reading
// still accepts all of these forms; only writes are normalized. Legacy
// windows with equal start and end also normalize to off.
func (p *ProviderPeakHours) NormalizeForWrite() *ProviderPeakHours {
	if p == nil {
		return nil
	}
	if p.Legacy != nil && p.Policy == nil {
		if p.Legacy.Start == "" && p.Legacy.End == "" {
			return nil
		}
		if p.Legacy.Start == p.Legacy.End {
			return nil
		}
		return p
	}
	if p.Policy != nil && p.Legacy == nil {
		if p.Policy.IsZero() {
			return nil
		}
		return p
	}
	return p
}
