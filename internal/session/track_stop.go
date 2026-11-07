package session

import (
	"context"
	"sync/atomic"
)

var processTreesStopping atomic.Bool

// StartProcessUnlessStopping закрывает admission новых отслеживаемых запусков.
func StartProcessUnlessStopping(start func() error) error {
	if processTreesStopping.Load() {
		return context.Canceled
	}
	return start()
}

// StopTrackedProcessTrees используется только перед немедленным выходом CLI.
func StopTrackedProcessTrees() {
	processTreesStopping.Store(true)
	stopTrackedProcessTrees()
}
