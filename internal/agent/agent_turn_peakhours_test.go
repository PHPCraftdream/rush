package agent

// The mid-turn peak-hours watcher tests were removed together with the
// watcher in task #56: the peak-hours gate is pre-flight only, and a window
// opening mid-turn never aborts a running stream.
