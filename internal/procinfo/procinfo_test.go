package procinfo

import (
	"context"
	"os"
	"os/exec"
	"testing"
)

func TestSelfIdentity(t *testing.T) {
	if !Alive(os.Getpid()) {
		t.Fatal("self is not alive")
	}
	if !tokensSupported {
		if tok := StartToken(os.Getpid()); tok != "" {
			t.Fatalf("token expected empty on this platform: %q", tok)
		}
	} else {
		tok := StartToken(os.Getpid())
		if tok == "" {
			t.Fatal("self token is empty")
		}
		if tok != StartToken(os.Getpid()) {
			t.Fatal("self token changed")
		}
		if StartToken(2147483647) != "" {
			t.Fatal("bogus token is not empty")
		}
	}
	if Alive(0) || Alive(-1) {
		t.Fatal("nonpositive pid is alive")
	}
}

func TestExitedChild(t *testing.T) {
	if os.Getenv("PROCINFO_CHILD") == "1" {
		return
	}
	cmd := exec.CommandContext(context.Background(), os.Args[0], "-test.run=^$")
	cmd.Env = append(os.Environ(), "PROCINFO_CHILD=1")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	if err := cmd.Wait(); err != nil {
		t.Fatal(err)
	}
	if Alive(cmd.Process.Pid) {
		t.Fatal("exited child is alive")
	}
}
