package privatepath

import (
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/sys/windows"
)

func TestWindowsAccessRules(t *testing.T) {
	user, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		t.Fatal(err)
	}
	sid := user.User.Sid.String()
	private := "(A;OICI;FA;;;" + sid + ")(A;OICI;FA;;;SY)(A;OICI;FA;;;BA)"
	for _, test := range []struct {
		name, sddl string
		allowed    bool
	}{
		{"private", "O:" + sid + "D:P" + private, true},
		{"administrator-owned", "O:BAD:P" + private, true},
		{"creator-owner", "O:" + sid + "D:P" + private + "(A;OICIIO;FA;;;CO)", true},
		{"owner-rights", "O:" + sid + "D:P(A;OICI;FA;;;OW)", true},
		{"untrusted-owner-rights", "O:WDD:P(A;OICI;FA;;;OW)", false},
		{"world-readable", "O:" + sid + "D:P" + private + "(A;;FR;;;WD)", false},
		{"users-writable", "O:" + sid + "D:P" + private + "(A;;FW;;;BU)", false},
		{"public-children", "O:" + sid + "D:P" + private + "(A;OICIIO;FR;;;WD)", false},
		{"untrusted-owner", "O:WDD:P" + private, false},
		{"missing-dacl", "O:" + sid, false},
		{"null-dacl", "O:" + sid + "D:NO_ACCESS_CONTROL", false},
	} {
		t.Run(test.name, func(t *testing.T) {
			descriptor, err := windows.SecurityDescriptorFromString(test.sddl)
			if err != nil {
				t.Fatal(err)
			}
			if err := checkDescriptor(descriptor, user.User.Sid); (err == nil) != test.allowed {
				t.Fatalf("allowed=%v, error=%v", test.allowed, err)
			}
		})
	}
}

func TestWindowsPrivateTemporaryPaths(t *testing.T) {
	directory := t.TempDir()
	path := filepath.Join(directory, "private.db")
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{directory, path} {
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if err := Check(path, info.Mode()); err != nil {
			t.Fatalf("private path %s: %v", path, err)
		}
	}
	if err := Check(filepath.Join(directory, "missing"), 0o600); err == nil {
		t.Fatal("missing path accepted")
	}
}
