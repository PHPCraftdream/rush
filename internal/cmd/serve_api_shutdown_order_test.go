package cmd

import (
	"context"
	"net"
	"os"
	"testing"
	"time"

	"github.com/PHPCraftdream/rush/internal/app"
	"github.com/PHPCraftdream/rush/internal/config"
	"github.com/PHPCraftdream/rush/internal/server"
)

func TestServeStopsFlusherBeforeAppShutdown(t *testing.T) {
	globalDir, cwd := serveAPITestSetup(t)
	if _, _, err := config.GlobalAPITokenStore().CreateAPIToken("shutdown-order", []string{"run"}, "ask", nil); err != nil {
		t.Fatal(err)
	}
	oldListen, oldConsume, oldSignals := serveListen, serveConsumeBootstrapToken, serveSignals
	oldAPIStop, oldAppShutdown := serveAPIStop, serveAppShutdown
	t.Cleanup(func() {
		serveListen, serveConsumeBootstrapToken, serveSignals = oldListen, oldConsume, oldSignals
		serveAPIStop, serveAppShutdown = oldAPIStop, oldAppShutdown
	})
	listener, err := (&net.ListenConfig{}).Listen(context.Background(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	serveListen = func(string) (net.Listener, error) { return listener, nil }
	serveConsumeBootstrapToken = func(*config.APITokenStore, time.Time) (config.APIToken, bool, error) {
		return config.APIToken{}, false, nil
	}
	signals := make(chan os.Signal, 1)
	serveSignals = func() (<-chan os.Signal, func()) { return signals, func() {} }
	stopped := false
	serveAPIStop = func(api *server.ServeAPI) error {
		err := api.Stop(context.Background())
		stopped = true
		return err
	}
	originalShutdown := serveAppShutdown
	serveAppShutdown = func(instance *app.App) error {
		if !stopped {
			t.Error("app shutdown ran before API flusher stopped")
		}
		return originalShutdown(instance)
	}
	cmd := serveTestCommand()
	cmd.SetContext(context.Background())
	stderr := &serveReadinessBuffer{ready: make(chan struct{}, 1)}
	cmd.SetErr(stderr)
	_ = cmd.Flags().Set("cwd", cwd)
	_ = cmd.Flags().Set("global-dir", globalDir)
	done := startServeForReadinessTest(t, cmd, signals)
	waitServeReadiness(t, stderr)
	signals <- os.Interrupt
	waitServeStopped(t, done)
	if !stopped {
		t.Fatal("API flusher stop seam was not called")
	}
}
