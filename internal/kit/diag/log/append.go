// Inter-process safe append-only log file writing. Multiple rush
// processes (web server, several `rush run`) share one logs/rush.log;
// every handle opens the file with O_APPEND so the kernel places each
// write at the current end of file, and single-record writes cannot
// clobber lines written by another process.
package log

import (
	"os"
	"sync"
)

// appendFileWriter is an io.Writer over a file opened with
// O_APPEND|O_CREATE|O_WRONLY. The mutex serialises writes within the
// process; across processes, O_APPEND is what makes concurrent records
// safe. It replaces lumberjack, whose non-appending open (O_TRUNC, no
// O_APPEND) and rename-based rotation silently dropped or overwrote
// lines whenever more than one rush process wrote to the same file —
// and whose rotation permanently disabled logging on Windows while
// another process held the file open.
type appendFileWriter struct {
	mu sync.Mutex
	f  *os.File
}

// openAppend opens (creating if missing) path for append-only writing.
// The caller owns closing the returned writer.
func openAppend(path string) (*appendFileWriter, error) {
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return nil, err
	}
	return &appendFileWriter{f: f}, nil
}

// Write appends p atomically. The slog JSON handler passes one whole
// record per Write call, so under O_APPEND a record from this process
// either lands whole at the end of the file or not at all — it can
// never overwrite a record another process appended meanwhile.
func (w *appendFileWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.f.Write(p)
}

// Close releases the file handle. Safe to call once; the writer must
// not be used afterwards.
func (w *appendFileWriter) Close() error {
	return w.f.Close()
}
