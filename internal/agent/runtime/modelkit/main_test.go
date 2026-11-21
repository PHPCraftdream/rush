package modelkit

import (
	"log/slog"
	"testing"

	"github.com/PHPCraftdream/rush/internal/kit/dev/testguard"
)

func TestMain(m *testing.M) {
	slog.SetLogLoggerLevel(slog.LevelError)
	testguard.Main(m)
}
