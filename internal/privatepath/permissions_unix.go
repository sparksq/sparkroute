// SPDX-FileCopyrightText: 2026 Scitrera LLC
// SPDX-FileCopyrightText: 2026 Fox Engine Ltd.
// SPDX-License-Identifier: AGPL-3.0-only

//go:build !windows

// Package privatepath checks that persisted gateway data is private to its user.
package privatepath

import (
	"fmt"
	"io/fs"
)

// Check preserves POSIX owner-only permission checks. On Windows it inspects
// the owner and DACL instead of the synthetic mode bits returned by os.Stat.
func Check(_ string, mode fs.FileMode) error {
	if mode.Perm()&0o077 != 0 {
		return fmt.Errorf("path must not grant group or other permissions")
	}
	return nil
}
