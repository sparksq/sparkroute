package gateway

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/sparksq/sparkroute/pkg/config"
	"github.com/sparksq/sparkroute/pkg/ledger"
)

func TestEmbeddingsPassthroughAndUsage(t *testing.T) {
	t.Parallel()

	captured := make(chan capturedUpstreamRequest, 1)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		defer func() { _ = request.Body.Close() }()
		raw, err := io.ReadAll(request.Body)
		if err != nil {
			t.Errorf("ReadAll() error = %v", err)
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		var body map[string]json.RawMessage
		if err := json.Unmarshal(raw, &body); err != nil {
			t.Errorf("decode upstream body: %v", err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		captured <- capturedUpstreamRequest{
			Path:   request.URL.Path,
			Header: request.Header.Clone(),
			Body:   body,
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"object": "list",
			"data": []any{map[string]any{
				"object":    "embedding",
				"embedding": []float64{0.25, -0.5},
				"index":     0,
			}},
			"model":        "upstream-model",
			"future_field": map[string]any{"preserved": true},
			"usage": map[string]any{
				"prompt_tokens": 7,
				"total_tokens":  7,
			},
		})
	}))
	defer upstream.Close()

	recorder := &collectingRecorder{}
	handler, err := NewDataHandler(
		embeddingsDocument(upstream.URL+"/v1"),
		DataOptions{Ledger: recorder},
	)
	if err != nil {
		t.Fatalf("NewDataHandler() error = %v", err)
	}
	request := httptest.NewRequest(
		http.MethodPost,
		"/v1/embeddings",
		strings.NewReader("{\"model\":\"default\",\"input\":[\"first\",\"second\"],\"encoding_format\":\"float\",\"dimensions\":2,\"user\":\"caller-1\",\"future_request_field\":{\"preserve\":true}}"),
	)
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)

	if response.Code != http.StatusOK {
		t.Fatalf("status = %d; body=%s", response.Code, response.Body)
	}
	gotRequest := <-captured
	if gotRequest.Path != "/v1/embeddings" {
		t.Fatalf("upstream path = %q", gotRequest.Path)
	}
	if gotRequest.Header.Get("Accept") != "application/json" {
		t.Fatalf("upstream Accept = %q", gotRequest.Header.Get("Accept"))
	}
	if string(gotRequest.Body["model"]) != "\"upstream-model\"" ||
		string(gotRequest.Body["input"]) != "[\"first\",\"second\"]" ||
		string(gotRequest.Body["encoding_format"]) != "\"float\"" ||
		string(gotRequest.Body["dimensions"]) != "2" ||
		string(gotRequest.Body["future_request_field"]) != "{\"preserve\":true}" {
		t.Fatalf("upstream body = %#v", gotRequest.Body)
	}

	var body map[string]json.RawMessage
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if string(body["model"]) != "\"public\"" ||
		string(body["future_field"]) != "{\"preserved\":true}" {
		t.Fatalf("response body = %s", response.Body)
	}

	records := recorder.snapshot()
	if len(records) != 2 || records[0].Attempt == nil || records[1].Request == nil {
		t.Fatalf("records = %#v", records)
	}
	if records[1].Request.Operation != "embeddings" ||
		records[1].Request.Outcome != ledger.OutcomeSuccess {
		t.Fatalf("request record = %#v", records[1].Request)
	}
	assertTokenValue(t, "embedding input", records[0].Attempt.Usage.InputTokens, 7)
	assertTokenValue(t, "embedding output", records[1].Request.Usage.OutputTokens, 0)
	assertTokenValue(t, "embedding total", records[1].Request.Usage.TotalTokens, 7)
	if records[1].Request.Usage.Completeness != ledger.UsageComplete ||
		records[1].Request.Usage.NormalizationVersion != openAIEmbeddingsUsageVersion {
		t.Fatalf("usage = %#v", records[1].Request.Usage)
	}
}

func TestEmbeddingsRoutesOnlyToCapableTarget(t *testing.T) {
	t.Parallel()

	var plainCalls atomic.Int64
	plain := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		plainCalls.Add(1)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer plain.Close()

	var embeddingCalls atomic.Int64
	embedding := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		embeddingCalls.Add(1)
		if request.URL.Path != "/v1/embeddings" {
			t.Errorf("upstream path = %q", request.URL.Path)
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"object": "list",
			"data":   []any{},
			"model":  "upstream-second",
			"usage": map[string]any{
				"prompt_tokens": 1,
				"total_tokens":  1,
			},
		})
	}))
	defer embedding.Close()

	document := twoTargetDocument(plain.URL+"/v1", embedding.URL+"/v1")
	document.Deployments[1].Capabilities = append(
		document.Deployments[1].Capabilities,
		config.CapabilitySingleVectorEmbedding,
	)
	handler, err := NewDataHandler(document, DataOptions{RoutingPicker: zeroPicker{}})
	if err != nil {
		t.Fatalf("NewDataHandler() error = %v", err)
	}
	request := httptest.NewRequest(
		http.MethodPost,
		"/v1/embeddings",
		strings.NewReader("{\"model\":\"public\",\"input\":\"hello\"}"),
	)
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)

	if response.Code != http.StatusOK {
		t.Fatalf("status = %d; body=%s", response.Code, response.Body)
	}
	if plainCalls.Load() != 0 || embeddingCalls.Load() != 1 {
		t.Fatalf(
			"upstream calls = (%d, %d), want (0, 1)",
			plainCalls.Load(),
			embeddingCalls.Load(),
		)
	}
}

func TestEmbeddingsRequiresCapableTargetBeforeUpstream(t *testing.T) {
	t.Parallel()

	var calls atomic.Int64
	upstream := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		calls.Add(1)
	}))
	defer upstream.Close()

	handler, err := NewDataHandler(proxyDocument(upstream.URL+"/v1"), DataOptions{})
	if err != nil {
		t.Fatalf("NewDataHandler() error = %v", err)
	}
	request := httptest.NewRequest(
		http.MethodPost,
		"/v1/embeddings",
		strings.NewReader("{\"model\":\"public\",\"input\":\"hello\"}"),
	)
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)

	if response.Code != http.StatusBadRequest {
		t.Fatalf("status = %d; body=%s", response.Code, response.Body)
	}
	var body openAIErrorEnvelope
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil ||
		body.Error.Code != "unsupported_feature" {
		t.Fatalf("error = %#v, %v", body, err)
	}
	if calls.Load() != 0 {
		t.Fatalf("upstream calls = %d, want 0", calls.Load())
	}
}

func TestEmbeddingsRejectsInvalidOrStreamingRequestBeforeUpstream(t *testing.T) {
	t.Parallel()

	var calls atomic.Int64
	upstream := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		calls.Add(1)
	}))
	defer upstream.Close()

	handler, err := NewDataHandler(embeddingsDocument(upstream.URL+"/v1"), DataOptions{})
	if err != nil {
		t.Fatalf("NewDataHandler() error = %v", err)
	}
	tests := []struct {
		name string
		body string
		code string
	}{
		{
			name: "missing input",
			body: "{\"model\":\"public\"}",
			code: "invalid_request_body",
		},
		{
			name: "null input",
			body: "{\"model\":\"public\",\"input\":null}",
			code: "invalid_request_body",
		},
		{
			name: "streaming",
			body: "{\"model\":\"public\",\"input\":\"hello\",\"stream\":true}",
			code: "unsupported_feature",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			request := httptest.NewRequest(
				http.MethodPost,
				"/v1/embeddings",
				strings.NewReader(test.body),
			)
			request.Header.Set("Content-Type", "application/json")
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, request)

			if response.Code != http.StatusBadRequest {
				t.Fatalf("status = %d; body=%s", response.Code, response.Body)
			}
			var body openAIErrorEnvelope
			if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil ||
				body.Error.Code != test.code {
				t.Fatalf("error = %#v, %v; want code %q", body, err, test.code)
			}
		})
	}
	if calls.Load() != 0 {
		t.Fatalf("upstream calls = %d, want 0", calls.Load())
	}
}

func embeddingsDocument(baseURL string) config.Document {
	document := proxyDocument(baseURL)
	document.Deployments[0].Capabilities = append(
		document.Deployments[0].Capabilities,
		config.CapabilitySingleVectorEmbedding,
	)
	return document
}
