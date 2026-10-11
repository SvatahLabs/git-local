//go:build windows

package keymaterial

import (
	"path/filepath"
	"strings"
	"testing"
	"unsafe"

	"golang.org/x/sys/windows"
)

// keygen gives the key a protected owner-only ACL whatever its folder
// grants, and Load refuses a key others may read.
func TestWindowsKeyIsOwnerOnly(t *testing.T) {
	keys := t.TempDir()
	user, err := currentUser()
	if err != nil {
		t.Fatal(err)
	}
	// The folder grants Everyone read, inherited by new files.
	folder, err := windows.SecurityDescriptorFromString("D:P(A;OICI;FA;;;" + user.String() + ")(A;OICI;FR;;;WD)")
	if err != nil {
		t.Fatal(err)
	}
	folderACL, _, err := folder.DACL()
	if err != nil {
		t.Fatal(err)
	}
	if err := windows.SetNamedSecurityInfo(keys, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION, nil, nil, folderACL, nil); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(keys, "sensitive.key")
	if err := Create(path, t.TempDir()); err != nil {
		t.Fatal(err)
	}
	sd, err := windows.GetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, windows.OWNER_SECURITY_INFORMATION|windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		t.Fatal(err)
	}
	control, _, err := sd.Control()
	if err != nil || control&windows.SE_DACL_PROTECTED == 0 {
		t.Fatalf("key ACL isn't protected: %v %v", control, err)
	}
	dacl, _, err := sd.DACL()
	if err != nil || dacl == nil || dacl.AceCount != 1 {
		t.Fatalf("key ACL: %v %v", dacl, err)
	}
	var ace *windows.ACCESS_ALLOWED_ACE
	if err := windows.GetAce(dacl, 0, &ace); err != nil {
		t.Fatal(err)
	}
	if sid := (*windows.SID)(unsafe.Pointer(&ace.SidStart)); !sid.Equals(user) {
		t.Fatalf("key ACL grants %s", sid)
	}
	if _, err := Load(path, t.TempDir()); err != nil {
		t.Fatalf("load owner-only key: %v", err)
	}
	shared, err := windows.SecurityDescriptorFromString("D:P(A;;FA;;;" + user.String() + ")(A;;FR;;;AU)")
	if err != nil {
		t.Fatal(err)
	}
	sharedACL, _, err := shared.DACL()
	if err != nil {
		t.Fatal(err)
	}
	if err := windows.SetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION, nil, nil, sharedACL, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(path, t.TempDir()); err == nil || !strings.Contains(err.Error(), "owner-private") {
		t.Fatalf("key readable by Authenticated Users loaded: %v", err)
	}
}
