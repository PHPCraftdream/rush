//go:build !windows

package cahgen

import "os/exec"

func configureRunnerCommand(*exec.Cmd, string, []string) {}
