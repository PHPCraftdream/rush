package cahgen

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
)

// RefreshBuild launches the generator with the real environment, not pnpm's synthetic CI.
func RefreshBuild(ctx context.Context, root string, stdout, stderr io.Writer) error {
	return refreshBuild(ctx, root, stdout, stderr, os.Getenv, func(cmd *exec.Cmd) error { return cmd.Run() })
}

func refreshBuild(ctx context.Context, root string, stdout, stderr io.Writer, env func(string) string, launch func(*exec.Cmd) error) error {
	cmd := exec.CommandContext(ctx, "go", "run", "./internal/tools/cahsync", "-mode", "refresh")
	cmd.Dir, cmd.Stdout, cmd.Stderr = root, stdout, stderr
	if err := launch(cmd); err != nil {
		_, err = failure(env("CI") == "true", fmt.Errorf("generator launch: %w", err), func(s string) { fmt.Fprintln(stderr, s) })
		return err
	}
	return nil
}

func failure(strict bool, err error, log func(string)) (int, error) {
	if _, broken := err.(*RollbackError); strict || broken {
		return 1, err
	}
	log("warning: cahsync failed; committed outputs untouched: " + err.Error())
	return 0, nil
}
