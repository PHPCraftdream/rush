package catalogsrc

import (
	"io"
	"log/slog"
	"os"
	"testing"

	"github.com/PHPCraftdream/rush/internal/kit/dev/testguard"
)

func TestMain(m *testing.M) {
	os.Setenv("RUSH_PROVIDER_CACHE_TTL", "0")
	// Same legacy-JSON default as the config root's ktav_testmain_gate_test.go.
	setKtavAutoMigrateForTest(false)
	slog.SetDefault(slog.New(slog.NewTextHandler(io.Discard, nil)))
	testguard.Main(m)
}
