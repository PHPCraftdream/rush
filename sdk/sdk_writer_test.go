package sdk_test

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/PHPCraftdream/rush/sdk"
)

// concurrentWriterProbe records overlapping calls while protecting its own
// state. The delay makes a missing SDK writer lock observable when both
// provider responses are released together.
type concurrentWriterProbe struct {
	mu       sync.Mutex
	active   int
	overlap  bool
	contents strings.Builder
}

func (w *concurrentWriterProbe) Write(p []byte) (int, error) {
	w.mu.Lock()
	w.active++
	if w.active > 1 {
		w.overlap = true
	}
	w.mu.Unlock()

	time.Sleep(10 * time.Millisecond)

	w.mu.Lock()
	w.contents.Write(p)
	w.active--
	w.mu.Unlock()
	return len(p), nil
}

func (w *concurrentWriterProbe) state() (string, bool) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.contents.String(), w.overlap
}

// TestOptionsDefaultWritersAreSafeAcrossConcurrentRuns verifies the public
// concurrent-run contract using one Run and one RunWithCredentials call,
// both with nil request-level writers. The shared Options-level writer must
// not be entered concurrently; output order remains intentionally
// unspecified and may interleave at Write-call boundaries.
func TestOptionsDefaultWritersAreSafeAcrossConcurrentRuns(t *testing.T) {
	isolateGlobalConfigForWorkdirTest(t)

	const marker = "OPTIONS_DEFAULT_WRITER_OK"
	var providerCalls atomic.Int32
	responsesReady := make(chan struct{})
	var releaseOnce sync.Once
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		if providerCalls.Add(1) >= 2 {
			releaseOnce.Do(func() { close(responsesReady) })
		}
		<-responsesReady

		w.Header().Set("Content-Type", "text/event-stream")
		fl, _ := w.(http.Flusher)
		fmt.Fprintf(w, "data: %s\n\n", fmt.Sprintf(`{"id":"c1","object":"chat.completion.chunk","created":1,"model":"probe","choices":[{"index":0,"delta":{"role":"assistant","content":%q},"finish_reason":null}]}`, marker))
		if fl != nil {
			fl.Flush()
		}
		fmt.Fprint(w, `data: {"id":"c1","object":"chat.completion.chunk","created":1,"model":"probe","choices":[{"index":0,"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":3,"completion_tokens":3,"total_tokens":6}}`+"\n\n")
		fmt.Fprint(w, "data: [DONE]\n\n")
		if fl != nil {
			fl.Flush()
		}
	}))
	t.Cleanup(provider.Close)

	var output concurrentWriterProbe
	client, err := sdk.Open(context.Background(), sdk.Options{
		Mode:          sdk.ModeLibrary,
		LibraryConfig: libraryConfigFor(provider.URL, "sk-writer-test"),
		Stdout:        &output,
		Stderr:        &output,
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = client.Close() })

	creds := sdk.CredentialSet(*libraryConfigFor(provider.URL, "sk-writer-test"))
	start := make(chan struct{})
	errs := make(chan error, 2)
	go func() {
		<-start
		_, runErr := client.Run(context.Background(), sdk.RunRequest{
			Prompt:            "reply with the marker",
			Mode:              sdk.RunModeTerse,
			ContinueSessionID: "options-default-run",
			HideSpinner:       true,
		})
		errs <- runErr
	}()
	go func() {
		<-start
		_, runErr := client.RunWithCredentials(context.Background(), sdk.RunRequest{
			Prompt:            "reply with the marker",
			Mode:              sdk.RunModeTerse,
			ContinueSessionID: "options-default-credentials",
			HideSpinner:       true,
		}, creds)
		errs <- runErr
	}()
	close(start)

	require.NoError(t, <-errs)
	require.NoError(t, <-errs)
	contents, overlap := output.state()
	require.False(t, overlap, "Options-level stdout/stderr defaults must serialize each Write call")
	require.Equal(t, 2, strings.Count(contents, marker), "both runs must contribute their final output")
}
