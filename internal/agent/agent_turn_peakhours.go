package agent

// The mid-turn peak-hours watcher was removed in task #56: the peak-hours
// gate is pre-flight only (buildCall plus the summarize/sub-agent
// pre-checks), so a window opening mid-turn never aborts a running stream.
