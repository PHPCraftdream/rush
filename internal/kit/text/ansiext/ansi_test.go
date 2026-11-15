package ansiext

import (
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/assert"
)

func TestTruncateDisplayASCII(t *testing.T) {
	assert.Equal(t, "", TruncateDisplay("hello", 0))
	assert.Equal(t, "", TruncateDisplay("hello", -1))
	assert.Equal(t, "hello", TruncateDisplay("hello", 10))
	assert.Equal(t, "hello", TruncateDisplay("hello", 5))
	assert.Equal(t, "hell…", TruncateDisplay("hello world", 5))
	assert.Equal(t, "h", TruncateDisplay("hello", 1)) // no room for a tail at n==1
}

func TestTruncateDisplayCyrillic(t *testing.T) {
	in := "Запрос пустой: в сообщении пусто"
	got := TruncateDisplay(in, 12)
	assert.True(t, utf8.ValidString(got), "result must be valid UTF-8")
	assert.LessOrEqual(t, ansi.StringWidth(got), 12)
	assert.True(t, strings.HasSuffix(got, "…"), "cut result must end with ellipsis")
	assert.Equal(t, in, TruncateDisplay(in, 40), "input within budget is untouched")
}

func TestTruncateDisplayCJKWidth(t *testing.T) {
	// Each CJK rune is 2 cells wide: "世界世" is 6 cells, cutting to 5 cells
	// must drop the whole last rune, not slice mid-rune.
	got := TruncateDisplay("世界世界世", 5)
	assert.True(t, utf8.ValidString(got), "result must be valid UTF-8")
	assert.LessOrEqual(t, ansi.StringWidth(got), 5)
	assert.True(t, strings.HasSuffix(got, "…"), "cut result must end with ellipsis")
	assert.Equal(t, "世界…", got) // 2+2+1 = 5 cells
}

func TestTruncateDisplayEmoji(t *testing.T) {
	// ZWJ family emoji is one grapheme cluster ~2 cells wide.
	got := TruncateDisplay("ok 👨‍👩‍👧‍👦 done", 6)
	assert.Equal(t, "ok 👨‍👩‍👧‍👦…", got)
	assert.True(t, utf8.ValidString(got), "result must be valid UTF-8")
	assert.LessOrEqual(t, ansi.StringWidth(got), 6)
}
