//go:build !windows

package mcp

import (
	"testing"

	"github.com/PHPCraftdream/rush/internal/testguard"
)

func TestMain(m *testing.M) { testguard.Main(m) }
