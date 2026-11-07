//go:build windows

package session

import "os/exec"

func configureLateStopChild(*exec.Cmd)    {}
func registerLateStopChild(pid int) error { return TrackProcessTree(pid) }
