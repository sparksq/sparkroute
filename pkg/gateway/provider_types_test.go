package gateway

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/sparksq/sparkroute/pkg/config"
)

func TestNativeResponsesProviderUsesResponsesWithoutAnExplicitCapability(t *testing.T) {
	var calls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.URL.Path != "/v1/responses" {
			t.Errorf("unexpected upstream path %s", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"resp_native","object":"response","status":"completed","output":[],"model":"upstream"}`))
	}))
	defer upstream.Close()
	document := responsesDocument(upstream.URL + "/v1")
	document.Providers[0].Type = "openai_responses"
	document.Deployments[0].Capabilities = nil
	document.VirtualModels[0].RequiredCapabilities = []config.Capability{config.CapabilityResponses}
	handler, err := NewDataHandler(document, DataOptions{})
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"`+document.VirtualModels[0].Name+`","input":"hello","store":false}`))
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("Responses status=%d body=%s", response.Code, response.Body.String())
	}
	if calls.Load() != 1 {
		t.Fatalf("upstream calls=%d", calls.Load())
	}
	request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"`+document.VirtualModels[0].Name+`","messages":[{"role":"user","content":"hello"}]}`))
	request.Header.Set("Content-Type", "application/json")
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code < 400 || calls.Load() != 1 {
		t.Fatalf("misrouted Chat to Responses provider: status=%d calls=%d", response.Code, calls.Load())
	}
}
