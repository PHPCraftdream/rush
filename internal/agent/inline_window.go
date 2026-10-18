// Package agent inline-window constant and test seam, moved verbatim from
// work_ledger_inline.go (R12-H1): the A14 inline-window constant and test
// seam, kept here because the batch runtime and the app test harness
// outlive the H4 work_ledger deletion.
package agent

import "time"

// inlineWindowSeconds is the fixed, non-configurable window (design doc
// "N and scope": a constant, not a setting). The compile-time assertion
// below keeps it strictly under the explicit-timeout floor: a timeout{} of
// timeoutSecondsFloor (5s) or more can then never fire inside a 3s window,
// so the inline decision never has to consider one.
const inlineWindowSeconds = 3

const inlineWindow = inlineWindowSeconds * time.Second

// inlineWindowDisabler is the test seam the app package reaches through:
// only the real coordinator's ledger carries the window; test fakes that
// implement Coordinator another way simply return false.
type inlineWindowDisabler interface {
	disableInlineWindowForTest()
}

// SetInlineWindowForTest zeroes (on=false) or restores (on=true) the
// coordinator's inline window -- the A14 test seam: 0 = the pre-A14
// "started" flow, byte for byte. package app's e2e harnesses disable it
// for scenarios whose scripts match the literal "Async ... started" text or
// count pulled notices; the window-on e2e re-enables it explicitly.
// Returns false for a coordinator with no window to set (test fakes).
func SetInlineWindowForTest(c Coordinator, on bool) bool {
	if s, ok := c.(inlineWindowSetter); ok {
		s.setInlineWindowForTest(on)
		return true
	}
	return false
}

type inlineWindowSetter interface {
	setInlineWindowForTest(on bool)
}

func (c *coordinator) setInlineWindowForTest(on bool) {
	if c != nil && c.asyncJobs != nil {
		if on {
			c.asyncJobs.inlineWindow = inlineWindow
		} else {
			c.asyncJobs.inlineWindow = 0
		}
	}
}
