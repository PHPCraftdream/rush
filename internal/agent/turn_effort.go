package agent

import (
	"context"

	"github.com/PHPCraftdream/rush/internal/session"
)

// turnSmartReasoningEffort is the effort of THIS call's request, which is
// what a persisted message row must record. The per-call model override
// wins over the session row: the reviewer pass resolves its own slot's
// effort into a non-persisted ModelOverride (buildReviewerPassTurn's
// WithModelOverrides), so the session row still carries the executor's
// effort and recording it would label the reviewer's rows wrong. For every
// other caller -- Run, an ordinary RunWithOverrides -- the override, when
// present, resolves to the same slot the session row names, so the two
// agree and this changes nothing.
func turnSmartReasoningEffort(ctx context.Context, sess session.Session) string {
	if smart, _ := modelOverridesFrom(ctx); smart != nil && smart.ReasoningEffort != "" {
		return smart.ReasoningEffort
	}
	return sess.SmartModelReasoningEffort
}
