package agent

import (
	"context"
	"database/sql"

	"github.com/PHPCraftdream/rush/internal/agent/runtime/batch/batchlife"
)

type (
	BatchHeldQuestionRoute  = batchlife.BatchHeldQuestionRoute
	BatchHeldQuestionSource = batchlife.BatchHeldQuestionSource
)

var ErrBatchAwaitHeldQuestion = batchlife.ErrBatchAwaitHeldQuestion

func batchQuestionMarkerSequence(text string) int64 { return batchlife.QuestionMarkerSequence(text) }

func batchActionableHeldQuestionRoutes(ctx context.Context, writer *sql.DB, owner string) ([]BatchHeldQuestionRoute, error) {
	return batchlife.ActionableHeldQuestionRoutes(ctx, writer, owner, batchlife.HeldQuestionRouteDeps{
		ParseMarker:      parseBatchQuestionMarker,
		MarkerChild:      batchQuestionMarkerChild,
		NodeStateLive:    batchNodeStateLive,
		FactsUnavailable: errBatchFactsUnavailable,
	})
}

func batchHeldQuestionAwaitError(routes []BatchHeldQuestionRoute) error {
	return batchlife.HeldQuestionAwaitError(routes)
}

func batchHeldQuestionHintText(routes []BatchHeldQuestionRoute) string {
	return batchlife.HeldQuestionHintText(routes)
}

// heldQuestionAwaitRefusal is the root AwaitTasks gate: a non-empty route
// set (or an unreadable read) refuses the await before any sleep/timer is
// armed. A coordinator whose facts lack the capability never gates.
func (c *coordinator) heldQuestionAwaitRefusal(ctx context.Context, owner string) error {
	return newBatchCutoverGlueComponent(c).heldQuestionAwaitRefusal(ctx, owner)
}

// batchHeldQuestionResumeRefusal запрещает новый Run при held-вопросе.
// Ошибка чтения не доказывает отсутствие hold; legacy без capability не меняем.
func (c *coordinator) batchHeldQuestionResumeRefusal(ctx context.Context, owner, child string) string {
	return newBatchCutoverGlueComponent(c).batchHeldQuestionResumeRefusal(ctx, owner, child)
}

// batchHeldQuestionResumeHint is the resume-error suffix for
// runSubAgent's refusal branches: "" when the capability is absent, the
// read fails, or no route exists.
func (c *coordinator) batchHeldQuestionResumeHint(ctx context.Context, owner string) string {
	return newBatchCutoverGlueComponent(c).batchHeldQuestionResumeHint(ctx, owner)
}
