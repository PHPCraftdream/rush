package verify

import (
	"errors"
	"fmt"
	"os"

	"github.com/spf13/cobra"
)

type ExitError struct {
	Code    int
	Message string
}

func (e *ExitError) Error() string { return e.Message }
func (e *ExitError) ExitCode() int { return e.Code }

func NewCommand() *cobra.Command {
	root := &cobra.Command{Use: "verify", Short: "Verify a worktree", SilenceErrors: true, SilenceUsage: true}
	guards := &cobra.Command{Use: "guards [path]", Short: "Check repository guard rules", Args: cobra.MaximumNArgs(1)}
	jsonOutput := false
	guards.Flags().BoolVar(&jsonOutput, "json", false, "Emit machine-readable JSON")
	guards.RunE = func(cmd *cobra.Command, args []string) error {
		target := "."
		if len(args) > 0 {
			target = args[0]
		}
		countTests := os.Getenv("DIR_ENTRIES_COUNT_TESTS") == "1"
		env, code, err := Run(cmd.Context(), target, countTests)
		if err != nil {
			exit := 3
			if errors.Is(err, errTarget) {
				exit = 2
			}
			if jsonOutput {
				failure := failureEnvelope(target, err, exit)
				b, _ := Encode(failure)
				_, _ = fmt.Fprintln(cmd.OutOrStdout(), string(b))
			}
			return &ExitError{Code: exit, Message: err.Error()}
		}
		if jsonOutput {
			b, e := Encode(env)
			if e != nil {
				return &ExitError{Code: 3, Message: e.Error()}
			}
			_, _ = fmt.Fprintln(cmd.OutOrStdout(), string(b))
		} else {
			for _, s := range env.Stages {
				_, _ = fmt.Fprintf(cmd.OutOrStdout(), "%s: %s (%s)\n", s.Name, s.Status, s.Summary)
				for _, g := range s.Guards {
					_, _ = fmt.Fprintf(cmd.OutOrStdout(), "  %s: %s (%s)\n", g.Name, g.Status, g.Summary)
					if g.Excerpt != "" {
						_, _ = fmt.Fprintln(cmd.OutOrStdout(), "  "+g.Excerpt)
					}
				}
			}
		}
		if code != 0 {
			return &ExitError{Code: code, Message: "verify guards failed"}
		}
		return nil
	}
	root.AddCommand(guards)
	return root
}
