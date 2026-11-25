//go:build !windows && !linux && !darwin && !freebsd && !openbsd && !netbsd && !dragonfly

package process

import (
	"os/exec"

	"github.com/PHPCraftdream/rush/internal/session/jobbatch"
)

type ProcessTree struct{}

func ProcessLeaderExited(*exec.Cmd) bool             { return false }
func ProcessSupported() bool                         { return false }
func ProcessPrepare(*exec.Cmd) (*ProcessTree, error) { return nil, jobbatch.ErrUnsupported }
func (*ProcessTree) attach(*exec.Cmd) error          { return jobbatch.ErrUnsupported }
func (*ProcessTree) term() error                     { return jobbatch.ErrUnsupported }
func (*ProcessTree) kill() error                     { return jobbatch.ErrUnsupported }
func (*ProcessTree) awaitGone()                      {}
func (*ProcessTree) dispose() error                  { return nil }
