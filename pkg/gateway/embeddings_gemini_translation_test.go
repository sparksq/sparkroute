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

func TestEmbeddingsTranslatesToGeminiEmbedContent(t *testing.T) {
	t.Parallel()

	captured := make(chan capturedGeminiRequest, 1)
	upstream := httptest.NewServer(http.HandlerFunc(func(
		w http.ResponseWriter,
		request *http.Request,
	) {
		captured <- capturedGeminiRequest{
			Path:     request.URL.Path,
			RawQuery: request.URL.RawQuery,
			Header:   request.Header.Clone(),
			Body:     readJSONEnvelope(t, request.Body),
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("ETag", `"provider-representation"`)
		_, _ = io.WriteString(w, `{
			"embedding":{"values":[0.25,-0.5]},
			"usageMetadata":{
				"promptTokenCount":7,
				"promptTokenDetails":[{
					"modality":"TEXT",
					"tokenCount":7
				}]
			}
		}`)
	}))
	defer upstream.Close()

	document := geminiDocument(
		upstream.URL+"/v1beta",
		config.CapabilitySingleVectorEmbedding,
	)
	recorder := &collectingRecorder{}
	handler, err := NewDataHandler(
		document,
		DataOptions{Ledger: recorder},
	)
	if err != nil {
		t.Fatalf("NewDataHandler() error = %v", err)
	}
	response := serveEmbeddingsRequest(
		handler,
		`{
			"model":"default",
			"input":["hello"],
			"encoding_format":"float",
			"dimensions":2
		}`,
	)
	if response.Code != http.StatusOK {
		t.Fatalf(
			"response=%d body=%s",
			response.Code,
			response.Body,
		)
	}
	request := <-captured
	if request.Path !=
		"/v1beta/models/gemini-upstream:embedContent" ||
		request.RawQuery != "" ||
		request.Header.Get("Accept") != "application/json" {
		t.Fatalf("upstream request = %#v", request)
	}
	if string(request.Body["content"]) !=
		`{"parts":[{"text":"hello"}]}` ||
		string(request.Body["embedContentConfig"]) !=
			`{"autoTruncate":false,"outputDimensionality":2}` ||
		rawNonNull(request.Body["model"]) ||
		rawNonNull(request.Body["input"]) {
		t.Fatalf("upstream body = %#v", request.Body)
	}
	if response.Header().Get("Content-Type") !=
		"application/json" ||
		response.Header().Get("ETag") != "" {
		t.Fatalf("downstream headers = %#v", response.Header())
	}
	var body struct {
		Object string `json:"object"`
		Data   []struct {
			Object    string    `json:"object"`
			Embedding []float64 `json:"embedding"`
			Index     int       `json:"index"`
		} `json:"data"`
		Model string `json:"model"`
		Usage struct {
			PromptTokens int64 `json:"prompt_tokens"`
			TotalTokens  int64 `json:"total_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil ||
		body.Object != "list" ||
		body.Model != "public" ||
		len(body.Data) != 1 ||
		body.Data[0].Object != "embedding" ||
		body.Data[0].Index != 0 ||
		len(body.Data[0].Embedding) != 2 ||
		body.Data[0].Embedding[0] != 0.25 ||
		body.Data[0].Embedding[1] != -0.5 ||
		body.Usage.PromptTokens != 7 ||
		body.Usage.TotalTokens != 7 {
		t.Fatalf("translated response = %s", response.Body)
	}

	records := recorder.snapshot()
	if len(records) != 2 ||
		records[0].Attempt == nil ||
		records[1].Request == nil {
		t.Fatalf("records = %#v", records)
	}
	usage := records[0].Attempt.Usage
	assertTokenValue(t, "Gemini embedding input", usage.InputTokens, 7)
	assertTokenValue(t, "Gemini embedding output", usage.OutputTokens, 0)
	assertTokenValue(t, "Gemini embedding total", usage.TotalTokens, 7)
	if usage.Completeness != ledger.UsageComplete ||
		usage.NormalizationVersion !=
			geminiEmbedContentUsageVersion ||
		usage.ProviderComponents["prompt.text"] != 7 {
		t.Fatalf("usage = %#v", usage)
	}
}

func TestEmbeddingsTranslatesToGeminiBatchEmbedContents(t *testing.T) {
	t.Parallel()

	captured := make(chan capturedGeminiRequest, 1)
	upstream := httptest.NewServer(http.HandlerFunc(func(
		w http.ResponseWriter,
		request *http.Request,
	) {
		captured <- capturedGeminiRequest{
			Path:     request.URL.Path,
			RawQuery: request.URL.RawQuery,
			Header:   request.Header.Clone(),
			Body:     readJSONEnvelope(t, request.Body),
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{
			"embeddings":[
				{"values":[0.1,0.2]},
				{"values":[0.3,0.4]}
			],
			"usageMetadata":{
				"promptTokenCount":5,
				"promptTokenDetails":[{
					"modality":"TEXT",
					"tokenCount":5
				}]
			}
		}`)
	}))
	defer upstream.Close()

	recorder := &collectingRecorder{}
	handler, err := NewDataHandler(
		geminiDocument(
			upstream.URL+"/v1beta",
			config.CapabilitySingleVectorEmbedding,
		),
		DataOptions{Ledger: recorder},
	)
	if err != nil {
		t.Fatalf("NewDataHandler() error = %v", err)
	}
	response := serveEmbeddingsRequest(
		handler,
		`{
			"model":"default",
			"input":["first","second"],
			"dimensions":2
		}`,
	)
	if response.Code != http.StatusOK {
		t.Fatalf(
			"response=%d body=%s",
			response.Code,
			response.Body,
		)
	}

	request := <-captured
	if request.Path !=
		"/v1beta/models/gemini-upstream:batchEmbedContents" ||
		request.RawQuery != "" ||
		request.Header.Get("Accept") != "application/json" {
		t.Fatalf("upstream request = %#v", request)
	}
	var requests []map[string]json.RawMessage
	if json.Unmarshal(request.Body["requests"], &requests) != nil ||
		len(requests) != 2 {
		t.Fatalf("upstream body = %#v", request.Body)
	}
	for index, text := range []string{"first", "second"} {
		if string(requests[index]["model"]) !=
			`"models/gemini-upstream"` ||
			string(requests[index]["content"]) !=
				`{"parts":[{"text":"`+text+`"}]}` ||
			string(requests[index]["embedContentConfig"]) !=
				`{"autoTruncate":false,"outputDimensionality":2}` {
			t.Fatalf(
				"upstream request %d = %#v",
				index,
				requests[index],
			)
		}
	}
	if rawNonNull(request.Body["model"]) ||
		rawNonNull(request.Body["content"]) ||
		rawNonNull(request.Body["embedContentConfig"]) {
		t.Fatalf("upstream body = %#v", request.Body)
	}

	var body struct {
		Object string `json:"object"`
		Data   []struct {
			Object    string    `json:"object"`
			Embedding []float64 `json:"embedding"`
			Index     int       `json:"index"`
		} `json:"data"`
		Model string `json:"model"`
		Usage struct {
			PromptTokens int64 `json:"prompt_tokens"`
			TotalTokens  int64 `json:"total_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil ||
		body.Object != "list" ||
		body.Model != "public" ||
		len(body.Data) != 2 ||
		body.Data[0].Object != "embedding" ||
		body.Data[0].Index != 0 ||
		len(body.Data[0].Embedding) != 2 ||
		body.Data[0].Embedding[0] != 0.1 ||
		body.Data[0].Embedding[1] != 0.2 ||
		body.Data[1].Object != "embedding" ||
		body.Data[1].Index != 1 ||
		len(body.Data[1].Embedding) != 2 ||
		body.Data[1].Embedding[0] != 0.3 ||
		body.Data[1].Embedding[1] != 0.4 ||
		body.Usage.PromptTokens != 5 ||
		body.Usage.TotalTokens != 5 {
		t.Fatalf("translated response = %s", response.Body)
	}

	records := recorder.snapshot()
	if len(records) != 2 ||
		records[0].Attempt == nil ||
		records[1].Request == nil {
		t.Fatalf("records = %#v", records)
	}
	usage := records[0].Attempt.Usage
	assertTokenValue(t, "Gemini batch input", usage.InputTokens, 5)
	assertTokenValue(t, "Gemini batch output", usage.OutputTokens, 0)
	assertTokenValue(t, "Gemini batch total", usage.TotalTokens, 5)
	if usage.Completeness != ledger.UsageComplete ||
		usage.NormalizationVersion !=
			geminiBatchEmbedContentsUsageVersion ||
		usage.ProviderComponents["prompt.text"] != 5 {
		t.Fatalf("usage = %#v", usage)
	}
}

func TestEmbeddingsRejectsUnrepresentableGeminiRequestsBeforeTraffic(
	t *testing.T,
) {
	t.Parallel()

	var calls atomic.Int64
	upstream := httptest.NewServer(http.HandlerFunc(func(
		http.ResponseWriter,
		*http.Request,
	) {
		calls.Add(1)
	}))
	defer upstream.Close()
	handler, err := NewDataHandler(
		geminiDocument(
			upstream.URL+"/v1beta",
			config.CapabilitySingleVectorEmbedding,
		),
		DataOptions{},
	)
	if err != nil {
		t.Fatalf("NewDataHandler() error = %v", err)
	}
	for _, test := range []struct {
		name string
		body string
		code string
	}{
		{
			name: "token array",
			body: `{"model":"default","input":[1,2,3]}`,
			code: "unsupported_feature",
		},
		{
			name: "base64 encoding",
			body: `{"model":"default","input":"one","encoding_format":"base64"}`,
			code: "unsupported_feature",
		},
		{
			name: "caller attribution",
			body: `{"model":"default","input":"one","user":"caller-1"}`,
			code: "unsupported_feature",
		},
		{
			name: "unknown active field",
			body: `{"model":"default","input":"one","future":{"active":true}}`,
			code: "unsupported_feature",
		},
		{
			name: "empty input",
			body: `{"model":"default","input":""}`,
			code: "invalid_request_body",
		},
		{
			name: "empty input item",
			body: `{"model":"default","input":["one",""]}`,
			code: "invalid_request_body",
		},
		{
			name: "invalid dimensions",
			body: `{"model":"default","input":"one","dimensions":0}`,
			code: "invalid_request_body",
		},
	} {
		test := test
		t.Run(test.name, func(t *testing.T) {
			response := serveEmbeddingsRequest(handler, test.body)
			var failure openAIErrorEnvelope
			if response.Code != http.StatusBadRequest ||
				json.Unmarshal(
					response.Body.Bytes(),
					&failure,
				) != nil ||
				failure.Error.Code != test.code {
				t.Fatalf(
					"response=%d body=%s failure=%#v",
					response.Code,
					response.Body,
					failure,
				)
			}
		})
	}
	if calls.Load() != 0 {
		t.Fatalf("upstream calls = %d, want 0", calls.Load())
	}
}

func TestGeminiEmbeddingInputsRejectsMoreThanOpenAILimit(t *testing.T) {
	t.Parallel()

	inputs := make([]string, maxOpenAIEmbeddingInputs+1)
	for index := range inputs {
		inputs[index] = "text"
	}
	raw, err := json.Marshal(inputs)
	if err != nil {
		t.Fatalf("json.Marshal() error = %v", err)
	}
	_, err = geminiEmbeddingInputs(raw)
	if err == nil ||
		!strings.Contains(err.Error(), "more than 2048 items") {
		t.Fatalf("geminiEmbeddingInputs() error = %v", err)
	}
}

func TestEmbeddingsMixedPoolSkipsIncompatibleGeminiTarget(
	t *testing.T,
) {
	t.Parallel()

	var geminiCalls atomic.Int64
	gemini := httptest.NewServer(http.HandlerFunc(func(
		http.ResponseWriter,
		*http.Request,
	) {
		geminiCalls.Add(1)
	}))
	defer gemini.Close()
	var openAICalls atomic.Int64
	openAI := httptest.NewServer(http.HandlerFunc(func(
		w http.ResponseWriter,
		request *http.Request,
	) {
		openAICalls.Add(1)
		if request.URL.Path != "/v1/embeddings" {
			t.Errorf("OpenAI path = %q", request.URL.Path)
		}
		body := readJSONEnvelope(t, request.Body)
		if string(body["model"]) != `"openai-upstream"` ||
			string(body["input"]) != `[[1,2],[3,4]]` {
			t.Errorf("OpenAI body = %#v", body)
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"object": "list",
			"data": []any{
				map[string]any{
					"object":    "embedding",
					"embedding": []float64{0.1},
					"index":     0,
				},
				map[string]any{
					"object":    "embedding",
					"embedding": []float64{0.2},
					"index":     1,
				},
			},
			"model": "openai-upstream",
			"usage": map[string]any{
				"prompt_tokens": 2,
				"total_tokens":  2,
			},
		})
	}))
	defer openAI.Close()

	document := geminiDocument(
		gemini.URL+"/v1beta",
		config.CapabilitySingleVectorEmbedding,
	)
	document.Providers = append(document.Providers, config.Provider{
		Name:    "openai",
		Type:    "openai_compatible",
		BaseURL: openAI.URL + "/v1",
	})
	document.Deployments = append(
		document.Deployments,
		config.Deployment{
			Name:     "openai",
			Provider: "openai",
			Model:    "openai-upstream",
			Capabilities: []config.Capability{
				config.CapabilitySingleVectorEmbedding,
			},
		},
	)
	document.VirtualModels[0].Pools[0].Targets = append(
		document.VirtualModels[0].Pools[0].Targets,
		config.WeightedTarget{
			Deployment: "openai",
			Weight:     100,
		},
	)
	handler, err := NewDataHandler(document, DataOptions{
		RoutingPicker: zeroPicker{},
	})
	if err != nil {
		t.Fatalf("NewDataHandler() error = %v", err)
	}
	response := serveEmbeddingsRequest(
		handler,
		`{"model":"default","input":[[1,2],[3,4]]}`,
	)
	if response.Code != http.StatusOK ||
		geminiCalls.Load() != 0 ||
		openAICalls.Load() != 1 {
		t.Fatalf(
			"response=%d gemini=%d openai=%d body=%s",
			response.Code,
			geminiCalls.Load(),
			openAICalls.Load(),
			response.Body,
		)
	}
}

func TestEmbeddingsRejectsInvalidGeminiResponses(t *testing.T) {
	t.Parallel()

	for _, test := range []struct {
		name string
		body string
	}{
		{
			name: "dimension mismatch",
			body: `{
				"embedding":{"values":[0.25]},
				"usageMetadata":{"promptTokenCount":1}
			}`,
		},
		{
			name: "provider-only shape",
			body: `{
				"embedding":{"values":[0.25,-0.5],"shape":[1,2]},
				"usageMetadata":{"promptTokenCount":1}
			}`,
		},
		{
			name: "unknown active field",
			body: `{
				"embedding":{"values":[0.25,-0.5]},
				"usageMetadata":{"promptTokenCount":1},
				"future":{"active":true}
			}`,
		},
		{
			name: "missing usage",
			body: `{"embedding":{"values":[0.25,-0.5]}}`,
		},
	} {
		test := test
		t.Run(test.name, func(t *testing.T) {
			upstream := httptest.NewServer(
				http.HandlerFunc(func(
					w http.ResponseWriter,
					_ *http.Request,
				) {
					w.Header().Set(
						"Content-Type",
						"application/json",
					)
					_, _ = io.WriteString(w, test.body)
				}),
			)
			defer upstream.Close()
			handler, err := NewDataHandler(
				geminiDocument(
					upstream.URL+"/v1beta",
					config.CapabilitySingleVectorEmbedding,
				),
				DataOptions{},
			)
			if err != nil {
				t.Fatalf("NewDataHandler() error = %v", err)
			}
			response := serveEmbeddingsRequest(
				handler,
				`{"model":"default","input":"hello","dimensions":2}`,
			)
			var failure openAIErrorEnvelope
			if response.Code != http.StatusBadGateway ||
				json.Unmarshal(
					response.Body.Bytes(),
					&failure,
				) != nil ||
				failure.Error.Code != "upstream_response_error" {
				t.Fatalf(
					"response=%d body=%s failure=%#v",
					response.Code,
					response.Body,
					failure,
				)
			}
		})
	}
}

func TestEmbeddingsRejectsInvalidGeminiBatchResponses(t *testing.T) {
	t.Parallel()

	for _, test := range []struct {
		name string
		body string
	}{
		{
			name: "embedding count mismatch",
			body: `{
				"embeddings":[{"values":[0.1,0.2]}],
				"usageMetadata":{"promptTokenCount":2}
			}`,
		},
		{
			name: "item dimension mismatch",
			body: `{
				"embeddings":[
					{"values":[0.1,0.2]},
					{"values":[0.3]}
				],
				"usageMetadata":{"promptTokenCount":2}
			}`,
		},
		{
			name: "provider-only item shape",
			body: `{
				"embeddings":[
					{"values":[0.1,0.2]},
					{"values":[0.3,0.4],"shape":[1,2]}
				],
				"usageMetadata":{"promptTokenCount":2}
			}`,
		},
		{
			name: "unknown active field",
			body: `{
				"embeddings":[
					{"values":[0.1,0.2]},
					{"values":[0.3,0.4]}
				],
				"usageMetadata":{"promptTokenCount":2},
				"future":{"active":true}
			}`,
		},
	} {
		test := test
		t.Run(test.name, func(t *testing.T) {
			upstream := httptest.NewServer(
				http.HandlerFunc(func(
					w http.ResponseWriter,
					request *http.Request,
				) {
					if request.URL.Path !=
						"/v1beta/models/gemini-upstream:batchEmbedContents" {
						t.Errorf(
							"upstream path = %q",
							request.URL.Path,
						)
					}
					w.Header().Set(
						"Content-Type",
						"application/json",
					)
					_, _ = io.WriteString(w, test.body)
				}),
			)
			defer upstream.Close()
			handler, err := NewDataHandler(
				geminiDocument(
					upstream.URL+"/v1beta",
					config.CapabilitySingleVectorEmbedding,
				),
				DataOptions{},
			)
			if err != nil {
				t.Fatalf("NewDataHandler() error = %v", err)
			}
			response := serveEmbeddingsRequest(
				handler,
				`{
					"model":"default",
					"input":["first","second"],
					"dimensions":2
				}`,
			)
			var failure openAIErrorEnvelope
			if response.Code != http.StatusBadGateway ||
				json.Unmarshal(
					response.Body.Bytes(),
					&failure,
				) != nil ||
				failure.Error.Code != "upstream_response_error" {
				t.Fatalf(
					"response=%d body=%s failure=%#v",
					response.Code,
					response.Body,
					failure,
				)
			}
		})
	}
}

func TestEmbeddingsTranslatesGeminiHTTPErrorEnvelope(t *testing.T) {
	t.Parallel()

	upstream := httptest.NewServer(http.HandlerFunc(func(
		w http.ResponseWriter,
		_ *http.Request,
	) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = io.WriteString(w, `{
			"error":{
				"code":400,
				"message":"invalid embedding input",
				"status":"INVALID_ARGUMENT"
			}
		}`)
	}))
	defer upstream.Close()
	handler, err := NewDataHandler(
		geminiDocument(
			upstream.URL+"/v1beta",
			config.CapabilitySingleVectorEmbedding,
		),
		DataOptions{},
	)
	if err != nil {
		t.Fatalf("NewDataHandler() error = %v", err)
	}
	response := serveEmbeddingsRequest(
		handler,
		`{"model":"default","input":"hello"}`,
	)
	var failure openAIErrorEnvelope
	if response.Code != http.StatusBadRequest ||
		json.Unmarshal(response.Body.Bytes(), &failure) != nil ||
		failure.Error.Code != "upstream_error" ||
		failure.Error.Message != "invalid embedding input" {
		t.Fatalf(
			"response=%d body=%s failure=%#v",
			response.Code,
			response.Body,
			failure,
		)
	}
}

func serveEmbeddingsRequest(
	handler http.Handler,
	body string,
) *httptest.ResponseRecorder {
	request := httptest.NewRequest(
		http.MethodPost,
		"/v1/embeddings",
		strings.NewReader(body),
	)
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	return response
}
