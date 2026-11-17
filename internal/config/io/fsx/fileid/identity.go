// OS-neutral file-identity errors and the opened-file reader shared by every
// platform implementation.
package fileid

import (
	"errors"
	"io"
	"os"
)

var errConfigOwnerMismatch = errors.New("config file owner does not match its trust policy")

var errStableReadUnstable = errors.New("config file remained unstable while reading")

// ErrConfigOwnerMismatch returns the package's single sentinel identity.
func ErrConfigOwnerMismatch() error { return errConfigOwnerMismatch }

// ErrStableReadUnstable returns the package's single sentinel identity.
func ErrStableReadUnstable() error { return errStableReadUnstable }

var errConfigCommitVerification = errors.New("config destination could not be verified immediately before commit")

// ErrConfigCommitVerification returns the package's single sentinel identity.
func ErrConfigCommitVerification() error { return errConfigCommitVerification }

// ErrConfigHardLink identifies a config file with more than one directory
// entry. Atomic replacement would update only one spelling and leave the
// other hard-link alias stale, so mutable config APIs reject it.
var ErrConfigHardLink = errors.New("config file has multiple hard links")

// ErrConfigNonRegular rejects pipes, devices, sockets, and directories as
// configuration sources before their contents are read.
var ErrConfigNonRegular = errors.New("config source is not a regular file")

func ReadOpenedConfigBytes(file *os.File) ([]byte, error) {
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
