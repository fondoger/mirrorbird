//go:build windows

package platform

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

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
	path := t.TempDir()
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
	root := filepath.Join(t.TempDir(), "private")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
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
