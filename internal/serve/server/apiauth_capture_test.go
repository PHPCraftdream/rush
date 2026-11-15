package server

import (
	"context"
	"log/slog"
	"reflect"
	"sync"
	"testing"
	"time"
)

type capturedRecord struct {
	message string
	attrs   map[string]any
}

type captureHandler struct {
	mu      sync.Mutex
	records []capturedRecord
	notify  chan struct{}
}

func (c *captureHandler) Enabled(context.Context, slog.Level) bool { return true }

func (c *captureHandler) Handle(_ context.Context, r slog.Record) error {
	rec := capturedRecord{message: r.Message, attrs: make(map[string]any)}
	r.Attrs(func(attr slog.Attr) bool {
		captureAttr(rec.attrs, "", attr)
		return true
	})
	c.mu.Lock()
	c.records = append(c.records, rec)
	c.mu.Unlock()
	if c.notify != nil {
		select {
		case c.notify <- struct{}{}:
		default:
		}
	}
	return nil
}

func captureAttr(dst map[string]any, prefix string, attr slog.Attr) {
	key := attr.Key
	if prefix != "" {
		key = prefix + "." + key
	}
	value := attr.Value.Resolve()
	switch value.Kind() {
	case slog.KindGroup:
		for _, child := range value.Group() {
			captureAttr(dst, key, child)
		}
	case slog.KindInt64:
		dst[key] = int(value.Int64())
	case slog.KindUint64:
		dst[key] = int(value.Uint64())
	case slog.KindDuration:
		dst[key] = value.Duration()
	case slog.KindString:
		dst[key] = value.String()
	case slog.KindBool:
		dst[key] = value.Bool()
	case slog.KindTime:
		dst[key] = value.Time()
	case slog.KindAny:
		dst[key] = value.Any()
	default:
		dst[key] = value.Any()
	}
}

func (c *captureHandler) snapshot() []capturedRecord {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]capturedRecord(nil), c.records...)
}

func waitForRecords(t *testing.T, c *captureHandler, n int) []capturedRecord {
	t.Helper()
	if c.notify == nil {
		t.Fatal("capture handler notifier must be initialized")
	}
	deadline := time.NewTimer(10 * time.Second)
	defer deadline.Stop()
	for {
		records := c.snapshot()
		if len(records) >= n {
			return records
		}
		select {
		case <-c.notify:
		case <-deadline.C:
			t.Fatalf("timed out waiting for %d records; saw %d", n, len(c.snapshot()))
		}
	}
}

func TestCaptureHandlerNormalizesKindsAndFlattensGroups(t *testing.T) {
	isolateAPIGlobals(t)
	h := &captureHandler{notify: make(chan struct{}, 1)}
	logger := slog.New(h)
	logger.LogAttrs(context.Background(), slog.LevelInfo, "normalization",
		slog.Int64("int", 7),
		slog.Uint64("uint", 8),
		slog.Duration("duration", 9*time.Millisecond),
		slog.String("string", "value"),
		slog.Bool("bool", true),
		slog.Time("time", time.Unix(10, 0)),
		slog.Any("any", []string{"a", "b"}),
		slog.Group("group", slog.Int("nested", 11), slog.Group("inner", slog.String("leaf", "flat"))),
	)
	records := waitForRecords(t, h, 1)
	attrs := records[0].attrs
	want := map[string]any{
		"int": 7, "uint": 8, "duration": 9 * time.Millisecond, "string": "value",
		"bool": true, "time": time.Unix(10, 0), "any": []string{"a", "b"},
		"group.nested": 11, "group.inner.leaf": "flat",
	}
	for key, expected := range want {
		if got := attrs[key]; !reflect.DeepEqual(got, expected) {
			t.Errorf("%s = %#v (%T), want %#v (%T)", key, got, got, expected, expected)
		}
	}
	if len(attrs) != len(want) {
		t.Fatalf("attributes=%v, want %d flattened attributes", attrs, len(want))
	}
}

func (c *captureHandler) WithAttrs([]slog.Attr) slog.Handler { return c }
func (c *captureHandler) WithGroup(string) slog.Handler      { return c }
