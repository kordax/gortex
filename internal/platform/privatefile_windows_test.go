//go:build windows

package platform

import (
	"testing"
	"unsafe"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/sys/windows"
)

func assertPrivateFilePermissions(t *testing.T, path string) {
	t.Helper()
	user, err := windows.GetCurrentProcessToken().GetTokenUser()
	require.NoError(t, err)
	descriptor, err := windows.GetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION|windows.OWNER_SECURITY_INFORMATION)
	require.NoError(t, err)
	require.NotNil(t, descriptor)
	owner, _, err := descriptor.Owner()
	require.NoError(t, err)
	require.NotNil(t, owner)
	assert.True(t, owner.Equals(user.User.Sid), "the private file owner must be the current user")
	control, _, err := descriptor.Control()
	require.NoError(t, err)
	assert.NotZero(t, control&windows.SE_DACL_PROTECTED, "parent ACLs must not be inherited")
	acl, _, err := descriptor.DACL()
	require.NoError(t, err)
	require.NotNil(t, acl, "a missing DACL grants unrestricted access")
	require.EqualValues(t, 1, acl.AceCount, "only the current user may receive access")
	var ace *windows.ACCESS_ALLOWED_ACE
	require.NoError(t, windows.GetAce(acl, 0, &ace))
	require.NotNil(t, ace)
	assert.EqualValues(t, windows.ACCESS_ALLOWED_ACE_TYPE, ace.Header.AceType)
	assert.Zero(t, ace.Header.AceFlags&windows.INHERITED_ACE)
	wanted := windows.ACCESS_MASK(windows.FILE_GENERIC_READ | windows.FILE_GENERIC_WRITE | windows.DELETE)
	assert.Equal(t, wanted, ace.Mask&wanted, "the owner must be able to read, write and remove the snapshot")
	principal := (*windows.SID)(unsafe.Pointer(&ace.SidStart))
	assert.True(t, principal.Equals(user.User.Sid), "the sole grant must name the current user")
}
