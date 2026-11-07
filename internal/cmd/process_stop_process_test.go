package cmd

import (
	"bufio"
	"context"
	"database/sql"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/PHPCraftdream/rush/internal/platform"
)

func TestProcessStopHelper(t *testing.T) {
	mode := os.Getenv("RUSH_STOP_HELPER")
	if mode == "" {
		return
	}
	if mode == "leaf" {
		signal.Ignore(os.Interrupt)
		fmt.Println("leaf-ready")
		<-time.After(time.Hour)
		return
	}
	if err := platform.AssignToNewJobObject(); err != nil {
		t.Fatal(err)
	}
	stop := InstallProcessStop()
	defer stop()
	marker := os.Getenv("RUSH_STOP_MARKER")
	defer func() { _ = os.WriteFile(marker, []byte("deferred cleanup ran"), 0o600) }()
	conn, err := sql.Open("sqlite", os.Getenv("RUSH_STOP_DB"))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	for _, query := range []string{"PRAGMA journal_mode=WAL", "CREATE TABLE recovery (id INTEGER)", "INSERT INTO recovery VALUES (1)"} {
		if _, err := conn.ExecContext(t.Context(), query); err != nil {
			t.Fatal(err)
		}
	}
	tx, err := conn.BeginTx(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.ExecContext(t.Context(), "INSERT INTO recovery VALUES (2)"); err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	leaf := exec.CommandContext(t.Context(), os.Args[0], "-test.run=^TestProcessStopHelper$")
	leaf.Env = append(os.Environ(), "RUSH_STOP_HELPER=leaf")
	leaf.Stdout, leaf.Stderr = os.Stdout, os.Stderr
	configureStopLeaf(leaf)
	if err := leaf.Start(); err != nil {
		t.Fatal(err)
	}
	if err := trackStopLeaf(leaf.Process.Pid); err != nil {
		_ = leaf.Process.Kill()
		t.Fatal(err)
	}
	commandJournal.mu.Lock()
	fmt.Printf("ready %d\n", leaf.Process.Pid)
	if mode == "console" {
		if _, err := bufio.NewReader(os.Stdin).ReadString('\n'); err != nil {
			t.Fatal(err)
		}
		if err := triggerOwnStopConsole(); err != nil {
			t.Fatal(err)
		}
	}
	<-time.After(time.Hour)
}

func TestProcessStopFirstSignalSkipsCleanupAndRecoversDatabase(t *testing.T) {
	for _, scenario := range processStopScenarios() {
		t.Run(scenario.name, func(t *testing.T) {
			dir := t.TempDir()
			marker, database := filepath.Join(dir, "cleanup-marker"), filepath.Join(dir, "recovery.db")
			command := exec.CommandContext(t.Context(), os.Args[0], "-test.run=^TestProcessStopHelper$")
			command.Dir = dir
			trace := &processStopTrace{}
			command.Stderr = trace
			t.Cleanup(func() {
				if t.Failed() {
					t.Logf("process-stop failure report:\n%s", trace.String())
				}
			})
			command.Env = append(os.Environ(), "RUSH_STOP_HELPER="+scenario.mode, "RUSH_STOP_MARKER="+marker,
				"RUSH_STOP_DB="+database, "RUSH_GLOBAL_CONFIG="+filepath.Join(dir, "config"), "RUSH_GLOBAL_DATA="+filepath.Join(dir, "data"), "GORACE=atexit_sleep_ms=0")
			configureStopParent(command)
			stdout, err := command.StdoutPipe()
			if err != nil {
				t.Fatal(err)
			}
			stdin, err := command.StdinPipe()
			if err != nil {
				t.Fatal(err)
			}
			defer stdin.Close()
			if err := command.Start(); err != nil {
				t.Fatal(err)
			}
			done := make(chan error, 1)
			var leaf *os.Process
			finished := false
			t.Cleanup(func() {
				if !finished {
					if leaf != nil {
						_ = leaf.Kill()
					}
					_ = command.Process.Kill()
					select {
					case <-done:
					case <-time.After(5 * time.Second):
						t.Error("stop helper cleanup did not join the killed process tree")
					}
				}
				if leaf != nil {
					_ = leaf.Release()
				}
			})
			ready := make(chan int, 1)
			go func() {
				scanner := bufio.NewScanner(stdout)
				leafReady, pid, notified := false, 0, false
				for scanner.Scan() {
					line := scanner.Text()
					_, _ = trace.Write([]byte(line + "\n"))
					if line == "leaf-ready" {
						leafReady = true
					}
					if strings.HasPrefix(line, "ready ") {
						pid, _ = strconv.Atoi(strings.TrimPrefix(line, "ready "))
					}
					if leafReady && pid != 0 && !notified {
						ready <- pid
						notified = true
					}
				}
				done <- command.Wait()
				close(done)
			}()
			select {
			case pid := <-ready:
				leaf, err = os.FindProcess(pid)
				if err != nil {
					t.Fatal(err)
				}
			case err := <-done:
				t.Fatalf("helper exited before readiness: %v", err)
			case <-time.After(20 * time.Second):
				t.Fatal("stop helper did not become ready")
			}
			began := time.Now()
			if err := scenario.send(command, stdin); err != nil {
				t.Fatal(err)
			}
			select {
			case err := <-done:
				finished = true
				if exit, ok := err.(*exec.ExitError); !ok || exit.ExitCode() != scenario.code {
					t.Fatalf("first signal exit=%v, want code %d", err, scenario.code)
				}
			case <-time.After(5 * time.Second):
				t.Fatalf("first signal did not close process tree/output within guard; elapsed=%s", time.Since(began))
			}
			if _, err := os.Stat(marker); !os.IsNotExist(err) {
				t.Fatalf("signal ran deferred disk cleanup: %v", err)
			}
			conn, err := sql.Open("sqlite", database)
			if err != nil {
				t.Fatal(err)
			}
			defer conn.Close()
			queryCtx, cancelQuery := context.WithTimeout(t.Context(), 5*time.Second)
			defer cancelQuery()
			var sum int
			if err := conn.QueryRowContext(queryCtx, "SELECT sum(id) FROM recovery").Scan(&sum); err != nil {
				t.Fatal(err)
			}
			if sum != 1 {
				t.Fatalf("uncommitted signal-time write survived: sum=%d", sum)
			}
			var integrity string
			if err := conn.QueryRowContext(t.Context(), "PRAGMA integrity_check").Scan(&integrity); err != nil || integrity != "ok" {
				t.Fatalf("database recovery=%q err=%v", integrity, err)
			}
			if _, err := conn.ExecContext(t.Context(), "INSERT INTO recovery VALUES (3)"); err != nil {
				t.Fatal(err)
			}
		})
	}
}

type processStopScenario struct {
	name, mode string
	code       int
	send       func(*exec.Cmd, io.Writer) error
}
