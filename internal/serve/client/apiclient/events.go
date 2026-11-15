package apiclient

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

const maxEventBackoff = 30 * time.Second

type Event struct {
	ID   uint64          `json:"id"`
	Type string          `json:"type"`
	Data json.RawMessage `json:"data"`
	Gap  bool            `json:"gap,omitempty"`
}

// EventsOptions controls reconnection behavior for an event stream.
type EventsOptions struct {
	LastEventID      uint64
	MaxReconnects    int
	Backoff          func(n int) time.Duration
	HeartbeatTimeout time.Duration
	// WatchdogTimer allows deterministic heartbeat-timeout tests.
	WatchdogTimer func(time.Duration) EventsTimer
	// Sleeper and Rand allow deterministic reconnect scheduling.
	Sleeper func(context.Context, time.Duration) error
	Rand    io.Reader
}

// EventsTimer is the timer contract used by the stream heartbeat watchdog.
type EventsTimer interface {
	C() <-chan time.Time
	Stop() bool
	Reset(time.Duration) bool
}

type realEventsTimer struct{ timer *time.Timer }

func (t realEventsTimer) C() <-chan time.Time        { return t.timer.C }
func (t realEventsTimer) Stop() bool                 { return t.timer.Stop() }
func (t realEventsTimer) Reset(d time.Duration) bool { return t.timer.Reset(d) }

// Events opens a resumable session event stream. The returned error function
// joins the stream worker and returns its terminal error; call it after closure.
func (c *Client) Events(ctx context.Context, sessionID string, opts EventsOptions) (<-chan Event, func() error) {
	out := make(chan Event)
	done := make(chan struct{})
	var mu sync.Mutex
	var terminal error
	getErr := func() error {
		<-done
		mu.Lock()
		defer mu.Unlock()
		return terminal
	}
	go func() {
		defer close(done)
		defer close(out)
		err := c.runEvents(ctx, sessionID, opts, out)
		mu.Lock()
		terminal = err
		mu.Unlock()
	}()
	return out, getErr
}

func (c *Client) runEvents(ctx context.Context, sessionID string, opts EventsOptions, out chan<- Event) error {
	path := "/sessions/" + url.PathEscape(sessionID) + "/events"
	if err := validateRequestPath(path); err != nil {
		return err
	}
	if opts.HeartbeatTimeout <= 0 {
		opts.HeartbeatTimeout = 45 * time.Second
	}
	sleeper := opts.Sleeper
	if sleeper == nil {
		sleeper = c.options.Sleeper
	}
	random := opts.Rand
	if random == nil {
		random = c.options.Rand
	}
	lastID := opts.LastEventID
	attempt, reconnects := 0, 0
	minimumBackoff := time.Duration(0)
	for {
		if err := ctx.Err(); err != nil {
			return nil
		}
		response, err := c.openEventStream(ctx, path, lastID)
		if err != nil {
			if errors.Is(err, errSSELineTooLong) || errors.Is(err, errSSEFrameTooLarge) {
				return err
			}
			if ctx.Err() != nil {
				return nil
			}
			var apiErr *APIError
			if errors.As(err, &apiErr) {
				if apiErr.Status == 401 || apiErr.Status == 403 || apiErr.Status == 404 {
					return apiErr
				}
				if apiErr.Status != 429 && apiErr.Status != 503 {
					return apiErr
				}
				if apiErr.RetryAfter > minimumBackoff {
					minimumBackoff = apiErr.RetryAfter
				}
			}
			if !c.eventReconnectAllowed(opts, reconnects) {
				return err
			}
			reconnects++
			if waitErr := c.waitEventReconnect(ctx, opts, sleeper, random, attempt, minimumBackoff); waitErr != nil {
				if ctx.Err() != nil {
					return nil
				}
				return waitErr
			}
			attempt++
			continue
		}
		status := response.StatusCode
		if status != http.StatusOK {
			body, _ := io.ReadAll(io.LimitReader(response.Body, c.options.MaxResponseBytes+1))
			_ = response.Body.Close()
			apiErr := parseAPIError(status, response.Header, body, c.options.Token, c.options.Now())
			apiErr.method, apiErr.path, apiErr.token = http.MethodGet, path, c.options.Token
			if status == 401 || status == 403 || status == 404 {
				return apiErr
			}
			if status != 429 && status != 503 {
				return apiErr
			}
			if apiErr.RetryAfter > minimumBackoff {
				minimumBackoff = apiErr.RetryAfter
			}
			if !c.eventReconnectAllowed(opts, reconnects) {
				return apiErr
			}
			reconnects++
			if waitErr := c.waitEventReconnect(ctx, opts, sleeper, random, attempt, minimumBackoff); waitErr != nil {
				if ctx.Err() != nil {
					return nil
				}
				return waitErr
			}
			attempt++
			continue
		}

		streamCtx, cancel := context.WithCancel(ctx)
		activity := make(chan struct{}, 1)
		watchdogDone := make(chan struct{})
		newWatchdogTimer := opts.WatchdogTimer
		if newWatchdogTimer == nil {
			newWatchdogTimer = func(d time.Duration) EventsTimer { return realEventsTimer{timer: time.NewTimer(d)} }
		}
		go eventWatchdog(streamCtx, cancel, response.Body, opts.HeartbeatTimeout, newWatchdogTimer, activity, watchdogDone)
		reader := bufio.NewReader(&activityReader{r: response.Body, activity: activity})
		result := c.consumeEventStream(streamCtx, reader, out, &lastID)
		streamErr, cleanClosed, overflow, serverRetry, healthy := result.err, result.closed, result.overflow, result.serverRetry, result.healthy
		cancel()
		_ = response.Body.Close()
		<-watchdogDone
		if ctx.Err() != nil {
			return nil
		}
		if cleanClosed {
			return nil
		}
		if serverRetry > minimumBackoff {
			minimumBackoff = serverRetry
		}
		if healthy {
			attempt = 0
			reconnects = 0
		}
		if streamErr != nil && (errors.Is(streamErr, errSSELineTooLong) || errors.Is(streamErr, errSSEFrameTooLarge)) {
			return streamErr
		}
		if !c.eventReconnectAllowed(opts, reconnects) {
			if streamErr != nil {
				return streamErr
			}
			return io.EOF
		}
		reconnects++
		if overflow {
			attempt = 0
			continue
		}
		if waitErr := c.waitEventReconnect(ctx, opts, sleeper, random, attempt, minimumBackoff); waitErr != nil {
			if ctx.Err() != nil {
				return nil
			}
			return waitErr
		}
		attempt++
	}
}

func (c *Client) openEventStream(ctx context.Context, path string, lastID uint64) (*http.Response, error) {
	endpoint := *c.baseURL
	decodedPath, _ := url.PathUnescape(path)
	basePath := strings.TrimRight(endpoint.Path, "/") + "/api/v1"
	endpoint.Path = basePath + decodedPath
	endpoint.RawPath = (&url.URL{Path: basePath}).EscapedPath() + path
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint.String(), nil)
	if err != nil {
		return nil, errors.New("invalid API request")
	}
	request.Header.Set("Authorization", "Bearer "+c.options.Token)
	request.Header.Set("Accept", "text/event-stream")
	request.Header.Set("Cache-Control", "no-cache")
	if c.options.UserAgent != "" {
		request.Header.Set("User-Agent", c.options.UserAgent)
	}
	requestID, err := c.newRequestID()
	if err != nil {
		return nil, errors.New("generate API request id")
	}
	request.Header.Set("X-Request-Id", requestID)
	if lastID != 0 {
		request.Header.Set("Last-Event-ID", strconv.FormatUint(lastID, 10))
	}
	response, err := c.http.Do(request)
	if err != nil {
		return nil, &requestFailure{method: http.MethodGet, path: path, token: c.options.Token, err: safeRequestCause(err)}
	}
	return response, nil
}

// eventStreamResult is the outcome of one SSE connection.
type eventStreamResult struct {
	err         error
	closed      bool
	overflow    bool
	serverRetry time.Duration
	healthy     bool
}

func (c *Client) consumeEventStream(ctx context.Context, reader *bufio.Reader, out chan<- Event, lastID *uint64) eventStreamResult {
	var minimumBackoff time.Duration
	currentID := *lastID
	healthy := false
	for {
		frame, err := readSSEFrame(reader)
		if err != nil {
			if errors.Is(err, io.EOF) || ctx.Err() != nil {
				return eventStreamResult{err: nil, closed: false, overflow: false, serverRetry: minimumBackoff, healthy: healthy}
			}
			return eventStreamResult{err: err, closed: false, overflow: false, serverRetry: minimumBackoff, healthy: healthy}
		}
		if frame.hasRetry && frame.retry > minimumBackoff {
			minimumBackoff = frame.retry
		}
		if frame.hasID {
			if parsed, parseErr := strconv.ParseUint(frame.id, 10, 64); parseErr == nil {
				currentID = parsed
			}
		}
		if frame.event == "overflow" {
			return eventStreamResult{err: nil, closed: false, overflow: true, serverRetry: minimumBackoff, healthy: healthy}
		}
		if !frame.hasData {
			continue
		}
		eventType := frame.event
		if eventType == "" {
			eventType = "message"
		}
		if eventType == "closed" {
			return eventStreamResult{err: nil, closed: true, overflow: false, serverRetry: minimumBackoff, healthy: healthy}
		}
		if eventType == "overflow" {
			return eventStreamResult{err: nil, closed: false, overflow: true, serverRetry: minimumBackoff, healthy: healthy}
		}
		id := currentID
		if eventType != "reset" && id != 0 && id <= *lastID {
			continue
		}
		data := json.RawMessage(append([]byte(nil), frame.data...))
		if !json.Valid(data) {
			return eventStreamResult{err: errors.New("invalid event JSON"), closed: false, overflow: false, serverRetry: minimumBackoff, healthy: healthy}
		}
		event := Event{ID: id, Type: eventType, Data: data, Gap: eventType == "reset"}
		select {
		case out <- event:
			healthy = true
			if id > *lastID {
				*lastID = id
			}
		case <-ctx.Done():
			return eventStreamResult{err: nil, closed: false, overflow: false, serverRetry: minimumBackoff, healthy: healthy}
		}
	}
}

func eventWatchdog(ctx context.Context, cancel context.CancelFunc, body io.Closer, timeout time.Duration, newTimer func(time.Duration) EventsTimer, activity <-chan struct{}, done chan<- struct{}) {
	defer close(done)
	timer := newTimer(timeout)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-activity:
			if !timer.Stop() {
				select {
				case <-timer.C():
				default:
				}
			}
			timer.Reset(timeout)
		case <-timer.C():
			cancel()
			_ = body.Close()
			return
		}
	}
}

type activityReader struct {
	r        io.Reader
	activity chan<- struct{}
}

func (r *activityReader) Read(p []byte) (int, error) {
	n, err := r.r.Read(p)
	if n > 0 {
		select {
		case r.activity <- struct{}{}:
		default:
		}
	}
	return n, err
}

func (c *Client) eventReconnectAllowed(opts EventsOptions, reconnects int) bool {
	return reconnects < opts.MaxReconnects
}

func (c *Client) waitEventReconnect(ctx context.Context, opts EventsOptions, sleeper func(context.Context, time.Duration) error, random io.Reader, attempt int, minimum time.Duration) error {
	delay := time.Duration(0)
	if opts.Backoff != nil {
		delay = opts.Backoff(attempt)
	} else {
		shift := attempt
		if shift > 16 {
			shift = 16
		}
		delay = 200 * time.Millisecond * time.Duration(1<<shift)
	}
	if delay < 0 {
		delay = 0
	}
	if delay > maxEventBackoff {
		delay = maxEventBackoff
	}
	var jitter [1]byte
	if _, err := io.ReadFull(random, jitter[:]); err == nil {
		delay += time.Duration(uint64(delay) * uint64(jitter[0]) * 25 / (255 * 100))
	}
	if delay > maxEventBackoff {
		delay = maxEventBackoff
	}
	if minimum > delay {
		delay = minimum
	}
	if delay > maxEventBackoff {
		delay = maxEventBackoff
	}
	return sleeper(ctx, delay)
}

const (
	sseMaxLineBytes  = 1 << 20
	sseMaxFrameBytes = 4 << 20
)

var (
	errSSELineTooLong   = errors.New("SSE line exceeds 1 MiB")
	errSSEFrameTooLarge = errors.New("SSE frame exceeds 4 MiB")
)

// sseFrame represents one dispatch; id and retry are optional state updates.
type sseFrame struct {
	id       string
	event    string
	data     string
	retry    time.Duration
	hasID    bool
	hasRetry bool
	hasData  bool
}

// readSSEFrame reads one blank-line-terminated frame. An incomplete frame at
// EOF is discarded. Frames without data are returned with hasData false.
func readSSEFrame(reader *bufio.Reader) (sseFrame, error) {
	var frame sseFrame
	var dataLines []string
	frameBytes := 0
	for {
		line, size, err := readSSELine(reader)
		if err != nil {
			return sseFrame{}, err
		}
		frameBytes += size
		if frameBytes > sseMaxFrameBytes {
			return sseFrame{}, errSSEFrameTooLarge
		}
		if line == "" {
			frame.data = strings.Join(dataLines, "\n")
			frame.hasData = len(dataLines) > 0
			return frame, nil
		}
		if strings.HasPrefix(line, ":") {
			continue
		}
		field, value, found := strings.Cut(line, ":")
		if !found {
			value = ""
		} else if strings.HasPrefix(value, " ") {
			value = value[1:]
		}
		switch field {
		case "data":
			dataLines = append(dataLines, value)
		case "event":
			frame.event = value
		case "id":
			if !strings.ContainsRune(value, '\x00') {
				frame.id = value
				frame.hasID = true
			}
		case "retry":
			if isASCIIDigits(value) {
				if millis, parseErr := strconv.ParseUint(value, 10, 64); parseErr == nil {
					maxMillis := uint64(time.Duration(1<<63-1) / time.Millisecond)
					if millis > maxMillis {
						millis = maxMillis
					}
					frame.retry = time.Duration(millis) * time.Millisecond
					frame.hasRetry = true
				}
			}
		}
	}
}

func isASCIIDigits(value string) bool {
	if value == "" {
		return false
	}
	for i := 0; i < len(value); i++ {
		if value[i] < '0' || value[i] > '9' {
			return false
		}
	}
	return true
}

func readSSELine(reader *bufio.Reader) (string, int, error) {
	line := make([]byte, 0, 128)
	for {
		part, err := reader.ReadSlice('\n')
		if len(line)+len(part) > sseMaxLineBytes+2 {
			return "", 0, errSSELineTooLong
		}
		line = append(line, part...)
		if err == bufio.ErrBufferFull {
			continue
		}
		if err != nil {
			if errors.Is(err, io.EOF) {
				return "", 0, io.EOF
			}
			return "", 0, err
		}
		line = line[:len(line)-1]
		rawSize := len(line) + 1
		if len(line) > 0 && line[len(line)-1] == '\r' {
			line = line[:len(line)-1]
		}
		if len(line) > sseMaxLineBytes {
			return "", 0, errSSELineTooLong
		}
		return string(line), rawSize, nil
	}
}
