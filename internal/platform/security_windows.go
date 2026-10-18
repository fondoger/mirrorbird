//go:build windows

package platform

import (
	"errors"
	"fmt"

	"golang.org/x/sys/windows"
)

// CurrentUserID returns the SID of the process token's user, not a username or
// an environment variable. Thread impersonation does not change this identity.
func CurrentUserID() (string, error) {
	user, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		return "", fmt.Errorf("query process user: %w", err)
	}
	if user.User.Sid == nil || !user.User.Sid.IsValid() {
		return "", errors.New("process token contains an invalid user SID")
	}
	sid := user.User.Sid.String()
	if sid == "" {
		return "", errors.New("convert process user SID")
	}
	return sid, nil
}

func secureDirectory(path string) error {
	sid, err := CurrentUserID()
	if err != nil {
		return err
	}
	sd, err := windows.SecurityDescriptorFromString("O:" + sid + "D:P(A;OICI;FA;;;" + sid + ")")
	if err != nil {
		return fmt.Errorf("construct private directory security: %w", err)
	}
	owner, _, err := sd.Owner()
	if err != nil {
		return fmt.Errorf("read private directory owner: %w", err)
	}
	dacl, _, err := sd.DACL()
	if err != nil {
		return fmt.Errorf("read private directory DACL: %w", err)
	}
	if owner == nil || dacl == nil {
		return errors.New("private directory security has no owner or DACL")
	}
	path16, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return fmt.Errorf("encode private directory path: %w", err)
	}
	// Open the directory itself, without following a final reparse point.
	// Exclude FILE_SHARE_DELETE so it cannot be replaced while we secure it.
	handle, err := windows.CreateFile(path16,
		windows.READ_CONTROL|windows.WRITE_DAC|windows.FILE_READ_ATTRIBUTES,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE, nil, windows.OPEN_EXISTING,
		windows.FILE_FLAG_BACKUP_SEMANTICS|windows.FILE_FLAG_OPEN_REPARSE_POINT, 0)
	if err != nil {
		return fmt.Errorf("open private directory: %w", err)
	}
	defer windows.CloseHandle(handle)

	var info windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(handle, &info); err != nil {
		return fmt.Errorf("inspect private directory: %w", err)
	}
	if info.FileAttributes&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0 {
		return errors.New("private directory must not be a reparse point")
	}
	if info.FileAttributes&windows.FILE_ATTRIBUTE_DIRECTORY == 0 {
		return errors.New("private directory path is not a directory")
	}
	// Being the owner grants READ_CONTROL and WRITE_DAC, but not WRITE_OWNER.
	// Requiring WRITE_OWNER would reject directories the user just created
	// beneath a parent that grants Modify (rather than Full Control). Verify
	// ownership instead of asking for the unnecessary right to change it.
	existing, err := windows.GetSecurityInfo(handle, windows.SE_FILE_OBJECT, windows.OWNER_SECURITY_INFORMATION)
	if err != nil {
		return fmt.Errorf("query private directory owner: %w", err)
	}
	existingOwner, _, err := existing.Owner()
	if err != nil {
		return fmt.Errorf("read existing private directory owner: %w", err)
	}
	if existingOwner == nil || !existingOwner.Equals(owner) {
		return errors.New("private directory must be owned by the current process user")
	}
	if err := windows.SetSecurityInfo(handle, windows.SE_FILE_OBJECT,
		windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION,
		nil, nil, dacl, nil); err != nil {
		return fmt.Errorf("restrict private directory access: %w", err)
	}
	return nil
}
