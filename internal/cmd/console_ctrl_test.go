package cmd

import "testing"

func TestInstallConsoleProcessStop(t *testing.T) {
	uninstall := installConsoleProcessStop(func(int) { t.Error("unexpected console stop during installation") })
	if uninstall == nil {
		t.Fatal("console handler must return an uninstall function")
	}
	uninstall()
}
