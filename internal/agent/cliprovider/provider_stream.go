// Stream's full request lifecycle: MCP wiring, PTY/pipe process launch,
// the eager scanner goroutine, and the iterator closure with its cleanup
// and wait plumbing. The method is one ~900-line function by design; its
// internals are deliberately kept whole rather than split across seams.

package cliprovider

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"runtime"
	"strings"
	"sync"

	"charm.land/fantasy"
	"github.com/PHPCraftdream/rush/internal/platform"
	"github.com/PHPCraftdream/rush/internal/session"
	gopty "github.com/aymanbagabas/go-pty"
)

func (m *cliModel) Stream(ctx context.Context, call fantasy.Call) (fantasy.StreamResponse, error) {
	yolo := m.yoloFn != nil && m.yoloFn()
	// Fork patch: batch 14 — `rush run` has no human at the keyboard, so any
	// inner CLI process MUST get bypass-permissions or it will hang on the
	// interactive permission prompt. RunNonInteractive sets this context key.
	if !yolo {
		if v, ok := ctx.Value(NonInteractiveContextKey).(bool); ok && v {
			yolo = true
		}
	}

	// Save any attached files (images, etc.) to temp dir so the CLI agent
	// can access them via its file-reading tools.
	attachTmpDir, filePaths, fileErr := saveFileParts(call.Prompt)
	if fileErr != nil {
		slog.Warn("cliprovider: failed to save attachments", "err", fileErr)
	}
	if len(filePaths) > 0 {
		slog.Info("cliprovider: saved attachments to temp dir", "dir", attachTmpDir, "count", len(filePaths))
	}

	prompt := formatPrompt(call.Prompt, filePaths)

	// Will be overridden below if resuming (only the new user message is needed).
	var resumePrompt string

	args := m.spec.BuildArgs(yolo)

	// Apply dynamic reasoning effort from context so the UI toggle takes
	// effect. Dispatch is per-spec: this used to append `--effort <level>` to
	// whatever binary was being launched, which killed every non-claude run
	// the moment a session carried an effort. See effort.go.
	if effort, ok := ctx.Value(ReasoningEffortContextKey).(string); ok {
		args = m.spec.applyEffort(args, effort)
	}

	// Extract session ID from context (set by agent.go before calling Stream).
	sessionID, _ := ctx.Value(SessionIDContextKey).(string)

	// Resume a previous CLI session if available, to leverage API prompt caching.
	// The key includes the model ID so switching models starts a fresh session.
	// We also hash the conversation prefix (all messages except the last user
	// message) and only resume if the hash matches — this detects edits/deletes
	// that would make the CLI session's history stale.
	var resuming bool
	var cliSessionKey string
	var prefixHash uint64
	if m.spec.SupportsResume && sessionID != "" {
		cliSessionKey = sessionID + ":" + m.spec.ModelID
		prefixHash = hashPromptPrefix(call.Prompt)
		if entry, ok := m.cliSessions.Get(cliSessionKey); ok {
			if entry.PrefixHash == prefixHash {
				args = append(args, "--resume", entry.CLISessionID)
				resuming = true
				resumePrompt = extractLatestUserMessage(call.Prompt, filePaths)
				slog.Info("cliprovider: resuming CLI session", "rushSession", sessionID, "cliSession", entry.CLISessionID, "resumePromptLen", len(resumePrompt))
			} else {
				// History was edited/deleted — start fresh CLI session.
				m.cliSessions.Del(cliSessionKey)
				slog.Info("cliprovider: conversation prefix changed, starting fresh CLI session", "rushSession", sessionID)
			}
		}
	}
	if resuming {
		prompt = resumePrompt
	}

	// When running in non-yolo mode with a spec that opts into rush's MCP
	// server, start an in-process MCP server and pass its config to the CLI
	// so tool calls go through rush's permission dialog instead of the CLI's
	// own (invisible) permission prompts.
	// mcpSrv and mcpTmpCfg are cleaned up inside the returned closure, not
	// via defer here — defer in Stream() would fire when Stream() returns
	// (before the closure runs), deleting the config file before claude CLI
	// can read it.
	var mcpSrv *rushMCPServer
	var mcpTmpCfg string     // path to temp MCP config file (claude-style); "" if not used
	var qwenMCPName string   // registered name in ~/.qwen/settings.json; "" if not used
	var geminiMCPName string // registered name in ~/.gemini/settings.json; "" if not used
	// Fork patch: batch 20 — keep the MCP bridge active even in yolo/bypass mode.
	// Before this fix, `!yolo` here meant that `rush run` (which sets yolo=true via
	// NonInteractiveContextKey in batch 14) would skip MCP setup entirely. Inner
	// claude then ran with no --allowedTools and could reach for its native Bash /
	// Write / Task tools, bypassing agentguard (batch 16) and the MCP permission
	// dialog. yolo only controls whether claude needs the bypass-permissions flag,
	// not whether our MCP bridge sits in the loop.
	if m.spec.UseRushMCP && m.perms != nil {
		var err error
		mcpSrv, err = newRushMCPServer(ctx, m.perms, m.sessions, sessionID, m.workingDir, "", m.mcpProxy)
		if err != nil {
			slog.Warn("cliprovider: failed to start MCP server, falling back to CLI permissions", "err", err)
		} else {
			cfgJSON, jsonErr := mcpSrv.mcpConfigJSON()
			if jsonErr != nil {
				slog.Warn("cliprovider: failed to marshal MCP config", "err", jsonErr)
				mcpSrv.stop()
				mcpSrv = nil
			} else {
				// Write the config to a temp file; the claude CLI reads it via --mcp-config.
				tmpFile, tmpErr := os.CreateTemp("", "rush-mcp-*.json")
				if tmpErr != nil {
					slog.Warn("cliprovider: failed to create MCP config temp file", "err", tmpErr)
					mcpSrv.stop()
					mcpSrv = nil
				} else {
					if _, werr := tmpFile.Write(cfgJSON); werr != nil {
						slog.Warn("cliprovider: failed to write MCP config", "err", werr)
						_ = tmpFile.Close()
						_ = os.Remove(tmpFile.Name())
						mcpSrv.stop()
						mcpSrv = nil
					} else {
						_ = tmpFile.Close()
						mcpTmpCfg = tmpFile.Name()
						args = append(args, "--mcp-config", mcpTmpCfg)
						slog.Info("cliprovider: MCP config written", "path", mcpTmpCfg, "addr", mcpSrv.addr)
					}
				}
			}
		}
	}

	// When rush's own MCP server is active, tell the CLI to only allow our
	// MCP tools. This pre-approves them inside the CLI's own permission layer
	// (so calls reach our handlers), while the CLI's built-in tools remain
	// blocked. Rush still shows its own permission dialog in the UI for each
	// tool call via perms.Request() inside the MCP handlers.
	// We also explicitly disallow TodoWrite so the model uses mcp__rush__todos
	// (which persists tasks to the rush session) instead of the CLI-native
	// TodoWrite tool that writes to a local file unknown to the rush UI.
	if mcpSrv != nil {
		allowed := []string{
			// Rush MCP bridge tools (go through rush's permission system).
			"mcp__rush__Bash",
			"mcp__rush__Read",
			"mcp__rush__Write",
			"mcp__rush__Glob",
			"mcp__rush__Grep",
			"mcp__rush__todos",
			// CLI built-in tools that rush doesn't replicate.
			// These are safe read-only or internal tools that don't need
			// rush's permission system.
			"WebSearch",
			"WebFetch",
			"Task",
			"Agent",
		}
		// Include external MCP tools registered on the rush MCP bridge.
		if m.mcpProxy != nil {
			for _, ext := range m.mcpProxy.ListTools() {
				allowed = append(allowed, "mcp__rush__"+ext.ServerName+"__"+ext.Name)
			}
		}
		args = append(
			args,
			"--allowedTools",
			strings.Join(allowed, ","),
			"--disallowedTools",
			"TodoWrite",
		)
	}

	// Qwen MCP integration: register rush's MCP server in ~/.qwen/settings.json
	// using a stable per-project ID stored in <workingDir>/.rush/qwen-mcp-id.
	// Qwen doesn't support --mcp-config, so we write the settings directly.
	// The Authorization: Bearer header is stored in the settings (qwen CLI
	// supports custom headers for httpUrl transports); the server is
	// localhost-only with a random port.
	if m.spec.QwenMCPIntegration && m.perms != nil {
		id, idErr := qwenMCPID(m.workingDir)
		if idErr != nil {
			slog.Warn("cliprovider: failed to get qwen MCP ID", "err", idErr)
		} else {
			var err error
			mcpSrv, err = newRushMCPServer(ctx, m.perms, m.sessions, sessionID, m.workingDir, "", m.mcpProxy)
			if err != nil {
				slog.Warn("cliprovider: failed to start qwen MCP server", "err", err)
			} else {
				// registerQwenMCP below unconditionally replaces
				// mcpServers[id] with a fresh map, so a leading
				// deregister-then-register here was always redundant
				// (removed: it also used to unsafely delete a possibly
				// concurrent session's live entry — see
				// deregisterQwenMCP's own doc for the full story).
				if regErr := registerQwenMCP(id, mcpSrv.addr, mcpSrv.token); regErr != nil {
					slog.Warn("cliprovider: failed to register qwen MCP server", "err", regErr)
					mcpSrv.stop()
					mcpSrv = nil
				} else {
					qwenMCPName = id
					args = append(args, "--allowed-mcp-server-names", id)
					// Restrict qwen to only rush MCP tools so its built-in
					// tools (read_file, glob, etc.) cannot bypass rush's
					// permission system.
					// Also block the native todo_write so the model uses
					// mcp__rush__todos which persists tasks to the rush session.
					args = append(
						args,
						"--allowed-tools",
						"mcp__"+id+"__Bash",
						"mcp__"+id+"__Read",
						"mcp__"+id+"__Write",
						"mcp__"+id+"__Glob",
						"mcp__"+id+"__Grep",
						"mcp__"+id+"__todos",
						"--exclude-tools",
						"todo_write",
					)
				}
			}
		}
	}

	// Gemini MCP integration: register rush's MCP server in ~/.gemini/settings.json
	// using a stable per-project ID. Gemini supports Authorization: Bearer headers and
	// a trust:true flag to bypass its own confirmation prompts, so tool calls go
	// directly to our MCP server which shows rush's permission dialog.
	if m.spec.GeminiMCPIntegration && m.perms != nil {
		id, idErr := geminiMCPID(m.workingDir)
		if idErr != nil {
			slog.Warn("cliprovider: failed to get gemini MCP ID", "err", idErr)
		} else {
			var err error
			mcpSrv, err = newRushMCPServer(ctx, m.perms, m.sessions, sessionID, m.workingDir, "", m.mcpProxy)
			if err != nil {
				slog.Warn("cliprovider: failed to start gemini MCP server", "err", err)
			} else {
				// registerGeminiMCP below unconditionally replaces
				// mcpServers[id] with a fresh map, so a leading
				// deregister-then-register here was always redundant
				// (removed: it also used to unsafely delete a possibly
				// concurrent session's live entry — see
				// deregisterGeminiMCP's own doc for the full story).
				if regErr := registerGeminiMCP(id, mcpSrv.addr, mcpSrv.token); regErr != nil {
					slog.Warn("cliprovider: failed to register gemini MCP server", "err", regErr)
					mcpSrv.stop()
					mcpSrv = nil
				} else {
					geminiMCPName = id
					args = append(args, "--allowed-mcp-server-names", id)
					slog.Info("cliprovider: gemini MCP registered", "name", id, "addr", mcpSrv.addr)
				}
			}
		}
	}

	// Codex MCP integration: pass rush's MCP server URL to codex via -c flag
	// (inline config override). No persistent changes to ~/.codex/config.toml.
	// The token is passed via environment variable and Authorization: Bearer header
	// to avoid leaking secrets in process lists (query params are visible in /proc/<pid>/cmdline).
	if m.spec.CodexMCPIntegration && m.perms != nil {
		var err error
		mcpSrv, err = newRushMCPServer(ctx, m.perms, m.sessions, sessionID, m.workingDir, "", m.mcpProxy)
		if err != nil {
			slog.Warn("cliprovider: failed to start codex MCP server", "err", err)
		} else {
			args = append(args, codexMCPConfigArgs(mcpSrv.addr)...)
			slog.Info("cliprovider: codex MCP configured", "addr", mcpSrv.addr)
		}
	}

	noPTY := m.spec.NoPTY || testDisablePTY
	useStdin := m.spec.AlwaysStdin || noPTY || len(prompt) > effectiveMaxPromptArgLen(m.spec.Binary)
	if !useStdin && m.spec.PromptFlag != "" {
		args = append(args, m.spec.PromptFlag, prompt)
	}

	// procHandle abstracts PTY-backed and pipe-backed processes behind a
	// uniform interface so the streaming loop below is platform-agnostic.
	type procHandle struct {
		stdout   io.Reader
		usingPTY bool
		// kill aborts the process and blocks until all resources are freed.
		kill func()
		// wait blocks until the process exits; returns (stderr output, error).
		// In PTY mode stderr is merged so it is always "".
		wait func() (string, error)
	}

	var proc procHandle

	// childPid is the direct CLI child's pid (0 when no start
	// succeeded). It feeds the stream closure's deferred
	// UntrackProcessTree, which releases Windows tree-teardown state
	// on every exit path of the stream.
	var childPid int

	if !useStdin && !noPTY {
		// Use a PTY so the subprocess (e.g. Node.js claude CLI) sees a TTY on
		// stdout and does not buffer output internally. go-pty supports both
		// Unix PTY and Windows ConPTY transparently.
		//
		// On Windows, ClosePseudoConsole (called by p.Close) is what signals
		// EOF on the output pipe — the process exiting alone does not do it.
		// We therefore run Wait in a goroutine and close the PTY afterwards,
		// which guarantees the scanner always sees EOF on both platforms.
		p, ptyErr := gopty.New()
		if ptyErr == nil {
			// Resize to a very wide terminal to prevent the PTY from hard-wrapping
			// long JSON lines. Claude CLI emits lines that can be many KB; wrapping
			// at the default 80-column width splits them across scanner tokens,
			// causing json.Unmarshal to fail on every partial line.
			_ = p.Resize(8192, 50)
			// Resolve the binary to an absolute path before passing to go-pty.
			// On Windows, go-pty/ConPTY may resolve binary names relative to
			// cmd.Dir instead of PATH, so we do the PATH lookup ourselves —
			// via resolveBinary so a bare "bash" cannot resolve to the WSL
			// launcher ahead of Git Bash/MSYS on PATH (see resolveBinary's
			// doc comment).
			binaryPath := m.spec.Binary
			if resolved, lookErr := resolveBinary(m.spec.Binary); lookErr == nil {
				binaryPath = resolved
			}
			ptycmd := p.CommandContext(ctx, binaryPath, args...)
			ptycmd.Dir = m.workingDir
			if startErr := ptycmd.Start(); startErr == nil {
				// Track the child's tree for teardown. On Windows this
				// assigns a KILL_ON_JOB_CLOSE Job Object that
				// KillProcess terminates (go-pty's ConPTY spawn
				// hand-rolls CreateProcess, so post-Start tracking is
				// the only hook we get). On Unix it is a no-op:
				// go-pty's start() sets SysProcAttr{Setsid: true}, and
				// a session leader is by definition a process-group
				// leader, so KillProcess's group-kill path already
				// applies.
				childPid = trackChildTree(ptycmd.Process, m.dataDir, sessionID)
				// Log command-line diagnostics. In production mode, args are sanitized
				// to remove sensitive values (prompts, tokens). In diagnostic mode
				// (RUSH_CLIPROVIDER_LOG_RAW_PROMPT=1), the full args are logged.
				// The promptHead/promptTail fields are only included in diagnostic mode.
				argsToLog := strings.Join(sanitizeArgs(args), " ")
				if logRawPromptEnabled() {
					slog.Info(
						"cliprovider: using PTY",
						"binary", binaryPath,
						"args", argsToLog,
						"argsCount", len(args),
						"argsByteLen", argsByteLen(args),
						"promptLen", len(prompt),
						"promptHead", clipString(prompt, 200),
						"promptTail", tailString(prompt, 200),
					)
				} else {
					slog.Info(
						"cliprovider: using PTY",
						"binary", binaryPath,
						"args", argsToLog,
						"argsCount", len(args),
						"argsByteLen", argsByteLen(args),
						"promptLen", len(prompt),
					)
				}
				// go-pty's unixPty.Start() (cmd_unix.go) sets cmd.Stdin/Stdout/
				// Stderr = pty.slave directly — our own process keeps its own
				// open handle to the slave end in addition to the child's
				// inherited copy. Unless we close OUR copy, the master read
				// NEVER sees EOF, even after the child (and any grandchildren)
				// exit and close theirs: as long as any open slave fd exists
				// anywhere, the kernel keeps the master side alive. This is not
				// a corner case — it is unconditional on this library, and
				// without this close the scanner blocks forever on every normal
				// (non-cancelled) exit (confirmed by a CI hang: TestStreamExitError
				// timed out after 10m with the scanner parked in Read()).
				//
				// Closing our slave copy immediately after Start() is the
				// standard Unix PTY pattern: it lets the master reach genuine
				// EOF once every *other* holder (the direct child, and any
				// legitimate grandchild that inherited the tty) has also
				// closed its copy — which is exactly the natural-EOF signal
				// the scanner needs, and still respects a grandchild
				// legitimately holding the tty open (mirrors the pipe branch's
				// stderr-holding-grandchild protection). Windows ConPTY has no
				// separate slave fd (UnixPty type assertion fails there), so
				// this is a no-op on Windows and the existing eager-close
				// goroutine below remains the correct mechanism there.
				if up, ok := p.(gopty.UnixPty); ok {
					_ = up.Slave().Close()
				}

				// ptycmd.Wait() runs eagerly in a background goroutine for zombie
				// reaping. p.Close() (closing the master too) is deliberately
				// deferred to wait() — NOT called eagerly here — so a
				// fast-exiting child's already-buffered output can still be
				// drained via the natural EOF above before the master itself
				// is torn down. On Windows ConPTY, the child exit does NOT
				// deliver EOF on the master read pipe at all, so a separate
				// goroutine closes the PTY after the process exits to unblock
				// the scanner.
				var ptyWaitErr error
				ptyWaitDone := make(chan struct{})
				go func() {
					ptyWaitErr = ptycmd.Wait()
					close(ptyWaitDone)
				}()

				var closeOnce sync.Once
				closePTY := func() { closeOnce.Do(func() { _ = p.Close() }) }

				if runtime.GOOS == "windows" {
					// ConPTY: child exit alone does not signal EOF on the master
					// read pipe. Close the PTY as soon as the process exits so
					// the scanner sees EOF. (Not exercised by the test suite —
					// testDisablePTY=true on Windows forces pipe mode.)
					go func() {
						select {
						case <-ptyWaitDone:
						case <-ctx.Done():
						}
						closePTY()
					}()
				}

				var ptyKillOnce sync.Once
				proc = procHandle{
					stdout:   p,
					usingPTY: true,
					kill: func() {
						// Use sync.Once so this is safe to call from multiple goroutines
						// (context-cancel watcher + scanner loop) without double-killing.
						ptyKillOnce.Do(func() {
							if ptycmd.Process != nil {
								_ = session.KillProcess(ptycmd.Process.Pid)
							}
						})
					},
					wait: func() (string, error) {
						// Bound the wait against ctx.Done() for parity with the
						// pipe branch: a grandchild holding the PTY's underlying
						// handles can keep ptycmd.Wait() from returning even after
						// the direct child exits. PTY mode merges stderr into the
						// tty, so there is no stderrBuf to race on here.
						// closePTY() is idempotent (sync.Once): on Unix it cleans
						// up the master fd after the scanner has drained; on
						// Windows it may have already been called by the eager
						// goroutine above (which is fine — Do is a no-op).
						select {
						case <-ptyWaitDone:
							closePTY()
							return "", ptyWaitErr
						case <-ctx.Done():
							closePTY()
							slog.Warn("cliprovider: PTY wait aborted on ctx cancellation", "binary", m.spec.Binary)
							return "", ctx.Err()
						}
					},
				}
			} else {
				_ = p.Close()
				slog.Info("cliprovider: PTY start failed, falling back to pipe", "err", startErr)
			}
		} else {
			slog.Info("cliprovider: PTY unavailable, falling back to pipe", "err", ptyErr)
		}
	}

	if proc.stdout == nil {
		// Pipe fallback: large prompt (stdin required) or PTY unavailable.
		// Resolve the binary explicitly via resolveBinary rather than
		// letting os/exec's own bare-name lookup run at Start(): on Windows
		// that lookup can hand back the WSL launcher (System32\bash.exe) if
		// it precedes Git Bash/MSYS bash on PATH, which cannot run anything
		// given the Windows-style m.workingDir/args we pass below. Falling
		// through to the bare name on lookup failure preserves the previous
		// error surface (os/exec's own "not found" from Start) for the
		// genuinely-missing-binary case.
		binaryPath := m.spec.Binary
		if resolved, lookErr := resolveBinary(m.spec.Binary); lookErr == nil {
			binaryPath = resolved
		}
		cmd := platform.Command(ctx, binaryPath, args...)
		cmd.Dir = m.workingDir
		// Make the child a process-group leader so KillProcess can
		// kill its whole tree (grandchildren included) with one
		// killpg. No-op on Windows, where tree teardown is the Job
		// Object assigned by trackChildTree below.
		configureChildProcessGroup(cmd)
		if useStdin {
			cmd.Stdin = strings.NewReader(prompt)
		}

		// For Codex MCP integration, set the token env var referenced by
		// codexMCPConfigArgs's bearer_token_env_var override — see
		// codexMCPTokenEnvVar's doc for why this stays out of argv.
		if m.spec.CodexMCPIntegration && mcpSrv != nil {
			cmd.Env = append(os.Environ(), codexMCPTokenEnvVar+"="+mcpSrv.token)
		}
		// Log command-line diagnostics. In production mode, args are sanitized
		// to remove sensitive values (prompts, tokens). In diagnostic mode
		// (RUSH_CLIPROVIDER_LOG_RAW_PROMPT=1), the full args are logged.
		// The promptHead/promptTail fields are only included in diagnostic mode.
		argsToLog := strings.Join(sanitizeArgs(args), " ")
		if logRawPromptEnabled() {
			slog.Info(
				"cliprovider: launching pipe mode",
				"binary", m.spec.Binary,
				"args", argsToLog,
				"argsCount", len(args),
				"argsByteLen", argsByteLen(args),
				"useStdin", useStdin,
				"promptLen", len(prompt),
				"promptHead", clipString(prompt, 200),
				"promptTail", tailString(prompt, 200),
				"noPTY", m.spec.NoPTY,
			)
		} else {
			slog.Info(
				"cliprovider: launching pipe mode",
				"binary", m.spec.Binary,
				"args", argsToLog,
				"argsCount", len(args),
				"argsByteLen", argsByteLen(args),
				"useStdin", useStdin,
				"promptLen", len(prompt),
				"noPTY", m.spec.NoPTY,
			)
		}

		// For NoPTY models (e.g. npx wrappers), merge stdout and stderr
		// into a single reader via io.Pipe + concurrent copy goroutines.
		// Claude CLI may send JSON to stderr when the prompt is delivered
		// via stdin instead of -p flag. We use cmd.StdoutPipe/StderrPipe
		// (not os.Pipe) because Go's exec package handles Windows handle
		// inheritance correctly for those.
		var reader io.Reader
		var stderrBuf bytes.Buffer
		if m.spec.NoPTY {
			stdoutPipe, pipeErr := cmd.StdoutPipe()
			if pipeErr != nil {
				return nil, fmt.Errorf("stdout pipe: %w", pipeErr)
			}
			stderrPipe, pipeErr := cmd.StderrPipe()
			if pipeErr != nil {
				return nil, fmt.Errorf("stderr pipe: %w", pipeErr)
			}
			if startErr := cmd.Start(); startErr != nil {
				return nil, fmt.Errorf("start %s: %w", m.spec.Binary, startErr)
			}
			pr, pw := io.Pipe()
			var copyWg sync.WaitGroup
			copyWg.Add(2)
			go func() { _, _ = io.Copy(pw, stdoutPipe); copyWg.Done() }()
			go func() { _, _ = io.Copy(pw, stderrPipe); copyWg.Done() }()
			go func() { copyWg.Wait(); pw.Close() }()
			reader = pr
		} else {
			cmd.Stderr = &stderrBuf
			pipe, pipeErr := cmd.StdoutPipe()
			if pipeErr != nil {
				return nil, fmt.Errorf("stdout pipe: %w", pipeErr)
			}
			if startErr := cmd.Start(); startErr != nil {
				return nil, fmt.Errorf("start %s: %w", m.spec.Binary, startErr)
			}
			reader = pipe
		}
		childPid = trackChildTree(cmd.Process, m.dataDir, sessionID)
		slog.Info("cliprovider: process started", "binary", m.spec.Binary, "pid", cmd.Process.Pid)
		// Do NOT start cmd.Wait() eagerly. Per Go docs on StdoutPipe:
		//   "it is incorrect to call Wait before all reads from the pipe have
		//   completed."
		// Wait() closes the StdoutPipe read end; starting it eagerly races with
		// the scanner — on a fast-exiting child (echo/cat of a small file) the
		// pipe can be closed before the scanner reads the last buffered line,
		// losing data (the CI failure: final usage line lost). Instead, Wait()
		// is started lazily inside the wait() closure below, which is only
		// called after scanDone (the scanner has drained stdout to EOF). The
		// wait() closure is idempotent via sync.Once so it can be safely
		// deferred for cleanup on early-return paths.
		var pipeWaitOnce sync.Once
		var pipeWaitStderr string
		var pipeWaitErr error
		pipeWaitCh := make(chan error, 1)
		proc = procHandle{
			stdout:   reader,
			usingPTY: false,
			kill: func() {
				if cmd.Process != nil {
					_ = session.KillProcess(cmd.Process.Pid)
				}
			},
			wait: func() (string, error) {
				pipeWaitOnce.Do(func() {
					// cmd.Wait() blocks until EVERY process holding the stderr
					// write handle exits, not just the direct child — and the
					// external CLI binaries this launches routinely spawn
					// grandchildren (claude.cmd → cmd.exe → node.exe, MCP
					// servers) that inherit stderr. If the direct child dies
					// (stdout EOFs, scanner loop ends) but a grandchild is still
					// alive holding stderr, an unbounded cmd.Wait() would block
					// forever with no ctx check on this path. Bound it: run
					// Wait in its own goroutine and select against ctx.Done().
					go func() { pipeWaitCh <- cmd.Wait() }()
					select {
					case waitErr := <-pipeWaitCh:
						// Only safe to read stderrBuf after Wait truly completed;
						// os/exec's stderr-copy goroutine is joined by Wait.
						pipeWaitStderr = strings.TrimSpace(stderrBuf.String())
						if pipeWaitStderr != "" {
							slog.Warn("cliprovider: process stderr", "binary", m.spec.Binary, "stderr", pipeWaitStderr)
						}
						pipeWaitErr = waitErr
					case <-ctx.Done():
						// ctx cancelled before Wait returned — stderrBuf may
						// still be written to concurrently by the not-yet-joined
						// stderr-copy goroutine, so do NOT read it (would race).
						// The stderr-holding grandchild is the very thing ctx
						// cancellation (via kill()) is trying to tear down.
						slog.Warn("cliprovider: wait aborted on ctx cancellation; stderr discarded (may be incomplete)", "binary", m.spec.Binary)
						pipeWaitErr = ctx.Err()
					}
				})
				return pipeWaitStderr, pipeWaitErr
			},
		}
	}

	var parsePart func([]byte) (fantasy.StreamPart, bool)
	if m.spec.NewPartParser != nil {
		parsePart = m.spec.NewPartParser()
	}

	// scanResult carries one scanner event: a raw line, or a terminal
	// signal with any scanner error.
	type scanResult struct {
		raw  []byte
		done bool
		err  error
	}
	// Start reading proc.stdout SYNCHRONOUSLY here — before Stream() returns
	// and regardless of when (or whether) the caller starts ranging over the
	// returned iterator. This closes a structural race: the wait goroutine
	// tears down the child's output the instant it exits (PTY branch:
	// p.Close() discards buffered PTY output; pipe branch: cmd.Wait() closes
	// the StdoutPipe read end), and a fast-exiting child (echo / cat of a
	// small file) can finish before a lazily-started scanner reads a single
	// byte, yielding an empty stream. Draining into a buffered channel from
	// the moment the process starts guarantees the output is captured up
	// front. The channel is sized generously so short outputs never block the
	// scanner before a consumer arrives; larger outputs stream over time and
	// backpressure safely (data stays in the OS pipe/PTY buffer, not lost).
	scanCh := make(chan scanResult, 64)
	go func() {
		scanner := bufio.NewScanner(proc.stdout)
		scanner.Buffer(make([]byte, 64*1024), 4*1024*1024)
		for scanner.Scan() {
			b := scanner.Bytes()
			cp := make([]byte, len(b))
			copy(cp, b)
			select {
			case scanCh <- scanResult{raw: cp}:
			case <-ctx.Done():
				return
			}
		}
		// Guard the terminal signal against ctx.Done() as well: the goroutine
		// now runs eagerly (before any consumer), so if the caller abandons
		// the iterator without draining, an unguarded send on a full channel
		// would leak this goroutine.
		select {
		case scanCh <- scanResult{done: true, err: scanner.Err()}:
		case <-ctx.Done():
		}
	}()

	// Abandonment watchdog for the tree-teardown state registered by
	// trackChildTree above. Everything else that releases it — the
	// deferred session.UntrackProcessTree inside the closure below —
	// lives INSIDE the iterator, and an iter.Seq carries no guarantee
	// that a consumer ever starts the range: a caller error between
	// Stream() returning and the loop beginning, or a lazy wrapper
	// whose own consumer disappears (fantasy's object.StreamWithTool
	// returns exactly such a lazy wrapper around this stream), would
	// leave the trackedJobs entry and its Job Object handle registered
	// forever — and after the child exits, a stale entry keyed by a pid
	// the OS is free to recycle. The watchdog arms here, in Stream's
	// synchronous part, stands down the moment iteration starts, and on
	// ctx cancellation — which every real abandon path funnels through —
	// performs the same kill the closure's own ctx watcher would have
	// (proc.kill races the closure safely: on Windows the tracked entry
	// is consumed exactly once under trackedJobsMu, on Unix a second
	// kill targets an already-dead process group). Residual gap,
	// accepted: a caller that neither iterates nor cancels leaks the
	// child process itself, which no fix inside this function survives.
	iterStarted := make(chan struct{})
	var iterStartedOnce sync.Once
	if childPid != 0 {
		go func() {
			select {
			case <-iterStarted:
			case <-ctx.Done():
				proc.kill()
			}
		}()
	}

	return func(yield func(fantasy.StreamPart) bool) {
		// Signal the abandonment watchdog (see above) that the closure
		// has taken ownership of cleanup: from here on every exit path
		// runs the deferred UntrackProcessTree/wait below. The Once
		// guards against a legal second iteration of the same Seq.
		iterStartedOnce.Do(func() { close(iterStarted) })
		// Cleanup MCP resources when the stream ends (cannot use defer in
		// Stream() because that fires before the closure executes).
		if mcpSrv != nil {
			defer mcpSrv.stop()
		}
		if mcpTmpCfg != "" {
			defer os.Remove(mcpTmpCfg)
		}
		if attachTmpDir != "" {
			defer os.RemoveAll(attachTmpDir)
		}
		if qwenMCPName != "" {
			// Pass the exact addr this call registered (mcpSrv is
			// guaranteed non-nil here: qwenMCPName is only set after a
			// successful registerQwenMCP above, using this same mcpSrv) —
			// see deregisterQwenMCP's doc for why an unconditional delete
			// is unsafe.
			defer deregisterQwenMCP(qwenMCPName, mcpSrv.addr)
		}
		if geminiMCPName != "" {
			defer deregisterGeminiMCP(geminiMCPName, mcpSrv.addr)
		}

		// Release tree-teardown state on every exit path. On Windows
		// this closes the KILL_ON_JOB_CLOSE job handle, killing any
		// straggler descendants still inside the job; it must run
		// after the wait below, hence the registration order (defers
		// are LIFO).
		defer func() {
			session.UntrackProcessTree(childPid)
			session.UnregisterChildGroup(m.dataDir, sessionID, childPid)
		}()
		// Ensure the child process is reaped and its output fds are cleaned up
		// on EVERY exit path — including early returns from ctx-cancel and
		// yield-false. proc.wait() is idempotent (sync.Once internally), so
		// calling it here in addition to the explicit calls in the normal and
		// scanErr paths below is safe: the second call is a no-op. Without
		// this defer, the ctx-cancel and yield-false paths would skip Wait()
		// entirely, leaving a zombie process and leaking the StdoutPipe/PTY fd.
		defer func() { _, _ = proc.wait() }()

		// Kill the subprocess immediately when ctx is cancelled, even while
		// scanner.Scan() is blocking between CLI output lines (e.g. during a
		// long MCP tool call). Without this goroutine the cancellation would
		// only be observed at the next ctx.Done() check inside the scan loop,
		// which requires a new line to arrive first.
		killDone := make(chan struct{})
		defer close(killDone)
		go func() {
			select {
			case <-ctx.Done():
				proc.kill()
			case <-killDone:
			}
		}()

		const textID = "0"
		if !yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeTextStart, ID: textID}) {
			proc.kill()
			return
		}

		// toolCh is the read side of the MCP tool-event channel.
		// When nil (no MCP server), selecting on it never fires.
		var toolCh <-chan mcpToolEvent
		if mcpSrv != nil {
			toolCh = mcpSrv.toolCh
		}

		var finalUsage fantasy.Usage
		scanDone := false
		var scanErr error
		var linesSeen int
		for !scanDone {
			select {
			case <-ctx.Done():
				proc.kill()
				yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeError, Error: ctx.Err()}) //nolint:errcheck
				return

			case ev := <-toolCh:
				// Emit ToolInputStart + Delta + End from the MCP tool event.
				id := ev.id
				if ev.name != "" {
					// start event
					if !yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeToolInputStart, ID: id, ToolCallName: ev.name}) {
						proc.kill()
						return
					}
					if ev.input != "" {
						if !yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeToolInputDelta, ID: id, Delta: ev.input}) {
							proc.kill()
							return
						}
					}
				} else {
					// end event
					if !yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeToolInputEnd, ID: id}) {
						proc.kill()
						return
					}
				}

			case res := <-scanCh:
				if res.done {
					scanDone = true
					scanErr = res.err
					break
				}
				linesSeen++

				// Strip ANSI/VT sequences that PTY drivers (especially Windows ConPTY)
				// inject into the output stream. JSON parsers need clean bytes.
				raw := res.raw
				slog.Debug("cliprovider: raw line", "raw", string(raw))
				line := bytes.TrimSpace(ansiEscape.ReplaceAll(raw, nil))

				// Capture CLI session ID from the system init event for --resume.
				if m.spec.SupportsResume && cliSessionKey != "" {
					var initEv streamEvent
					if json.Unmarshal(line, &initEv) == nil && initEv.Type == "system" && initEv.Subtype == "init" && initEv.SessionID != "" {
						m.cliSessions.Set(cliSessionKey, cliSessionEntry{CLISessionID: initEv.SessionID, PrefixHash: prefixHash})
						slog.Info("cliprovider: captured CLI session ID", "key", cliSessionKey, "cliSession", initEv.SessionID)
					}
				}

				if m.spec.ParseUsageLine != nil {
					if u, ok := m.spec.ParseUsageLine(line); ok {
						finalUsage = u
					}
				}

				var part fantasy.StreamPart
				if parsePart != nil {
					var ok bool
					part, ok = parsePart(line)
					if !ok {
						continue
					}
				} else {
					clean := strings.TrimSpace(string(line))
					if clean == "" {
						continue
					}
					part = fantasy.StreamPart{
						Type:  fantasy.StreamPartTypeTextDelta,
						ID:    textID,
						Delta: clean + "\n",
					}
				}

				if !yield(part) {
					proc.kill()
					return
				}
			}
		}

		// os.ErrClosed ("read |0: file already closed") is a benign
		// end-of-stream signal, not a real I/O failure. cmd.Wait() is no
		// longer started eagerly (it now runs lazily from proc.wait(), only
		// after the scanner itself reaches scanDone), so this can no longer be
		// caused by a concurrent Wait() racing the scanner's read. It is kept
		// as a defensive, platform-agnostic classification: some OS/exec
		// implementations can still surface a "file already closed" read
		// error as part of normal teardown once the child has exited. Treat
		// it like EOF and fall through to the normal proc.wait() path, which
		// still surfaces a genuine error if the child exited non-zero.
		if scanErr != nil && !errors.Is(scanErr, io.EOF) && !errors.Is(scanErr, os.ErrClosed) {
			// PTY master returns EIO (Unix) or similar when child exits.
			// Treat any scanner error in PTY mode as normal end-of-stream.
			if !proc.usingPTY {
				_, _ = proc.wait()
				yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeError, Error: scanErr}) //nolint:errcheck
				return
			}
		}

		stderr, waitErr := proc.wait()
		// Fork patch (operator UX): also log stderr content (clipped) and
		// total lines seen on stdout. The original log said "err=null
		// stderrLen=0" for the silent-claude-exit bug which gave the
		// operator nothing actionable. Now they see if anything was
		// emitted at all and what the stderr tail looked like.
		slog.Info(
			"cliprovider: process finished",
			"binary", m.spec.Binary,
			"err", waitErr,
			"stderrLen", len(stderr),
			"stderrTail", tailString(stderr, 500),
			"linesSeen", linesSeen,
		)
		// If resume failed, clear the stale CLI session mapping so next call starts fresh.
		if waitErr != nil && resuming && cliSessionKey != "" {
			m.cliSessions.Del(cliSessionKey)
			slog.Warn("cliprovider: resume failed, cleared CLI session mapping", "key", cliSessionKey)
		}
		if waitErr != nil {
			var exitErr error
			if stderr != "" {
				exitErr = fmt.Errorf("%s failed: %w\nstderr: %s", m.spec.Binary, waitErr, stderr)
			} else {
				exitErr = fmt.Errorf("%s failed: %w", m.spec.Binary, waitErr)
			}
			yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeError, Error: exitErr}) //nolint:errcheck
			return
		}

		yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeTextEnd, ID: textID}) //nolint:errcheck
		yield(fantasy.StreamPart{                                                  //nolint:errcheck
			Type:         fantasy.StreamPartTypeFinish,
			FinishReason: fantasy.FinishReasonStop,
			Usage:        finalUsage,
		})
	}, nil
}
