package server

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"testing"

	"github.com/stretchr/testify/require"
)

const (
	wsDeniedScope = "forbidden: scope"
	wsDeniedAdmin = "forbidden: admin commands are disabled on this listener"
	wsDeniedUnmap = "forbidden: unknown command"
)

var wsScopeLevels = []string{"read", "run", "config", "admin"}

func scopeLevel(scope string) int {
	for i, level := range wsScopeLevels {
		if scope == level {
			return i
		}
	}
	return -1
}

// expectedWSDenial restates the documented rule (design section 5) without
// calling AuthorizeCommand, so a wrong guard cannot agree with itself.
func expectedWSDenial(command, tokenScope string, allowAdmin bool) string {
	needed, ok := commandScopes[command]
	switch {
	case !ok:
		return wsDeniedUnmap
	case needed == "admin" && !allowAdmin:
		return wsDeniedAdmin
	case scopeLevel(tokenScope) < scopeLevel(needed):
		return wsDeniedScope
	}
	return ""
}

func sortedCommands() []string {
	commands := make([]string, 0, len(commandScopes))
	for command := range commandScopes {
		commands = append(commands, command)
	}
	sort.Strings(commands)
	return commands
}

func isForbiddenReply(reply WSMessage) bool {
	return reply.Type == EventError && (reply.Error == wsDeniedScope || reply.Error == wsDeniedAdmin || reply.Error == wsDeniedUnmap)
}

// TestServeWSCommandScopeMatrix sends EVERY command in commandScopes, as a
// token of each scope level, to a serve listener with and without
// allow_admin. Denied commands must produce the exact error envelope with the
// request id echoed, keep the connection usable and have no side effect.
// Allowed commands are executed only when they are side-effect safe here (the
// others are listed in skipExec; their denied side is still covered).
func TestServeWSCommandScopeMatrix(t *testing.T) {
	skipExec := map[string]bool{}
	execSafe := map[string]bool{CmdListSessions: true, CmdGetConfig: true, CmdGetLogs: true, CmdCreateSession: true}
	for _, command := range sortedCommands() {
		if !execSafe[command] {
			skipExec[command] = true
		}
	}
	for _, allowAdmin := range []bool{true, false} {
		t.Run(fmt.Sprintf("allowAdmin=%t", allowAdmin), func(t *testing.T) {
			f := newServeWSScopeFixture(t, allowAdmin, map[string][]string{
				"read": {"read"}, "run": {"run"}, "config": {"config"}, "admin": {"admin"},
			})
			victim, err := f.app.Sessions.Create(t.Context(), "must-survive")
			require.NoError(t, err)
			logPath := f.app.Store().LogPath()
			require.NoError(t, os.MkdirAll(filepath.Dir(logPath), 0o700))
			require.NoError(t, os.WriteFile(logPath, []byte("line one\nline two\n"), 0o600))
			created := 0
			for _, tokenScope := range wsScopeLevels {
				conn := f.dial(t, tokenScope)
				for _, command := range sortedCommands() {
					wantDenial := expectedWSDenial(command, tokenScope, allowAdmin)
					if wantDenial == "" && skipExec[command] {
						continue
					}
					message := WSMessage{ID: tokenScope + "/" + command, Type: command}
					if command == CmdGetLogs {
						message.Payload = json.RawMessage(`{}`)
					}
					if command == CmdDeleteSession {
						message.Payload = json.RawMessage(`{"sessionID":"` + victim.ID + `"}`)
					}
					if wantDenial == "" && command == CmdCreateSession {
						// Success is announced by a session_created broadcast, not by a correlated reply.
						sendCommand(t, conn, message)
						awaitType(t, conn, EventSessionCreated)
						created++
						continue
					}
					reply, _ := exchange(t, conn, message)
					if wantDenial != "" {
						if reply.Type != EventError || reply.ID != message.ID || reply.Error != wantDenial {
							t.Fatalf("%s as %s: reply %+v, want error %q", command, tokenScope, reply, wantDenial)
						}
						continue
					}
					if isForbiddenReply(reply) {
						t.Fatalf("%s as %s must be allowed, got %+v", command, tokenScope, reply)
					}
					if command == CmdGetLogs && reply.Type == EventError {
						t.Fatalf("get_logs for an admin token failed: %+v", reply)
					}
				}
				// Liveness after every denial burst: the connection still works.
				if list, _ := exchange(t, conn, WSMessage{ID: "alive-" + tokenScope, Type: CmdListSessions}); list.Type != EventSessionsList {
					t.Fatalf("list after %s burst = %+v", tokenScope, list)
				}
			}
			// No denied command had a side effect: the victim exists and the
			// only new sessions are the explicitly allowed create_session calls.
			_, err = f.app.Sessions.Get(t.Context(), victim.ID)
			require.NoError(t, err, "a denied delete_session removed the session")
			all, err := f.app.Sessions.List(t.Context())
			require.NoError(t, err)
			require.Len(t, all, 1+created, "sessions after the matrix")
		})
	}
}

// TestServeWSUnmappedCommandDeniedForAdmin pins fail-closed behaviour: a
// command with no table entry is refused even for an admin token on a listener
// that allows admin commands.
func TestServeWSUnmappedCommandDeniedForAdmin(t *testing.T) {
	f := newServeWSScopeFixture(t, true, map[string][]string{"admin": {"admin"}})
	conn := f.dial(t, "admin")
	reply, _ := exchange(t, conn, WSMessage{ID: "unknown", Type: "definitely_not_a_command"})
	if reply.Type != EventError || reply.ID != "unknown" || reply.Error != wsDeniedUnmap {
		t.Fatalf("reply = %+v", reply)
	}
}

// TestServeWSControlPlaneIsGuarded covers the commands that bypass the normal
// worker queue: a read token must be refused before any control action runs
// and the server must keep serving.
func TestServeWSControlPlaneIsGuarded(t *testing.T) {
	f := newServeWSScopeFixture(t, true, map[string][]string{"read": {"read"}})
	conn := f.dial(t, "read")
	sess, err := f.app.Sessions.Create(t.Context(), "control-victim")
	require.NoError(t, err)
	payload := json.RawMessage(`{"sessionID":"` + sess.ID + `"}`)
	for _, command := range []string{CmdCancelAgent, CmdInterruptAndSend, CmdShutdownServer} {
		reply, _ := exchange(t, conn, WSMessage{ID: "control-" + command, Type: command, Payload: payload})
		if reply.Type != EventError || reply.ID != "control-"+command || reply.Error != wsDeniedScope {
			t.Fatalf("%s reply = %+v", command, reply)
		}
	}
	if list, _ := exchange(t, conn, WSMessage{ID: "still-alive", Type: CmdListSessions}); list.Type != EventSessionsList {
		t.Fatalf("server stopped serving after a refused shutdown_server: %+v", list)
	}
}
