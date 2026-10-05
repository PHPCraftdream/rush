package tools

import "charm.land/fantasy"

// bgShellStop is the outcome of consulting the resolver's optional
// BGShellStopper for a shell id.
type bgShellStop struct {
	// handled: job_kill is finished (the row was already terminal); resp is
	// the answer and the shell manager must not be touched.
	handled bool
	resp    fantasy.ToolResponse
	// stopped: the row was moved to cancelled; the shell must now be killed
	// and the answer is text, fused onto the row through claimID.
	stopped bool
	text    string
	claimID string
}

// tryStopBGShellRow asks the resolver to stop shellID's durable bg_shell row.
// The zero value means "no such row (or no stopper): take the older path".
func tryStopBGShellRow(resolver JobShellResolver, sessionID, shellID string) bgShellStop {
	stopper, ok := resolver.(BGShellStopper)
	if !ok {
		return bgShellStop{}
	}
	text, claimID, verdict := stopper.StopBackgroundShellRow(sessionID, shellID)
	switch verdict {
	case JobStopStopped:
		return bgShellStop{stopped: true, text: text, claimID: claimID}
	case JobStopAlreadyTerminal:
		return bgShellStop{handled: true, resp: fantasy.NewTextResponse(text)}
	}
	return bgShellStop{}
}
