//go:build !windows && !linux && !darwin && !freebsd && !openbsd && !netbsd && !dragonfly

package lab

import (
	"os/exec"

	"github.com/PHPCraftdream/rush/internal/jobbatch"
)

type processTree struct{}

func processLeaderExited(*exec.Cmd) bool             { return false }
func processSupported() bool                         { return false }
func processPrepare(*exec.Cmd) (*processTree, error) { return nil, jobbatch.ErrUnsupported }
func (*processTree) attach(*exec.Cmd) error          { return jobbatch.ErrUnsupported }
func (*processTree) term() error                     { return jobbatch.ErrUnsupported }
func (*processTree) kill() error                     { return jobbatch.ErrUnsupported }
func (*processTree) awaitGone()                      {}
func (*processTree) dispose() error                  { return nil }
