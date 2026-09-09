package config

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/PHPCraftdream/rush/internal/env"
	"github.com/PHPCraftdream/rush/internal/session"
	"github.com/stretchr/testify/require"
)

// fakeExpander returns a canned value/error for the last passed value and
// records the context, raw value, and env slice it was called with. It
// lets the config-layer tests assert on delegation behaviour without
// spinning up a real interpreter — real-shell coverage lives in
// internal/shell/expand_test.go and resolve_real_test.go.
type fakeExpander struct {
	expand    func(ctx context.Context, value string, env []string) (string, error)
	lastValue string
	lastEnv   []string
	calls     int
}

func (f *fakeExpander) Expand(ctx context.Context, value string, env []string) (string, error) {
	f.calls++
	f.lastValue = value
	f.lastEnv = env
	if f.expand == nil {
		return value, nil
	}
	return f.expand(ctx, value, env)
}

func TestShellVariableResolver_DelegatesToExpander(t *testing.T) {
	t.Parallel()

	fe := &fakeExpander{
		expand: func(_ context.Context, value string, _ []string) (string, error) {
			if value == "hello $FOO" {
				return "hello bar", nil
			}
			return value, nil
		},
	}

	e := env.NewFromMap(map[string]string{"FOO": "bar"})
	r := NewShellVariableResolver(e, WithExpander(fe.Expand))

	got, err := r.ResolveValue("hello $FOO")
	require.NoError(t, err)
	require.Equal(t, "hello bar", got)
	require.Equal(t, 1, fe.calls)
	require.Equal(t, "hello $FOO", fe.lastValue)
	require.Contains(t, fe.lastEnv, "FOO=bar")
}

func TestShellVariableResolver_ContextCancellationStopsExpansion(t *testing.T) {
	t.Parallel()

	entered := make(chan struct{})
	r := NewShellVariableResolver(env.NewFromMap(nil), WithExpander(func(ctx context.Context, _ string, _ []string) (string, error) {
		close(entered)
		<-ctx.Done()
		return "", ctx.Err()
	}))
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := ResolveValueContext(ctx, r, "$(blocked)")
		done <- err
	}()

	<-entered
	cancel()
	err := <-done
	require.ErrorIs(t, err, context.Canceled)
}

func TestShellVariableResolver_ContextDeadlineBeatsResolverCeiling(t *testing.T) {
	entered := make(chan struct{})
	r := NewShellVariableResolver(env.NewFromMap(nil), WithExpander(func(ctx context.Context, _ string, _ []string) (string, error) {
		close(entered)
		select {
		case <-ctx.Done():
			return "", errors.New("expander context closed before trigger")
		default:
		}
		<-ctx.Done()
		return "", ctx.Err()
	}))
	ctx := newControlledDeadlineContext()
	done := make(chan error, 1)
	go func() {
		_, err := ResolveValueContext(ctx, r, "$(deadline)")
		done <- err
	}()

	<-entered
	ctx.trigger()
	<-ctx.Done()
	err := <-done
	require.ErrorIs(t, err, context.DeadlineExceeded)
}

type controlledDeadlineContext struct {
	done chan struct{}
	once sync.Once
}

func newControlledDeadlineContext() *controlledDeadlineContext {
	return &controlledDeadlineContext{done: make(chan struct{})}
}

func (c *controlledDeadlineContext) Deadline() (time.Time, bool) {
	return time.Unix(4102444800, 0), true
}

func (c *controlledDeadlineContext) Done() <-chan struct{} { return c.done }

func (c *controlledDeadlineContext) Err() error {
	select {
	case <-c.done:
		return context.DeadlineExceeded
	default:
		return nil
	}
}

func (c *controlledDeadlineContext) Value(any) any { return nil }

func (c *controlledDeadlineContext) trigger() {
	c.once.Do(func() { close(c.done) })
}

type cancelingContextResolver struct {
	cancel context.CancelFunc
}

func (r cancelingContextResolver) ResolveValue(value string) (string, error) {
	return value, nil
}

func (r cancelingContextResolver) ResolveValueContext(_ context.Context, value string) (string, error) {
	r.cancel()
	return value, nil
}

type legacyResolver struct {
	calls int
}

func (r *legacyResolver) ResolveValue(value string) (string, error) {
	r.calls++
	return value, nil
}

func TestResolveValueContext_LinearizesCancellationAroundResolver(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())
	resolver := cancelingContextResolver{cancel: cancel}
	_, err := ResolveValueContext(ctx, resolver, "value")
	require.ErrorIs(t, err, context.Canceled)
}

func TestResolveValueContext_PrechecksIdentityAndLegacyResolvers(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := ResolveValueContext(ctx, IdentityResolver(), "value")
	require.ErrorIs(t, err, context.Canceled)

	legacy := &legacyResolver{}
	_, err = ResolveValueContext(context.Background(), legacy, "value")
	require.ErrorIs(t, err, ErrContextResolverUnsupported)
	require.Zero(t, legacy.calls)

	_, err = ResolveValueContext(ctx, legacy, "value")
	require.ErrorIs(t, err, context.Canceled)
	require.Zero(t, legacy.calls)
}

const resolveCommandHelperEnv = "RUSH_RESOLVE_COMMAND_HELPER"

type resolveResult struct {
	value string
	err   error
}

func TestResolveValueCommandSubstitutionHelper(t *testing.T) {
	if os.Getenv(resolveCommandHelperEnv) != "1" {
		return
	}
	conn, err := net.Dial("tcp", os.Getenv("RUSH_RESOLVE_COMMAND_HELPER_ADDR"))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if _, err := fmt.Fprintf(conn, "%d\n", os.Getpid()); err != nil {
		t.Fatal(err)
	}
	_, _ = io.Copy(io.Discard, conn)
}

func TestShellVariableResolver_CommandSubstitutionCancellationReapsChild(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)

	accepted := make(chan net.Conn, 1)
	acceptErr := make(chan error, 1)
	acceptDone := make(chan struct{})
	go func() {
		defer close(acceptDone)
		conn, err := listener.Accept()
		if err != nil {
			acceptErr <- err
			return
		}
		accepted <- conn
	}()
	defer func() {
		_ = listener.Close()
		<-acceptDone
	}()

	self, err := os.Executable()
	require.NoError(t, err)
	command := fmt.Sprintf("$(%s -test.run=^TestResolveValueCommandSubstitutionHelper$)", strconv.Quote(filepath.ToSlash(self)))
	r := NewShellVariableResolver(env.NewFromMap(map[string]string{
		"PATH":                             os.Getenv("PATH"),
		resolveCommandHelperEnv:            "1",
		"RUSH_RESOLVE_COMMAND_HELPER_ADDR": listener.Addr().String(),
	}))
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan resolveResult, 1)
	go func() {
		value, err := ResolveValueContext(ctx, r, command)
		done <- resolveResult{value: value, err: err}
	}()

	var conn net.Conn
	resultReceived := false
	var readDone chan struct{}
	defer func() {
		cancel()
		if !resultReceived {
			<-done
		}
		if conn != nil {
			_ = conn.Close()
		}
		if readDone != nil {
			<-readDone
		}
	}()
	select {
	case conn = <-accepted:
	case err := <-acceptErr:
		require.NoError(t, err)
	case result := <-done:
		resultReceived = true
		t.Fatalf("resolver exited before helper connected: %v", result.err)
	}
	pidLine, err := bufio.NewReader(conn).ReadString('\n')
	require.NoError(t, err)
	pid, err := strconv.Atoi(strings.TrimSpace(pidLine))
	require.NoError(t, err)
	readDone = make(chan struct{})
	go func() {
		_, _ = io.Copy(io.Discard, conn)
		close(readDone)
	}()

	cancel()
	result := <-done
	resultReceived = true
	require.ErrorIs(t, result.err, context.Canceled)
	<-readDone
	require.False(t, session.IsProcessAlive(pid))
}

func TestShellVariableResolver_LoneDollarIsError(t *testing.T) {
	t.Parallel()

	// Lone "$" must short-circuit before reaching the expander: the
	// underlying shell parser would accept it as a literal, but this
	// resolver has historically rejected it and callers depend on
	// that early-fail behaviour.
	fe := &fakeExpander{}
	r := NewShellVariableResolver(env.NewFromMap(nil), WithExpander(fe.Expand))

	_, err := r.ResolveValue("$")
	require.Error(t, err)
	require.Equal(t, 0, fe.calls, "expander must not be called for lone $")
}

func TestShellVariableResolver_PassesThroughLiterals(t *testing.T) {
	t.Parallel()

	fe := &fakeExpander{
		expand: func(_ context.Context, value string, _ []string) (string, error) {
			return value, nil
		},
	}
	r := NewShellVariableResolver(env.NewFromMap(nil), WithExpander(fe.Expand))

	got, err := r.ResolveValue("plain-string")
	require.NoError(t, err)
	require.Equal(t, "plain-string", got)
}

func TestShellVariableResolver_WrapsErrorsWithTemplate(t *testing.T) {
	t.Parallel()

	inner := errors.New("cat: /run/secrets/x: permission denied")
	fe := &fakeExpander{
		expand: func(_ context.Context, _ string, _ []string) (string, error) {
			return "", inner
		},
	}
	r := NewShellVariableResolver(env.NewFromMap(nil), WithExpander(fe.Expand))

	_, err := r.ResolveValue("$(cat /run/secrets/x)")
	require.Error(t, err)
	require.ErrorIs(t, err, inner)
	require.Contains(t, err.Error(), "$(cat /run/secrets/x)")
	require.Contains(t, err.Error(), "permission denied")
}

func TestSanitizeResolveError(t *testing.T) {
	t.Parallel()

	t.Run("nil passes through", func(t *testing.T) {
		t.Parallel()
		require.NoError(t, sanitizeResolveError("anything", nil))
	})

	t.Run("includes template and wraps inner", func(t *testing.T) {
		t.Parallel()
		inner := errors.New("cat: /run/secrets/x: permission denied")
		got := sanitizeResolveError("$(cat /run/secrets/x)", inner)
		require.Error(t, got)
		require.ErrorIs(t, got, inner)
		require.Contains(t, got.Error(), "$(cat /run/secrets/x)")
		require.Contains(t, got.Error(), "permission denied")
	})

	t.Run("unwrap preserves original for errors.Is", func(t *testing.T) {
		t.Parallel()
		inner := errors.New("sentinel")
		got := sanitizeResolveError("$FOO", inner)
		require.ErrorIs(t, got, inner)
	})

	t.Run("truncates over-budget inner message", func(t *testing.T) {
		t.Parallel()
		// Inner message holds far more than the budget. After
		// sanitization the rendered inner portion must not exceed
		// maxResolveErrBytes, and the characters beyond the budget
		// (marked by a distinct tail sentinel) must be gone.
		const tailSentinel = "TAIL_SENTINEL_BEYOND_BUDGET"
		body := strings.Repeat("x", maxResolveErrBytes)
		inner := errors.New(body + tailSentinel)

		got := sanitizeResolveError("$TEMPLATE", inner)
		require.Error(t, got)

		prefix := `resolving "$TEMPLATE": `
		rendered := got.Error()
		require.True(
			t,
			strings.HasPrefix(rendered, prefix),
			"rendered error must start with template prefix",
		)
		innerRendered := strings.TrimPrefix(rendered, prefix)
		require.LessOrEqual(
			t,
			len(innerRendered),
			maxResolveErrBytes,
			"inner message must be bounded to maxResolveErrBytes",
		)
		require.NotContains(
			t,
			rendered,
			tailSentinel,
			"content past the budget must not leak",
		)
	})

	t.Run("replaces non-printable bytes", func(t *testing.T) {
		t.Parallel()
		// NUL, BEL, ESC, DEL, and a UTF-8 high byte should all be
		// scrubbed to '?'. Tab and newline are preserved because
		// they show up legitimately in command stderr.
		inner := errors.New("ok\x00bad\x07\x1b\x7f\xffend\ttab\nline")
		got := sanitizeResolveError("$T", inner)
		rendered := got.Error()

		require.NotContains(t, rendered, "\x00")
		require.NotContains(t, rendered, "\x07")
		require.NotContains(t, rendered, "\x1b")
		require.NotContains(t, rendered, "\x7f")
		require.NotContains(t, rendered, "\xff")
		require.Contains(t, rendered, "ok?bad????end\ttab\nline")
	})

	t.Run("scrubbing does not depend on shell.ExpandValue upstream", func(t *testing.T) {
		t.Parallel()
		// A custom Expander can inject arbitrary error text. The
		// config-layer helper is the single chokepoint; it must
		// bound + scrub regardless of the error source.
		nasty := strings.Repeat("A", maxResolveErrBytes+64) + "\x00BEYOND"
		fe := &fakeExpander{
			expand: func(_ context.Context, _ string, _ []string) (string, error) {
				return "", errors.New(nasty)
			},
		}
		r := NewShellVariableResolver(env.NewFromMap(nil), WithExpander(fe.Expand))

		_, err := r.ResolveValue("$T")
		require.Error(t, err)
		require.NotContains(t, err.Error(), "BEYOND", "over-budget tail must not leak")
		require.NotContains(t, err.Error(), "\x00", "non-printables must be scrubbed")
	})
}

func TestScrubErrorMessage(t *testing.T) {
	t.Parallel()

	t.Run("bounds output to maxResolveErrBytes", func(t *testing.T) {
		t.Parallel()
		got := scrubErrorMessage(strings.Repeat("a", maxResolveErrBytes*3))
		require.Len(t, got, maxResolveErrBytes)
	})

	t.Run("preserves printable ASCII tab and newline", func(t *testing.T) {
		t.Parallel()
		require.Equal(t, "a\tb\nc d!", scrubErrorMessage("a\tb\nc d!"))
	})

	t.Run("replaces control and non-ASCII bytes", func(t *testing.T) {
		t.Parallel()
		require.Equal(t, "a?b??c", scrubErrorMessage("a\x01b\x1b\xe2c"))
	})
}

func TestNewShellVariableResolver(t *testing.T) {
	testEnv := env.NewFromMap(map[string]string{"TEST": "value"})
	resolver := NewShellVariableResolver(testEnv)

	require.NotNil(t, resolver)
	require.Implements(t, (*VariableResolver)(nil), resolver)
}
