//go:build windows

package config

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"unsafe"

	"golang.org/x/sys/windows"
)

// Windows uses the volume serial number and file-index pair returned by the
// opened handle. This is the Windows equivalent of the Unix device/inode
// identity; descriptor-relative rename is still unavailable through the
// portable API, so commitConfigFile keeps the explicit pre/post path checks.
type configFileIdentity struct {
	device uint64
	inode  uint64
	valid  bool
}

func configFileIdentityOf(os.FileInfo) configFileIdentity { return configFileIdentity{} }

func configFileIdentityOfOpened(file *os.File, _ os.FileInfo) configFileIdentity {
	var info windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(windows.Handle(file.Fd()), &info); err != nil {
		return configFileIdentity{}
	}
	return configFileIdentity{
		device: uint64(info.VolumeSerialNumber),
		inode:  uint64(info.FileIndexHigh)<<32 | uint64(info.FileIndexLow),
		valid:  true,
	}
}

func configFileIdentityAtPath(path string) (configFileIdentity, error) {
	file, err := openWindowsConfigHandle(path, windows.GENERIC_READ, windows.FILE_FLAG_OPEN_REPARSE_POINT)
	if err != nil {
		return configFileIdentity{}, err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return configFileIdentity{}, err
	}
	handleInfo, err := windowsFileInformation(file)
	if err != nil {
		return configFileIdentity{}, err
	}
	if !info.Mode().IsRegular() || handleInfo.FileAttributes&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0 {
		return configFileIdentity{}, os.ErrInvalid
	}
	identity := configFileIdentityOfOpened(file, info)
	if !identity.valid {
		return configFileIdentity{}, os.ErrInvalid
	}
	return identity, nil
}

func configFileNlinkAtPath(path string) (uint64, error) {
	file, err := openWindowsConfigHandle(path, windows.GENERIC_READ, windows.FILE_FLAG_OPEN_REPARSE_POINT)
	if err != nil {
		return 0, err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return 0, err
	}
	handleInfo, err := windowsFileInformation(file)
	if err != nil {
		return 0, err
	}
	if !info.Mode().IsRegular() || handleInfo.FileAttributes&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0 {
		return 0, os.ErrInvalid
	}
	return uint64(handleInfo.NumberOfLinks), nil
}

func removeConfigTempIfIdentity(path string, expected configFileIdentity) error {
	file, err := openWindowsConfigHandle(path, windows.DELETE|windows.FILE_READ_ATTRIBUTES, windows.FILE_FLAG_OPEN_REPARSE_POINT)
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
	handleInfo, err := windowsFileInformation(file)
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() || handleInfo.FileAttributes&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0 {
		return os.ErrInvalid
	}
	identity := configFileIdentityOfOpened(file, info)
	if identity != expected {
		return os.ErrInvalid
	}
	return deleteWindowsConfigHandle(file)
}

func openWindowsConfigHandle(path string, access, flags uint32) (*os.File, error) {
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

func windowsFileInformation(file *os.File) (windows.ByHandleFileInformation, error) {
	var info windows.ByHandleFileInformation
	err := windows.GetFileInformationByHandle(windows.Handle(file.Fd()), &info)
	return info, err
}

func openWindowsConfigParent(path string) (*os.File, error) {
	parentPath := filepath.Dir(filepath.Clean(path))
	parent, err := openWindowsConfigHandle(parentPath, windows.GENERIC_READ|windows.GENERIC_WRITE, windows.FILE_FLAG_BACKUP_SEMANTICS|windows.FILE_FLAG_OPEN_REPARSE_POINT)
	if err != nil {
		return nil, fmt.Errorf("open config parent: %w", err)
	}
	info, infoErr := windowsFileInformation(parent)
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

type windowsStagedConfigFile struct {
	path     string
	name     string
	file     *os.File
	identity configFileIdentity
}

const configTempNameAttempts = 16

func stageConfigFileHandle(path string, data []byte, perm os.FileMode) (windowsStagedConfigFile, error) {
	return stageConfigFileHandleAt(nil, path, data, perm)
}

func stageConfigFileHandleAt(parent *os.File, path string, data []byte, perm os.FileMode) (windowsStagedConfigFile, error) {
	dir := filepath.Dir(filepath.Clean(path))
	base := filepath.Base(path)
	for range configTempNameAttempts {
		suffix := make([]byte, 16)
		if _, err := rand.Read(suffix); err != nil {
			return windowsStagedConfigFile{}, fmt.Errorf("create temporary config name: %w", err)
		}
		tmpPath := filepath.Join(dir, "."+base+"."+hex.EncodeToString(suffix)+".tmp")
		var handle windows.Handle
		var err error
		if parent == nil {
			apiPath, pathErr := windowsConfigAPIPath(tmpPath)
			if pathErr != nil {
				return windowsStagedConfigFile{}, pathErr
			}
			name, nameErr := windows.UTF16PtrFromString(apiPath)
			if nameErr != nil {
				return windowsStagedConfigFile{}, nameErr
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
			return windowsStagedConfigFile{}, fmt.Errorf("create temporary config: %w", err)
		}
		file := os.NewFile(uintptr(handle), filepath.Clean(tmpPath))
		cleanup := true
		defer func() {
			if cleanup {
				// Delete while the handle still pins the object.
				_ = deleteWindowsConfigHandle(file)
				_ = file.Close()
			}
		}()
		info, statErr := file.Stat()
		if statErr != nil {
			return windowsStagedConfigFile{}, statErr
		}
		identity := configFileIdentityOfOpened(file, info)
		if !identity.valid || !info.Mode().IsRegular() {
			return windowsStagedConfigFile{}, os.ErrInvalid
		}
		if _, err := file.Write(data); err != nil {
			return windowsStagedConfigFile{}, err
		}
		if err := file.Chmod(perm); err != nil {
			return windowsStagedConfigFile{}, err
		}
		if err := file.Sync(); err != nil {
			return windowsStagedConfigFile{}, err
		}
		// The identity is deliberately captured while this DELETE-capable
		// handle remains open. The pathname is not used as an identity oracle.
		cleanup = false
		return windowsStagedConfigFile{path: filepath.Clean(tmpPath), name: filepath.Base(tmpPath), file: file, identity: identity}, nil
	}
	return windowsStagedConfigFile{}, fmt.Errorf("create temporary config: %w", os.ErrExist)
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

func openWindowsConfigEntryAt(parent *os.File, name string) (*os.File, error) {
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

func isWindowsEntryNotFound(err error) bool {
	return err == windows.STATUS_OBJECT_NAME_NOT_FOUND || err == windows.STATUS_OBJECT_PATH_NOT_FOUND
}

func deleteWindowsConfigHandle(file *os.File) error {
	setInfo := func(class uint32, buffer *byte, length uint32) error {
		configTestHooks.Lock()
		hook := configTestHooks.setFileInformation
		configTestHooks.Unlock()
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

func flushWindowsConfigHandle(file *os.File) error {
	configTestHooks.Lock()
	hook := configTestHooks.flushFileBuffers
	configTestHooks.Unlock()
	if hook != nil {
		return hook(uintptr(file.Fd()))
	}
	return windows.FlushFileBuffers(windows.Handle(file.Fd()))
}

func readStableConfigHandle(file *os.File, expectedOwner int, enforceOwner bool) ([]byte, reloadFileFingerprint, error) {
	info, err := file.Stat()
	if err != nil {
		return nil, reloadFileFingerprint{}, err
	}
	handleInfo, err := windowsFileInformation(file)
	if err != nil {
		return nil, reloadFileFingerprint{}, err
	}
	if !info.Mode().IsRegular() || handleInfo.FileAttributes&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0 {
		return nil, reloadFileFingerprint{}, os.ErrInvalid
	}
	owner, ownerKnown := configFileOwner(info)
	if enforceOwner && (!ownerKnown || owner != expectedOwner) {
		return nil, reloadFileFingerprint{}, errConfigOwnerMismatch
	}
	first, err := readOpenedConfigBytes(file)
	if err != nil {
		return nil, reloadFileFingerprint{}, err
	}
	second, err := readOpenedConfigBytes(file)
	if err != nil {
		return nil, reloadFileFingerprint{}, err
	}
	if !bytes.Equal(first, second) {
		return nil, reloadFileFingerprint{}, errStableReadUnstable
	}
	finalInfo, err := file.Stat()
	if err != nil {
		return nil, reloadFileFingerprint{}, err
	}
	return second, reloadFileFingerprint{
		exists: true, size: int64(len(second)), modTime: finalInfo.ModTime().UnixNano(),
		digest: sha256.Sum256(second), owner: owner,
		nlink:    configFileNlinkOfOpened(file, finalInfo),
		identity: configFileIdentityOfOpened(file, finalInfo),
	}, nil
}

func configFileOwner(os.FileInfo) (int, bool) { return -1, true }

func configFileNlinkOfOpened(file *os.File, _ os.FileInfo) uint64 {
	var info windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(windows.Handle(file.Fd()), &info); err != nil {
		return 0
	}
	return uint64(info.NumberOfLinks)
}

func sameConfigFileIdentity(left, right os.FileInfo) bool {
	// The opened handle identity is checked separately. FileInfo does not carry
	// the handle's volume/file-index pair, so this path-level comparison is only
	// the portable read-stability check, not a security decision.
	return true
}

func configFilePathIdentityMatches(path string, opened *os.File, openedInfo os.FileInfo) (bool, error) {
	current, err := openWindowsConfigHandle(path, windows.GENERIC_READ, 0)
	if err != nil {
		return false, err
	}
	defer current.Close()

	currentInfo, err := current.Stat()
	if err != nil {
		return false, err
	}
	currentHandleInfo, err := windowsFileInformation(current)
	if err != nil {
		return false, err
	}
	if currentInfo.IsDir() || !currentInfo.Mode().IsRegular() || currentHandleInfo.FileAttributes&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0 {
		return false, nil
	}
	openedHandleInfo, err := windowsFileInformation(opened)
	if err != nil {
		return false, err
	}
	if openedInfo.IsDir() || !openedInfo.Mode().IsRegular() || openedHandleInfo.FileAttributes&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0 {
		return false, nil
	}
	openedIdentity := configFileIdentityOfOpened(opened, openedInfo)
	currentIdentity := configFileIdentityOfOpened(current, currentInfo)
	return openedIdentity.valid && openedIdentity == currentIdentity, nil
}
