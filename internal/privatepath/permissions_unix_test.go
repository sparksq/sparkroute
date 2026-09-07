//go:build !windows

package privatepath

import (
	"io/fs"
	"testing"
)

func TestOwnerOnlyModes(t *testing.T) {
	for _, mode := range []fs.FileMode{0o600, 0o700, 0o400, fs.ModeDir | 0o700} {
		if err := Check("unused", mode); err != nil {
			t.Fatalf("private mode %o: %v", mode, err)
		}
	}
	for _, mode := range []fs.FileMode{0o644, 0o755, 0o640, 0o620, 0o601} {
		if err := Check("unused", mode); err == nil {
			t.Fatalf("shared mode %o accepted", mode)
		}
	}
}
