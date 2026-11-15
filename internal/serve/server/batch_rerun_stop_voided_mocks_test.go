package server

import (
	"context"

	"github.com/PHPCraftdream/rush/internal/session"
)

// The agent.Coordinator contract carries BatchRerunStopVoided; these test
// doubles never reach the post-rerun stop, so the method is a no-op.

func (*attachmentsLifecycleCoordinator) BatchRerunStopVoided(context.Context, string, []session.BatchRerunVoidedNode) {
}

func (*blockingCoordinator) BatchRerunStopVoided(context.Context, string, []session.BatchRerunVoidedNode) {
}

func (*cancellableHoldCoordinator) BatchRerunStopVoided(context.Context, string, []session.BatchRerunVoidedNode) {
}

func (*compactingErrorCoordinator) BatchRerunStopVoided(context.Context, string, []session.BatchRerunVoidedNode) {
}

func (*compactingSuccessCoordinator) BatchRerunStopVoided(context.Context, string, []session.BatchRerunVoidedNode) {
}

func (*concurrentWriterFailureCoordinator) BatchRerunStopVoided(context.Context, string, []session.BatchRerunVoidedNode) {
}

func (*earlyReturnCoordinator) BatchRerunStopVoided(context.Context, string, []session.BatchRerunVoidedNode) {
}

func (*effortCaptureCoordinator) BatchRerunStopVoided(context.Context, string, []session.BatchRerunVoidedNode) {
}

func (*fakeAlwaysBusyCoordinator) BatchRerunStopVoided(context.Context, string, []session.BatchRerunVoidedNode) {
}

func (*fakeIsSessionBusyCoordinator) BatchRerunStopVoided(context.Context, string, []session.BatchRerunVoidedNode) {
}

func (*handoffOnlyErrorCoordinator) BatchRerunStopVoided(context.Context, string, []session.BatchRerunVoidedNode) {
}

func (*mailboxLikeCoordinator) BatchRerunStopVoided(context.Context, string, []session.BatchRerunVoidedNode) {
}

func (*mockAgentCoordinator) BatchRerunStopVoided(context.Context, string, []session.BatchRerunVoidedNode) {
}

func (*orderedHoldCoordinator) BatchRerunStopVoided(context.Context, string, []session.BatchRerunVoidedNode) {
}

func (*postHandoffPanicCoordinator) BatchRerunStopVoided(context.Context, string, []session.BatchRerunVoidedNode) {
}

func (*promptThenErrorCoordinator) BatchRerunStopVoided(context.Context, string, []session.BatchRerunVoidedNode) {
}
