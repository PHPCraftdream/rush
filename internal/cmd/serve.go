package cmd

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/PHPCraftdream/rush/internal/app"
	"github.com/PHPCraftdream/rush/internal/config"
	"github.com/PHPCraftdream/rush/internal/server"
	"github.com/spf13/cobra"
)

var serveSignals = func() (<-chan os.Signal, func()) {
	ch := make(chan os.Signal, 1)
	signal.Notify(ch, os.Interrupt, syscall.SIGTERM)
	return ch, func() { signal.Stop(ch) }
}

var (
	serveNow        = time.Now
	serveListen     = func(addr string) (net.Listener, error) { return net.Listen("tcp", addr) }
	serveAppFactory = func(cmd *cobra.Command) (*app.App, error) {
		return setupAppWithOptions(cmd, serveAppOptions())
	}
	serveAppShutdown = func(instance *app.App) error {
		result := instance.ShutdownWithResult()
		if len(result.CleanupErrors) > 0 {
			return errors.Join(result.CleanupErrors...)
		}
		if result.Forced {
			return errors.New("app shutdown was forced")
		}
		return nil
	}
	serveConsumeBootstrapToken = config.ConsumeServeBootstrapTokenFromEnv
	serveAuthFactory           = server.DenyAllAuth
	serveResolver              = net.LookupIP
)

func serveAppOptions() []app.Option {
	return []app.Option{app.RestrictMCPToCLI()}
}

func init() {
	serveCmd.Flags().String("listen", "", "Override the first configured listener address")
	serveCmd.Flags().Bool("allow-cleartext", false, "Allow cleartext on non-loopback listeners")
	serveCmd.Flags().Bool("print-config", false, "Print effective listener configuration without starting")
	rootCmd.AddCommand(serveCmd)
}

var serveCmd = &cobra.Command{
	Use:   "serve",
	Short: "Start the authenticated headless API server",
	Long: `Start the headless API server for the explicitly selected workspace.
No UI, browser, clipboard, or transient UI token is used.

Exit codes:
  0 clean stop
  1 runtime or store error
  2 usage/configuration error`,
	Example: "  rush serve --cwd ./project\n  rush serve --cwd ./project --listen 127.0.0.1:8443 --print-config",
	Hidden:  true,
	Args:    cobra.NoArgs,
	RunE:    runServe,
}

func listenerSurfaceNames(surfaces []server.ListenerSurface) []string {
	names := make([]string, 0, len(surfaces))
	for _, surface := range surfaces {
		names = append(names, string(surface))
	}
	return names
}

func tokenStoreGuidance(err error, cwd, globalDir string) error {
	return fmt.Errorf("%w; if no API token exists, create one with: %s", err, tokenCreateCommand(cwd, globalDir))
}

func tokenCreateCommand(cwd, globalDir string) string {
	command := "rush tokens create --name <n> --scope run"
	if cwd != "" {
		command += " --cwd " + fmt.Sprintf("%q", cwd)
	}
	if globalDir != "" {
		command += " --global-dir " + fmt.Sprintf("%q", globalDir)
	}
	return command
}

func serveGlobalDir(cmd *cobra.Command) string {
	globalDir, _ := cmd.Flags().GetString("global-dir")
	if globalDir == "" {
		globalDir = os.Getenv(config.GlobalDirEnv)
	}
	return globalDir
}

func watchServeSignals(ctx context.Context, cancel context.CancelFunc, signals <-chan os.Signal) {
	go func() {
		select {
		case <-signals:
			cancel()
		case <-ctx.Done():
		}
	}()
}

func runServe(cmd *cobra.Command, _ []string) error {
	if cmd.Context() == nil {
		cmd.SetContext(context.Background())
	}
	if !cmd.Flags().Changed("cwd") || strings.TrimSpace(cmdFlagString(cmd, "cwd")) == "" {
		return &tokenCLIError{code: 2, err: errors.New("rush serve requires an explicit --cwd")}
	}
	if value, err := cmd.Flags().GetString("global-dir"); err == nil {
		applyGlobalDir(value)
	}
	listen, _ := cmd.Flags().GetString("listen")
	clear, _ := cmd.Flags().GetBool("allow-cleartext")
	flags := config.ServerListenerFlags{}
	if cmd.Flags().Changed("listen") {
		flags.Addr = &listen
	}
	if cmd.Flags().Changed("allow-cleartext") {
		flags.AllowCleartext = &clear
	}
	listeners, fieldSources, err := config.GlobalServerListenersWithSources(flags)
	if err != nil {
		code := 2
		if errors.Is(err, config.ErrListenerConfigRead) {
			code = 1
		}
		return &tokenCLIError{code: code, err: err}
	}
	configuredListeners := len(listeners) > 0
	if len(listeners) == 0 {
		addr := "127.0.0.1:8080"
		if cmd.Flags().Changed("listen") {
			addr = listen
		}
		listeners = []config.ServerListener{{Addr: addr, Surfaces: []string{"rest", "ws"}, AllowCleartext: false}}
		if cmd.Flags().Changed("allow-cleartext") {
			listeners[0].AllowCleartext = clear
		}
	}
	policies := make([]server.ListenerPolicy, 0, len(listeners))
	for _, l := range listeners {
		if l.Surfaces == nil {
			l.Surfaces = []string{"rest", "ws"}
		}
		surfaces := make([]server.ListenerSurface, 0, len(l.Surfaces))
		for _, surface := range l.Surfaces {
			surfaces = append(surfaces, server.ListenerSurface(surface))
		}
		for _, surface := range surfaces {
			if surface == server.SurfaceUI {
				return &tokenCLIError{code: 2, err: errors.New("UI surface is not permitted for serve policies")}
			}
		}
		policy := server.ListenerPolicy{Address: l.Addr, Surfaces: surfaces, AllowCleartext: l.AllowCleartext, AllowAdmin: l.AllowAdmin, AllowAutoApprove: l.AllowAutoApprove, AllowedOrigins: l.AllowedOrigins, TokenIDs: l.Tokens, Limits: server.ListenerLimits{RPS: l.Limits.RPS, Burst: l.Limits.Burst, MaxConnsPerToken: l.Limits.MaxConnsPerToken, MaxBody: l.Limits.MaxBody}, Auth: serveAuthFactory}.WithResolver(serveResolver)
		policy.TLS = server.ListenerTLS{Cert: l.TLS.Cert, Key: l.TLS.Key, ClientCA: l.TLS.ClientCA, MinVersion: l.TLS.MinVersion}
		if cmd.Flags().Changed("allow-cleartext") && len(policies) == 0 {
			policy.AllowCleartext = clear
		}
		resolved, err := policy.ResolvedAddress()
		if err != nil {
			return &tokenCLIError{code: 2, err: err}
		}
		policy.Address = resolved
		if err := policy.Validate(); err != nil {
			return &tokenCLIError{code: 2, err: err}
		}
		policies = append(policies, policy)
	}
	if err := server.ValidateListenerSet(policies); err != nil {
		return &tokenCLIError{code: 2, err: err}
	}
	if printConfig, _ := cmd.Flags().GetBool("print-config"); printConfig {
		var lines []string
		for i, policy := range policies {
			fieldSource := func(field string) string {
				if i == 0 && field == "addr" && cmd.Flags().Changed("listen") {
					return "flag"
				}
				if i == 0 && field == "allow_cleartext" && cmd.Flags().Changed("allow-cleartext") {
					return "flag"
				}
				if configuredListeners && i < len(listeners) && i < len(fieldSources) && fieldSources[i][field] != "" {
					return fieldSources[i][field]
				}
				return "default"
			}
			addrSource := fieldSource("addr")
			surfacesSource := fieldSource("surfaces")
			clearSource := fieldSource("allow_cleartext")
			adminSource := fieldSource("allow_admin")
			autoApproveSource := fieldSource("allow_auto_approve")
			originsSource := fieldSource("allowed_origins")
			tokensSource := fieldSource("tokens")
			rpsSource := fieldSource("limits.rps")
			burstSource := fieldSource("limits.burst")
			connsSource := fieldSource("limits.max_conns_per_token")
			bodySource := fieldSource("limits.max_body")
			certSource := fieldSource("tls.cert")
			keySource := fieldSource("tls.key")
			caSource := fieldSource("tls.client_ca")
			versionSource := fieldSource("tls.min_version")
			lines = append(lines, fmt.Sprintf("address: %s (source: %s)\nsurfaces: %s (source: %s)\nallow_cleartext: %t (source: %s)\nallow_admin: %t (source: %s)\nallow_auto_approve: %t (source: %s)\nallowed_origins: %s (source: %s)\ntoken_ids: %s (source: %s)\nlimits.rps: %g (source: %s)\nlimits.burst: %d (source: %s)\nlimits.max_conns_per_token: %d (source: %s)\nlimits.max_body: %d (source: %s)\ntls.cert: %s (source: %s)\ntls.key: %s (source: %s)\ntls.client_ca: %s (source: %s)\ntls.min_version: %s (source: %s)", policy.Address, addrSource, strings.Join(listenerSurfaceNames(policy.Surfaces), ","), surfacesSource, policy.AllowCleartext, clearSource, policy.AllowAdmin, adminSource, policy.AllowAutoApprove, autoApproveSource, strings.Join(policy.AllowedOrigins, ","), originsSource, strings.Join(policy.TokenIDs, ","), tokensSource, policy.Limits.RPS, rpsSource, policy.Limits.Burst, burstSource, policy.Limits.MaxConnsPerToken, connsSource, policy.Limits.MaxBody, bodySource, policy.TLS.Cert, certSource, policy.TLS.Key, keySource, policy.TLS.ClientCA, caSource, policy.TLS.MinVersion, versionSource))
		}
		_, err := fmt.Fprintln(cmd.OutOrStdout(), strings.Join(lines, "\n"))
		return err
	}
	store := config.GlobalAPITokenStore()
	now := serveNow()
	bootstrap, bootstrapOK, err := serveConsumeBootstrapToken(store, now)
	_ = bootstrap
	if err != nil {
		return &tokenCLIError{code: 1, err: tokenStoreGuidance(err, cmdFlagString(cmd, "cwd"), serveGlobalDir(cmd))}
	}
	tokens, err := store.ListAPITokens()
	if err != nil {
		return &tokenCLIError{code: 1, err: tokenStoreGuidance(err, cmdFlagString(cmd, "cwd"), serveGlobalDir(cmd))}
	}
	usable := bootstrapOK
	for _, token := range tokens {
		if token.RevokedAt == nil && (token.Expires == nil || now.Before(*token.Expires)) {
			usable = true
			break
		}
	}
	if !usable {
		return &tokenCLIError{code: 2, err: fmt.Errorf("no usable API token; create one with: %s", tokenCreateCommand(cmdFlagString(cmd, "cwd"), serveGlobalDir(cmd)))}
	}
	bound := make([]net.Listener, 0, len(policies))
	for _, policy := range policies {
		ln, err := serveListen(policy.Address)
		if err != nil {
			for _, old := range bound {
				_ = old.Close()
			}
			return &tokenCLIError{code: 1, err: err}
		}
		bound = append(bound, ln)
	}
	appInstance, err := serveAppFactory(cmd)
	if err != nil {
		for _, ln := range bound {
			_ = ln.Close()
		}
		return &tokenCLIError{code: 1, err: err}
	}
	ctx, cancel := context.WithCancel(cmd.Context())
	defer cancel()
	signals, stop := serveSignals()
	defer stop()
	watchServeSignals(ctx, cancel, signals)
	servers := make([]*server.Server, 0, len(bound))
	serveDone := make(chan error, len(bound))
	for i, policy := range policies {
		s := server.NewWithPolicy(appInstance, policy, nil)
		servers = append(servers, s)
		ln := bound[i]
		go func() { serveDone <- s.ServeListener(ctx, ln, nil) }()
	}
	var serverErr error
	remaining := len(servers)
	for remaining > 0 {
		select {
		case err := <-serveDone:
			remaining--
			if err != nil && !errors.Is(err, http.ErrServerClosed) && serverErr == nil {
				serverErr = err
				cancel()
			}
		case <-ctx.Done():
			for _, s := range servers {
				s.RequestShutdown()
			}
			ctx = context.Background()
		}
	}
	shutdownErr := serveAppShutdown(appInstance)
	if serverErr != nil {
		return &tokenCLIError{code: 1, err: serverErr}
	}
	if shutdownErr != nil {
		return &tokenCLIError{code: 1, err: shutdownErr}
	}
	return nil
}
