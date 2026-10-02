package cmd

// The anomaly rules themselves: auditDataDir calls auditFindings with the
// open read-only handle, the schema it probed, the session ids it selected
// and the --since window, so adding a rule is a change here and nothing else.
//
// Shape of the scan (the readers live in sessions_audit_rules.go): one query
// reads the selected sessions' messages and one reads their unfinished async
// jobs, and every row is folded into the per-session counters of auditTally.
// A row that cannot be parsed is skipped and counted rather than reported — an
// audit of a database with a few broken rows still has to describe the healthy
// ones around them.

import (
	"context"
	"database/sql"
	"sort"
	"strings"
	"time"
)

// auditFinding is one suspicion tripped by one session: which rule fired,
// the evidence, and how many times it happened.
type auditFinding struct {
	Rule      string `json:"rule"`
	SessionID string `json:"session_id"`
	Detail    string `json:"detail"`
	Count     int    `json:"count"`
}

// The rule ids as they appear in the text report and in --json. The A-numbers
// are the entries of the anomaly catalogue each rule implements.
const (
	auditRuleRepeatedView    = "repeated-view"    // A22
	auditRuleRepeatedCommand = "repeated_command" // A23
	auditRuleWaitOnly        = "wait_only"
	auditRuleToolErrors      = "tool_errors" // A31
	auditRuleNotices         = "notices"
	auditRuleUnfinishedJobs  = "unfinished_jobs"
)

// auditFindings reports the rules each selected session tripped.
func auditFindings(ctx context.Context, db *sql.DB, schema auditSchema, ids []string, since time.Duration) ([]auditFinding, error) {
	if len(ids) == 0 {
		return nil, nil
	}

	tally := newAuditTally(len(ids))
	if err := auditScanMessages(ctx, db, schema, ids, since, tally); err != nil {
		return nil, err
	}
	if err := auditScanAsyncJobs(ctx, db, schema, ids, tally); err != nil {
		return nil, err
	}
	return tally.findings(ids), nil
}

// auditTally is the per-session evidence the rules count. Every counter is
// keyed by session id so one scan of many sessions keeps them apart, and every
// one is compared against exactly one threshold.
type auditTally struct {
	// fileViews counts, per session, the views of each file path.
	fileViews map[string]map[string]int
	// commands counts, per session, each normalized bash command.
	commands map[string]map[string]int
	// waitOnly counts, per session, the wait-only commands it ran, keyed by
	// the normalized command they waited with.
	waitOnly map[string]map[string]int
	// toolErrors counts, per session, each class of failed tool result.
	toolErrors map[string]map[string]int
	// notices counts, per session, each supervision/wake_failed notice.
	notices map[string]map[string]int
	// unfinishedJobs lists, per session, the never-finished async jobs as
	// "tool_call_id:kind" pairs.
	unfinishedJobs map[string][]string
	// parseFailures counts the rows the audit could not read. It is
	// deliberately not a finding: a few broken rows are noise, and the rules
	// are about what the readable rows say.
	parseFailures int
}

func newAuditTally(sessions int) *auditTally {
	return &auditTally{
		fileViews:      make(map[string]map[string]int, sessions),
		commands:       make(map[string]map[string]int, sessions),
		waitOnly:       make(map[string]map[string]int, sessions),
		toolErrors:     make(map[string]map[string]int, sessions),
		notices:        make(map[string]map[string]int, sessions),
		unfinishedJobs: make(map[string][]string, sessions),
	}
}

// auditEvidence is one Detail/Count pair a rule reached its threshold on.
type auditEvidence struct {
	Detail string
	Count  int
}

// findings emits, per selected session and in a fixed rule order, every
// suspicion the session's counters reached the threshold for. The session
// order is the caller's selection order (newest first), the evidence order is
// by count then detail, so the same database always renders the same report.
func (tally *auditTally) findings(ids []string) []auditFinding {
	var findings []auditFinding
	for _, id := range ids {
		for _, evidence := range auditThresholded(tally.fileViews[id], auditThresholdFileViews) {
			findings = append(findings, auditFinding{
				Rule: auditRuleRepeatedView, SessionID: id, Detail: evidence.Detail, Count: evidence.Count,
			})
		}
		for _, evidence := range auditThresholded(tally.commands[id], auditThresholdBashCommands) {
			findings = append(findings, auditFinding{
				Rule: auditRuleRepeatedCommand, SessionID: id, Detail: evidence.Detail, Count: evidence.Count,
			})
		}
		// wait_only counts the waits themselves, across every command they
		// were spent on: three turns of `sleep 5`, `sleep 10` and `sleep 30`
		// is a session that waited three times.
		if total := auditSum(tally.waitOnly[id]); total >= auditThresholdWaitOnlyCommands {
			findings = append(findings, auditFinding{
				Rule: auditRuleWaitOnly, SessionID: id,
				Detail: auditMostCommon(tally.waitOnly[id]), Count: total,
			})
		}
		for _, evidence := range auditThresholded(tally.toolErrors[id], auditThresholdToolErrorsPerClass) {
			findings = append(findings, auditFinding{
				Rule: auditRuleToolErrors, SessionID: id, Detail: evidence.Detail, Count: evidence.Count,
			})
		}
		for _, evidence := range auditThresholded(tally.notices[id], auditThresholdNotices) {
			findings = append(findings, auditFinding{
				Rule: auditRuleNotices, SessionID: id, Detail: evidence.Detail, Count: evidence.Count,
			})
		}
		if jobs := tally.unfinishedJobs[id]; len(jobs) >= auditThresholdUnfinishedJobs {
			findings = append(findings, auditFinding{
				Rule: auditRuleUnfinishedJobs, SessionID: id,
				Detail: strings.Join(jobs, ", "), Count: len(jobs),
			})
		}
	}
	return findings
}

// auditThresholded keeps the evidence whose count reached the threshold, most
// repeated first and by detail within a tie.
func auditThresholded(counts map[string]int, threshold int) []auditEvidence {
	var evidence []auditEvidence
	for detail, count := range counts {
		if count >= threshold {
			evidence = append(evidence, auditEvidence{Detail: detail, Count: count})
		}
	}
	sort.Slice(evidence, func(i, j int) bool {
		if evidence[i].Count != evidence[j].Count {
			return evidence[i].Count > evidence[j].Count
		}
		return evidence[i].Detail < evidence[j].Detail
	})
	return evidence
}

func auditSum(counts map[string]int) int {
	total := 0
	for _, count := range counts {
		total += count
	}
	return total
}

// auditMostCommon names the wait command a session leaned on, so a wait_only
// finding says what it waited with, not only how often.
func auditMostCommon(counts map[string]int) string {
	detail, best := "", 0
	for candidate, count := range counts {
		if count > best || (count == best && candidate < detail) {
			detail, best = candidate, count
		}
	}
	return detail
}
