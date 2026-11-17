package apitoken

import (
	"testing"

	"github.com/PHPCraftdream/rush/internal/config/io/slots"
	"github.com/PHPCraftdream/rush/internal/kit/dev/testguard"
)

func init() {
	slots.SetKtavAutoMigrateForTest(false)
}

func TestMain(m *testing.M) {
	testguard.Main(m)
}
