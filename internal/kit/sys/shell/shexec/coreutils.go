package shexec

import (
	"os"
	"runtime"
	"strconv"
)

var UseGoCoreUtils bool

func init() {
	// If RUSH_CORE_UTILS is set to either true or false, respect that.
	// By default, enable on Windows only.
	if v, err := strconv.ParseBool(os.Getenv("RUSH_CORE_UTILS")); err == nil {
		UseGoCoreUtils = v
	} else {
		UseGoCoreUtils = runtime.GOOS == "windows"
	}
}
