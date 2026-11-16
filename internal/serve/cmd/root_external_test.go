package cmd_test

import (
	"github.com/PHPCraftdream/rush/internal/serve/cmd"
	"github.com/PHPCraftdream/rush/internal/serve/cmd/cli"
)

func init() { cmd.InstallRootForTest(cli.NewRootCmd(), cli.Execute) }
