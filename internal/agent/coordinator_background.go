// Background-job completion notices, Phase-4 autonomous idle-resume, and
// busy-state queries: bodies moved to background_component.go (the
// backgroundComponent host pattern); this file keeps the thin coordinator
// delegates.
package agent

import (
	"context"

	"github.com/PHPCraftdream/rush/internal/kit/sys/shell/shellbg"
)

func (c *coordinator) notifyAsyncCompletion(completion AsyncCompletion) {
	newBackgroundComponent(c).notifyAsyncCompletion(completion)
}

func (c *coordinator) notifyBackgroundJobDone(sessionID string, sh *shellbg.BackgroundShell) {
	newBackgroundComponent(c).notifyBackgroundJobDone(sessionID, sh)
}

func (c *coordinator) IsBusy() bool {
	return newBackgroundComponent(c).isBusy()
}

func (c *coordinator) IsSessionBusy(sessionID string) bool {
	return newBackgroundComponent(c).isSessionBusy(sessionID)
}

func (c *coordinator) delegatedChildDriven(ctx context.Context, sessionID string) (bool, error) {
	return newBackgroundComponent(c).delegatedChildDriven(ctx, sessionID)
}

func (c *coordinator) notifyOnBackgroundJobDone() bool {
	return newBackgroundComponent(c).notifyOnBackgroundJobDone()
}

func (c *coordinator) persistBGShellCompletion(sessionID, shellID, summary string) (claimed bool) {
	return newBackgroundComponent(c).persistBGShellCompletion(sessionID, shellID, summary)
}

func (c *coordinator) pruneBGShellOverCap(sessionID string) {
	newBackgroundComponent(c).pruneBGShellOverCap(sessionID)
}

func (c *coordinator) bgShellOverCapCount(sessionID string) int {
	return newBackgroundComponent(c).bgShellOverCapCount(sessionID)
}
