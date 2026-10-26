package agent

// Background-shell completion plumbing, batch edition. The deleted legacy
// row machinery (asyncTool.claimBackgroundShellRow / finishBGShellRow over
// async_jobs) is gone: a batch bash call's background escape is claimed and
// settled by the batch ingress tool (batch_install_tools.go's
// batchEscapeAuto invocation and the executor's windowed delivery), and the
// completion notice + auto-resume slot decision run through
// persistBGShellCompletion (coordinator_bgshell_cap.go) from the shell's
// OnDone notifier (coordinator_background.go).

// notifyOnBackgroundJobDone mirrors buildTools' kill-switch default
// (coordinator_tools.go): a nil store/options or a nil pointer means enabled.
func (c *coordinator) notifyOnBackgroundJobDone() bool {
	if c == nil || c.cfg == nil {
		return true
	}
	opts := c.cfg.Config().Options
	return opts == nil || opts.NotifyOnBackgroundJobDone == nil || *opts.NotifyOnBackgroundJobDone
}
