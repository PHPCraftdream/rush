package agent

import "fmt"

// WatchdogResumeGuidance returns the orchestrator-facing resume
// instruction for a timeout-caused stop (a tool/sub-agent that ran past
// toolMaxDuration, or a turn that hit its --timeout-hard-cap) — sibling
// to AwaitingAnswerGuidance (question_stop.go) and PeakHoursGuidance
// (peak_hours_stop.go): same "this is not a crash, here is the exact
// command" contract, so an orchestrating agent never has to reconstruct
// --session/--timeout syntax from memory under the pressure of a failed
// run.
//
// timeoutFlag is the flag name to suggest raising — "--timeout" for a
// tool-timeout or run-timeout stop, "--timeout-hard-cap" for a hard-cap
// stop — so the suggested command actually targets the limit that fired.
func WatchdogResumeGuidance(sessionID, timeoutFlag string) string {
	return fmt.Sprintf(
		"This is not a crash — rush is intentionally stopping this turn "+
			"because it ran too long. rush is exiting now; it will not retry "+
			"on its own.\n\n"+
			"If an orchestrating agent is driving this session: resume with a "+
			"larger timeout:\n\n"+
			"  rush run --session %s %s <larger-value> \"continue\"\n\n"+
			"This is a normal continuation, not a retry from scratch — the "+
			"session's context is intact.",
		sessionID, timeoutFlag,
	)
}
