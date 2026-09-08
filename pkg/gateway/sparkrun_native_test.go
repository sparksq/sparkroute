package gateway

import (
	"context"
	"encoding/json"
	"github.com/sparksq/sparkroute/pkg/config"
	"github.com/sparksq/sparkroute/pkg/endpointregistry"
	"github.com/sparksq/sparkroute/pkg/lifecycle"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestSparkrunDeploymentNativeAPIs(t *testing.T) {
	for _, test := range []struct{ path, body, response string }{
		{"/v1/chat/completions", `{"model":"virtual","messages":[{"role":"user","content":"hello"}]}`, `{"id":"chat-1","object":"chat.completion","model":"upstream","choices":[]}`},
		{"/v1/responses", `{"model":"virtual","input":"hello","store":false}`, `{"id":"resp_1","object":"response","status":"completed","model":"upstream","output":[]}`},
		{"/v1/messages", `{"model":"virtual","messages":[{"role":"user","content":"hello"}],"max_tokens":16}`, `{"id":"msg_1","type":"message","role":"assistant","model":"upstream","content":[{"type":"text","text":"ok"}],"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`},
	} {
		t.Run(test.path, func(t *testing.T) {
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != test.path {
					t.Errorf("translated native request to %s", r.URL.Path)
				}
				var body map[string]json.RawMessage
				_ = json.NewDecoder(r.Body).Decode(&body)
				if string(body["model"]) != `"upstream"` {
					t.Error("upstream model was not resolved")
				}
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(test.response))
			}))
			defer upstream.Close()
			doc := dynamicGatewayDocument(config.CapabilityResponses)
			doc.Providers[0].Type = "sparkrun"
			doc.Deployments[0].NativeProtocols = []config.Protocol{config.ProtocolOpenAI, config.ProtocolAnthropic}
			registry := endpointregistry.NewMemory()
			controller := &gatewayRuntimeController{registry: registry, endpoint: endpointregistry.Endpoint{ID: "endpoint", Target: "deployment", BaseURL: upstream.URL + "/v1", Controller: "controller", Protocol: "openai", ServedModels: []string{"upstream"}, State: endpointregistry.StateReady}}
			targets, err := lifecycle.TargetsFromDocument(doc)
			if err != nil {
				t.Fatal(err)
			}
			coordinator, err := lifecycle.NewAdmissionCoordinator(targets, lifecycle.AdmissionOptions{Registry: registry, Controllers: map[string]lifecycle.Controller{"controller": controller}, Authorizer: lifecycle.EndpointAuthorizerFunc(func(context.Context, endpointregistry.Endpoint) error { return nil }), InstanceID: "test", PollInterval: 10 * time.Millisecond})
			if err != nil {
				t.Fatal(err)
			}
			handler, err := NewDataHandler(doc, DataOptions{Lifecycle: coordinator})
			if err != nil {
				t.Fatal(err)
			}
			request := httptest.NewRequest(http.MethodPost, test.path, strings.NewReader(test.body))
			request.Header.Set("Content-Type", "application/json")
			request.Header.Set("Anthropic-Version", "2023-06-01")
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, request)
			if response.Code != 200 {
				t.Fatalf("%d: %s", response.Code, response.Body.String())
			}
		})
	}
}
