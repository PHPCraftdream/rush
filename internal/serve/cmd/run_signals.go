package cmd

import (
	"os"
	"syscall"
)

var cancelSignals = []os.Signal{os.Interrupt, syscall.SIGTERM}
