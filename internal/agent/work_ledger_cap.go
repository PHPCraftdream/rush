// Cap admission for workLedger.Start: the per-session async job limit counts
// only non-terminal jobs (docs/plans/2026-10-02-async-job-cap.md, ASYNC-12 in
// docs/async-invariants.md). A terminal job still in the owner's map --
// delivered-on-ack, waiting for its "started" ack or an inline Tx2 -- does not
// hold a slot, so a lost ack can never block a session's bash jobs forever.
// No separate counter: the predicate is a scan of the owner's ≤ ~55-entry map
// under l.mu, so no terminal transition has to maintain anything.
package agent

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/PHPCraftdream/rush/internal/shell"
)

// asyncCapListingMax bounds how many running jobs the refusal quotes: enough
// to see what is holding the slots, short enough to keep the tool response
// readable for the model.
const asyncCapListingMax = 10

// asyncCapJob is one running job quoted in the cap refusal's listing.
type asyncCapJob struct {
	ID        string
	Tool      string
	Input     string
	StartedAt time.Time
}

// asyncCapError is the typed refusal of a Start that would exceed the
// session's cap of non-terminal async jobs. It carries a snapshot of the
// running jobs (timers first, then oldest, capped at asyncCapListingMax) so
// the model can free slots with job_kill instead of waiting. asyncTool.Run
// turns it into an error tool response tagged with async_cap metadata.
type asyncCapError struct {
	Running int
	Limit   int
	Timers  int
	Jobs    []asyncCapJob
}

// runningJobsLocked counts the owner's jobs that are NOT terminal (ASYNC-12):
// running ones, including unacknowledged, expired wake_only and parked
// delegations. A terminal entry still in the map -- waiting for its "started"
// ack, or for an inline Tx2 -- holds no slot. Caller must hold l.mu.
func runningJobsLocked(s *sessionJobs) int {
	if s == nil {
		return 0
	}
	running := 0
	for _, j := range s.jobs {
		if !j.state.terminal() {
			running++
		}
	}
	return running
}

// capSnapshotLocked copies the owner's non-terminal jobs into the listing
// shape asyncCapError quotes. Caller must hold l.mu (it reads s.jobs and each
// job's fields); everything after the snapshot -- timer classification and
// sorting -- runs outside the lock, in newAsyncCapError.
func (l *workLedger) capSnapshotLocked(s *sessionJobs) []asyncCapJob {
	if s == nil {
		return nil
	}
	jobs := make([]asyncCapJob, 0, len(s.jobs))
	for _, j := range s.jobs {
		if j.state.terminal() {
			continue
		}
		jobs = append(jobs, asyncCapJob{
			ID: j.toolCallID, Tool: j.toolName,
			Input: j.input, StartedAt: j.startedAt,
		})
	}
	return jobs
}

// newAsyncCapError builds the refusal from a snapshot taken under l.mu;
// timer classification (shell.IsNoOpCommand parse) and sorting run outside
// the lock. Each job's input is parsed exactly once: the flag is carried
// alongside the job through the sort, which orders wait-only timers first and
// then oldest first, and only the listing's head survives.
func newAsyncCapError(running, limit int, jobs []asyncCapJob) *asyncCapError {
	e := &asyncCapError{Running: running, Limit: limit}
	type timerTag struct {
		job   asyncCapJob
		timer bool
	}
	tagged := make([]timerTag, len(jobs))
	for i, j := range jobs {
		timer := shell.IsNoOpCommand(j.Input)
		tagged[i] = timerTag{job: j, timer: timer}
		if timer {
			e.Timers++
		}
	}
	slices.SortStableFunc(tagged, func(a, b timerTag) int {
		if a.timer != b.timer {
			if a.timer {
				return -1
			}
			return 1
		}
		if c := a.job.StartedAt.Compare(b.job.StartedAt); c != 0 {
			return c
		}
		// A map walk has no order: break exact ties on the id so the listing
		// (and the tests reading its head) stay deterministic.
		if a.job.ID < b.job.ID {
			return -1
		}
		if a.job.ID > b.job.ID {
			return 1
		}
		return 0
	})
	if n := min(len(tagged), asyncCapListingMax); n > 0 {
		e.Jobs = make([]asyncCapJob, n)
		for i := range e.Jobs {
			e.Jobs[i] = tagged[i].job
		}
	}
	return e
}

// asyncCapMetadata is the JSON shape asyncCapError.Metadata emits: the guard
// reading the step's tool results keys off exactly these names.
type asyncCapMetadata struct {
	AsyncCap struct {
		Running    int `json:"running"`
		Limit      int `json:"limit"`
		WaitTimers int `json:"wait_timers"`
	} `json:"async_cap"`
}

// Metadata is the tool-response metadata for this refusal: a compact
// {"async_cap":{"running":N,"limit":L,"wait_timers":T}}, deliberately without
// a claim_id -- the refusal never reached store.Claim, so there is no row.
func (e *asyncCapError) Metadata() string {
	var m asyncCapMetadata
	m.AsyncCap.Running = e.Running
	m.AsyncCap.Limit = e.Limit
	m.AsyncCap.WaitTimers = e.Timers
	out, err := json.Marshal(m)
	if err != nil {
		// A struct of three ints has no failure mode; fall back to the
		// hand-written shape rather than dropping the tag.
		return fmt.Sprintf(`{"async_cap":{"running":%d,"limit":%d,"wait_timers":%d}}`, e.Running, e.Limit, e.Timers)
	}
	return string(out)
}

// Error answers the question the model asked through ask_question during the
// wa14/wskills incidents: nothing is queued, every slot is busy right now, the
// completion of each running job wakes it as a session message, and the way
// forward is freeing slots (job_kill, timers first) or ending the turn --
// never waiting, polling or escalating the limit to the operator.
func (e *asyncCapError) Error() string {
	var b strings.Builder
	fmt.Fprintf(&b, "Refused: this job was not started — this session already has %d running\n"+
		"async jobs (limit %d). Nothing is queued: all %d are running now.\n",
		e.Running, e.Limit, e.Running)
	if e.Timers > 0 {
		fmt.Fprintf(&b, "%d of them are wait-only timers (sleep/echo).\n", e.Timers)
	}
	b.WriteString("You do not need to wait, poll or ask anyone: each running job reports its\n")
	b.WriteString("result as a session message and wakes you when it finishes.\n")
	b.WriteString("Next step, one of:\n")
	b.WriteString("- free slots: job_kill the jobs you no longer need, timers first\n")
	b.WriteString("  (a delegation: stop_agent);\n")
	b.WriteString("- otherwise end your turn now: a short status and NO tool call.\n")
	b.WriteString("Do not retry this call, do not start sleep/echo timers, and do not call\n")
	b.WriteString("ask_question about this limit: it is not a decision for the user or the\n")
	b.WriteString("orchestrator.\n")
	fmt.Fprintf(&b, "Running (timers first, then oldest; %d of %d):\n", len(e.Jobs), e.Running)
	for _, j := range e.Jobs {
		fmt.Fprintf(&b, "- %s  %s  %s  `%s`\n", j.ID, j.Tool, asyncCapElapsed(time.Since(j.StartedAt)), j.Input)
	}
	return b.String()
}

// asyncCapElapsed formats a job's runtime for the listing, truncated to
// seconds: "45s", "7m12s", "1h3m". Clamped at zero so a clock that moved
// backwards never quotes a negative age.
func asyncCapElapsed(d time.Duration) string {
	if d < 0 {
		d = 0
	}
	d = d.Truncate(time.Second)
	if d < time.Minute {
		return fmt.Sprintf("%ds", int64(d/time.Second))
	}
	if d < time.Hour {
		return fmt.Sprintf("%dm%02ds", int64(d/time.Minute), int64(d%time.Minute/time.Second))
	}
	return fmt.Sprintf("%dh%02dm%02ds", int64(d/time.Hour), int64(d%time.Hour/time.Minute), int64(d%time.Minute/time.Second))
}
