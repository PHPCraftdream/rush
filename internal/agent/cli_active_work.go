package agent

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/PHPCraftdream/rush/internal/message"
)

// IsHardQuotaLimit identifies a provider's hard usage wall, not a retryable 429.
func IsHardQuotaLimit(err error) bool {
	return classifyHardProviderLimit(err, "", time.Now()) == providerLimitHard
}

// CLIActiveWorkSource reads real work without pulling notices or admitting turns.
type CLIActiveWorkSource interface {
	CLIActiveWork(context.Context, string) (bool, error)
}

// CLIActiveWork excludes schedules, debt, and question-paused delegations, but
// includes those children's jobs and completion-persistence holds.
func (c *coordinator) CLIActiveWork(ctx context.Context, root string) (bool, error) {
	if c.asyncJobs == nil || c.asyncJobs.store == nil {
		return false, errors.New("active work store unavailable")
	}
	c.asyncJobs.store.RecoverOwnerScope(ctx, root, c.messages)
	owners := map[string]bool{root: true}
	tree, treeIncomplete := c.asyncJobs.store.JobsInTree(ctx, root)
	if treeIncomplete {
		return false, errors.New("work tree read incomplete")
	}
	for _, row := range tree {
		owners[row.OwnerSessionID] = true
		if row.ChildSessionID.Valid {
			owners[row.ChildSessionID.String] = true
		}
	}
	active := false
	for owner := range owners {
		if c.background != nil && (c.background.ActiveOwned(owner) > 0 || c.background.PendingCompletionsOwned(owner) > 0) {
			active = true
		}
	}
	jobs, incomplete := c.asyncJobs.store.LiveJobs(ctx, root)
	if incomplete {
		return false, errors.New("active work read incomplete")
	}
	for _, job := range jobs {
		owners[job.SessionID] = true
		if job.ChildSessionID == "" {
			active = true
			continue
		}
		owners[job.ChildSessionID] = true
		// Read durable finish metadata even if the question's delegation is no
		// longer in this process's ledger. A busy child is never paused.
		ag := c.agentFor(job.ChildSessionID)
		if ag != nil && ag.IsSessionBusy(job.ChildSessionID) {
			active = true
			continue
		}
		msgs, err := c.messages.List(ctx, job.ChildSessionID)
		if err != nil {
			return false, err
		}
		paused := false
		for i := len(msgs) - 1; i >= 0; i-- {
			if msgs[i].Role != message.Assistant || !msgs[i].IsFinished() {
				continue
			}
			_, paused = subAgentQuestionFromFinish(job.ChildSessionID, msgs[i].FinishPart())
			break
		}
		if !paused {
			active = true
		} else {
			// A held question is announced asynchronously by the existing ledger
			// recheck. Do not release the CLI before that parent notice commits.
			c.asyncJobs.recheckChild(job.ChildSessionID)
			if c.asyncJobs.heldQuestionJob(job.ChildSessionID) != nil {
				notices, err := c.asyncJobs.store.ListSessionNotices(ctx, job.SessionID)
				if err != nil {
					return false, err
				}
				persisted := false
				for _, notice := range notices {
					if notice.Kind == "child_question" && strings.Contains(notice.Text, "SUB-AGENT QUESTION (session "+job.ChildSessionID+")") && strings.Contains(notice.Text, "job "+job.ToolCallID+" stays open") {
						persisted = true
						break
					}
				}
				if !persisted {
					active = true
				}
			}
		}
	}
	return active, nil
}

// IsAwaitingAnswerFinish identifies durable question-stop metadata.
func IsAwaitingAnswerFinish(fp *message.Finish) bool {
	_, ok := subAgentQuestionBlockFromFinish(fp)
	return ok
}

var _ CLIActiveWorkSource = (*coordinator)(nil)
