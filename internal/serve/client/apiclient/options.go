package apiclient

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const (
	defaultMaxResponseBytes int64 = 8 << 20
	maxResponseBytesLimit   int64 = 1 << 30
)

type Timeouts struct {
	Dial           time.Duration
	TLSHandshake   time.Duration
	ResponseHeader time.Duration
	Idle           time.Duration
}

type Options struct {
	BaseURL            string
	Token              string
	CAFile             string
	Pin                string
	ClientCertFile     string
	ClientKeyFile      string
	AllowCleartext     bool
	InsecureSkipVerify bool
	UserAgent          string
	Timeouts           Timeouts
	MaxResponseBytes   int64
	Sleeper            func(context.Context, time.Duration) error
	Rand               io.Reader
	Now                func() time.Time
}

// Client holds the validated immutable configuration used by subsequent request methods.
type Client struct {
	options Options
	baseURL *url.URL
	http    *http.Client
}

// New validates options and returns the normalized configuration for a client.
func New(opts Options) (*Client, []string, error) {
	if opts.Token == "" {
		return nil, nil, &UsageError{Err: errors.New("API token is required")}
	}
	if opts.MaxResponseBytes < 0 || opts.MaxResponseBytes > maxResponseBytesLimit {
		return nil, nil, &UsageError{Err: errors.New("max response bytes must be between 0 and 1 GiB")}
	}
	baseURL, loopback, err := validateBaseURL(opts.BaseURL, opts.AllowCleartext)
	if err != nil {
		return nil, nil, &UsageError{Err: err}
	}
	if opts.InsecureSkipVerify && !loopback {
		return nil, nil, &UsageError{Err: errors.New("insecure skip verify is allowed only for loopback URLs")}
	}
	if opts.Timeouts.Dial == 0 {
		opts.Timeouts.Dial = 10 * time.Second
	}
	if opts.Timeouts.TLSHandshake == 0 {
		opts.Timeouts.TLSHandshake = 10 * time.Second
	}
	if opts.Timeouts.ResponseHeader == 0 {
		opts.Timeouts.ResponseHeader = 30 * time.Second
	}
	if opts.Timeouts.Idle == 0 {
		opts.Timeouts.Idle = 90 * time.Second
	}
	if opts.MaxResponseBytes == 0 {
		opts.MaxResponseBytes = defaultMaxResponseBytes
	}
	if opts.Sleeper == nil {
		opts.Sleeper = defaultSleeper
	}
	if opts.Rand == nil {
		opts.Rand = rand.Reader
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	httpClient, err := apiHTTPClient(opts)
	if err != nil {
		return nil, nil, &UsageError{Err: err}
	}
	client := &Client{options: opts, baseURL: baseURL, http: httpClient}
	var warnings []string
	if opts.InsecureSkipVerify {
		warnings = append(warnings, "TLS certificate verification is disabled for a loopback URL")
	}
	if opts.AllowCleartext && baseURL.Scheme == "http" && !loopback {
		warnings = append(warnings, "cleartext HTTP is enabled for a non-loopback URL")
	}
	return client, warnings, nil
}

func validateBaseURL(raw string, allowCleartext bool) (*url.URL, bool, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return nil, false, errors.New("invalid API base URL")
	}
	if u.Scheme != "https" && u.Scheme != "http" {
		return nil, false, errors.New("API base URL must use https or http")
	}
	if u.Opaque != "" || u.Host == "" || u.Hostname() == "" {
		return nil, false, errors.New("API base URL must include a host")
	}
	if u.User != nil {
		return nil, false, errors.New("API base URL must not contain userinfo")
	}
	if u.Fragment != "" || strings.Contains(raw, "#") {
		return nil, false, errors.New("API base URL must not contain a fragment")
	}
	query, err := url.ParseQuery(u.RawQuery)
	if err != nil {
		return nil, false, errors.New("invalid API base URL query")
	}
	for key := range query {
		if strings.EqualFold(key, "token") || strings.EqualFold(key, "access_token") {
			return nil, false, errors.New("API base URL must not contain a token query parameter")
		}
	}
	loopback := isLoopbackHost(u.Hostname())
	if u.Scheme == "http" && !loopback && !allowCleartext {
		return nil, false, errors.New("cleartext HTTP requires a loopback host or AllowCleartext")
	}
	u.Path = strings.TrimRight(u.Path, "/")
	if u.Path == "" {
		u.Path = "/"
	}
	u.RawPath = ""
	return u, loopback, nil
}

func isLoopbackHost(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func defaultSleeper(ctx context.Context, duration time.Duration) error {
	timer := time.NewTimer(duration)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

// Session is a safe summary of an API session.
type Session struct {
	ID           string `json:"id"`
	Title        string `json:"title"`
	CreatedAt    string `json:"created_at"`
	UpdatedAt    string `json:"updated_at"`
	MessageCount int64  `json:"message_count"`
}

// Message is a message in a session.
type Message struct {
	ID        string        `json:"id"`
	SessionID string        `json:"session_id"`
	Role      string        `json:"role"`
	Parts     []MessagePart `json:"parts"`
	CreatedAt string        `json:"created_at"`
}

// MessagePart preserves unrecognized part types in Raw.
type MessagePart struct {
	Type       string          `json:"type"`
	Text       string          `json:"text,omitempty"`
	ID         string          `json:"id,omitempty"`
	Name       string          `json:"name,omitempty"`
	Input      string          `json:"input,omitempty"`
	ToolCallID string          `json:"tool_call_id,omitempty"`
	Content    string          `json:"content,omitempty"`
	IsError    bool            `json:"is_error,omitempty"`
	Raw        json.RawMessage `json:"-"`
}

// UnmarshalJSON keeps unknown part types as their original JSON object.
func (p *MessagePart) UnmarshalJSON(data []byte) error {
	var header struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal(data, &header); err != nil {
		return err
	}
	p.Type = header.Type
	switch p.Type {
	case "text", "reasoning", "tool_call", "tool_result":
		type known MessagePart
		return json.Unmarshal(data, (*known)(p))
	default:
		p.Raw = append(p.Raw[:0], data...)
		return nil
	}
}

// MarshalJSON returns unknown parts unchanged, and known parts with wire fields.
func (p MessagePart) MarshalJSON() ([]byte, error) {
	switch p.Type {
	case "text", "reasoning", "tool_call", "tool_result":
		type known MessagePart
		return json.Marshal(known(p))
	default:
		if len(p.Raw) == 0 {
			return json.Marshal(struct {
				Type string `json:"type"`
			}{Type: p.Type})
		}
		return append([]byte(nil), p.Raw...), nil
	}
}

// Model describes a model available to the API.
type Model struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	Provider string `json:"provider"`
	Role     string `json:"role,omitempty"`
}

// Run describes the current state of an asynchronous run.
type Run struct {
	ID         string     `json:"id"`
	SessionID  string     `json:"session_id"`
	State      string     `json:"state"`
	CreatedAt  time.Time  `json:"created_at"`
	StartedAt  *time.Time `json:"started_at,omitempty"`
	FinishedAt *time.Time `json:"finished_at,omitempty"`
	Error      *RunError  `json:"error,omitempty"`
	MessageID  string     `json:"message_id,omitempty"`
}

// RunError is the server-reported failure for a run.
type RunError struct {
	Code    string `json:"code"`
	Summary string `json:"summary"`
	Run     *Run   `json:"-"`
}

func (e *RunError) Error() string {
	if e == nil {
		return "run failed"
	}
	if e.Summary != "" {
		return e.Summary
	}
	if e.Code != "" {
		return e.Code
	}
	return "run failed"
}

// RunAccepted describes an accepted run submission.
type RunAccepted struct {
	RunID     string `json:"run_id"`
	SessionID string `json:"session_id"`
	State     string `json:"state"`
}

// Permission represents a pending tool permission request.
type Permission struct {
	ID          string    `json:"id"`
	SessionID   string    `json:"session_id"`
	RunID       string    `json:"run_id"`
	Tool        string    `json:"tool"`
	Action      string    `json:"action"`
	Description string    `json:"description"`
	Path        string    `json:"path"`
	CreatedAt   time.Time `json:"created_at"`
}

// Page is a cursor-paginated response.
type Page[T any] struct {
	Items     []T     `json:"items"`
	NextAfter *string `json:"next_after"`
}

// RunRequest is the body used to create a run.
type RunRequest struct {
	Prompt string `json:"prompt"`
	Role   string `json:"role,omitempty"`
	Model  string `json:"model,omitempty"`
}

// ListOptions contains common cursor pagination parameters.
type ListOptions struct {
	After string
	Limit int
}

// PermissionFilter filters permission requests by session or run.
type PermissionFilter struct {
	SessionID string `json:"session_id,omitempty"`
	RunID     string `json:"run_id,omitempty"`
	State     string `json:"state,omitempty"`
}
