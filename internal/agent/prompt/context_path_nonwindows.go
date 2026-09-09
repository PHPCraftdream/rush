//go:build !windows

package prompt

func isContextReparsePoint(string) bool {
	return false
}
