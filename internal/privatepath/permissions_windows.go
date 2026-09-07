package privatepath

import (
	"fmt"
	"io/fs"
	"unsafe"

	"golang.org/x/sys/windows"
)

// Check accepts Windows files and directories accessible only to the current
// process user, SYSTEM, and local Administrators. It never rewrites existing
// ACLs. Inheritable grants are checked too, protecting newly created SQLite
// journals and other child files. Unknown ACE forms are rejected conservatively.
func Check(path string, _ fs.FileMode) error {
	user, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		return fmt.Errorf("read current Windows user: %w", err)
	}
	descriptor, err := windows.GetNamedSecurityInfo(path, windows.SE_FILE_OBJECT,
		windows.OWNER_SECURITY_INFORMATION|windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		return fmt.Errorf("read Windows path permissions: %w", err)
	}
	return checkDescriptor(descriptor, user.User.Sid)
}

func trustedSID(sid, user *windows.SID) bool {
	return sid != nil && (sid.Equals(user) || sid.IsWellKnown(windows.WinLocalSystemSid) ||
		sid.IsWellKnown(windows.WinBuiltinAdministratorsSid))
}

func checkDescriptor(descriptor *windows.SECURITY_DESCRIPTOR, user *windows.SID) error {
	owner, _, err := descriptor.Owner()
	if err != nil || !trustedSID(owner, user) {
		return fmt.Errorf("path must have a trusted owner")
	}
	dacl, _, err := descriptor.DACL()
	if err != nil || dacl == nil {
		return fmt.Errorf("path must have a restrictive DACL")
	}
	for index := uint32(0); index < uint32(dacl.AceCount); index++ {
		var ace *windows.ACCESS_ALLOWED_ACE
		if err := windows.GetAce(dacl, index, &ace); err != nil {
			return fmt.Errorf("read Windows access rule: %w", err)
		}
		switch ace.Header.AceType {
		case windows.ACCESS_DENIED_ACE_TYPE:
			continue // A denial cannot grant access.
		case windows.ACCESS_ALLOWED_ACE_TYPE:
			sid := (*windows.SID)(unsafe.Pointer(&ace.SidStart))
			if trustedSID(sid, user) || ace.Mask == 0 {
				continue
			}
			// OWNER RIGHTS denotes the current owner, which was validated above.
			// Python's private temporary directories use this form on Windows.
			if sid.IsWellKnown(windows.WinCreatorOwnerRightsSid) {
				continue
			}
			// Windows substitutes the creator's SID when this rule is inherited.
			if ace.Header.AceFlags&windows.INHERIT_ONLY_ACE != 0 && sid.IsWellKnown(windows.WinCreatorOwnerSid) {
				continue
			}
			return fmt.Errorf("path must not grant access to other users (%s)", sid.String())
		default:
			return fmt.Errorf("path has an unsupported access rule type %d", ace.Header.AceType)
		}
	}
	return nil
}
