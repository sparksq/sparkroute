// SPDX-FileCopyrightText: 2026 Scitrera LLC
// SPDX-FileCopyrightText: 2026 Fox Engine Ltd.
// SPDX-License-Identifier: AGPL-3.0-only

package version

import (
	"strings"
	"testing"
)

func TestUserAgentCombinesNameAndVersion(t *testing.T) {
	got := UserAgent()
	want := Name + "/" + Version
	if got != want {
		t.Fatalf("UserAgent() = %q, want %q", got, want)
	}
}

// The User-Agent goes to upstream providers, so a stray space or newline would
// produce a malformed header rather than a visible failure here.
func TestUserAgentIsAWellFormedHeaderValue(t *testing.T) {
	got := UserAgent()
	if strings.ContainsAny(got, " \t\r\n") {
		t.Fatalf("UserAgent() = %q contains whitespace", got)
	}
	name, ver, ok := strings.Cut(got, "/")
	if !ok || name == "" || ver == "" {
		t.Fatalf("UserAgent() = %q, want a non-empty <name>/<version>", got)
	}
}

// Version must stay a `var`: the release build overrides it with
// `-ldflags -X`, which the linker cannot apply to a constant. Assigning to it
// is what makes this a compile-time guard rather than a comment.
func TestVersionIsOverridable(t *testing.T) {
	original := Version
	t.Cleanup(func() { Version = original })

	Version = "9.9.9"
	if Version != "9.9.9" {
		t.Fatalf("Version = %q, want %q", Version, "9.9.9")
	}
}

func TestVersionIsPopulated(t *testing.T) {
	if strings.TrimSpace(Version) == "" {
		t.Fatal("Version is empty; sync-versions keeps it in step with versions.yaml")
	}
	if Name != "sparkroute" {
		t.Fatalf("Name = %q; upstream providers and telemetry key on this value", Name)
	}
}
