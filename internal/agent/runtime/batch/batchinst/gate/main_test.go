package gate

import (
	"log/slog"
	"testing"

	"github.com/PHPCraftdream/rush/internal/kit/dev/testguard"
	_ "github.com/joho/godotenv/autoload"
)

func TestMain(m *testing.M) {
	slog.SetLogLoggerLevel(slog.LevelError)
	testguard.Main(m)
}
