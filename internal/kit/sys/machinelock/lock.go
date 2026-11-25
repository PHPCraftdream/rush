package machinelock

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

const pollInterval = 150 * time.Millisecond

// StuckAction controls what a waiter does when a holder is diagnosed as stuck.
type StuckAction string

const (
	StuckWait    StuckAction = "wait"
	StuckRelease StuckAction = "release"
	StuckStop    StuckAction = "stop"
)

// Record describes a process holding a named machine-wide lock.
type Record struct {
	PID         int       `json:"pid"`
	Command     string    `json:"command"`
	StartedAt   time.Time `json:"started_at"`
	Checkout    string    `json:"checkout"`
	Token       string    `json:"token"`
	Stuck       bool      `json:"stuck,omitempty"`
	Snapshot    string    `json:"snapshot,omitempty"`
	Quarantined bool      `json:"quarantined,omitempty"`
}

// Waiter describes a process waiting for a named lock.
type Waiter struct {
	PID     int       `json:"pid"`
	Since   time.Time `json:"since"`
	Command string    `json:"command"`
}

// Entry is a lock and its current holder and waiters.
type Entry struct {
	Name        string   `json:"name"`
	Holder      *Record  `json:"holder,omitempty"`
	Quarantined []Record `json:"quarantined,omitempty"`
	Waiters     []Waiter `json:"waiters,omitempty"`
}

// Options configures command supervision while a lock is held.
type Options struct {
	Wait      time.Duration
	Timeout   time.Duration // Explicit upper bound; invokes stuck handling, never implicit killing.
	Idle      time.Duration
	OnStuck   StuckAction
	ForceSafe bool
	Grace     time.Duration
}

// Handle owns an acquired lock until Release is called.
type Handle struct{ path, token string }

// Acquire waits up to wait for a named machine-wide lock.
func Acquire(ctx context.Context, name string, record Record, wait time.Duration) (*Handle, error) {
	if strings.TrimSpace(name) == "" {
		return nil, errors.New("lock name must not be empty")
	}
	base := lockDir(name)
	if err := os.MkdirAll(filepath.Join(base, "waiters"), 0o755); err != nil {
		return nil, fmt.Errorf("create lock directory: %w", err)
	}
	waitDir := filepath.Join(base, "waiters")
	now := time.Now()
	waiter := Waiter{PID: os.Getpid(), Since: now, Command: record.Command}
	wpath := filepath.Join(waitDir, fmt.Sprintf("%020d-%010d-%d.json", now.UnixNano(), os.Getpid(), time.Now().UnixNano()))
	if err := writeJSONExclusive(wpath, waiter); err != nil {
		return nil, err
	}
	defer os.Remove(wpath)
	deadline := time.NewTimer(wait)
	defer deadline.Stop()
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		first, err := firstWaiter(waitDir)
		if err != nil {
			return nil, err
		}
		if first == wpath {
			record.PID, record.StartedAt = os.Getpid(), time.Now()
			if record.Token == "" {
				record.Token = fmt.Sprintf("%d-%d", record.PID, time.Now().UnixNano())
			}
			if record.Checkout == "" {
				record.Checkout, _ = os.Getwd()
			}
			lockPath := filepath.Join(base, "holder.json")
			if err := writeJSONExclusive(lockPath, record); err == nil {
				return &Handle{path: lockPath, token: record.Token}, nil
			} else if !errors.Is(err, os.ErrExist) {
				return nil, err
			}
			var held Record
			if readJSON(lockPath, &held) == nil && !processAlive(held.PID) {
				_ = os.Remove(lockPath)
			}
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-deadline.C:
			return nil, fmt.Errorf("timed out waiting for lock %q", name)
		case <-time.After(pollInterval):
		}
	}
}

// Release releases the lock if this handle still owns it.
func (h *Handle) Release() error {
	var r Record
	if err := readJSON(h.path, &r); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	}
	if r.Token != h.token {
		return nil
	}
	if r.Stuck && (r.Quarantined || processAlive(r.PID)) {
		return nil
	}
	return os.Remove(h.path)
}

// List returns current machine-wide locks and their waiters.
func List() ([]Entry, error) {
	root := filepath.Join(os.TempDir(), "rush-locks")
	dirs, err := os.ReadDir(root)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var entries []Entry
	for _, d := range dirs {
		if !d.IsDir() {
			continue
		}
		var e Entry
		e.Name = d.Name()
		if readJSON(filepath.Join(root, d.Name(), "holder.json"), &e.Holder) != nil {
			e.Holder = nil
		}
		files, _ := os.ReadDir(filepath.Join(root, d.Name(), "quarantine"))
		for _, f := range files {
			var r Record
			if readJSON(filepath.Join(root, d.Name(), "quarantine", f.Name()), &r) == nil {
				e.Quarantined = append(e.Quarantined, r)
			}
		}
		files, _ = os.ReadDir(filepath.Join(root, d.Name(), "waiters"))
		for _, f := range files {
			var w Waiter
			if readJSON(filepath.Join(root, d.Name(), "waiters", f.Name()), &w) == nil {
				e.Waiters = append(e.Waiters, w)
			}
		}
		sort.Slice(e.Waiters, func(i, j int) bool { return e.Waiters[i].Since.Before(e.Waiters[j].Since) })
		if e.Holder != nil || len(e.Waiters) > 0 || len(e.Quarantined) > 0 {
			entries = append(entries, e)
		}
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name < entries[j].Name })
	return entries, nil
}

// Run preserves the original API while using non-destructive stuck handling.
func Run(ctx context.Context, name string, record Record, wait, timeout time.Duration, argv []string, stdout, stderr *os.File) (int, error) {
	return RunWithOptions(ctx, name, record, Options{Wait: wait, Timeout: timeout, Idle: 10 * time.Minute}, argv, stdout, stderr)
}

// RunWithOptions runs argv under the named lock and never terminates a stuck tree unless explicitly requested.
func RunWithOptions(ctx context.Context, name string, record Record, options Options, argv []string, stdout, stderr *os.File) (int, error) {
	if len(argv) == 0 {
		return 1, errors.New("command is required")
	}
	if options.Idle <= 0 {
		options.Idle = 10 * time.Minute
	}
	if options.Grace <= 0 {
		options.Grace = 10 * time.Second
	}
	record.Checkout, _ = os.Getwd()
	waitCtx, cancel := context.WithTimeout(ctx, options.Wait)
	h, err := Acquire(waitCtx, name, record, options.Wait)
	cancel()
	if err != nil {
		return 1, err
	}
	defer h.Release()
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Stdout, cmd.Stderr, cmd.Stdin = stdout, stderr, os.Stdin
	configureProcess(cmd)
	if err := cmd.Start(); err != nil {
		return 1, err
	}
	_ = updateRecord(h, func(r *Record) { r.PID = cmd.Process.Pid })
	started := time.Now()
	result := make(chan error, 1)
	go func() { result <- cmd.Wait() }()
	idleSince := started
	last := processProgress(cmd.Process.Pid)
	warned := time.Time{}
	diagnosed := false
	var timeoutAt time.Time
	if options.Timeout > 0 {
		timeoutAt = started.Add(options.Timeout)
	}
	for {
		select {
		case err := <-result:
			return commandExit(err)
		default:
		}
		progress := processProgress(cmd.Process.Pid)
		if progress != last {
			last, idleSince = progress, time.Now()
		}
		stuck := time.Since(idleSince) >= options.Idle || (!timeoutAt.IsZero() && time.Now().After(timeoutAt))
		if stuck {
			if !diagnosed {
				diagnosed = true
				snapshot := diagnosticSnapshot(cmd.Process.Pid, record, started)
				_ = updateRecord(h, func(r *Record) { r.Stuck, r.Snapshot = true, snapshot })
				fmt.Fprintf(stderr, "Lock holder appears stuck; process evidence follows (exit evidence is lost once anything is killed):\n%s\nUse `rush lock list`; opt in with --on-stuck=release or --on-stuck=stop. You may terminate PID %d yourself.\n", snapshot, cmd.Process.Pid)
				if options.OnStuck == StuckStop {
					terminateProcess(cmd, options.Grace, options.ForceSafe, stderr)
					select {
					case err := <-result:
						return commandExit(err)
					case <-time.After(options.Grace + time.Second):
					}
				}
				if options.OnStuck == StuckRelease {
					_ = quarantineRecord(h)
					return 0, nil
				}
			}
			if time.Since(warned) >= time.Minute {
				fmt.Fprintf(stderr, "Lock remains held by stuck PID %d; run `rush lock list` or opt in to release/stop.\n", cmd.Process.Pid)
				warned = time.Now()
			}
			select {
			case err := <-result:
				return commandExit(err)
			case <-ctx.Done():
				return 1, ctx.Err()
			case <-time.After(pollInterval):
			}
			continue
		}
		select {
		case err := <-result:
			return commandExit(err)
		case <-ctx.Done():
			return 1, ctx.Err()
		case <-time.After(pollInterval):
		}
	}
}

func commandExit(err error) (int, error) {
	if err == nil {
		return 0, nil
	}
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		return exit.ExitCode(), nil
	}
	return 1, err
}

func quarantineRecord(h *Handle) error {
	var r Record
	if err := readJSON(h.path, &r); err != nil {
		return err
	}
	if r.Token != h.token {
		return nil
	}
	dir := filepath.Join(filepath.Dir(h.path), "quarantine")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	if err := writeJSONExclusive(filepath.Join(dir, fmt.Sprintf("%d-%s.json", r.PID, r.Token)), r); err != nil {
		return err
	}
	return os.Remove(h.path)
}

func updateRecord(h *Handle, fn func(*Record)) error {
	var r Record
	if err := readJSON(h.path, &r); err != nil {
		return err
	}
	if r.Token != h.token {
		return nil
	}
	fn(&r)
	b, err := json.Marshal(r)
	if err != nil {
		return err
	}
	return os.WriteFile(h.path, b, 0o600)
}

func lockDir(name string) string {
	sum := sha256.Sum256([]byte(name))
	return filepath.Join(os.TempDir(), "rush-locks", hex.EncodeToString(sum[:]))
}

func firstWaiter(dir string) (string, error) {
	files, err := os.ReadDir(dir)
	if err != nil {
		return "", err
	}
	for _, f := range files {
		if !f.IsDir() {
			return filepath.Join(dir, f.Name()), nil
		}
	}
	return "", nil
}

func writeJSONExclusive(path string, v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	_, werr := f.Write(b)
	cerr := f.Close()
	if werr != nil {
		_ = os.Remove(path)
		return werr
	}
	return cerr
}

func readJSON(path string, v any) error {
	b, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	return json.Unmarshal(b, v)
}

// processProgress reports CPU/I/O activity where supported; unsupported systems return a stable value.
func processProgress(pid int) uint64 { return processCounters(pid) }

func diagnosticSnapshot(pid int, record Record, started time.Time) string {
	return processSnapshot(pid, record, started)
}

func terminateProcess(cmd *exec.Cmd, grace time.Duration, forceSafe bool, stderr *os.File) {
	gracefulStop(cmd)
	if cmd.Process != nil {
		timer := time.NewTimer(grace)
		defer timer.Stop()
		<-timer.C
	}
	if forceSafe {
		forceStop(cmd)
	} else {
		fmt.Fprintln(stderr, "Force-kill skipped: command class is not declared --force-safe.")
	}
}

// lockDirPathForTest exposes no additional public surface.
var _ = os.ErrNotExist
