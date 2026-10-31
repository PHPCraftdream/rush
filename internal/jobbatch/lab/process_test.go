//go:build windows || linux || darwin || freebsd || openbsd || netbsd || dragonfly

package lab

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/PHPCraftdream/rush/internal/jobbatch"
	"github.com/PHPCraftdream/rush/internal/platform"
	"github.com/PHPCraftdream/rush/internal/testguard"
)

var processTestBuild struct {
	sync.Once
	path string
	dir  string
	err  error
}

func processTestHelper(t *testing.T) string {
	t.Helper()
	processTestBuild.Do(func() {
		processTestBuild.dir, processTestBuild.err = os.MkdirTemp("", "jobbatch-process-")
		if processTestBuild.err != nil {
			return
		}
		processTestBuild.path = filepath.Join(processTestBuild.dir, "helper")
		if runtime.GOOS == "windows" {
			processTestBuild.path += ".exe"
		}
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		cmd := platform.Command(ctx, "go", "build", "-p", "1", "-o", processTestBuild.path, "./testdata/process/helper")
		output, err := cmd.CombinedOutput()
		if err != nil {
			processTestBuild.err = fmt.Errorf("helper build: %w: %s", err, output)
		}
	})
	if processTestBuild.err != nil {
		t.Fatal(processTestBuild.err)
	}
	return processTestBuild.path
}

func TestMain(m *testing.M) {
	cleanup, err := testguard.Isolate()
	if err != nil {
		fmt.Fprintf(os.Stderr, "TestMain: isolate global state: %v\n", err)
		os.Exit(1)
	}
	code := m.Run()
	if processTestBuild.dir != "" {
		_ = os.RemoveAll(processTestBuild.dir)
	}
	cleanup()
	os.Exit(code)
}

func processTestContext(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)
	return ctx
}

func processTestLaunch(program string, args []string, cap *int) jobbatch.Launch {
	data, _ := json.Marshal(Payload{Kind: "process", Program: &program, Args: &args, MaxOutputBytes: cap})
	return jobbatch.Launch{Task: jobbatch.TaskSpec{Kind: "process", Payload: data}}
}

func processTestStart(t *testing.T, ceiling int, cap *int, args ...string) (*processExecution, chan jobbatch.Result) {
	t.Helper()
	executor, err := NewProcessExecutor(ProcessOptions{MaxRetainedBytes: ceiling})
	if err != nil {
		t.Fatal(err)
	}
	reports := make(chan jobbatch.Result, 4)
	helper := processTestHelper(t)
	ctx := processTestContext(t)
	handle, err := executor.Start(ctx, processTestLaunch(helper, args, cap), func(result jobbatch.Result) { reports <- result })
	if err != nil {
		t.Fatal(err)
	}
	execution := handle.(*processExecution)
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if processTestAlive(execution.cmd.Process.Pid) {
			processTestKillPID(execution.cmd.Process.Pid)
		}
		processTestWaitPID(t, execution.cmd.Process.Pid)
		if err := execution.Stop(ctx); err != nil {
			t.Errorf("cleanup stop PID %d: %v", execution.cmd.Process.Pid, err)
		}
		if err := execution.Dispose(ctx); err != nil {
			t.Errorf("cleanup dispose: %v", err)
		}
		if processTestAlive(execution.cmd.Process.Pid) {
			t.Errorf("leaked PID %d", execution.cmd.Process.Pid)
		}
	})
	return execution, reports
}

func processTestReport(t *testing.T, reports <-chan jobbatch.Result) jobbatch.Result {
	t.Helper()
	select {
	case result := <-reports:
		return result
	case <-processTestContext(t).Done():
		t.Fatal("report timeout")
	}
	return jobbatch.Result{}
}

func processTestReady(t *testing.T, p *processExecution, needle string) string {
	t.Helper()
	var text string
	var cursor uint64
	for !strings.Contains(text, needle) || !strings.HasSuffix(text, "\n") {
		chunk, err := p.ReadOutput(processTestContext(t), cursor)
		if err != nil {
			t.Fatal(err)
		}
		text += string(chunk.Data)
		cursor = chunk.Next
		if chunk.EOF {
			t.Fatalf("premature EOF: %q", text)
		}
	}
	return text
}

func TestProcessExitsAndMissingProgram(t *testing.T) {
	// Rejects missing-program acceptance and incorrect nonzero exit codes.
	for _, code := range []int{0, 7} {
		p, reports := processTestStart(t, 128, nil, "exit", strconv.Itoa(code))
		result := processTestReport(t, reports)
		expected := jobbatch.Result{State: jobbatch.StateCompleted}
		if code != 0 {
			expected = jobbatch.Result{State: jobbatch.StateFailed, Code: "exit_7"}
		}
		if result.State != expected.State || result.Code != expected.Code {
			t.Fatalf("result: %+v", result)
		}
		if err := p.Stop(processTestContext(t)); err != nil {
			t.Fatal(err)
		}
		select {
		case extra := <-reports:
			t.Fatalf("duplicate report: %+v", extra)
		default:
		}
	}
	executor, _ := NewProcessExecutor(ProcessOptions{MaxRetainedBytes: 128})
	missing := filepath.Join(t.TempDir(), "missing-program")
	handle, err := executor.Start(processTestContext(t), processTestLaunch(missing, nil, nil), func(jobbatch.Result) { t.Error("rejected start reported") })
	var coded interface{ Code() string }
	if handle != nil || !errors.As(err, &coded) || coded.Code() != "missing_program" {
		t.Fatalf("missing: %v %v", handle, err)
	}
}

func TestProcessMergedOutputRetention(t *testing.T) {
	// Catches separate-stream reordering, oldest-byte retention, raised ceilings and aliased reads.
	for _, limit := range []int{4, 8, 100} {
		p, reports := processTestStart(t, 8, &limit, "print", "abcd", "EFGH", "ijkl")
		_ = processTestReport(t, reports)
		chunk, err := p.ReadOutput(processTestContext(t), 0)
		if err != nil {
			t.Fatal(err)
		}
		retained := min(limit, 8)
		want := "abcdEFGHijkl"[12-retained:]
		if string(chunk.Data) != want || chunk.Next != 12 || chunk.Oldest != uint64(12-retained) || !chunk.Truncated || !chunk.EOF {
			t.Fatalf("chunk: %+v %q", chunk, chunk.Data)
		}
		chunk.Data[0] = '?'
		again, err := p.ReadOutput(processTestContext(t), chunk.Oldest+1)
		if err != nil || string(again.Data) != want[1:] || again.Truncated || !again.EOF {
			t.Fatalf("cursor: %+v %v", again, err)
		}
		original, _ := p.ReadOutput(processTestContext(t), chunk.Oldest)
		if string(original.Data) != want {
			t.Fatal("aliased output")
		}
		end, _ := p.ReadOutput(processTestContext(t), 12)
		if len(end.Data) != 0 || end.Next != 12 || !end.EOF {
			t.Fatalf("EOF: %+v", end)
		}
	}
}

func TestProcessLiveOutputAndRepeatStop(t *testing.T) {
	// Catches null-stdin termination, false live EOF and non-idempotent cancellation.
	p, reports := processTestStart(t, 128, nil, "hold")
	if p.cmd.Stdin != nil {
		t.Fatal("stdin must be nil")
	}
	text := processTestReady(t, p, "ready ")
	pid := p.cmd.Process.Pid
	if !strings.Contains(text, fmt.Sprintf("ready %d\n", pid)) || !processTestAlive(pid) {
		t.Fatalf("ready: %q", text)
	}
	current, _ := p.ReadOutput(processTestContext(t), 0)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := p.ReadOutput(ctx, current.Next); !errors.Is(err, context.Canceled) {
		t.Fatalf("live read: %v", err)
	}
	read := make(chan jobbatch.OutputChunk, 1)
	go func() { chunk, _ := p.ReadOutput(processTestContext(t), current.Next); read <- chunk }()
	if err := p.Stop(processTestContext(t)); err != nil {
		t.Fatal(err)
	}
	if err := p.Stop(processTestContext(t)); err != nil {
		t.Fatal(err)
	}
	result := processTestReport(t, reports)
	if result.State != jobbatch.StateCancelled || result.Code != "" {
		t.Fatalf("result: %+v", result)
	}
	if processTestAlive(pid) {
		t.Fatalf("leader alive at report: %d", pid)
	}
	select {
	case chunk := <-read:
		if !chunk.EOF {
			t.Fatal("no final EOF")
		}
	case <-processTestContext(t).Done():
		t.Fatal("read not woken")
	}
	select {
	case <-reports:
		t.Fatal("duplicate report")
	default:
	}
}

func TestProcessTreeIsolation(t *testing.T) {
	// Catches leader-only kills, premature tree reports and collateral sibling/sentinel kills.
	helper := processTestHelper(t)
	sentinel := platform.Command(context.Background(), helper, "hold")
	pipe, err := sentinel.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := sentinel.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = sentinel.Process.Kill()
		_ = sentinel.Wait()
		processTestWaitPID(t, sentinel.Process.Pid)
		_ = pipe.Close()
		if processTestAlive(sentinel.Process.Pid) {
			t.Errorf("leaked sentinel PID %d", sentinel.Process.Pid)
		}
	})
	line, err := bufio.NewReader(pipe).ReadString('\n')
	if err != nil {
		t.Fatal(err)
	}
	if line != fmt.Sprintf("ready %d\n", sentinel.Process.Pid) {
		t.Fatalf("sentinel: %q", line)
	}
	sentinelOutput := line
	sibling, siblingReports := processTestStart(t, 128, nil, "hold")
	siblingOutput := processTestReady(t, sibling, "ready ")
	tree, reports := processTestStart(t, 256, nil, "spawn")
	output := processTestReady(t, tree, "descendant-ready ")
	var descendant int
	for _, line := range strings.Split(output, "\n") {
		if strings.HasPrefix(line, "descendant-ready ") {
			_, _ = fmt.Sscanf(line, "descendant-ready %d", &descendant)
		}
	}
	if descendant <= 0 || !processTestAlive(descendant) {
		t.Fatalf("descendant handshake: %q", output)
	}
	t.Cleanup(func() {
		if processTestAlive(descendant) {
			processTestKillPID(descendant)
			processTestWaitPID(t, descendant)
			t.Logf("fallback cleanup descendant PID %d", descendant)
		}
	})
	processTestMember(t, tree, descendant)
	if err := tree.Stop(processTestContext(t)); err != nil {
		t.Fatal(err)
	}
	if result := processTestReport(t, reports); result.State != jobbatch.StateCancelled {
		t.Fatalf("result: %+v", result)
	}
	if processTestAlive(descendant) || processTestAlive(tree.cmd.Process.Pid) {
		t.Fatal("tree alive at report")
	}
	if !processTestAlive(sentinel.Process.Pid) || !processTestAlive(sibling.cmd.Process.Pid) {
		t.Fatal("collateral kill")
	}
	if sentinelOutput != fmt.Sprintf("ready %d\n", sentinel.Process.Pid) {
		t.Fatal("sentinel output lost")
	}
	chunk, err := sibling.ReadOutput(processTestContext(t), 0)
	if err != nil || string(chunk.Data) != siblingOutput || chunk.EOF {
		t.Fatalf("sibling: %+v %v", chunk, err)
	}
	select {
	case <-siblingReports:
		t.Fatal("sibling reported")
	default:
	}
	t.Logf("PID checks: leader %d and descendant %d gone; sentinel %d and sibling %d alive", tree.cmd.Process.Pid, descendant, sentinel.Process.Pid, sibling.cmd.Process.Pid)
}

func TestProcessLeaderGoneReportBarrier(t *testing.T) {
	// Catches reporting before descendant termination and relabelling a dead leader as cancelled.
	p, reports := processTestStart(t, 256, nil, "spawn")
	output := processTestReady(t, p, "descendant-ready ")
	var descendant int
	_, _ = fmt.Sscanf(output, "descendant-ready %d", &descendant)
	if descendant <= 0 {
		t.Fatalf("handshake: %q", output)
	}
	t.Cleanup(func() {
		if processTestAlive(descendant) {
			processTestKillPID(descendant)
			processTestWaitPID(t, descendant)
			t.Logf("fallback cleanup descendant PID %d", descendant)
		}
	})
	processTestMember(t, p, descendant)
	if err := p.cmd.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	processTestWaitPID(t, p.cmd.Process.Pid)
	select {
	case <-p.leaderDone:
	case <-processTestContext(t).Done():
		t.Fatal("leader not reaped")
	}
	if !processTestAlive(descendant) {
		t.Fatal("descendant died with leader")
	}
	// A bounded negative observation, not a sleep or PID polling loop.
	bound := time.NewTimer(100 * time.Millisecond)
	defer bound.Stop()
	select {
	case result := <-reports:
		t.Fatalf("report before tree gone: %+v", result)
	case <-bound.C:
	}
	if err := p.Stop(processTestContext(t)); err != nil {
		t.Fatal(err)
	}
	result := processTestReport(t, reports)
	if result.State != jobbatch.StateFailed || result.Code == "" {
		t.Fatalf("natural leader outcome lost: %+v", result)
	}
	processTestWaitPID(t, descendant)
	t.Logf("report barrier confirmed for leader %d descendant %d", p.cmd.Process.Pid, descendant)
}

func TestProcessBlockingCallbackAndCancelledStart(t *testing.T) {
	// Catches callback-blocked Start and side effects on pre-cancelled setup.
	executor, _ := NewProcessExecutor(ProcessOptions{MaxRetainedBytes: 128})
	entered, release := make(chan struct{}), make(chan struct{})
	defer close(release)
	returned := make(chan jobbatch.Execution, 1)
	go func() {
		handle, err := executor.Start(processTestContext(t), processTestLaunch(processTestHelper(t), []string{"exit", "0"}, nil), func(jobbatch.Result) { close(entered); <-release })
		if err != nil {
			t.Error(err)
		}
		returned <- handle
	}()
	var p jobbatch.Execution
	select {
	case p = <-returned:
	case <-processTestContext(t).Done():
		t.Fatal("Start blocked")
	}
	select {
	case <-entered:
	case <-processTestContext(t).Done():
		t.Fatal("callback missing")
	}
	if err := p.Stop(processTestContext(t)); err != nil {
		t.Fatal(err)
	}
	if err := p.(jobbatch.Disposer).Dispose(processTestContext(t)); err != nil {
		t.Fatal(err)
	}
	if processTestAlive(p.(*processExecution).cmd.Process.Pid) {
		t.Fatal("leaked callback process")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	before := processTestChildren(t)
	handle, err := executor.Start(ctx, processTestLaunch(processTestHelper(t), []string{"spawn"}, nil), func(jobbatch.Result) { t.Error("cancelled report") })
	for pid := range processTestChildren(t) {
		if !before[pid] {
			processTestKillPID(pid)
			processTestWaitPID(t, pid)
			t.Errorf("cancelled Start leaked PID %d", pid)
		}
	}
	if runtime.GOOS == "windows" {
		t.Log("cancelled Start: Toolhelp parent-chain helper PID snapshot unchanged")
	}
	if handle != nil || !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled: %v %v", handle, err)
	}
}
