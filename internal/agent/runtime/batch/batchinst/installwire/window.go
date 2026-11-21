package installwire

import "time"

// InlineWindowSeconds is fixed below the explicit timeout floor (5s).
const (
	InlineWindowSeconds = 3
	InlineWindow        = InlineWindowSeconds * time.Second
)

// inlineWindowDisabler is the former one-way test port. The setter supports
// restoring the default window as well.
type inlineWindowDisabler interface {
	disableInlineWindowForTest()
}

type windowToggle func(bool)

// SetInlineWindowForTest sets an available coordinator window. A nil setter
// identifies a fake without the seam.
func SetInlineWindowForTest(set windowToggle, on bool) bool {
	if set == nil {
		return false
	}
	set(on)
	return true
}
