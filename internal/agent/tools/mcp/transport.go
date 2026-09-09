package mcp

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptrace"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"

	"github.com/PHPCraftdream/rush/internal/config"
	"github.com/PHPCraftdream/rush/internal/home"
	"github.com/PHPCraftdream/rush/internal/platform"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

const (
	stdioDiagnosticTimeout   = 5 * time.Second
	stdioDiagnosticMaxOutput = 32 << 10
)

// ErrStdioDiagnosticTooLarge reports that a diagnostic rerun exceeded its
// bounded output budget.
var ErrStdioDiagnosticTooLarge = errors.New("MCP stdio diagnostic output exceeded limit")

// StdioDiagnosticOutputLimitError reports the byte budget exceeded by a
// diagnostic rerun.
type StdioDiagnosticOutputLimitError struct {
	Limit int
}

func (e *StdioDiagnosticOutputLimitError) Error() string {
	return fmt.Sprintf("MCP stdio diagnostic output exceeded %d-byte limit", e.Limit)
}

func (e *StdioDiagnosticOutputLimitError) Unwrap() error {
	return ErrStdioDiagnosticTooLarge
}

func transportCleanup(transport mcp.Transport) func() {
	var roundTripper http.RoundTripper
	switch transport := transport.(type) {
	case *mcp.StreamableClientTransport:
		if transport.HTTPClient != nil {
			roundTripper = transport.HTTPClient.Transport
		}
	case *mcp.SSEClientTransport:
		if transport.HTTPClient != nil {
			roundTripper = transport.HTTPClient.Transport
		}
	}
	owned, ok := roundTripper.(interface{ CloseIdleConnections() })
	if !ok {
		return nil
	}
	return owned.CloseIdleConnections
}

// so, if we got an EOF err, and the transport is STDIO, we try to exec it
// again with a timeout and collect the output so we can add details to the
// error.
// this happens particularly when starting things with npx, e.g. if node can't
// be found or some other error like that.
func maybeStdioErr(err error, transport mcp.Transport) error {
	if !errors.Is(err, io.EOF) {
		return err
	}
	ct, ok := transport.(*mcp.CommandTransport)
	if !ok {
		return err
	}
	if err2 := stdioCheck(ct.Command); err2 != nil {
		err = errors.Join(err, err2)
	}
	return err
}

func createTransport(ctx context.Context, m config.MCPConfig, resolver config.VariableResolver) (mcp.Transport, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	switch m.Type {
	case config.MCPStdio:
		command, err := config.ResolveValueContext(ctx, resolver, m.Command)
		if err != nil {
			return nil, fmt.Errorf("invalid mcp command: %w", err)
		}
		if strings.TrimSpace(command) == "" {
			return nil, fmt.Errorf("mcp stdio config requires a non-empty 'command' field")
		}
		args, err := m.ResolvedArgsContext(ctx, resolver)
		if err != nil {
			return nil, err
		}
		envs, err := m.ResolvedEnvContext(ctx, resolver)
		if err != nil {
			return nil, err
		}
		cmd := platform.Command(ctx, home.Long(command), args...)
		cmd.Env = append(os.Environ(), envs...)
		// Run the child in its own process group and kill the whole group when
		// the session context is cancelled. A stdio server often spawns its own
		// children (signal-mcp launches signal-cli); os/exec's default
		// cancellation kills only the direct child, orphaning the rest with
		// PPID 1 — production accumulated 15+ such zombies over two days.
		configureStdioProcess(cmd)
		return &mcp.CommandTransport{
			Command: cmd,
		}, nil
	case config.MCPHttp:
		url, err := m.ResolvedURLContext(ctx, resolver)
		if err != nil {
			return nil, err
		}
		if strings.TrimSpace(url) == "" {
			return nil, fmt.Errorf("mcp http config requires a non-empty 'url' field")
		}
		headers, err := m.ResolvedHeadersContext(ctx, resolver)
		if err != nil {
			return nil, err
		}
		transport, err := cloneHTTPTransport()
		if err != nil {
			return nil, err
		}
		client := &http.Client{
			Transport: &headerRoundTripper{
				headers:   headers,
				ctx:       ctx,
				transport: transport,
			},
		}
		return &mcp.StreamableClientTransport{
			Endpoint:   url,
			HTTPClient: client,
		}, nil
	case config.MCPSSE:
		url, err := m.ResolvedURLContext(ctx, resolver)
		if err != nil {
			return nil, err
		}
		if strings.TrimSpace(url) == "" {
			return nil, fmt.Errorf("mcp sse config requires a non-empty 'url' field")
		}
		headers, err := m.ResolvedHeadersContext(ctx, resolver)
		if err != nil {
			return nil, err
		}
		transport, err := cloneHTTPTransport()
		if err != nil {
			return nil, err
		}
		client := &http.Client{
			Transport: &headerRoundTripper{
				headers:   headers,
				ctx:       ctx,
				transport: transport,
			},
		}
		return &mcp.SSEClientTransport{
			Endpoint:   url,
			HTTPClient: client,
		}, nil
	default:
		return nil, fmt.Errorf("unsupported mcp type: %s", m.Type)
	}
}

type headerRoundTripper struct {
	headers   map[string]string
	ctx       context.Context
	transport *http.Transport
}

func cloneHTTPTransport() (*http.Transport, error) {
	if base, ok := http.DefaultTransport.(*http.Transport); ok {
		return base.Clone(), nil
	}
	dialer := &net.Dialer{
		Timeout:   30 * time.Second,
		KeepAlive: 30 * time.Second,
	}
	return &http.Transport{
		Proxy:                 http.ProxyFromEnvironment,
		DialContext:           dialer.DialContext,
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          100,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ExpectContinueTimeout: time.Second,
	}, nil
}

func (rt *headerRoundTripper) CloseIdleConnections() {
	if rt.transport != nil {
		rt.transport.CloseIdleConnections()
	}
}

type ownerResponseBody struct {
	io.ReadCloser
	stop   func() bool
	cancel context.CancelFunc
	once   sync.Once
}

func (b *ownerResponseBody) release() {
	b.once.Do(func() {
		b.stop()
		b.cancel()
	})
}

func (b *ownerResponseBody) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	if err != nil {
		b.release()
	}
	return n, err
}

func (b *ownerResponseBody) Close() error {
	b.release()
	return b.ReadCloser.Close()
}

func (rt *headerRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	if rt.transport == nil {
		return nil, errors.New("mcp http transport is not initialized")
	}
	for k, v := range rt.headers {
		req.Header.Set(k, v)
	}
	if rt.ctx != nil {
		var connMu sync.Mutex
		var requestConn net.Conn
		canceled := false
		ctx := httptrace.WithClientTrace(req.Context(), &httptrace.ClientTrace{
			GotConn: func(info httptrace.GotConnInfo) {
				connMu.Lock()
				defer connMu.Unlock()
				requestConn = info.Conn
				if canceled {
					_ = info.Conn.Close()
				}
			},
		})
		ctx, cancel := context.WithCancel(ctx)
		req = req.WithContext(ctx)
		legacyCancel := make(chan struct{})
		req.Cancel = legacyCancel
		var cancelOnce sync.Once
		cancelRequest := func() {
			cancelOnce.Do(func() {
				connMu.Lock()
				canceled = true
				if requestConn != nil {
					_ = requestConn.Close()
				}
				connMu.Unlock()
				close(legacyCancel)
				cancel()
				// The MCP SDK deliberately detaches its connection context from
				// Connect's context. CancelRequest is retained here as an explicit
				// transport fence for an HTTP request that is blocked before it has
				// produced a response body; context cancellation alone is not
				// sufficient for every Windows net/http transport path.
				rt.transport.CancelRequest(req)
			})
		}
		stop := context.AfterFunc(rt.ctx, cancelRequest)
		resp, err := rt.transport.RoundTrip(req)
		if err != nil {
			stop()
			cancelRequest()
			return nil, err
		}
		if resp.Body == nil {
			stop()
			cancelRequest()
			return resp, nil
		}
		resp.Body = &ownerResponseBody{
			ReadCloser: resp.Body,
			stop:       stop,
			cancel:     cancel,
		}
		return resp, nil
	}
	return rt.transport.RoundTrip(req)
}

func mcpTimeout(m config.MCPConfig) time.Duration {
	return time.Duration(cmp.Or(m.Timeout, 15)) * time.Second
}

// stdioDiagnosticWriter retains a bounded prefix while allowing stdout and
// stderr to write concurrently. The first write beyond the limit cancels the
// diagnostic command through onLimit.
type stdioDiagnosticWriter struct {
	mu       sync.Mutex
	limit    int
	buf      []byte
	onLimit  func()
	exceeded bool
	limitErr error
	once     sync.Once
}

func newStdioDiagnosticWriter(limit int, onLimit func()) *stdioDiagnosticWriter {
	return &stdioDiagnosticWriter{limit: limit, onLimit: onLimit}
}

func (w *stdioDiagnosticWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	if w.exceeded {
		err := w.limitErr
		w.mu.Unlock()
		return 0, err
	}
	remaining := w.limit - len(w.buf)
	if len(p) <= remaining {
		w.buf = append(w.buf, p...)
		w.mu.Unlock()
		return len(p), nil
	}
	if remaining > 0 {
		w.buf = append(w.buf, p[:remaining]...)
	}
	w.exceeded = true
	w.limitErr = &StdioDiagnosticOutputLimitError{Limit: w.limit}
	err := w.limitErr
	w.mu.Unlock()
	w.once.Do(func() {
		if w.onLimit != nil {
			w.onLimit()
		}
	})
	return remaining, err
}

func (w *stdioDiagnosticWriter) Bytes() []byte {
	w.mu.Lock()
	defer w.mu.Unlock()
	return append([]byte(nil), w.buf...)
}

func (w *stdioDiagnosticWriter) Exceeded() bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.exceeded
}

func (w *stdioDiagnosticWriter) LimitError() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.limitErr
}

func sanitizeStdioDiagnosticPrefix(output []byte) string {
	if len(output) > stdioDiagnosticMaxOutput {
		output = output[:stdioDiagnosticMaxOutput]
	}
	sanitized := make([]byte, len(output))
	for i, b := range output {
		if b == '\t' || b == '\r' || b == '\n' || (b >= 0x20 && b != 0x7f) {
			sanitized[i] = b
			continue
		}
		sanitized[i] = '?'
	}
	return string(sanitized)
}

func diagnosticCommand(ctx context.Context, old *exec.Cmd) *exec.Cmd {
	args := old.Args
	if len(args) > 0 {
		args = args[1:]
	}
	cmd := platform.Command(ctx, old.Path, args...)
	if len(old.Args) > 0 {
		// platform.Command gives the new process the correct argument count;
		// restore a custom argv[0] when the original command supplied one.
		cmd.Args = append([]string(nil), old.Args...)
	}
	if old.Env != nil {
		cmd.Env = append([]string{}, old.Env...)
	}
	cmd.Dir = old.Dir
	if old.ExtraFiles != nil {
		cmd.ExtraFiles = append([]*os.File{}, old.ExtraFiles...)
	}
	if old.SysProcAttr != nil {
		attrs := *old.SysProcAttr
		cmd.SysProcAttr = &attrs
	}
	configureStdioProcess(cmd)
	return cmd
}

func stdioCheck(old *exec.Cmd) error {
	if old == nil {
		return errors.New("MCP stdio diagnostic command unavailable")
	}
	ctx, cancel := context.WithTimeout(context.Background(), stdioDiagnosticTimeout)
	defer cancel()
	writer := newStdioDiagnosticWriter(stdioDiagnosticMaxOutput, cancel)
	cmd := diagnosticCommand(ctx, old)
	cmd.Stdout = writer
	cmd.Stderr = writer
	err := cmd.Run()
	if writer.Exceeded() {
		return fmt.Errorf("%w: %s", writer.LimitError(), sanitizeStdioDiagnosticPrefix(writer.Bytes()))
	}
	if err == nil || errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return nil
	}
	return fmt.Errorf("%w: %s", err, sanitizeStdioDiagnosticPrefix(writer.Bytes()))
}
