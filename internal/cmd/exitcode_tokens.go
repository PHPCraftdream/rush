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
	if code, ok := tokenCLIExitCode(err); ok {
		return code, true
	}
	if err != nil && isTokensInvocation(args) {
		return 2, true
	}
	return 0, false
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

func tokenStoreError(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, config.ErrAPITokenNotFound) {
		return &tokenCLIError{code: 4, err: err}
	}
	return &tokenCLIError{code: 1, err: err}
}
