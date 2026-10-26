package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/signal"

	"github.com/PHPCraftdream/rush/internal/jobbatch/lab"
)

const help = `Usage:
  batchlab run scenario.json [--events events.jsonl]
  batchlab replay scenario.json events.jsonl
Run uses one Runner and the scenario's overall wait bound. Replay validates
metadata without executing tasks. Exit codes: 0 success, 1 failed aggregate,
2 invalid input, 3 unfinished/cleanup failure, 4 mismatch, 5 incomplete journal.
`

// Main is the in-process command entry point. Context cancellation invokes
// independent bounded Runner cleanup; diagnostics contain no task data.
func Main(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprint(stderr, help)
		return lab.ExitInvalid
	}
	if args[0] == "help" || args[0] == "--help" || args[0] == "-h" {
		fmt.Fprint(stdout, help)
		return lab.ExitSuccess
	}
	mode := args[0]
	var scenarioPath, journalPath string
	if mode == "run" {
		for i := 1; i < len(args); i++ {
			if args[i] == "--events" {
				if journalPath != "" || i+1 == len(args) {
					return lab.ExitInvalid
				}
				i++
				journalPath = args[i]
			} else if scenarioPath == "" {
				scenarioPath = args[i]
			} else {
				return lab.ExitInvalid
			}
		}
	} else if mode == "replay" && len(args) == 3 {
		scenarioPath, journalPath = args[1], args[2]
	} else {
		fmt.Fprint(stderr, help)
		return lab.ExitInvalid
	}
	file, err := os.Open(scenarioPath)
	if err != nil {
		fmt.Fprintln(stderr, "cannot read scenario")
		return lab.ExitInvalid
	}
	scenario, err := lab.LoadScenario(file)
	file.Close()
	if err != nil {
		fmt.Fprintln(stderr, "invalid scenario:", lab.ScenarioValidationRule(err))
		return lab.ExitInvalid
	}
	if mode == "replay" {
		journal, err := os.Open(journalPath)
		if err != nil {
			fmt.Fprintln(stderr, "cannot read journal")
			return lab.ExitInvalid
		}
		defer journal.Close()
		return lab.Replay(scenario, journal).ExitCode
	}
	opts := lab.RunOptions{}
	if journalPath != "" {
		opts.OpenEvents = func() (io.WriteCloser, error) { return os.Create(journalPath) }
	}
	result := lab.RunScenario(ctx, scenario, opts)
	if result.ExitCode == lab.ExitMismatch {
		fmt.Fprintf(stderr, "scenario mismatch: actions %v; oracle %s\n", result.Mismatches, result.OracleRule)
	}
	return result.ExitCode
}

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	os.Exit(Main(ctx, os.Args[1:], os.Stdout, os.Stderr))
}
