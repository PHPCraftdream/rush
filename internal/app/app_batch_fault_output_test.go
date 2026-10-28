package app

import (
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestBatchFaultHandleFaultWritesStderrAndJSONBeforeExitExactlyOnce(t *testing.T) {
	fault := errors.New("commit projection failed")
	stderrFile, err := os.CreateTemp(t.TempDir(), "stderr")
	require.NoError(t, err)
	oldStderr := os.Stderr
	os.Stderr = stderrFile
	t.Cleanup(func() {
		os.Stderr = oldStderr
		_ = stderrFile.Close()
	})

	var output strings.Builder
	writer := &batchFaultEnvelopeWriter{writer: &output}
	app := &App{
		batchFaultWriter: writer,
		batchFaultOutput: func(err error) {
			if writeErr := writer.writeFault("session-1", err); writeErr != nil {
				t.Errorf("write fault envelope: %v", writeErr)
			}
		},
	}
	called := 0
	var rt *BatchRuntime
	rt = NewBatchRuntime(BatchRuntimeConfig{
		ContainmentWait: time.Nanosecond,
		WaitFunc: func(time.Duration) <-chan time.Time {
			ch := make(chan time.Time, 1)
			ch <- time.Now()
			return ch
		},
		BeforeExit: func(err error) {
			called++
			require.True(t, rt.Fenced(), "fault must fence before publishing output")
			require.ErrorIs(t, err, fault)
			app.handleBatchFaultOutput(err)
		},
		ExitFunc: func(code int) {
			require.Equal(t, 1, called, "output callback must run before process exit")
			require.Equal(t, BatchFaultExitCode, code)
			_, seekErr := stderrFile.Seek(0, 0)
			require.NoError(t, seekErr)
			stderr, readErr := os.ReadFile(stderrFile.Name())
			require.NoError(t, readErr)
			require.Equal(t, "rush run: batch runtime fault: commit projection failed; exiting with code 70\n", string(stderr))
			require.Contains(t, output.String(), `"exit_reason":"batch_fault"`)
			require.Contains(t, output.String(), `"error":"commit projection failed"`)
			require.Contains(t, output.String(), `"session_id":"session-1"`)
		},
	})

	rt.handleFault(fault)
	rt.handleFault(fault)
	require.Equal(t, 1, called)
	require.Equal(t, 1, strings.Count(output.String(), "\n"))
}

func TestBatchFaultJSONEnvelopeWriterPreventsCompetingEnvelope(t *testing.T) {
	var output strings.Builder
	writer := &batchFaultEnvelopeWriter{writer: &output}
	require.NoError(t, writer.writeFault("s", errors.New("fault")))
	_, err := writer.Write([]byte(`{"exit_reason":"stop"}`))
	require.NoError(t, err)
	require.Equal(t, 1, strings.Count(output.String(), "\n"))
	require.Contains(t, output.String(), `"exit_reason":"batch_fault"`)
	require.NotContains(t, output.String(), `"exit_reason":"stop"`)
}

func TestBatchFaultJSONEnvelopeWriterWritesSplitNormalImmediatelyAndSuppressesFault(t *testing.T) {
	var output strings.Builder
	writer := &batchFaultEnvelopeWriter{writer: &output}
	first := []byte(`{"session_id":"session-1",`)
	second := []byte(`"exit_reason":"completed"}` + "\n")

	n, err := writer.Write(first)
	require.NoError(t, err)
	require.Equal(t, len(first), n)
	require.Equal(t, string(first), output.String(), "first normal write must pass through immediately")

	n, err = writer.Write(second)
	require.NoError(t, err)
	require.Equal(t, len(second), n)
	normal := append(append([]byte(nil), first...), second...)
	require.Equal(t, string(normal), output.String(), "every normal write must pass through")

	require.NoError(t, writer.writeFault("session-1", errors.New("late fault")))
	require.Equal(t, string(normal), output.String(), "normal output suppresses later fault envelope")
}

func TestBatchFaultJSONEnvelopeWriterWritesNormalImmediatelyAndSuppressesFault(t *testing.T) {
	var output strings.Builder
	writer := &batchFaultEnvelopeWriter{writer: &output}
	normal := []byte(`{"session_id":"session-1","exit_reason":"completed"}` + "\n")
	n, err := writer.Write(normal)
	require.NoError(t, err)
	require.Equal(t, len(normal), n)
	require.Equal(t, string(normal), output.String(), "normal JSON must pass through immediately")

	require.NoError(t, writer.writeFault("session-1", errors.New("late fault")))
	require.Equal(t, string(normal), output.String())
}

func TestBatchFaultJSONEnvelopeWriterFaultBeforeOutput(t *testing.T) {
	var output strings.Builder
	writer := &batchFaultEnvelopeWriter{writer: &output}
	require.NoError(t, writer.writeFault("session-1", errors.New("fault")))
	_, err := writer.Write([]byte(`{"exit_reason":"completed"}` + "\n"))
	require.NoError(t, err)
	require.Equal(t, 1, strings.Count(output.String(), "\n"))
	require.Contains(t, output.String(), `"exit_reason":"batch_fault"`)
	require.NotContains(t, output.String(), `"exit_reason":"completed"`)
}

func TestBatchFaultJSONEnvelopeWriterFaultTwice(t *testing.T) {
	var output strings.Builder
	writer := &batchFaultEnvelopeWriter{writer: &output}
	require.NoError(t, writer.writeFault("session-1", errors.New("first fault")))
	require.NoError(t, writer.writeFault("session-1", errors.New("second fault")))
	require.Equal(t, 1, strings.Count(output.String(), "\n"))
	require.Contains(t, output.String(), `"error":"first fault"`)
	require.NotContains(t, output.String(), `"error":"second fault"`)
}

type partialFailWriter struct {
	written []byte
}

func (w *partialFailWriter) Write(p []byte) (int, error) {
	n := len(p) / 2
	w.written = append(w.written, p[:n]...)
	return n, errors.New("partial write")
}

func TestBatchFaultJSONEnvelopeWriterDoesNotAppendAfterPartialWrite(t *testing.T) {
	out := &partialFailWriter{}
	writer := &batchFaultEnvelopeWriter{writer: out}
	require.Error(t, writer.writeFault("s", errors.New("fault")))
	first := append([]byte(nil), out.written...)
	_, err := writer.Write([]byte("normal"))
	require.NoError(t, err)
	require.NoError(t, writer.writeFault("s", errors.New("retry")))
	require.Equal(t, first, out.written, "partial JSON must not get a second envelope appended")
}

func TestWriteBatchFaultOutputUsesSingleStderrLine(t *testing.T) {
	var stderr strings.Builder
	writeBatchFaultOutput(&stderr, errors.New("commit projection failed"))
	require.Equal(t, "rush run: batch runtime fault: commit projection failed; exiting with code 70\n", stderr.String())
}

// A fault after the normal envelope has been written must not add a second
// JSON document to stdout; the stderr diagnostic still appears.
func TestBatchFaultHandleFaultAfterNormalOutputAddsNoSecondEnvelope(t *testing.T) {
	fault := errors.New("late commit fault")
	stderrFile, err := os.CreateTemp(t.TempDir(), "stderr")
	require.NoError(t, err)
	oldStderr := os.Stderr
	os.Stderr = stderrFile
	t.Cleanup(func() {
		os.Stderr = oldStderr
		_ = stderrFile.Close()
	})

	var output strings.Builder
	writer := &batchFaultEnvelopeWriter{writer: &output}
	normal := `{"session_id":"session-1","exit_reason":"completed"}` + "\n"
	_, err = writer.Write([]byte(normal))
	require.NoError(t, err)

	app := &App{
		batchFaultWriter: writer,
		batchFaultOutput: func(err error) { _ = writer.writeFault("session-1", err) },
	}
	exited := 0
	rt := NewBatchRuntime(BatchRuntimeConfig{
		ContainmentWait: time.Nanosecond,
		WaitFunc: func(time.Duration) <-chan time.Time {
			ch := make(chan time.Time, 1)
			ch <- time.Now()
			return ch
		},
		BeforeExit: app.handleBatchFaultOutput,
		ExitFunc: func(code int) {
			exited++
			require.Equal(t, BatchFaultExitCode, code)
		},
	})

	rt.handleFault(fault)

	require.Equal(t, 1, exited)
	require.Equal(t, normal, output.String(), "stdout keeps exactly the normal envelope")
	_, err = stderrFile.Seek(0, 0)
	require.NoError(t, err)
	stderr, err := os.ReadFile(stderrFile.Name())
	require.NoError(t, err)
	require.Equal(t, "rush run: batch runtime fault: late commit fault; exiting with code 70\n", string(stderr))
}
