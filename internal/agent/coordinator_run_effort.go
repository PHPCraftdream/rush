package agent

import "github.com/PHPCraftdream/rush/internal/session"

// inheritSessionEffort fills an override slot's empty reasoning effort from
// the session's durable effort, but only when the slot names the session's
// own model: the effort was recorded for that model, and applyModelOverrides
// deliberately clears the effort whenever the model differs.
func inheritSessionEffort(smart, fast *ModelOverride, sess session.Session) {
	if smart != nil && smart.ReasoningEffort == "" && sess.SmartModelReasoningEffort != "" &&
		smart.Model == sess.SmartModelID && smart.Provider == sess.SmartModelProvider {
		smart.ReasoningEffort = sess.SmartModelReasoningEffort
	}
	if fast != nil && fast.ReasoningEffort == "" && sess.FastModelReasoningEffort != "" &&
		fast.Model == sess.FastModelID && fast.Provider == sess.FastModelProvider {
		fast.ReasoningEffort = sess.FastModelReasoningEffort
	}
}
