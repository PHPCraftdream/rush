package server

import (
	"bytes"
	"io"
	"os"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

type countingReadSeeker struct {
	*bytes.Reader
	read int
}

func (r *countingReadSeeker) Read(p []byte) (int, error) {
	n, err := r.Reader.Read(p)
	r.read += n
	return n, err
}

func TestReadLastNLinesSeeksBackInChunks(t *testing.T) {
	data := bytes.Repeat([]byte("old line\n"), 500_000)
	data = append(data, []byte("last one\nlast two\n")...)
	reader := &countingReadSeeker{Reader: bytes.NewReader(data)}
	got, err := readLastNLinesFrom(reader, 2)
	require.NoError(t, err)
	require.Equal(t, "last two\n", got)
	require.Less(t, reader.read, len(data)/10, "tail read should not consume the whole file")
}

func TestReadLastNLinesSmallAndEmptyFiles(t *testing.T) {
	for _, tc := range []struct{ name, data, want string }{
		{"empty", "", ""},
		{"no newline", "one\ntwo", "one\ntwo"},
		{"trailing newline", "one\ntwo\n", "two\n"},
		{"fewer lines", "one\ntwo", "one\ntwo"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := t.TempDir() + "/log"
			require.NoError(t, os.WriteFile(path, []byte(tc.data), 0o600))
			got, err := readLastNLines(path, 2)
			require.NoError(t, err)
			require.Equal(t, tc.want, got)
		})
	}
}

func TestReadLastNLinesMatchesSplitReference(t *testing.T) {
	cases := []string{"", "a", "a\n", "a\nb", "a\nb\n", "a\nb\nc", "a\r\nb\r\nc", "\n", "\n\n", "a\n\nb\n", strings.Repeat("x", 37), strings.Repeat("x", 37) + "\ny\nz", strings.Repeat("x", 37) + "\n" + strings.Repeat("y", 41) + "\nz"}
	for length := 0; length < 80; length++ {
		for mask := 0; mask < 8; mask++ {
			var b bytes.Buffer
			for i := 0; i < length; i++ {
				switch (i*7 + mask*11) % 13 {
				case 0:
					b.WriteString("\r\n")
				case 1, 2:
					b.WriteByte('\n')
				default:
					b.WriteByte(byte('a' + (i+mask)%26))
				}
			}
			cases = append(cases, b.String())
		}
	}
	for _, data := range cases {
		for n := -1; n <= strings.Count(data, "\n")+3; n++ {
			want := data
			if n > 0 {
				parts := strings.Split(data, "\n")
				if n < len(parts) {
					want = strings.Join(parts[len(parts)-n:], "\n")
				}
			}
			got, err := readLastNLinesFromChunk(bytes.NewReader([]byte(data)), n, 7)
			require.NoError(t, err)
			require.Equalf(t, want, got, "data=%q n=%d", data, n)
		}
	}
}

func TestReadLastNLinesCrossChunkExplicit(t *testing.T) {
	got, err := readLastNLinesFromChunk(bytes.NewReader([]byte("a\nb\nc")), 2, 2)
	require.NoError(t, err)
	require.Equal(t, "b\nc", got)
}

var _ io.ReadSeeker = (*countingReadSeeker)(nil)
