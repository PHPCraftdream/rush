package cliout

import (
	"os"
	"strings"
	"time"

	"github.com/PHPCraftdream/rush/internal/kit/text/ansiext"
)

func Short(hash string) string {
	if len(hash) <= 8 {
		return hash
	}
	return hash[:8]
}

// truncate shortens s so its display width in terminal cells is at most n,
// appending an ellipsis when truncation happens. It is rune- and
// display-width-aware (Cyrillic, CJK wide runes, emoji), so multibyte text is
// never cut mid-rune and column widths still hold.
func Truncate(s string, n int) string {
	return ansiext.TruncateDisplay(s, n)
}

func Dash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

func MaskKey(k string) string {
	if k == "" {
		return "-"
	}
	if strings.HasPrefix(k, "$") {
		// Env-template still unresolved — show the template literally so
		// it's obvious where the key comes from.
		return k
	}
	if len(k) <= 4 {
		return "****"
	}
	return strings.Repeat("*", 4) + k[len(k)-4:]
}

func Age(info os.FileInfo) int {
	if info == nil {
		return 0
	}
	return int(time.Since(info.ModTime()).Seconds())
}
