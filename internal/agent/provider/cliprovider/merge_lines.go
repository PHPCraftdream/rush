package cliprovider

import (
	"bufio"
	"errors"
	"io"
	"sync"
)

// maxMergedLine bounds one buffered line; a longer line is flushed in pieces
// (the consumer's scanner caps a token at 4 MiB anyway).
const maxMergedLine = 4 * 1024 * 1024

// mergeLines copies every source into dst keeping each source's lines whole.
// stdout and stderr of a NoPTY child share one reader; a chunk-wise io.Copy of
// two concurrent sources splices one stream into the middle of the other's
// long line, which breaks the JSON event (lost failure evidence). The returned
// wait blocks until all sources reached EOF or a write failed.
func mergeLines(dst io.Writer, srcs ...io.Reader) (wait func()) {
	var mu sync.Mutex
	var wg sync.WaitGroup
	wg.Add(len(srcs))
	for _, src := range srcs {
		go func(src io.Reader) {
			defer wg.Done()
			copyWholeLines(&mu, dst, src)
		}(src)
	}
	return wg.Wait
}

func copyWholeLines(mu *sync.Mutex, dst io.Writer, src io.Reader) {
	r := bufio.NewReaderSize(src, 64*1024)
	var line []byte
	flush := func() bool {
		if len(line) == 0 {
			return true
		}
		mu.Lock()
		_, err := dst.Write(line)
		mu.Unlock()
		line = line[:0]
		return err == nil
	}
	for {
		chunk, err := r.ReadSlice('\n')
		line = append(line, chunk...)
		switch {
		case err == nil:
			if !flush() {
				return
			}
		case errors.Is(err, bufio.ErrBufferFull):
			if len(line) >= maxMergedLine && !flush() {
				return
			}
		default:
			flush()
			return
		}
	}
}
