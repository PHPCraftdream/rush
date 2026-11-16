// Exit-code classification shared by the api/tokens/serve command families.
// Merged from exitcode_api.go + exitcode_tokens.go (lane K1).
package cliout

import (
	"context"
	"errors"

	"github.com/PHPCraftdream/rush/internal/config"
	"github.com/spf13/cobra"
)

// Invocation command objects installed by the cmd package (the moved
// predicates compare against the live tree commands); set via
// SetInvocationCommands before any exit-code classification runs.
var (
	rootCmd   *cobra.Command
	apiCmd    *cobra.Command
	tokensCmd *cobra.Command
	serveCmd  *cobra.Command
)

// SetInvocationCommands installs the live command-tree objects the moved
// is*Invocation predicates need (lane K1 crossing: they were cmd globals).
func SetInvocationCommands(root, api, tokens, serve *cobra.Command) {
	rootCmd, apiCmd, tokensCmd, serveCmd = root, api, tokens, serve
}

// ExitCodeError carries an explicit process exit code (api family).
type ExitCodeError struct {
	code int
	err  error
}

func (e *ExitCodeError) Error() string { return e.err.Error() }
func (e *ExitCodeError) Unwrap() error { return e.err }

// Code returns the carried exit code (exported; lane K1: cmd tests read it).
func (e *ExitCodeError) Code() int { return e.code }

// NewExitCodeError builds the coded API error; exported because the fields
// stay unexported (lane K1 crossing: cmd literals needed a constructor).
func NewExitCodeError(code int, err error) *ExitCodeError {
	return &ExitCodeError{code: code, err: err}
}

func APIExitCode(err error) (int, bool) {
	if errors.Is(err, context.DeadlineExceeded) {
		return 5, true
	}
	var coded *ExitCodeError
	if !errors.As(err, &coded) {
		return 0, false
	}
	switch coded.code {
	case 0, 1, 2, 3, 4, 5, 6, 130:
		return coded.code, true
	default:
		return 0, false
	}
}

// TokenCLIError wraps token-store failures with the token CLI exit codes.
type TokenCLIError struct {
	code int
	err  error
}

func (e *TokenCLIError) Error() string { return e.err.Error() }
func (e *TokenCLIError) Unwrap() error { return e.err }

// NewTokenCLIError builds the token CLI error (see NewExitCodeError).
func NewTokenCLIError(code int, err error) *TokenCLIError {
	return &TokenCLIError{code: code, err: err}
}

func tokenCLIExitCode(err error) (int, bool) {
	if err == nil {
		return 0, false
	}
	var e *TokenCLIError
	if !errors.As(err, &e) {
		return 0, false
	}
	if e.code != 1 && e.code != 2 && e.code != 4 {
		return 0, false
	}
	return e.code, true
}

// TokenCLIExitCode reports the token-family exit code carried by err
// (exported; lane K1: cmd tests keep the lowercase bridge name).
func TokenCLIExitCode(err error) (int, bool) { return tokenCLIExitCode(err) }

// TokenStoreError wraps token-store failures with the token CLI exit codes.
func TokenStoreError(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, config.ErrAPITokenNotFound) {
		return &TokenCLIError{code: 4, err: err}
	}
	return &TokenCLIError{code: 1, err: err}
}

func TokensExitCode(args []string, err error) (int, bool) {
	// Typed exit codes: any error carrying an explicit process exit
	// code is honored for every subcommand, not just tokens/api/serve.
	// Today only reviewFailError (rush run --fail-on-review) implements
	// this; keep the interface anonymous and local to this check.
	var coder interface{ ExitCode() int }
	if err != nil && errors.As(err, &coder) {
		return coder.ExitCode(), true
	}
	if code, ok := APIExitCode(err); ok && isAPIInvocation(args) {
		return code, true
	}
	if code, ok := tokenCLIExitCode(err); ok {
		return code, true
	}
	if err != nil && (isTokensInvocation(args) || isServeInvocation(args) || isAPIInvocation(args)) {
		return 2, true
	}
	return 0, false
}

func isAPIInvocation(args []string) bool {
	command, _, err := rootCmd.Find(args)
	if err != nil || command == nil {
		return len(args) > 0 && args[0] == apiCmd.Name()
	}
	for command != nil {
		if command == apiCmd {
			return true
		}
		command = command.Parent()
	}
	return false
}

func isTokensInvocation(args []string) bool {
	command, _, err := rootCmd.Find(args)
	if err != nil || command == nil {
		return false
	}
	for command != nil {
		if command == tokensCmd {
			return true
		}
		command = command.Parent()
	}
	return false
}

// IsServeInvocation / IsTokensInvocation expose the invocation predicates to
// the cmd package's bridges (lane K1: cmd tests keep the lowercase names).
func IsServeInvocation(args []string) bool { return isServeInvocation(args) }

func IsTokensInvocation(args []string) bool { return isTokensInvocation(args) }

func isServeInvocation(args []string) bool {
	command, _, err := rootCmd.Find(args)
	if err != nil || command == nil {
		// Cobra's Find rejects unknown flags before returning the matched
		// command. Preserve serve's usage exit code for those invocations.
		return len(args) > 0 && args[0] == serveCmd.Name()
	}
	for command != nil {
		if command == serveCmd {
			return true
		}
		command = command.Parent()
	}
	return false
}
