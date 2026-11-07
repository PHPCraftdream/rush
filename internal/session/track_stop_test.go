package session

import (
	"context"
	"os"
	"os/exec"
	"testing"
	"time"
)

func TestProcessStopRejectsNewStarts(t *testing.T) {
	previous := processTreesStopping.Swap(true)
	t.Cleanup(func() { processTreesStopping.Store(previous) })
	called := false
	err := StartProcessUnlessStopping(func() error { called = true; return nil })
	if err != context.Canceled || called {
		t.Fatalf("start admitted after process stop: called=%v err=%v", called, err)
	}
}

func TestProcessStopLateRegistrationHelper(t *testing.T) {
	if os.Getenv("RUSH_LATE_STOP_HELPER") == "1" {
		<-time.After(time.Hour)
	}
}

func TestProcessStopKillsLateRegistration(t *testing.T) {
	previous := processTreesStopping.Swap(true)
	t.Cleanup(func() { processTreesStopping.Store(previous) })
	command := exec.CommandContext(t.Context(), os.Args[0], "-test.run=^TestProcessStopLateRegistrationHelper$")
	command.Env = append(os.Environ(), "RUSH_LATE_STOP_HELPER=1", "GORACE=atexit_sleep_ms=0")
	configureLateStopChild(command)
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = command.Process.Kill(); UntrackProcessTree(command.Process.Pid) })
	if err := registerLateStopChild(command.Process.Pid); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- command.Wait() }()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("late child was not killed")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("late registration escaped the process-stop sweep")
	}
}
