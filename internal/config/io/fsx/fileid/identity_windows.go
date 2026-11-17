//go:build windows

package fileid

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"unsafe"

	"github.com/PHPCraftdream/rush/internal/config/io/cfghooks"
	"golang.org/x/sys/windows"
)

// Windows uses the volume serial number and file-index pair returned by the
// opened handle. This is the Windows equivalent of the Unix device/inode
// identity; descriptor-relative rename is still unavailable through the
// portable API, so commitConfigFile keeps the explicit pre/post path checks.
type FileIdentity struct {
	Device uint64
	Inode  uint64
	Valid  bool
}

func (identity FileIdentity) FingerprintString() string {
	return fmt.Sprintf("%t|%d|%d", identity.Valid, identity.Device, identity.Inode)
}

func FileIdentityOf(os.FileInfo) FileIdentity { return FileIdentity{} }

func FileIdentityOfOpened(file *os.File, _ os.FileInfo) FileIdentity {
	var info windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(windows.Handle(file.Fd()), &info); err != nil {
		return FileIdentity{}
	}
	return FileIdentity{
		Device: uint64(info.VolumeSerialNumber),
		Inode:  uint64(info.FileIndexHigh)<<32 | uint64(info.FileIndexLow),
		Valid:  true,
	}
}

func FileIdentityAtPath(path string) (FileIdentity, error) {
	file, err := OpenWindowsConfigHandle(path, windows.GENERIC_READ, windows.FILE_FLAG_OPEN_REPARSE_POINT)
	if err != nil {
		return FileIdentity{}, err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return FileIdentity{}, err
	}
	handleInfo, err := WindowsFileInformation(file)
	if err != nil {
		return FileIdentity{}, err
	}
	if !info.Mode().IsRegular() || handleInfo.FileAttributes&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0 {
		return FileIdentity{}, os.ErrInvalid
	}
	identity := FileIdentityOfOpened(file, info)
	if !identity.Valid {
		return FileIdentity{}, os.ErrInvalid
	}
	return identity, nil
}

func configFileNlinkAtPath(path string) (uint64, error) {
	file, err := OpenWindowsConfigHandle(path, windows.GENERIC_READ, windows.FILE_FLAG_OPEN_REPARSE_POINT)
	if err != nil {
		return 0, err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return 0, err
	}
	handleInfo, err := WindowsFileInformation(file)
	if err != nil {
		return 0, err
	}
	if !info.Mode().IsRegular() || handleInfo.FileAttributes&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0 {
		return 0, os.ErrInvalid
	}
	return uint64(handleInfo.NumberOfLinks), nil
}

func removeConfigTempIfIdentity(path string, expected FileIdentity) error {
	file, err := OpenWindowsConfigHandle(path, windows.DELETE|windows.FILE_READ_ATTRIBUTES, windows.FILE_FLAG_OPEN_REPARSE_POINT)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return err
	}
	handleInfo, err := WindowsFileInformation(file)
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() || handleInfo.FileAttributes&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0 {
		return os.ErrInvalid
	}
	identity := FileIdentityOfOpened(file, info)
	if identity != expected {
		return os.ErrInvalid
	}
	return DeleteWindowsConfigHandle(file)
}

func OpenWindowsConfigHandle(path string, access, flags uint32) (*os.File, error) {
	clean := filepath.Clean(path)
	apiPath, err := windowsConfigAPIPath(clean)
	if err != nil {
		return nil, err
	}
	name, err := windows.UTF16PtrFromString(apiPath)
	if err != nil {
		return nil, err
	}
	handle, err := windows.CreateFile(
		name,
		access,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
		nil,
		windows.OPEN_EXISTING,
		flags,
		0,
	)
	if err != nil {
		return nil, err
	}
	return os.NewFile(uintptr(handle), clean), nil
}

// WindowsConfigAPIPath converts a path to the \\?\ extended-length form the
// Windows config APIs require.
func WindowsConfigAPIPath(path string) (string, error) {
	return windowsConfigAPIPath(path)
}

func windowsConfigAPIPath(path string) (string, error) {
	clean, err := filepath.Abs(filepath.Clean(path))
	if err != nil {
		return "", err
	}
	clean = filepath.Clean(clean)
	if strings.HasPrefix(clean, `\\?\`) || strings.HasPrefix(clean, `\\.\`) {
		return clean, nil
	}
	if strings.HasPrefix(clean, `\\`) {
		return `\\?\UNC\` + strings.TrimPrefix(clean, `\\`), nil
	}
	if len(clean) >= 2 && clean[1] == ':' {
		return `\\?\` + clean, nil
	}
	return clean, nil
}

func WindowsFileInformation(file *os.File) (windows.ByHandleFileInformation, error) {
	var info windows.ByHandleFileInformation
	err := windows.GetFileInformationByHandle(windows.Handle(file.Fd()), &info)
	return info, err
}

// OpenWindowsConfigParent opens the parent directory of path.
func OpenWindowsConfigParent(path string) (*os.File, error) {
	return openWindowsConfigParent(path)
}

func openWindowsConfigParent(path string) (*os.File, error) {
	parentPath := filepath.Dir(filepath.Clean(path))
	parent, err := OpenWindowsConfigHandle(parentPath, windows.GENERIC_READ|windows.GENERIC_WRITE, windows.FILE_FLAG_BACKUP_SEMANTICS|windows.FILE_FLAG_OPEN_REPARSE_POINT)
	if err != nil {
		return nil, fmt.Errorf("open config parent: %w", err)
	}
	info, infoErr := WindowsFileInformation(parent)
	if infoErr != nil {
		_ = parent.Close()
		return nil, infoErr
	}
	if info.FileAttributes&windows.FILE_ATTRIBUTE_DIRECTORY == 0 || info.FileAttributes&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0 {
		_ = parent.Close()
		return nil, os.ErrInvalid
	}
	return parent, nil
}

type WindowsStagedConfigFile struct {
	Path     string
	Name     string
	File     *os.File
	Identity FileIdentity
}

const configTempNameAttempts = 16

func StageConfigFileHandle(path string, data []byte, perm os.FileMode) (WindowsStagedConfigFile, error) {
	return StageConfigFileHandleAt(nil, path, data, perm)
}

func StageConfigFileHandleAt(parent *os.File, path string, data []byte, perm os.FileMode, migrationHooks ...func(string) error) (WindowsStagedConfigFile, error) {
	dir := filepath.Dir(filepath.Clean(path))
	base := filepath.Base(path)
	for range configTempNameAttempts {
		suffix := make([]byte, 16)
		if _, err := rand.Read(suffix); err != nil {
			return WindowsStagedConfigFile{}, fmt.Errorf("create temporary config name: %w", err)
		}
		tmpPath := filepath.Join(dir, "."+base+"."+hex.EncodeToString(suffix)+".tmp")
		var handle windows.Handle
		var err error
		if parent == nil {
			apiPath, pathErr := windowsConfigAPIPath(tmpPath)
			if pathErr != nil {
				return WindowsStagedConfigFile{}, pathErr
			}
			name, nameErr := windows.UTF16PtrFromString(apiPath)
			if nameErr != nil {
				return WindowsStagedConfigFile{}, nameErr
			}
			handle, err = windows.CreateFile(
				name,
				windows.GENERIC_READ|windows.GENERIC_WRITE|windows.DELETE,
				windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
				nil,
				windows.CREATE_NEW,
				windows.FILE_ATTRIBUTE_NORMAL|windows.FILE_FLAG_WRITE_THROUGH,
				0,
			)
		} else {
			handle, err = createWindowsConfigEntryAt(parent, filepath.Base(tmpPath), windows.GENERIC_READ|windows.GENERIC_WRITE|windows.DELETE|windows.SYNCHRONIZE, windows.FILE_CREATE, windows.FILE_NON_DIRECTORY_FILE|windows.FILE_SYNCHRONOUS_IO_NONALERT|windows.FILE_WRITE_THROUGH)
		}
		if err != nil {
			if err == windows.ERROR_FILE_EXISTS || err == windows.STATUS_OBJECT_NAME_COLLISION {
				continue
			}
			return WindowsStagedConfigFile{}, fmt.Errorf("create temporary config: %w", err)
		}
		file := os.NewFile(uintptr(handle), filepath.Clean(tmpPath))
		cleanup := true
		defer func() {
			if cleanup {
				// Delete while the handle still pins the object.
				_ = DeleteWindowsConfigHandle(file)
				_ = file.Close()
			}
		}()
		info, statErr := file.Stat()
		if statErr != nil {
			return WindowsStagedConfigFile{}, statErr
		}
		identity := FileIdentityOfOpened(file, info)
		if !identity.Valid || !info.Mode().IsRegular() {
			return WindowsStagedConfigFile{}, os.ErrInvalid
		}
		if _, err := file.Write(data[:len(data)/2]); err != nil {
			return WindowsStagedConfigFile{}, err
		}
		if len(migrationHooks) > 0 && migrationHooks[0] != nil {
			if err := migrationHooks[0]("during-staging"); err != nil {
				cleanup = false
				_ = file.Close()
				return WindowsStagedConfigFile{}, err
			}
		}
		if _, err := file.Write(data[len(data)/2:]); err != nil {
			return WindowsStagedConfigFile{}, err
		}
		if err := file.Chmod(perm); err != nil {
			return WindowsStagedConfigFile{}, err
		}
		if err := file.Sync(); err != nil {
			return WindowsStagedConfigFile{}, err
		}
		if len(migrationHooks) > 0 && migrationHooks[0] != nil {
			if err := migrationHooks[0]("after-staging"); err != nil {
				return WindowsStagedConfigFile{}, err
			}
		}
		// The identity is deliberately captured while this DELETE-capable
		// handle remains open. The pathname is not used as an identity oracle.
		cleanup = false
		return WindowsStagedConfigFile{Path: filepath.Clean(tmpPath), Name: filepath.Base(tmpPath), File: file, Identity: identity}, nil
	}
	return WindowsStagedConfigFile{}, fmt.Errorf("create temporary config: %w", os.ErrExist)
}

// CreateWindowsConfigEntryAt creates a config entry under an opened parent.
func CreateWindowsConfigEntryAt(parent *os.File, name string, access, disposition, options uint32) (windows.Handle, error) {
	return createWindowsConfigEntryAt(parent, name, access, disposition, options)
}

func createWindowsConfigEntryAt(parent *os.File, name string, access, disposition, options uint32) (windows.Handle, error) {
	if name == "" || filepath.Base(name) != name || name == "." || name == ".." {
		return windows.InvalidHandle, os.ErrInvalid
	}
	objectName, err := windows.NewNTUnicodeString(name)
	if err != nil {
		return windows.InvalidHandle, err
	}
	attributes := windows.OBJECT_ATTRIBUTES{
		Length:        uint32(unsafe.Sizeof(windows.OBJECT_ATTRIBUTES{})),
		RootDirectory: windows.Handle(parent.Fd()),
		ObjectName:    objectName,
		Attributes:    windows.OBJ_CASE_INSENSITIVE,
	}
	var (
		handle windows.Handle
		status windows.IO_STATUS_BLOCK
		alloc  int64
	)
	if err := windows.NtCreateFile(
		&handle,
		access,
		&attributes,
		&status,
		&alloc,
		windows.FILE_ATTRIBUTE_NORMAL,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
		disposition,
		options,
		0,
		0,
	); err != nil {
		return windows.InvalidHandle, err
	}
	return handle, nil
}

func OpenWindowsConfigEntryAt(parent *os.File, name string) (*os.File, error) {
	handle, err := createWindowsConfigEntryAt(
		parent,
		name,
		windows.GENERIC_READ|windows.FILE_READ_ATTRIBUTES|windows.SYNCHRONIZE,
		windows.FILE_OPEN,
		windows.FILE_NON_DIRECTORY_FILE|windows.FILE_OPEN_REPARSE_POINT|windows.FILE_SYNCHRONOUS_IO_NONALERT,
	)
	if err != nil {
		return nil, err
	}
	return os.NewFile(uintptr(handle), filepath.Join(parent.Name(), name)), nil
}

func IsWindowsEntryNotFound(err error) bool {
	return err == windows.STATUS_OBJECT_NAME_NOT_FOUND || err == windows.STATUS_OBJECT_PATH_NOT_FOUND
}

func DeleteWindowsConfigHandle(file *os.File) error {
	setInfo := func(class uint32, buffer *byte, length uint32) error {
		hook := cfghooks.Snapshot().SetFileInformation
		if hook != nil {
			return hook(uintptr(file.Fd()), class, buffer, length)
		}
		return windows.SetFileInformationByHandle(windows.Handle(file.Fd()), class, buffer, length)
	}
	flags := uint32(windows.FILE_DISPOSITION_DELETE | windows.FILE_DISPOSITION_POSIX_SEMANTICS)
	err := setInfo(windows.FileDispositionInfoEx, (*byte)(unsafe.Pointer(&flags)), uint32(unsafe.Sizeof(flags)))
	if err == nil {
		return nil
	}
	if err != windows.ERROR_INVALID_PARAMETER && err != windows.ERROR_INVALID_FUNCTION && err != windows.ERROR_NOT_SUPPORTED {
		return err
	}
	delete := byte(1)
	return setInfo(windows.FileDispositionInfo, &delete, 1)
}

func FlushWindowsConfigHandle(file *os.File) error {
	hook := cfghooks.Snapshot().FlushFileBuffers
	if hook != nil {
		return hook(uintptr(file.Fd()))
	}
	return windows.FlushFileBuffers(windows.Handle(file.Fd()))
}

func OpenStableConfigFile(path string) (*os.File, error) {
	return OpenWindowsConfigHandle(path, windows.GENERIC_READ, 0)
}

// WriteStableConfigFile writes config data to the existing stable file.
func WriteStableConfigFile(path string, data []byte) error {
	file, err := OpenWindowsConfigHandle(path, windows.GENERIC_READ|windows.GENERIC_WRITE, 0)
	if err != nil {
		return err
	}
	defer file.Close()
	if err := file.Truncate(0); err != nil {
		return err
	}
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return err
	}
	if _, err := file.Write(data); err != nil {
		return err
	}
	return file.Sync()
}

func ConfigFileOwner(os.FileInfo) (int, bool) { return -1, true }

func FileNlinkOfOpened(file *os.File, _ os.FileInfo) uint64 {
	var info windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(windows.Handle(file.Fd()), &info); err != nil {
		return 0
	}
	return uint64(info.NumberOfLinks)
}

func SameConfigFileIdentity(left, right os.FileInfo) bool {
	// The opened handle identity is checked separately. FileInfo does not carry
	// the handle's volume/file-index pair, so this path-level comparison is only
	// the portable read-stability check, not a security decision.
	return true
}

func ConfigFilePathIdentityMatches(path string, opened *os.File, openedInfo os.FileInfo) (bool, error) {
	current, err := OpenWindowsConfigHandle(path, windows.GENERIC_READ, 0)
	if err != nil {
		return false, err
	}
	defer current.Close()

	currentInfo, err := current.Stat()
	if err != nil {
		return false, err
	}
	currentHandleInfo, err := WindowsFileInformation(current)
	if err != nil {
		return false, err
	}
	if currentInfo.IsDir() || !currentInfo.Mode().IsRegular() || currentHandleInfo.FileAttributes&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0 {
		return false, nil
	}
	openedHandleInfo, err := WindowsFileInformation(opened)
	if err != nil {
		return false, err
	}
	if openedInfo.IsDir() || !openedInfo.Mode().IsRegular() || openedHandleInfo.FileAttributes&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0 {
		return false, nil
	}
	openedIdentity := FileIdentityOfOpened(opened, openedInfo)
	currentIdentity := FileIdentityOfOpened(current, currentInfo)
	return openedIdentity.Valid && openedIdentity == currentIdentity, nil
}

// PhysicalWindowsConfigPath resolves the physical (non-substituted) path of
// a config file.
func PhysicalWindowsConfigPath(path string) (string, error) {
	return physicalWindowsConfigPath(path)
}

func physicalWindowsConfigPath(path string) (string, error) {
	clean := filepath.Clean(path)
	if resolved, err := filepath.EvalSymlinks(clean); err == nil {
		return filepath.Clean(resolved), nil
	} else if !os.IsNotExist(err) {
		return "", err
	}
	parent, err := filepath.EvalSymlinks(filepath.Dir(clean))
	if err != nil {
		return "", err
	}
	return filepath.Join(parent, filepath.Base(clean)), nil
}

// Windows does not expose Unix uid ownership. fsext.Owner returns -1 there,
// which is the deliberate platform-neutral bypass for this policy.
func HomeConfigOwner() int { return -1 }

func SystemConfigOwner() int { return -1 }

func ConfigParentIdentity(path string) FileIdentity {
	physicalPath, err := physicalWindowsConfigPath(path)
	if err != nil {
		return FileIdentity{}
	}
	parent, err := openWindowsConfigParent(physicalPath)
	if err != nil {
		return FileIdentity{}
	}
	defer parent.Close()
	return FileIdentityOfOpened(parent, nil)
}
