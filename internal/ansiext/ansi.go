package ansiext

import (
	"strings"

	"github.com/charmbracelet/x/ansi"
)

// TruncateDisplay shortens s so that its display width in terminal cells is
// at most n, appending an ellipsis when truncation happens. It is safe for
// multibyte text (Cyrillic, CJK, emoji): runes are never cut mid-encoding and
// wide runes count as 2 cells. ANSI escape sequences are preserved.
//
// For n <= 0 an empty string is returned. For n == 1 no tail fits, so a
// single cell of content is kept instead of the ellipsis.
func TruncateDisplay(s string, n int) string {
	if n <= 0 {
		return ""
	}
	if n == 1 {
		return ansi.Truncate(s, 1, "")
	}
	return ansi.Truncate(s, n, "…")
}

// Escape replaces control characters with their Unicode Control Picture
// representations to ensure they are displayed correctly in the UI.
func Escape(content string) string {
	var sb strings.Builder
	sb.Grow(len(content))
	for _, r := range content {
		switch {
		case r >= 0 && r <= 0x1f: // Control characters 0x00-0x1F
			sb.WriteRune('\u2400' + r)
		case r == ansi.DEL:
			sb.WriteRune('\u2421')
		default:
			sb.WriteRune(r)
		}
	}
	return sb.String()
}
