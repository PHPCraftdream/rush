package cmd

import (
	"errors"

	"github.com/PHPCraftdream/rush/internal/config"
)

type tokenCLIError struct {
	code int
	err  error
}

func (e *tokenCLIError) Error() string { return e.err.Error() }
func (e *tokenCLIError) Unwrap() error { return e.err }

func tokenCLIExitCode(err error) (int, bool) {
	if err == nil {
		return 0, false
	}
	var e *tokenCLIError
	if !errors.As(err, &e) {
		return 0, false
	}
	if e.code != 1 && e.code != 2 && e.code != 4 {
		return 0, false
	}
	return e.code, true
}

func tokensExitCode(args []string, err error) (int, bool) {
	// Typed exit codes: any error carrying an explicit process exit
	// code is honored for every subcommand, not just tokens/api/serve.
	// Today only reviewFailError (rush run --fail-on-review) implements
	// this; keep the interface anonymous and local to this check.
	var coder interface{ ExitCode() int }
	if err != nil && errors.As(err, &coder) {
		return coder.ExitCode(), true
	}
	if code, ok := apiExitCode(err); ok && isAPIInvocation(args) {
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

func tokenStoreError(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, config.ErrAPITokenNotFound) {
		return &tokenCLIError{code: 4, err: err}
	}
	return &tokenCLIError{code: 1, err: err}
}
