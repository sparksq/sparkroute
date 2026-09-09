// SPDX-FileCopyrightText: 2026 Scitrera LLC
// SPDX-FileCopyrightText: 2026 Fox Engine Ltd.
// SPDX-License-Identifier: AGPL-3.0-only

package gateway

import (
	"net"
	"net/http"
	"strings"
)

// Surface is an HTTP namespace that may be mounted on another listener.
type Surface struct {
	Handler   http.Handler
	MatchPath func(string) bool
}

// ListenerSurface describes an optional independently addressable surface. An
// empty listener address mounts the surface on the data listener; a non-empty
// address creates a separate listener and leaves the data listener unchanged.
type ListenerSurface struct {
	Listener  ListenerSpec
	MatchPath func(string) bool
}

// AssembleListeners applies the optional-listener convention shared by all
// gateway profiles.
func AssembleListeners(data ListenerSpec, surfaces ...ListenerSurface) []ListenerSpec {
	coLocated := make([]Surface, 0, len(surfaces))
	listeners := make([]ListenerSpec, 1, 1+len(surfaces))
	listeners[0] = data
	for _, surface := range surfaces {
		if strings.TrimSpace(surface.Listener.Address) == "" {
			coLocated = append(coLocated, Surface{
				Handler: surface.Listener.Handler, MatchPath: surface.MatchPath,
			})
			continue
		}
		listeners = append(listeners, surface.Listener)
	}
	if len(coLocated) != 0 {
		listeners[0].Handler = MountSurfaces(data.Handler, coLocated...)
	}
	return listeners
}

// MountSurfaces dispatches matching namespaces to their surface handlers and
// sends every other request to fallback. Surface handlers keep ownership of
// their own authentication and authorization middleware.
func MountSurfaces(fallback http.Handler, surfaces ...Surface) http.Handler {
	return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		for _, surface := range surfaces {
			if surface.Handler != nil && surface.MatchPath != nil &&
				surface.MatchPath(request.URL.Path) {
				surface.Handler.ServeHTTP(writer, request)
				return
			}
		}
		fallback.ServeHTTP(writer, request)
	})
}

// IsOperationsPath reports whether path belongs to the operations surface.
// /metrics is reserved so a Prometheus-compatible handler can be added to that
// surface without changing listener composition. Go profiles are routed here
// even when disabled so they cannot fall through to the data plane.
func IsOperationsPath(path string) bool {
	return path == "/health" || strings.HasPrefix(path, "/health/") || path == "/metrics" ||
		path == "/debug/pprof" || strings.HasPrefix(path, "/debug/pprof/")
}

// IsLoopbackAddress reports whether a TCP listener address is confined to the
// local host. Wildcard and malformed addresses return false.
func IsLoopbackAddress(address string) bool {
	host, _, err := net.SplitHostPort(strings.TrimSpace(address))
	if err != nil {
		return false
	}
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}
