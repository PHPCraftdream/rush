package sessstate

// One STATUS vocabulary for the session surfaces: `sessions list`'s STATUS
// column, `sessions why`'s status line and `sessions status`'s phase mapping
// all render the shared session-activity classifier's verdict through this
// file, so the commands cannot drift apart on how a recorded ended_reason is
// presented (task FIX-STATUS (a)/(c): one source of truth).

import (
	"github.com/PHPCraftdream/rush/internal/serve/cmd/sessions/sesskit"
	sessionfacts "github.com/PHPCraftdream/rush/internal/session/state/sessionfacts"
)

const endedReasonPeakHours = sesskit.EndedReasonPeakHours

func sessionStatusLabel(v sessionfacts.ActivityVerdict, recordedEnd string) string {
	return sesskit.SessionStatusLabel(v, recordedEnd)
}

func annotateRecordedEnd(status, recordedEnd string) string {
	return sesskit.AnnotateRecordedEnd(status, recordedEnd)
}

func endedStatusLabel(reason string) string {
	return sesskit.EndedStatusLabel(reason)
}

func knownEndedReason(reason string) bool {
	return sesskit.KnownEndedReason(reason)
}
