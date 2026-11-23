//go:build windows

package shell

import (
	"fmt"
	"os"
	"testing"

	"github.com/PHPCraftdream/rush/internal/kit/dev/testguard"
)

func TestMain(m *testing.M) {
	cleanup, err := testguard.Isolate()
	if err != nil {
		fmt.Fprintf(os.Stderr, "TestMain: isolate global state: %v\n", err)
		os.Exit(1)
	}
	code := m.Run()
	cleanup()
	os.Exit(code)
}
