package cmd_test

import (
	"github.com/PHPCraftdream/rush/internal/serve/cmd"
	"github.com/PHPCraftdream/rush/internal/serve/cmd/cli"
	out "github.com/PHPCraftdream/rush/internal/serve/cmd/cli/cmdkit/out"
)

func init() {
	root := cli.NewRootCmd()
	cmd.InstallRootForTest(root, cli.Execute)
	api, _, _ := root.Find([]string{"api"})
	tokens, _, _ := root.Find([]string{"tokens"})
	serve, _, _ := root.Find([]string{"serve"})
	out.SetInvocationCommands(root, api, tokens, serve)
}
