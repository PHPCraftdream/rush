//go:build windows

package config

import (
	"errors"
	"fmt"
	"time"

	"golang.org/x/sys/windows"
)

// renameConfigTemp uses MoveFileEx directly so the expected-absent case is a
// true no-replace operation. os.Rename on Windows is intentionally not used:
// its implementation requests replacement semantics.
func renameConfigTemp(source, destination string, replace bool) error {
	from, err := windows.UTF16PtrFromString(source)
	if err != nil {
		return fmt.Errorf("encode temporary config path: %w", err)
	}
	to, err := windows.UTF16PtrFromString(destination)
	if err != nil {
		return fmt.Errorf("encode config path: %w", err)
	}
	flags := uint32(windows.MOVEFILE_WRITE_THROUGH)
	if replace {
		flags |= windows.MOVEFILE_REPLACE_EXISTING
	}
	for attempt := 0; ; attempt++ {
		configTestHooks.Lock()
		moveFileEx := configTestHooks.moveFileEx
		configTestHooks.Unlock()
		if moveFileEx != nil {
			err = moveFileEx(from, to, flags)
		} else {
			err = windows.MoveFileEx(from, to, flags)
		}
		if err == nil || !replace || !errors.Is(err, windows.ERROR_ACCESS_DENIED) || attempt == 7 {
			return err
		}
		// A cooperating writer can still be completing its pre-lock handle
		// close because target resolution deliberately precedes lock acquire.
		// Give that short-lived reader time to release FILE_SHARE_DELETE before
		// treating the replacement as a real access failure.
		time.Sleep(2 * time.Millisecond)
	}
}
