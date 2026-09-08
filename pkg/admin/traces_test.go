package admin

import (
	"context"
	"github.com/sparksq/sparkroute/pkg/config/managed"
	"github.com/sparksq/sparkroute/pkg/savedtrace"
	"net/http/httptest"
	"testing"
	"time"
)

type traceReaderFixture struct{ calls int }

func (f *traceReaderFixture) List(_ context.Context, q savedtrace.Query) (savedtrace.Page, error) {
	f.calls++
	if q.Cursor != "" {
		return savedtrace.Page{}, nil
	}
	now := time.Now().UTC()
	return savedtrace.Page{Records: []savedtrace.Record{{Version: 1, RequestID: "fixture", StartedAt: now, CompletedAt: now, Protocol: "openai", Operation: "chat_completions", Outcome: "success", CaptureOutcome: savedtrace.CaptureComplete, Request: savedtrace.Payload{Body: "payload"}}}}, nil
}
func TestTraceExportRequiresPayloadPrivilege(t *testing.T) {
	store := &traceReaderFixture{}
	handler := NewHandler(managed.EmptyDocument(), "test", Options{TraceReader: store, Authenticator: providerTestAuthenticator{}})
	for token, expected := range map[string]int{"": 401, "Bearer reader": 403, "Bearer writer": 403} {
		request := httptest.NewRequest("GET", "/v1/saved-traces/export", nil)
		request.Header.Set("Authorization", token)
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		if response.Code != expected {
			t.Fatalf("%s: %d", token, response.Code)
		}
	}
	if store.calls != 0 {
		t.Fatal("unauthorized request read payloads")
	}
	handler = NewHandler(managed.EmptyDocument(), "test", Options{TraceReader: store, AllowInsecureAdmin: true})
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest("GET", "/v1/saved-traces/export?max_records=1", nil))
	if response.Code != 200 || response.Header().Get("Cache-Control") != "no-store" {
		t.Fatal(response.Code, response.Body.String())
	}
	before := store.calls
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest("GET", "/v1/saved-traces/export?misspelled_filter=x", nil))
	if response.Code != 400 || store.calls != before {
		t.Fatal("unknown filter broadened payload export")
	}
}
