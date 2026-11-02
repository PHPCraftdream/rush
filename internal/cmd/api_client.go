package cmd

import (
	"context"
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/PHPCraftdream/rush/internal/apiclient"
	"github.com/PHPCraftdream/rush/internal/config"
	"github.com/spf13/cobra"
)

func newAPIClient(cmd *cobra.Command) (*apiclient.Client, error) {
	_, remote, err := config.ResolveAPIRemote(apiFlagString(cmd, "remote"), os.Getenv)
	if err != nil {
		return nil, apiConfigurationError(err)
	}
	token, err := config.ReadAPIRemoteToken(remote, os.Getenv)
	if err != nil {
		return nil, apiConfigurationError(err)
	}
	ca := remote.CA
	if cmd.Flags().Changed("ca") {
		ca = apiFlagString(cmd, "ca")
	}
	pin := remote.Pin
	if cmd.Flags().Changed("pin") {
		pin = apiFlagString(cmd, "pin")
	}
	cert := remote.ClientCert
	if cmd.Flags().Changed("client-cert") {
		cert = apiFlagString(cmd, "client-cert")
	}
	key := remote.ClientKey
	if cmd.Flags().Changed("client-key") {
		key = apiFlagString(cmd, "client-key")
	}
	allowCleartext := remote.AllowCleartext
	if cmd.Flags().Changed("allow-cleartext") {
		allowCleartext, _ = cmd.Flags().GetBool("allow-cleartext")
	}
	insecure, _ := cmd.Flags().GetBool("insecure-skip-verify")
	if _, err := apiTimeout(cmd); err != nil {
		return nil, err
	}
	options := apiclient.Options{
		BaseURL: remote.URL, Token: token, CAFile: ca, Pin: pin,
		ClientCertFile: cert, ClientKeyFile: key,
		AllowCleartext: allowCleartext, InsecureSkipVerify: insecure,
	}
	client, warnings, err := apiclient.New(options)
	for _, warning := range warnings {
		fmt.Fprintln(cmd.ErrOrStderr(), "WARNING: "+warning)
	}
	if err != nil {
		return nil, &exitCodeError{code: apiclient.ExitCodeFor(err), err: err}
	}
	return client, nil
}

func apiConfigurationError(err error) error {
	return &exitCodeError{code: 2, err: err}
}

// apiCommandContext bounds a complete API operation, not merely its HTTP
// response-header wait. Simple REST leaf commands should pass 60*time.Second;
// streaming and wait commands should pass 0 unless --timeout was provided.
// A leaf must use the returned context for its client call and defer cancel.
func apiCommandContext(cmd *cobra.Command, defaultTimeout time.Duration) (context.Context, context.CancelFunc, error) {
	timeout, err := apiTimeout(cmd)
	if err != nil {
		return nil, nil, err
	}
	if timeout == 0 {
		timeout = defaultTimeout
	}
	if timeout > 0 {
		ctx, cancel := context.WithTimeout(cmd.Context(), timeout)
		return ctx, cancel, nil
	}
	return cmd.Context(), func() {}, nil
}

func apiTimeout(cmd *cobra.Command) (time.Duration, error) {
	duration, err := cmd.Flags().GetDuration("timeout")
	if err != nil || duration < 0 {
		if err == nil {
			err = errors.New("--timeout must not be negative")
		}
		return 0, &exitCodeError{code: 2, err: err}
	}
	return duration, nil
}
