package sessscan

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestExtractFilePathsPreservesEscapedQuotesAndWindowsSeparators(t *testing.T) {
	input := `{"file_path":"C:\\work\\a\"b.go"}`
	require.Equal(t, []string{`C:\work\a"b.go`}, ExtractFilePaths("edit", input))
}
