//go:build !windows

package cmd

func installConsoleProcessStop(func(int)) func() {
	return func() {}
}
