package cmd

import (
	"testing"

	console "github.com/charmbracelet/x/windows"
	"github.com/stretchr/testify/require"
	"golang.org/x/sys/windows"
)

func TestNativeOAuthShortcutIgnoresKeyboardLayout(t *testing.T) {
	cases := []struct {
		name string
		key  console.KeyEventRecord
		want rune
	}{
		{"physical O with Russian character", console.KeyEventRecord{KeyDown: true, VirtualScanCode: physicalO, Char: 'щ'}, 'o'},
		{"physical C with Russian character", console.KeyEventRecord{KeyDown: true, VirtualScanCode: physicalC, Char: 'с'}, 'c'},
		{"physical O with Arabic character", console.KeyEventRecord{KeyDown: true, VirtualScanCode: physicalO, Char: 'خ'}, 'o'},
		{"unrelated physical key with O character", console.KeyEventRecord{KeyDown: true, VirtualScanCode: 0x2c, Char: 'o'}, 0},
		{"terminal-generated C without scan code", console.KeyEventRecord{KeyDown: true, Char: 'c'}, 'c'},
		{"F2", console.KeyEventRecord{KeyDown: true, VirtualKeyCode: windows.VK_F2, VirtualScanCode: physicalF2}, 'o'},
		{"F3", console.KeyEventRecord{KeyDown: true, VirtualKeyCode: windows.VK_F3, VirtualScanCode: physicalF3}, 'c'},
		{"release", console.KeyEventRecord{KeyDown: false, VirtualScanCode: physicalO, Char: 'щ'}, 0},
		{"control modifier", console.KeyEventRecord{KeyDown: true, VirtualScanCode: physicalC, ControlKeyState: windows.LEFT_CTRL_PRESSED}, 0},
		{"alt modifier", console.KeyEventRecord{KeyDown: true, VirtualScanCode: physicalO, ControlKeyState: windows.RIGHT_ALT_PRESSED}, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			require.Equal(t, tc.want, nativeOAuthShortcut(tc.key))
		})
	}
}
