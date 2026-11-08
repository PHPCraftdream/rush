// AsPolicy conversion tests: the nil and off cases, the owned-copy
// guarantee for the policy leg, the legacy-to-weekly conversion (whole
// week window, message carry, wrap semantics), and the Evaluate-backed
// check that an overnight legacy window keeps its wrap behaviour.
package config

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestProviderPeakHoursAsPolicy(t *testing.T) {
	t.Run("nil receiver returns nil", func(t *testing.T) {
		var p *ProviderPeakHours
		assert.Nil(t, p.AsPolicy())
	})

	t.Run("both legs nil returns nil", func(t *testing.T) {
		p := &ProviderPeakHours{}
		assert.Nil(t, p.AsPolicy())
	})

	t.Run("policy leg returns owned copy", func(t *testing.T) {
		orig := &PeakHoursPolicy{
			Timezone: "UTC+02:00",
			Windows:  []PeakHoursWindowSpec{{Days: "mon-fri", Start: "09:00", End: "18:00"}},
			Message:  "orig-msg",
		}
		p := &ProviderPeakHours{Policy: orig}
		got := p.AsPolicy()
		require.NotNil(t, got)
		require.Equal(t, orig, got, "the copy must equal the original policy leg")

		// Element write-through FIRST: got.Windows has len == cap == 1
		// here, so a shared backing array would corrupt the original
		// immediately — an append would silently reallocate and hide the
		// sharing.
		got.Windows[0].Start = "00:00"
		require.Equal(t, "09:00", orig.Windows[0].Start, "mutating an existing element must not write through to the original")
		got.Windows = append(got.Windows, PeakHoursWindowSpec{Days: "sat", Start: "10:00", End: "12:00"})
		got.Message = "mutated"
		require.Len(t, orig.Windows, 1, "appending to the copy must not change the original windows")
		assert.Equal(t, "orig-msg", orig.Message, "mutating the copy must not change the original message")
	})

	t.Run("legacy converts to one whole-week window", func(t *testing.T) {
		p := &ProviderPeakHours{Legacy: &PeakHoursWindow{Start: "09:00", End: "18:00"}}
		want := &PeakHoursPolicy{Windows: []PeakHoursWindowSpec{{Days: "", Start: "09:00", End: "18:00"}}}
		require.Equal(t, want, p.AsPolicy())
	})

	t.Run("legacy message is carried over", func(t *testing.T) {
		p := &ProviderPeakHours{Legacy: &PeakHoursWindow{Start: "09:00", End: "18:00", Message: "carry-me"}}
		got := p.AsPolicy()
		require.NotNil(t, got)
		assert.Equal(t, "carry-me", got.Message)
	})

	t.Run("legacy overnight keeps start and end as stored", func(t *testing.T) {
		p := &ProviderPeakHours{Legacy: &PeakHoursWindow{Start: "22:00", End: "06:00"}}
		got := p.AsPolicy()
		require.NotNil(t, got)
		require.Len(t, got.Windows, 1)
		assert.Equal(t, "22:00", got.Windows[0].Start, "the overnight start must be preserved verbatim")
		assert.Equal(t, "06:00", got.Windows[0].End, "the overnight end must be preserved verbatim")

		// The wrap semantics live in the weekly evaluator: build the
		// converted policy and probe two fixed instants in a known
		// fixed-offset zone.
		pol := &PeakHoursPolicy{
			Timezone: "UTC+02:00",
			Windows:  got.Windows,
		}
		fz := time.FixedZone("test", 2*3600)
		friLate := time.Date(2024, 1, 5, 23, 0, 0, 0, fz)
		require.Equal(t, time.Friday, friLate.Weekday(), "guard: the probe instant must be a Friday")
		st := pol.Evaluate(friLate)
		assert.True(t, st.InPeak, "Friday 23:00 local falls in the 22:00-06:00 overnight window")

		satMorning := time.Date(2024, 1, 6, 7, 0, 0, 0, fz)
		require.Equal(t, time.Saturday, satMorning.Weekday(), "guard: the probe instant must be a Saturday")
		st = pol.Evaluate(satMorning)
		assert.False(t, st.InPeak, "Saturday 07:00 local is past the overnight window's morning end")
	})

	t.Run("legacy empty window returns nil", func(t *testing.T) {
		p := &ProviderPeakHours{Legacy: &PeakHoursWindow{}}
		assert.Nil(t, p.AsPolicy())
	})

	t.Run("legacy equal start and end returns nil", func(t *testing.T) {
		p := &ProviderPeakHours{Legacy: &PeakHoursWindow{Start: "09:00", End: "09:00"}}
		assert.Nil(t, p.AsPolicy())
	})
}
