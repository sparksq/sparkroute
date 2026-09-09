// SPDX-FileCopyrightText: 2026 Scitrera LLC
// SPDX-FileCopyrightText: 2026 Fox Engine Ltd.
// SPDX-License-Identifier: AGPL-3.0-only

// Package version carries the identity this binary reports to anything outside
// the process: `--version`, the User-Agent sent to upstream providers, and the
// telemetry distro attributes.
//
// It is its own package rather than a `var` in main because the gateway and
// telemetry packages need the same values, and `main` cannot be imported.
package version

// Name is how this gateway introduces itself — to upstream providers in a
// User-Agent, and to collectors as the telemetry distro.
const Name = "sparkroute"

// Version is the build version.
//
// Deliberately a `var`: the release build overrides it with
// `-ldflags "-X github.com/sparksq/sparkroute/pkg/version.Version=<tag>"`,
// and the linker cannot write to a constant. The literal below is the in-tree
// default that `go install` and a plain `go build` report, kept in step with
// versions.yaml by `sync-versions`.
var Version = "0.0.1"

// Commit is the public OSS source commit stamped by release builds.
var Commit = "development"

// BuildInfo identifies the source corresponding to a distributed binary.
func BuildInfo() map[string]string {
	source := "https://github.com/sparksq/sparkroute"
	if Commit != "development" && Commit != "" {
		source += "/tree/" + Commit
	}
	return map[string]string{
		"name": Name, "version": Version, "commit": Commit,
		"source":  source,
		"license": "AGPL-3.0-only",
	}
}

// Assembled once at package initialisation rather than per request. `-X`
// rewrites Version's initial value at link time, so this already sees the
// released number.
var userAgent = Name + "/" + Version

// UserAgent is the User-Agent header value for requests to upstream providers.
func UserAgent() string { return userAgent }
