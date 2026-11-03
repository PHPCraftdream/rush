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
	"sync"
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
	serveAPIStop               = func(api *server.ServeAPI) error { return api.Stop(context.Background()) }
	serveAuthFactory           func(http.Handler) http.Handler
	serveResolver              = net.LookupIP
)

func serveAppOptions() []app.Option {
	return []app.Option{app.RestrictMCPToCLI()}
}

func init() {
	serveCmd.Flags().String("listen", "", "Override the first configured listener address")
	serveCmd.Flags().String("tls-cert", "", "PEM server certificate for the first listener")
	serveCmd.Flags().String("tls-key", "", "PEM server private key for the first listener")
	serveCmd.Flags().String("client-ca", "", "PEM client CA; require and verify client certificates")
	serveCmd.Flags().Bool("tls-self-signed", false, "Generate or reuse a self-signed TLS certificate")
	serveCmd.Flags().StringSlice("tls-san", nil, "Additional TLS subject alternative name (repeatable)")
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
	cert, _ := cmd.Flags().GetString("tls-cert")
	key, _ := cmd.Flags().GetString("tls-key")
	clientCA, _ := cmd.Flags().GetString("client-ca")
	selfSigned, _ := cmd.Flags().GetBool("tls-self-signed")
	sans, _ := cmd.Flags().GetStringSlice("tls-san")
	flags := config.ServerListenerFlags{}
	if cmd.Flags().Changed("listen") {
		flags.Addr = &listen
	}
	if cmd.Flags().Changed("allow-cleartext") {
		flags.AllowCleartext = &clear
	}
	if cmd.Flags().Changed("tls-cert") {
		flags.TLSCert = &cert
	}
	if cmd.Flags().Changed("tls-key") {
		flags.TLSKey = &key
	}
	if cmd.Flags().Changed("client-ca") {
		flags.TLSClientCA = &clientCA
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
		// The default listener has no config entry to merge the flags into.
		if cmd.Flags().Changed("tls-cert") {
			listeners[0].TLS.Cert = cert
		}
		if cmd.Flags().Changed("tls-key") {
			listeners[0].TLS.Key = key
		}
		if cmd.Flags().Changed("client-ca") {
			listeners[0].TLS.ClientCA = clientCA
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
		policyAuth := serveAuthFactory
		if policyAuth == nil {
			policyAuth = server.DenyAllAuth
		}
		policy := server.ListenerPolicy{Address: l.Addr, Surfaces: surfaces, AllowCleartext: l.AllowCleartext, AllowAdmin: l.AllowAdmin, AllowAutoApprove: l.AllowAutoApprove, AllowedOrigins: l.AllowedOrigins, TokenIDs: l.Tokens, Limits: server.ListenerLimits{RPS: l.Limits.RPS, Burst: l.Limits.Burst, MaxConnsPerToken: l.Limits.MaxConnsPerToken, MaxBody: l.Limits.MaxBody}, Auth: policyAuth}.WithResolver(serveResolver)
		policy.TLS = server.ListenerTLS{Cert: l.TLS.Cert, Key: l.TLS.Key, ClientCA: l.TLS.ClientCA, MinVersion: l.TLS.MinVersion, SelfSigned: l.TLS.SelfSigned, SANs: l.TLS.SANs}
		if len(policies) == 0 && cmd.Flags().Changed("tls-self-signed") {
			policy.TLS.SelfSigned = selfSigned
		}
		if len(policies) == 0 && cmd.Flags().Changed("tls-san") {
			policy.TLS.SANs = append([]string(nil), sans...)
		}
		if policy.TLS.SelfSigned && (policy.TLS.Cert != "" || policy.TLS.Key != "") {
			return &tokenCLIError{code: 2, err: errors.New("self-signed TLS cannot be combined with an explicit certificate or key")}
		}
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
	for i := range policies {
		if policies[i].TLS.SelfSigned {
			cwd := cmdFlagString(cmd, "cwd")
			dataDir, resolveErr := config.ResolveDataDirectory(cwd, cmdFlagString(cmd, "data-dir"))
			if resolveErr != nil {
				for _, listener := range bound {
					_ = listener.Close()
				}
				return &tokenCLIError{code: 2, err: resolveErr}
			}
			certPath, keyPath, certErr := server.EnsureSelfSignedCertificateForListener(dataDir, policies[i].Address, bound[i].Addr().String(), policies[i].TLS.SANs, serveNow())
			if certErr != nil {
				for _, listener := range bound {
					_ = listener.Close()
				}
				return &tokenCLIError{code: 2, err: certErr}
			}
			policies[i].Address = bound[i].Addr().String()
			policies[i].TLS.Cert, policies[i].TLS.Key = certPath, keyPath
			policies[i].TLS.SelfSigned = false
		}
	}
	appInstance, err := serveAppFactory(cmd)
	if err != nil {
		for _, ln := range bound {
			_ = ln.Close()
		}
		return &tokenCLIError{code: 1, err: err}
	}
	apis := make([]*server.ServeAPI, 0, len(policies))
	bootstrapIDs := []string(nil)
	if bootstrapOK {
		bootstrapIDs = []string{bootstrap.ID}
	}
	for i := range policies {
		api, err := server.NewServeAPI(policies[i], appInstance, store, bootstrapIDs)
		if err != nil {
			for _, listener := range bound {
				_ = listener.Close()
			}
			for _, existing := range apis {
				_ = existing.Stop(context.Background())
			}
			_ = serveAppShutdown(appInstance)
			return &tokenCLIError{code: 1, err: err}
		}
		api.Configure(&policies[i])
		if serveAuthFactory != nil {
			policies[i].Auth = serveAuthFactory
			policies[i].RESTHandler = serveAuthFactory(api.RESTHandler)
		}
		apis = append(apis, api)
	}
	ctx, cancel := context.WithCancel(cmd.Context())
	defer cancel()
	signals, stop := serveSignals()
	defer stop()
	watchServeSignals(ctx, cancel, signals)
	servers := make([]*server.Server, 0, len(bound))
	serveDone := make(chan error, len(bound))
	readyCount := 0
	readyMu := sync.Mutex{}
	// The loop below reassigns ctx after the first cancellation; the server
	// goroutines must keep reading the original one, not the variable.
	serveCtx := ctx
	for _, policy := range policies {
		servers = append(servers, server.NewWithPolicy(appInstance, policy, nil))
	}
	for i, s := range servers {
		ln := bound[i]
		go func(s *server.Server, ln net.Listener) {
			serveDone <- s.ServeListener(serveCtx, ln, func(string) {
				readyMu.Lock()
				defer readyMu.Unlock()
				readyCount++
				if readyCount != len(bound) {
					return
				}
				for j, readyServer := range servers {
					_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "rush serve: listening on %s\n", bound[j].Addr().String())
					if pins, pinErr := readyServer.TLSPins(); pinErr == nil && pins.PublicKeyHex != "" {
						_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "rush serve: TLS public-key pin: %s (curl: %s); leaf fingerprint: %s\n", pins.PublicKeyHex, pins.PublicKeyCurl, pins.Certificate)
					}
				}
			})
		}(s, ln)
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
	for _, ln := range bound {
		_ = ln.Close()
	}
	var apiStopErr error
	for _, api := range apis {
		apiStopErr = errors.Join(apiStopErr, serveAPIStop(api))
	}
	shutdownErr := serveAppShutdown(appInstance)
	if serverErr != nil {
		return &tokenCLIError{code: 1, err: serverErr}
	}
	if apiStopErr != nil {
		return &tokenCLIError{code: 1, err: apiStopErr}
	}
	if shutdownErr != nil {
		return &tokenCLIError{code: 1, err: shutdownErr}
	}
	return nil
}
