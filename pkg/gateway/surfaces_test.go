package gateway

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestMountSurfacesRoutesOnlyOwnedPaths(t *testing.T) {
	t.Parallel()

	handler := MountSurfaces(
		namedSurface("data"),
		Surface{Handler: namedSurface("admin"), MatchPath: func(path string) bool {
			return path == "/admin"
		}},
		Surface{Handler: namedSurface("operations"), MatchPath: IsOperationsPath},
	)
	for path, expected := range map[string]string{
		"/":                   "data",
		"/v1/models":          "data",
		"/admin":              "admin",
		"/health/live":        "operations",
		"/health/credentials": "operations",
		"/metrics":            "operations",
	} {
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, path, nil))
		if actual := response.Header().Get("X-Surface"); actual != expected {
			t.Errorf("surface for %q = %q, want %q", path, actual, expected)
		}
	}
}

func TestAssembleListenersCoLocatesEmptyAddresses(t *testing.T) {
	t.Parallel()

	listeners := AssembleListeners(
		ListenerSpec{Name: "data", Address: "127.0.0.1:8080", Handler: namedSurface("data")},
		ListenerSurface{
			Listener:  ListenerSpec{Name: "admin", Handler: namedSurface("admin")},
			MatchPath: func(path string) bool { return path == "/admin" },
		},
		ListenerSurface{
			Listener:  ListenerSpec{Name: "operations", Handler: namedSurface("operations")},
			MatchPath: IsOperationsPath,
		},
	)
	if len(listeners) != 1 {
		t.Fatalf("listener count = %d, want 1", len(listeners))
	}
	assertSurface(t, listeners[0].Handler, "/v1/models", "data")
	assertSurface(t, listeners[0].Handler, "/admin", "admin")
	assertSurface(t, listeners[0].Handler, "/health/ready", "operations")
}

func TestAssembleListenersKeepsConfiguredAddressesIsolated(t *testing.T) {
	t.Parallel()

	listeners := AssembleListeners(
		ListenerSpec{Name: "data", Address: "127.0.0.1:8080", Handler: namedSurface("data")},
		ListenerSurface{
			Listener: ListenerSpec{
				Name: "admin", Address: "127.0.0.1:8081", Handler: namedSurface("admin"),
			},
			MatchPath: func(path string) bool { return path == "/admin" },
		},
		ListenerSurface{
			Listener: ListenerSpec{
				Name: "operations", Address: "127.0.0.1:9090", Handler: namedSurface("operations"),
			},
			MatchPath: IsOperationsPath,
		},
	)
	if len(listeners) != 3 {
		t.Fatalf("listener count = %d, want 3", len(listeners))
	}
	assertSurface(t, listeners[0].Handler, "/admin", "data")
	assertSurface(t, listeners[0].Handler, "/health/ready", "data")
	assertSurface(t, listeners[1].Handler, "/admin", "admin")
	assertSurface(t, listeners[2].Handler, "/health/ready", "operations")
}

func TestIsOperationsPath(t *testing.T) {
	t.Parallel()

	for path, expected := range map[string]bool{
		"/":               false,
		"/health":         true,
		"/health/live":    true,
		"/healthcheck":    false,
		"/metrics":        true,
		"/metrics/detail": false,
		"/debug/pprof":    true,
		"/debug/pprof/":   true,
		"/debug/other":    false,
	} {
		if actual := IsOperationsPath(path); actual != expected {
			t.Errorf("IsOperationsPath(%q) = %t, want %t", path, actual, expected)
		}
	}
}

func TestIsLoopbackAddress(t *testing.T) {
	t.Parallel()

	for address, expected := range map[string]bool{
		"127.0.0.1:8080": true,
		"[::1]:8080":     true,
		"localhost:8080": true,
		"0.0.0.0:8080":   false,
		":8080":          false,
		"[::]:8080":      false,
		"invalid":        false,
	} {
		if actual := IsLoopbackAddress(address); actual != expected {
			t.Errorf("IsLoopbackAddress(%q) = %t, want %t", address, actual, expected)
		}
	}
}

func namedSurface(name string) http.Handler {
	return http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.Header().Set("X-Surface", name)
		writer.WriteHeader(http.StatusNoContent)
	})
}

func assertSurface(t *testing.T, handler http.Handler, path, expected string) {
	t.Helper()
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, path, nil))
	if actual := response.Header().Get("X-Surface"); actual != expected {
		t.Fatalf("surface for %q = %q, want %q", path, actual, expected)
	}
}
