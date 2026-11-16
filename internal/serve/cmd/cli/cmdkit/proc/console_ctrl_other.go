//go:build !windows

package cliproc

func installConsoleProcessStop(func(int)) func() {
	return func() {}
}
