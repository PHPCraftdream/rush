// Package agent inline-window constant and test seam: the A14 inline-window
// constant, kept here because the batch runtime and the app test harness
// consume it. The seam is REAL: a coordinator carries a disable flag the
// batch tool arming (batch_install_tools.go) resolves through its binding.
package agent

import "github.com/PHPCraftdream/rush/internal/agent/runtime/batch/batchinst/installwire"

const (
	inlineWindowSeconds = installwire.InlineWindowSeconds
	inlineWindow        = installwire.InlineWindow
)

// SetInlineWindowForTest zeroes (on=false) or restores (on=true) the
// coordinator's inline window -- the A14 test seam: 0 = the pre-A14
// "started" flow, byte for byte. package app's e2e harnesses disable it
// for scenarios whose scripts match the literal "Async ... started" text or
// count pulled notices; the window-on e2e re-enables it explicitly.
// Returns false for a coordinator with no window to set (test fakes).
func SetInlineWindowForTest(c Coordinator, on bool) bool {
	var set func(bool)
	if s, ok := c.(inlineWindowSetter); ok {
		set = s.setInlineWindowForTest
	}
	return installwire.SetInlineWindowForTest(set, on)
}

type inlineWindowSetter interface {
	setInlineWindowForTest(on bool)
}

// setInlineWindowForTest zeroes (on=false) or restores (on=true) THIS
// coordinator's inline window: the flag is consulted by the batch bash
// arming (inlineWindowDisabledFor), which arms bash detached -- the pre-A14
// "started" flow -- instead of with an escape window.
func (c *coordinator) setInlineWindowForTest(on bool) {
	c.inlineWindowDisabled.Store(!on)
}

// inlineWindowDisabledFor reports whether the coordinator bound to bind has
// its inline window disabled (the SetInlineWindowForTest seam). A build with
// no coordinator in scope (bind nil or unbound fixture) keeps the window.
func inlineWindowDisabledFor(bind *batchCoordinatorBinding) bool {
	return bind != nil && bind.coord != nil && bind.coord.inlineWindowDisabled.Load()
}
