// OS-neutral file-identity errors and the opened-file reader shared by every
// platform implementation.
package config

import (
	"errors"
	"io"
	"os"
)

var errConfigOwnerMismatch = errors.New("config file owner does not match its trust policy")

var errStableReadUnstable = errors.New("config file remained unstable while reading")

// ErrConfigNonRegular rejects pipes, devices, sockets, and directories as
// configuration sources before their contents are read.
var ErrConfigNonRegular = errors.New("config source is not a regular file")

func readOpenedConfigBytes(file *os.File) ([]byte, error) {
	info, err := file.Stat()
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, ErrConfigNonRegular
	}
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return nil, err
	}
	return io.ReadAll(file)
}
