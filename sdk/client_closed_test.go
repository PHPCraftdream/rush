package sdk_test

// R1-8 lifecycle guards: Open rejects unknown Options.Mode values, and a
// closed Client refuses Run/RunWithCredentials/Messages/Session with
// sdk.ErrClientClosed while the Subscribe methods return an
// already-closed channel. Library mode is used because it needs no disk
// layout and no provider round-trip: the guards fire before any provider
// contact, so the base URL below is never dialed.

import (
	"context"
	"testing"

	"github.com/PHPCraftdream/rush/sdk"
	"github.com/stretchr/testify/require"
)

// deadBaseURL is never dialed: every guard below fires before the
// provider would be contacted.
const deadBaseURL = "http://127.0.0.1:1"

// TestOpenRejectsUnknownMode pins that Open fails explicitly on an
// Options.Mode value outside the {ModeApplication, ModeLibrary} set
// instead of silently treating it as application mode.
func TestOpenRejectsUnknownMode(t *testing.T) {
	t.Parallel()

	_, err := sdk.Open(context.Background(), sdk.Options{Mode: sdk.OpenMode(99)})
	require.Error(t, err)
	require.ErrorContains(t, err, "unknown Options.Mode")
}

// TestClientMethodsAfterClose pins the closed-Client contract: the
// error-returning methods fail with sdk.ErrClientClosed, the Subscribe
// methods return an already-closed channel, and Close itself stays
// idempotent (a repeat call returns the cached first result).
func TestClientMethodsAfterClose(t *testing.T) {
	isolateGlobalConfigForWorkdirTest(t)

	client, err := sdk.Open(context.Background(), sdk.Options{
		Mode:          sdk.ModeLibrary,
		LibraryConfig: libraryConfigFor(deadBaseURL, "sk-closed-guard"),
	})
	require.NoError(t, err)

	first := client.Close()
	require.False(t, first.Forced)
	require.Empty(t, first.CleanupErrors)
	require.Equal(t, first, client.Close(), "repeat Close must return the cached result")

	ctx := context.Background()
	_, err = client.Run(ctx, sdk.RunRequest{Prompt: "after close"})
	require.ErrorIs(t, err, sdk.ErrClientClosed)

	_, err = client.RunWithCredentials(ctx,
		sdk.RunRequest{Prompt: "after close"},
		sdk.CredentialSet(*libraryConfigFor(deadBaseURL, "sk-closed-guard")))
	require.ErrorIs(t, err, sdk.ErrClientClosed)

	_, err = client.Messages(ctx, "no-such-session")
	require.ErrorIs(t, err, sdk.ErrClientClosed)

	_, err = client.Session(ctx, "no-such-session")
	require.ErrorIs(t, err, sdk.ErrClientClosed)

	_, ok := <-client.SubscribeMessages(ctx)
	require.False(t, ok, "SubscribeMessages on a closed Client must return an already-closed channel")

	_, ok = <-client.SubscribeSessions(ctx)
	require.False(t, ok, "SubscribeSessions on a closed Client must return an already-closed channel")
}

// TestZeroValueClientMethodsAfterClose pins the closed admission contract
// for the exported zero value. Close must make every later public method
// safe even though the zero value has no App to call.
func TestZeroValueClientMethodsAfterClose(t *testing.T) {
	var client sdk.Client

	first := client.Close()
	require.False(t, first.Forced)
	require.Empty(t, first.CleanupErrors)
	require.Equal(t, first, client.Close(), "repeat Close must remain idempotent")

	ctx := context.Background()
	_, err := client.Run(ctx, sdk.RunRequest{Prompt: "after zero-value close"})
	require.ErrorIs(t, err, sdk.ErrClientClosed)

	_, err = client.RunWithCredentials(ctx, sdk.RunRequest{Prompt: "after zero-value close"}, sdk.CredentialSet{})
	require.ErrorIs(t, err, sdk.ErrClientClosed)

	_, err = client.Messages(ctx, "no-such-session")
	require.ErrorIs(t, err, sdk.ErrClientClosed)

	_, err = client.Session(ctx, "no-such-session")
	require.ErrorIs(t, err, sdk.ErrClientClosed)

	_, ok := <-client.SubscribeMessages(ctx)
	require.False(t, ok, "SubscribeMessages on a zero-value Client after Close must return a closed channel")

	_, ok = <-client.SubscribeSessions(ctx)
	require.False(t, ok, "SubscribeSessions on a zero-value Client after Close must return a closed channel")
}
