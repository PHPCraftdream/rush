package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/PHPCraftdream/rush/internal/tasktree"
	"github.com/PHPCraftdream/rush/internal/tasktree/lab"
)

const cliSyntax = `tasklab run [--load checkpoint] [--snapshot checkpoint] scenario
 tasklab inspect checkpoint
 tasklab repl --tree-key key [--actor agent|operator] [--actor-id id] [limits] [--load checkpoint] [--snapshot checkpoint]
Flags MUST precede positional arguments. No Rush initialization or agent loop.
REPL: one {"request_id":"id","payload":{"op":"view"}} JSON object per line.
EOF or an empty/whitespace-only line terminates. No help/quit stream commands.
Malformed wrappers terminate with exit 1; correctable tool errors emit JSON and continue.
REPL transport lines are limited to 1048576 bytes. Loaded key/limits must match.
LAB-only: scenario/checkpoint input <= 4194304 bytes including whitespace; <= 512 steps.
Reports retain <= 262144 Text bytes and <= 4096 view/created node occurrences; overflow is an error.
JSON nesting <= 128; duplicate object members are rejected at every depth.
Exit: help 0; fulfilled expectations/clean REPL 0; validation/expectation/I/O 1; usage 2.
`

type cliOutput struct {
	writer io.Writer
	err    error
}

func (w *cliOutput) Write(p []byte) (int, error) {
	n, err := w.writer.Write(p)
	if err == nil && n != len(p) {
		err = io.ErrShortWrite
	}
	if err != nil && w.err == nil {
		w.err = err
	}
	return n, err
}

func runCLI(args []string, input io.Reader, output, diagnostics io.Writer) int {
	fail := func(err error) int {
		fmt.Fprintln(diagnostics, err)
		return 1
	}
	usage := func(code int) int {
		if _, err := io.WriteString(diagnostics, cliSyntax); err != nil {
			return 1
		}
		return code
	}
	if len(args) == 0 {
		return usage(2)
	}
	if args[0] == "help" || args[0] == "--help" || args[0] == "-h" {
		if len(args) != 1 {
			return usage(2)
		}
		if _, err := io.WriteString(output, cliSyntax); err != nil {
			return fail(err)
		}
		return 0
	}
	command := args[0]
	if command != "run" && command != "inspect" && command != "repl" {
		return usage(2)
	}
	flags := flag.NewFlagSet(command, flag.ContinueOnError)
	flagOutput := &cliOutput{writer: diagnostics}
	flags.SetOutput(flagOutput)
	flags.Usage = func() {
		io.WriteString(flagOutput, cliSyntax)
		flags.PrintDefaults()
	}
	var loadPath, snapshotPath, key, actor, actorID string
	limits := tasktree.Limits{}
	if command != "inspect" {
		flags.StringVar(&loadPath, "load", "", "validated checkpoint to seed (binding and limits must match)")
		flags.StringVar(&snapshotPath, "snapshot", "", "write full checkpoint on successful completion only")
	}
	if command == "repl" {
		flags.StringVar(&key, "tree-key", "", "required fixed trusted tree key")
		flags.StringVar(&actor, "actor", "agent", "fixed trusted actor: agent or operator")
		flags.StringVar(&actorID, "actor-id", "tasklab", "fixed trusted nonblank actor ID")
		flags.IntVar(&limits.MaxNodes, "max-nodes", 1000, "lab-only node bound including root (1..10000)")
		flags.IntVar(&limits.MaxDepth, "max-depth", 32, "lab-only depth bound (1..128)")
		flags.IntVar(&limits.MaxTitleBytes, "max-title-bytes", 1024, "lab-only user title byte bound (1..65536)")
		flags.IntVar(&limits.MaxReasonBytes, "max-reason-bytes", 4096, "lab-only reason byte bound (1..65536)")
		flags.IntVar(&limits.MaxTombstones, "max-tombstones", 1000, "lab-only tombstone/guard bound (1..10000)")
		flags.IntVar(&limits.MaxReceipts, "max-receipts", 1000, "lab-only durable receipt bound, no eviction (1..10000)")
	}
	parseErr := flags.Parse(args[1:])
	if flagOutput.err != nil {
		return fail(flagOutput.err)
	}
	if errors.Is(parseErr, flag.ErrHelp) {
		return 0
	}
	if parseErr != nil {
		return 2
	}
	badUsage := func() int {
		flags.Usage()
		if flagOutput.err != nil {
			return 1
		}
		return 2
	}
	if command == "repl" {
		if flags.NArg() != 0 || strings.TrimSpace(key) == "" || strings.TrimSpace(actorID) == "" || (actor != "agent" && actor != "operator") {
			return badUsage()
		}
		for _, bound := range [][2]int{{limits.MaxNodes, 10000}, {limits.MaxDepth, 128}, {limits.MaxTitleBytes, 65536}, {limits.MaxReasonBytes, 65536}, {limits.MaxTombstones, 10000}, {limits.MaxReceipts, 10000}} {
			if bound[0] < 1 || bound[0] > bound[1] {
				return badUsage()
			}
		}
	} else if flags.NArg() != 1 || strings.HasPrefix(flags.Arg(0), "-") {
		return badUsage()
	}
	ctx := context.Background()
	if command == "inspect" {
		checkpoint, err := loadCheckpoint(flags.Arg(0))
		if err != nil {
			return fail(err)
		}
		if err := lab.WriteCheckpoint(output, checkpoint); err != nil {
			return fail(err)
		}
		return 0
	}
	var loaded *lab.Checkpoint
	if loadPath != "" {
		checkpoint, err := loadCheckpoint(loadPath)
		if err != nil {
			return fail(err)
		}
		loaded = &checkpoint
	}
	var checkpoint lab.Checkpoint
	if command == "run" {
		scenario, err := loadScenario(flags.Arg(0))
		if err != nil {
			return fail(err)
		}
		report, err := lab.Run(ctx, scenario, loaded)
		if err != nil {
			return fail(err)
		}
		if err := json.NewEncoder(output).Encode(report); err != nil {
			return fail(err)
		}
		checkpoint = report.Checkpoint
	} else {
		var err error
		checkpoint, err = lab.REPL(ctx, input, output, lab.REPLOptions{
			TreeKey: tasktree.TreeKey(key), Actor: tasktree.Actor{Kind: tasktree.ActorKind(actor), ID: actorID}, Limits: limits, Loaded: loaded,
		})
		if err != nil {
			return fail(err)
		}
	}
	if snapshotPath != "" {
		if err := saveCheckpoint(snapshotPath, checkpoint); err != nil {
			return fail(err)
		}
	}
	return 0
}

func loadCheckpoint(path string) (lab.Checkpoint, error) {
	file, err := os.Open(path)
	if err != nil {
		return lab.Checkpoint{}, err
	}
	checkpoint, readErr := lab.ReadCheckpoint(file)
	closeErr := file.Close()
	return checkpoint, errors.Join(readErr, closeErr)
}

func loadScenario(path string) (lab.Scenario, error) {
	file, err := os.Open(path)
	if err != nil {
		return lab.Scenario{}, err
	}
	scenario, readErr := lab.ReadScenario(file)
	closeErr := file.Close()
	return scenario, errors.Join(readErr, closeErr)
}

type checkpointFile interface {
	io.Writer
	Name() string
	Sync() error
	Close() error
}

type checkpointFileOps struct {
	createTemp func(string, string) (checkpointFile, error)
	rename     func(string, string) error
}

func saveCheckpoint(path string, checkpoint lab.Checkpoint) error {
	return saveCheckpointWithOps(path, checkpoint, checkpointFileOps{
		createTemp: func(dir, pattern string) (checkpointFile, error) {
			return os.CreateTemp(dir, pattern)
		},
		rename: os.Rename,
	})
}

func saveCheckpointWithOps(path string, checkpoint lab.Checkpoint, ops checkpointFileOps) (err error) {
	file, err := ops.createTemp(filepath.Dir(path), ".tasklab-checkpoint-*")
	if err != nil {
		return err
	}
	tempPath := file.Name()
	closeAttempted := false
	defer func() {
		if !closeAttempted {
			err = errors.Join(err, file.Close())
		}
		if err != nil {
			err = errors.Join(err, os.Remove(tempPath))
		}
	}()
	if err = lab.WriteCheckpoint(file, checkpoint); err != nil {
		return err
	}
	if err = file.Sync(); err != nil {
		return err
	}
	closeAttempted = true
	if err = file.Close(); err != nil {
		return err
	}
	return ops.rename(tempPath, path)
}
