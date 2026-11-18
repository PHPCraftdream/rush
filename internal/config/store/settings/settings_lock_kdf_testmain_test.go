package settings

import (
	"testing"

	"github.com/PHPCraftdream/rush/internal/kit/dev/testguard"
)

func init() { settingsPasswordIterations.Store(1000) }

// TestMain isolates global state for this package's test binary.
func TestMain(m *testing.M) { testguard.Main(m) }
