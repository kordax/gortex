//go:build windows

package platform

import (
	"fmt"
	"os"
	"unsafe"

	"golang.org/x/sys/windows"
)

// CreatePrivateFile atomically creates an owner-only file. Windows ignores the
// POSIX permission bits passed to os.OpenFile, so a protected DACL is supplied
// at creation; inherited directory permissions never expose profile contents.
// CREATE_NEW also refuses existing files and reparse points without touching
// their contents. The caller owns the returned handle.
func CreatePrivateFile(path string) (*os.File, error) {
	user, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		return nil, fmt.Errorf("resolve private file owner: %w", err)
	}
	sid := user.User.Sid.String()
	descriptor, err := windows.SecurityDescriptorFromString("O:" + sid + "D:P(A;;FA;;;" + sid + ")")
	if err != nil {
		return nil, fmt.Errorf("build private file security descriptor: %w", err)
	}
	name, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return nil, err
	}
	attributes := windows.SecurityAttributes{
		Length: uint32(unsafe.Sizeof(windows.SecurityAttributes{})), SecurityDescriptor: descriptor,
	}
	handle, err := windows.CreateFile(name, windows.GENERIC_WRITE, windows.FILE_SHARE_READ,
		&attributes, windows.CREATE_NEW, windows.FILE_ATTRIBUTE_NORMAL, 0)
	if err != nil {
		return nil, &os.PathError{Op: "open", Path: path, Err: err}
	}
	return os.NewFile(uintptr(handle), path), nil
}
