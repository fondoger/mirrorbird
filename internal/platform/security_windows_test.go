//go:build windows

package platform

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"mirrorbird/internal/testutil"

	"golang.org/x/sys/windows"
)

func assertOnlyCurrentUserAccess(t *testing.T, sd *windows.SECURITY_DESCRIPTOR, inheritance string, protected bool) {
	t.Helper()
	sid, err := CurrentUserID()
	if err != nil {
		t.Fatal(err)
	}
	wantSID, err := windows.StringToSid(sid)
	if err != nil {
		t.Fatal(err)
	}
	owner, _, err := sd.Owner()
	if err != nil || owner == nil || !owner.Equals(wantSID) {
		t.Fatalf("wrong object owner: %v", err)
	}
	control, _, err := sd.Control()
	if err != nil {
		t.Fatal(err)
	}
	if control&windows.SE_DACL_PRESENT == 0 {
		t.Fatal("DACL is absent")
	}
	if protected && control&windows.SE_DACL_PROTECTED == 0 {
		t.Fatal("DACL is not protected from inheritance")
	}
	dacl, _, err := sd.DACL()
	if err != nil || dacl == nil {
		t.Fatalf("missing or null DACL: %v", err)
	}
	// Normalize expected SDDL with the same Windows converter as the actual
	// descriptor (which maps generic rights and well-known SID aliases).
	want, err := windows.SecurityDescriptorFromString("D:(A;" + inheritance + ";FA;;;" + sid + ")")
	if err != nil {
		t.Fatal(err)
	}
	actual := sd.String()
	wantText := want.String()
	if strings.Count(actual, "(") != 1 || !strings.Contains(actual, wantText[strings.IndexByte(wantText, '('):]) {
		t.Fatalf("DACL contains unexpected permissions: %s", actual)
	}
}

func TestSecureDirectoryDoesNotRequireWriteOwner(t *testing.T) {
	path := testutil.TempDir(t)
	sid, err := CurrentUserID()
	if err != nil {
		t.Fatal(err)
	}
	// Mirror a directory created below a parent granting only Modify, not
	// Full Control. Its owner can rewrite its DACL but has no WRITE_OWNER.
	sd, err := windows.SecurityDescriptorFromString("D:P(A;;0x1301bf;;;" + sid + ")")
	if err != nil {
		t.Fatal(err)
	}
	dacl, _, err := sd.DACL()
	if err != nil {
		t.Fatal(err)
	}
	if err := windows.SetNamedSecurityInfo(path, windows.SE_FILE_OBJECT,
		windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION,
		nil, nil, dacl, nil); err != nil {
		t.Fatal(err)
	}
	path16, err := windows.UTF16PtrFromString(path)
	if err != nil {
		t.Fatal(err)
	}
	handle, err := windows.CreateFile(path16, windows.WRITE_OWNER,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE, nil, windows.OPEN_EXISTING,
		windows.FILE_FLAG_BACKUP_SEMANTICS|windows.FILE_FLAG_OPEN_REPARSE_POINT, 0)
	if err == nil {
		windows.CloseHandle(handle)
		t.Fatal("Modify-only fixture unexpectedly grants WRITE_OWNER")
	}
	if !errors.Is(err, windows.ERROR_ACCESS_DENIED) {
		t.Fatalf("checking fixture WRITE_OWNER access: %v", err)
	}
	if err := SecureDirectory(path); err != nil {
		t.Fatalf("securing an owned Modify-only directory: %v", err)
	}
	actual, err := windows.GetNamedSecurityInfo(path, windows.SE_FILE_OBJECT,
		windows.OWNER_SECURITY_INFORMATION|windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		t.Fatal(err)
	}
	assertOnlyCurrentUserAccess(t, actual, "OICI", true)
}

func TestSecureDirectoryPrivateAndInherited(t *testing.T) {
	root := filepath.Join(testutil.TempDir(t), "private")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	testutil.SetCurrentUserOwner(t, root)
	for range 2 {
		if err := SecureDirectory(root); err != nil {
			t.Fatal(err)
		}
	}
	sd, err := windows.GetNamedSecurityInfo(root, windows.SE_FILE_OBJECT,
		windows.OWNER_SECURITY_INFORMATION|windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		t.Fatal(err)
	}
	assertOnlyCurrentUserAccess(t, sd, "OICI", true)

	child := filepath.Join(root, "attachments")
	if err := os.Mkdir(child, 0o700); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(child, "capability.json")
	if err := os.WriteFile(file, []byte("synthetic-test-metadata"), 0o600); err != nil {
		t.Fatal(err)
	}
	// Owner is not inherited from root on elevated Windows. Fix only owner;
	// leave the inherited DACLs untouched for the assertions below.
	testutil.SetCurrentUserOwner(t, child)
	testutil.SetCurrentUserOwner(t, file)
	for _, test := range []struct {
		path        string
		inheritance string
	}{
		{child, "OICIID"},
		{file, "ID"},
	} {
		sd, err := windows.GetNamedSecurityInfo(test.path, windows.SE_FILE_OBJECT,
			windows.OWNER_SECURITY_INFORMATION|windows.DACL_SECURITY_INFORMATION)
		if err != nil {
			t.Fatal(err)
		}
		assertOnlyCurrentUserAccess(t, sd, test.inheritance, false)
	}
}
