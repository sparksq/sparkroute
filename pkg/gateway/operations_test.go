package gateway

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestOperationsHandlerPprofIsExplicit(t *testing.T) {
	t.Parallel()
	health := http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path == "/health/live" {
			writer.WriteHeader(http.StatusNoContent)
			return
		}
		http.NotFound(writer, request)
	})
	for _, test := range []struct {
		name    string
		enabled bool
		path    string
		status  int
	}{
		{name: "health without pprof", path: "/health/live", status: http.StatusNoContent},
		{name: "pprof disabled", path: "/debug/pprof/", status: http.StatusNotFound},
		{name: "health with pprof", enabled: true, path: "/health/live", status: http.StatusNoContent},
		{name: "pprof enabled", enabled: true, path: "/debug/pprof/", status: http.StatusOK},
	} {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			response := httptest.NewRecorder()
			NewOperationsHandler(health, test.enabled).ServeHTTP(
				response, httptest.NewRequest(http.MethodGet, test.path, nil),
			)
			if response.Code != test.status {
				t.Fatalf("status = %d, want %d", response.Code, test.status)
			}
		})
	}
}
