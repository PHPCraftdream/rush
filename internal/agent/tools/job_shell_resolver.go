package tools

import (
	"errors"
	"fmt"
	"strings"
)

// JobShellResolver resolves a model-visible async job id (the id returned in
// "Async bash job <id> started", equal to the tool call id -- see
// asyncToolMetadata.JobID in internal/agent/async_tool.go) to the background
// shell id backing it, scoped to the owning session. Implemented by the
// agent package's work ledger and injected here so this package never
// imports internal/agent -- see coordinator_tools.go's wiring.
type JobShellResolver interface {
	// ResolveJobShellID resolves jobID, owned by sessionID, to a background
	// shell id. The returned error's message is safe to show the model
	// directly (job_output/job_kill return it via NewTextErrorResponse).
	ResolveJobShellID(sessionID, jobID string) (string, error)
}

// errJobShellIDRequired is the shared validation error for job_kill and
// job_output: exactly one of job_id/shell_id must be given.
var errJobShellIDRequired = errors.New("provide exactly one of job_id or shell_id")

// resolveShellID validates the job_id/shell_id pair shared by job_kill and
// job_output and returns the concrete shell id to operate on. shellID given
// directly is used as-is (backward compatible with pre-job_id callers);
// jobID is resolved through resolver, scoped to sessionID.
func resolveShellID(resolver JobShellResolver, sessionID, jobID, shellID string) (string, error) {
	jobID = strings.TrimSpace(jobID)
	shellID = strings.TrimSpace(shellID)
	if (jobID == "") == (shellID == "") { // both empty or both set
		return "", errJobShellIDRequired
	}
	if shellID != "" {
		return shellID, nil
	}
	if resolver == nil {
		return "", fmt.Errorf("job %s not found (job_id resolution is unavailable)", jobID)
	}
	return resolver.ResolveJobShellID(sessionID, jobID)
}
