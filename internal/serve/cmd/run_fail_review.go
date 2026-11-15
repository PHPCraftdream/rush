// Fork-only file: `--fail-on-review` maps a "fail" reviewer verdict to a
// dedicated process exit code, without changing anything else about `rush
// run`'s behavior.
package cmd

// reviewFailExitCode is the dedicated exit code for `rush run
// --fail-on-review` when the run completed but the reviewer verdict is
// exactly "fail". `rush run`'s existing exit-code vocabulary is 0
// (success, including a review-fail without the flag), 1 (generic error)
// and 124 (timeout force-kill); code 3 is unoccupied for run. Note that
// `rush ping` separately uses code 3 for its "degraded" status — that is a
// different subcommand's vocabulary and unrelated here. The code is part of
// the public contract for wrapper scripts and completion notifications: it
// must not change silently.
const reviewFailExitCode = 3

// reviewFailError is returned by RunE when --fail-on-review is set and the
// automatic reviewer rejected the run. It carries an explicit process exit
// code so the CLI framework's exit-code plumbing can honor it.
type reviewFailError struct {
	verdict   string
	sessionID string
}

// Error renders the operator-facing message shown in completion
// notifications. It names the flag, the verdict and the exit code, and
// points at where to read the full review.
func (e *reviewFailError) Error() string {
	readHint := "Read the review: rush result --review <envelope path or session id>"
	if e.sessionID != "" {
		readHint = "Read the review: rush result " + e.sessionID + " --review"
	}
	return "review verdict is \"fail\" (--fail-on-review): the run itself " +
		"completed, but the reviewer rejected it; exiting with code " +
		"3. " + readHint
}

// ExitCode implements the exit-coder interface consumed by the CLI
// framework; it is what turns this error into exit code 3.
func (e *reviewFailError) ExitCode() int {
	return reviewFailExitCode
}

// reviewVerdictExitCode maps a run envelope's review verdict to a process
// exit code under the --fail-on-review flag. It returns
// (reviewFailExitCode, true) if and only if failOnReview is true and
// verdict is exactly "fail". Every other verdict — pass, pass_with_notes,
// unverified, unparsed, error, and the empty verdict (no reviewer ran) —
// returns (0, false): success, as today. A false flag means `rush run`'s
// behavior is completely unchanged; only the process exit code is ever
// affected, never the stdout envelope.
func reviewVerdictExitCode(verdict string, failOnReview bool) (int, bool) {
	if failOnReview && verdict == "fail" {
		return reviewFailExitCode, true
	}
	return 0, false
}
