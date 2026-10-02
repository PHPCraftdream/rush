package cmd

import (
	"context"
	"fmt"
	"os"

	tea "charm.land/bubbletea/v2"
	console "github.com/charmbracelet/x/windows"
	"golang.org/x/sys/windows"
	"golang.org/x/term"
)

const (
	physicalO  = 0x18
	physicalC  = 0x2e
	physicalF2 = 0x3c
	physicalF3 = 0x3d
)

func nativeOAuthShortcut(key console.KeyEventRecord) rune {
	if !key.KeyDown || key.ControlKeyState&(windows.LEFT_ALT_PRESSED|windows.RIGHT_ALT_PRESSED|windows.LEFT_CTRL_PRESSED|windows.RIGHT_CTRL_PRESSED) != 0 {
		return 0
	}
	switch key.VirtualKeyCode {
	case windows.VK_F2:
		return 'o'
	case windows.VK_F3:
		return 'c'
	}
	switch key.VirtualScanCode {
	case physicalO:
		return 'o'
	case physicalC:
		return 'c'
	case 0:
		switch key.VirtualKeyCode {
		case 'O':
			return 'o'
		case 'C':
			return 'c'
		case 0:
			switch key.Char {
			case 'o', 'O':
				return 'o'
			case 'c', 'C':
				return 'c'
			}
		}
	}
	return 0
}

func startPhysicalOAuthLinkControls(ctx context.Context, model *oauthLinkModel) (func(), bool) {
	input := windows.Handle(os.Stdin.Fd())
	var originalMode uint32
	if err := windows.GetConsoleMode(input, &originalMode); err != nil {
		return nil, false
	}
	if originalMode&windows.ENABLE_VIRTUAL_TERMINAL_INPUT != 0 {
		if err := windows.SetConsoleMode(input, originalMode&^windows.ENABLE_VIRTUAL_TERMINAL_INPUT); err != nil {
			return nil, false
		}
	}
	if err := console.FlushConsoleInputBuffer(input); err != nil {
		if originalMode&windows.ENABLE_VIRTUAL_TERMINAL_INPUT != 0 {
			_ = windows.SetConsoleMode(input, originalMode)
		}
		return nil, false
	}
	if width, _, err := term.GetSize(int(os.Stdout.Fd())); err == nil {
		model.width = width
	}
	fmt.Print(model.View().Content)

	controlsCtx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		var openDown, copyDown bool
		for controlsCtx.Err() == nil {
			state, err := windows.WaitForSingleObject(input, 100)
			if err != nil {
				fmt.Printf("Keyboard input unavailable: %v\n", err)
				return
			}
			if state == uint32(windows.WAIT_TIMEOUT) {
				continue
			}
			if state != windows.WAIT_OBJECT_0 {
				fmt.Println("Keyboard input unavailable; copy the URL above manually.")
				return
			}
			var record console.InputRecord
			var count uint32
			if err := console.ReadConsoleInput(input, &record, 1, &count); err != nil {
				fmt.Printf("Keyboard input unavailable: %v\n", err)
				return
			}
			if count != 1 || record.EventType != console.KEY_EVENT {
				continue
			}
			key := record.KeyEvent()
			var down *bool
			switch key.VirtualScanCode {
			case physicalO, physicalF2:
				down = &openDown
			case physicalC, physicalF3:
				down = &copyDown
			}
			if down != nil {
				if !key.KeyDown {
					*down = false
					continue
				}
				if *down {
					continue
				}
				*down = true
			}
			if shortcut := nativeOAuthShortcut(key); shortcut != 0 {
				model.Update(tea.KeyPressMsg{Code: shortcut})
				fmt.Println(model.status)
			}
		}
	}()
	return func() {
		cancel()
		<-done
		if originalMode&windows.ENABLE_VIRTUAL_TERMINAL_INPUT != 0 {
			_ = windows.SetConsoleMode(input, originalMode)
		}
	}, true
}
