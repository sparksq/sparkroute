// SPDX-FileCopyrightText: 2026 Scitrera LLC
// SPDX-FileCopyrightText: 2026 Fox Engine Ltd.
// SPDX-License-Identifier: AGPL-3.0-only

package gateway

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sparksq/sparkroute/pkg/config"
	"github.com/sparksq/sparkroute/pkg/credentials"
	credentialawsworkload "github.com/sparksq/sparkroute/pkg/credentials/awsworkload"
	"github.com/sparksq/sparkroute/pkg/ledger"
)

type capturedBedrockRequest struct {
	Path        string
	EscapedPath string
	RawQuery    string
	Header      http.Header
	Body        []byte
	Host        string
}

func TestBedrockConversePassthroughSigningAndUsage(t *testing.T) {
	t.Parallel()

	captured := make(chan capturedBedrockRequest, 1)
	upstreamResponse := `{
		"output":{"message":{"role":"assistant","content":[{"text":"hello"}]}},
		"stopReason":"end_turn",
		"usage":{
			"inputTokens":12,
			"outputTokens":7,
			"totalTokens":19,
			"cacheReadInputTokens":3,
			"cacheWriteInputTokens":2,
			"cacheDetails":[{"inputTokens":2,"ttl":"5m"}]
		},
		"metrics":{"latencyMs":10},
		"futureResponseField":{"kept":true}
	}`
	upstream := httptest.NewServer(http.HandlerFunc(func(
		w http.ResponseWriter,
		request *http.Request,
	) {
		body, _ := io.ReadAll(request.Body)
		captured <- capturedBedrockRequest{
			Path:        request.URL.Path,
			EscapedPath: request.URL.EscapedPath(),
			RawQuery:    request.URL.RawQuery,
			Header:      request.Header.Clone(),
			Body:        body,
			Host:        request.Host,
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("X-Amzn-Requestid", "bedrock-request-1")
		_, _ = io.WriteString(w, upstreamResponse)
	}))
	defer upstream.Close()

	recorder := &collectingRecorder{}
	document := bedrockDocument(upstream.URL)
	handler, err := NewDataHandler(document, DataOptions{
		Credentials: bedrockCredentialSource(),
		Ledger:      recorder,
	})
	if err != nil {
		t.Fatalf("NewDataHandler() error = %v", err)
	}
	body := `{
		"messages":[{"role":"user","content":[{"text":"hello"}]}],
		"inferenceConfig":{"maxTokens":128},
		"futureRequestField":{"kept":true}
	}`
	response := serveBedrockRequest(
		handler,
		http.MethodPost,
		"/model/default/converse?caller=secret",
		body,
	)
	if response.Code != http.StatusOK ||
		response.Body.String() != upstreamResponse {
		t.Fatalf(
			"response = %d %q",
			response.Code,
			response.Body.String(),
		)
	}
	if response.Header().Get("X-Amzn-Requestid") !=
		"bedrock-request-1" {
		t.Fatalf(
			"X-Amzn-Requestid = %q",
			response.Header().Get("X-Amzn-Requestid"),
		)
	}
	got := <-captured
	if got.Path !=
		"/model/us.anthropic.claude-sonnet-4-20250514-v1:0/converse" ||
		got.EscapedPath !=
			"/model/us.anthropic.claude-sonnet-4-20250514-v1%3A0/converse" ||
		got.RawQuery != "" {
		t.Fatalf("upstream request = %#v", got)
	}
	if got.Header.Get("Accept") != "application/json" ||
		got.Header.Get("X-Amz-Date") == "" ||
		got.Header.Get("X-Amz-Content-Sha256") == "" ||
		got.Header.Get("X-Amz-Security-Token") != "session-token" ||
		!strings.Contains(
			got.Header.Get("Authorization"),
			"Credential=AKIDEXAMPLE/",
		) ||
		!strings.Contains(
			got.Header.Get("Authorization"),
			"/us-east-1/bedrock/aws4_request",
		) {
		t.Fatalf("signed headers = %#v", got.Header)
	}
	var upstreamBody map[string]json.RawMessage
	if err := json.Unmarshal(got.Body, &upstreamBody); err != nil ||
		!strings.Contains(
			string(upstreamBody["futureRequestField"]),
			`"kept":true`,
		) {
		t.Fatalf("upstream body = %s, %v", got.Body, err)
	}
	for _, forbidden := range []string{"model", "modelId", "stream"} {
		if _, exists := upstreamBody[forbidden]; exists {
			t.Fatalf("upstream body contains %s", forbidden)
		}
	}
	records := recorder.snapshot()
	if len(records) != 2 ||
		records[0].Attempt == nil ||
		records[1].Request == nil {
		t.Fatalf("records = %#v", records)
	}
	for _, usage := range []ledger.TokenUsage{
		records[0].Attempt.Usage,
		records[1].Request.Usage,
	} {
		assertBedrockUsage(t, usage, 12, 7, 19, 3, 2)
		if usage.ProviderComponents["cache_detail.5m"] != 2 {
			t.Errorf(
				"provider components = %#v",
				usage.ProviderComponents,
			)
		}
	}
	if records[1].Request.Protocol != "bedrock" ||
		records[1].Request.Operation != "converse" ||
		records[1].Request.RequestedModel != "default" ||
		records[0].Attempt.UpstreamRequestID != "bedrock-request-1" {
		t.Fatalf("ledger records = %#v", records)
	}
}

func TestBedrockConverseStreamBinaryLifecycleAndUsage(t *testing.T) {
	t.Parallel()

	stream := bytes.Join([][]byte{
		encodeAWSEventMessage(t, bedrockEventHeaders("messageStart"),
			[]byte(`{"role":"assistant"}`)),
		encodeAWSEventMessage(t, bedrockEventHeaders("contentBlockDelta"),
			[]byte(`{"contentBlockIndex":0,"delta":{"text":"hello"}}`)),
		encodeAWSEventMessage(t, bedrockEventHeaders("messageStop"),
			[]byte(`{"stopReason":"end_turn"}`)),
		encodeAWSEventMessage(t, bedrockEventHeaders("metadata"),
			[]byte(`{"usage":{"inputTokens":5,"outputTokens":2,`+
				`"totalTokens":7,"cacheReadInputTokens":1},`+
				`"metrics":{"latencyMs":4}}`)),
	}, nil)
	captured := make(chan capturedBedrockRequest, 1)
	upstream := httptest.NewServer(http.HandlerFunc(func(
		w http.ResponseWriter,
		request *http.Request,
	) {
		body, _ := io.ReadAll(request.Body)
		captured <- capturedBedrockRequest{
			Path: request.URL.Path, Header: request.Header.Clone(),
			Body: body,
		}
		w.Header().Set(
			"Content-Type",
			"application/vnd.amazon.eventstream",
		)
		w.WriteHeader(http.StatusOK)
		for _, value := range stream {
			_, _ = w.Write([]byte{value})
			if flusher, ok := w.(http.Flusher); ok {
				flusher.Flush()
			}
		}
	}))
	defer upstream.Close()
	recorder := &collectingRecorder{}
	handler, err := NewDataHandler(
		bedrockDocument(upstream.URL),
		DataOptions{
			Credentials: bedrockCredentialSource(),
			Ledger:      recorder,
		},
	)
	if err != nil {
		t.Fatalf("NewDataHandler() error = %v", err)
	}
	response := serveBedrockRequest(
		handler,
		http.MethodPost,
		"/model/default/converse-stream",
		`{"messages":[{"role":"user","content":[{"text":"hello"}]}]}`,
	)
	if response.Code != http.StatusOK ||
		!bytes.Equal(response.Body.Bytes(), stream) {
		t.Fatalf(
			"response = %d, body equal = %v",
			response.Code,
			bytes.Equal(response.Body.Bytes(), stream),
		)
	}
	got := <-captured
	if got.Path !=
		"/model/us.anthropic.claude-sonnet-4-20250514-v1:0/converse-stream" ||
		got.Header.Get("Accept") !=
			"application/vnd.amazon.eventstream" ||
		got.Header.Get("Authorization") == "" {
		t.Fatalf("upstream request = %#v", got)
	}
	records := recorder.snapshot()
	if len(records) != 2 ||
		records[0].Attempt == nil ||
		records[0].Attempt.Outcome != ledger.OutcomeSuccess ||
		records[1].Request == nil ||
		!records[1].Request.Stream {
		t.Fatalf("records = %#v", records)
	}
	assertBedrockUsage(
		t,
		records[0].Attempt.Usage,
		5,
		2,
		7,
		1,
		0,
	)
}

func TestBedrockConverseStreamClassifiesException(t *testing.T) {
	t.Parallel()

	exception := encodeAWSEventMessage(t, map[string]string{
		":message-type":   "exception",
		":exception-type": "throttlingException",
		":content-type":   "application/json",
	}, []byte(`{"message":"quota exceeded"}`))
	stream := bytes.Join([][]byte{
		exception,
		encodeAWSEventMessage(
			t,
			bedrockEventHeaders("messageStop"),
			[]byte(`{"stopReason":"end_turn"}`),
		),
	}, nil)
	upstream := httptest.NewServer(http.HandlerFunc(func(
		w http.ResponseWriter,
		_ *http.Request,
	) {
		w.Header().Set(
			"Content-Type",
			"application/vnd.amazon.eventstream",
		)
		_, _ = w.Write(stream)
	}))
	defer upstream.Close()
	recorder := &collectingRecorder{}
	handler, err := NewDataHandler(
		bedrockDocument(upstream.URL),
		DataOptions{
			Credentials: bedrockCredentialSource(),
			Ledger:      recorder,
		},
	)
	if err != nil {
		t.Fatalf("NewDataHandler() error = %v", err)
	}
	response := serveBedrockRequest(
		handler,
		http.MethodPost,
		"/model/default/converse-stream",
		`{"messages":[{"role":"user","content":[{"text":"hello"}]}]}`,
	)
	records := recorder.snapshot()
	if response.Code != http.StatusOK ||
		!bytes.Equal(response.Body.Bytes(), stream) ||
		len(records) != 2 ||
		records[0].Attempt == nil ||
		records[0].Attempt.Outcome != ledger.OutcomeUpstreamError ||
		records[0].Attempt.FailureClass !=
			"bedrock_stream_exception_throttling_exception" {
		t.Fatalf(
			"response=%d records=%#v",
			response.Code,
			records,
		)
	}
}

func TestBedrockConverseStreamRejectsCorruptFrame(t *testing.T) {
	t.Parallel()

	corrupt := encodeAWSEventMessage(
		t,
		bedrockEventHeaders("messageStop"),
		[]byte(`{"stopReason":"end_turn"}`),
	)
	corrupt[len(corrupt)-1] ^= 0xff
	upstream := httptest.NewServer(http.HandlerFunc(func(
		w http.ResponseWriter,
		_ *http.Request,
	) {
		w.Header().Set(
			"Content-Type",
			"application/vnd.amazon.eventstream",
		)
		_, _ = w.Write(corrupt)
	}))
	defer upstream.Close()
	recorder := &collectingRecorder{}
	handler, err := NewDataHandler(
		bedrockDocument(upstream.URL),
		DataOptions{
			Credentials: bedrockCredentialSource(),
			Ledger:      recorder,
		},
	)
	if err != nil {
		t.Fatalf("NewDataHandler() error = %v", err)
	}
	response := serveBedrockRequest(
		handler,
		http.MethodPost,
		"/model/default/converse-stream",
		`{"messages":[{"role":"user","content":[{"text":"hello"}]}]}`,
	)
	records := recorder.snapshot()
	if response.Code != http.StatusOK ||
		len(records) != 2 ||
		records[0].Attempt == nil ||
		records[0].Attempt.Outcome != ledger.OutcomeStreamError ||
		records[0].Attempt.FailureClass !=
			"bedrock_stream_invalid_message" {
		t.Fatalf(
			"response=%d records=%#v",
			response.Code,
			records,
		)
	}
}

func TestBedrockRejectsInvalidSuccessfulProviderResponses(t *testing.T) {
	t.Parallel()

	for _, test := range []struct {
		name        string
		path        string
		contentType string
		body        string
	}{
		{
			name:        "Converse missing usage",
			path:        "/model/default/converse",
			contentType: "application/json",
			body:        `{"output":{"message":{"role":"assistant","content":[]}}}`,
		},
		{
			name:        "ConverseStream wrong content type",
			path:        "/model/default/converse-stream",
			contentType: "application/json",
			body:        `{"message":"not an event stream"}`,
		},
	} {
		test := test
		t.Run(test.name, func(t *testing.T) {
			upstream := httptest.NewServer(http.HandlerFunc(func(
				w http.ResponseWriter,
				_ *http.Request,
			) {
				w.Header().Set("Content-Type", test.contentType)
				_, _ = io.WriteString(w, test.body)
			}))
			defer upstream.Close()
			handler, err := NewDataHandler(
				bedrockDocument(upstream.URL),
				DataOptions{
					Credentials: bedrockCredentialSource(),
				},
			)
			if err != nil {
				t.Fatalf("NewDataHandler() error = %v", err)
			}
			response := serveBedrockRequest(
				handler,
				http.MethodPost,
				test.path,
				`{"messages":[{"role":"user",`+
					`"content":[{"text":"hello"}]}]}`,
			)
			assertBedrockError(
				t,
				response,
				http.StatusBadGateway,
				"InternalServerException",
			)
		})
	}
}

func TestBedrockCapabilityDetection(t *testing.T) {
	t.Parallel()

	envelope := readRawEnvelope(t, `{
		"messages":[{"role":"user","content":[
			{"image":{"format":"png","source":{"bytes":"AA=="}}},
			{"document":{"format":"pdf","name":"doc","source":{"bytes":"AA=="}}},
			{"toolResult":{"toolUseId":"tool-1","content":[{"json":{"ok":true}}]}},
			{"cachePoint":{"type":"default"}},
			{"reasoningContent":{"reasoningText":{"text":"thinking"}}},
			{"guardContent":{"text":{"text":"screen"}}}
		]}],
		"toolConfig":{"tools":[]},
		"outputConfig":{"textFormat":{"type":"json_schema"}},
		"performanceConfig":{"latency":"optimized"},
		"promptVariables":{"genre":{"text":"rock"}},
		"requestMetadata":{"tenant":"tenant-a"},
		"additionalModelRequestFields":{"top_k":100}
	}`)
	got, err := detectBedrockCapabilities(envelope)
	if err != nil {
		t.Fatalf("detectBedrockCapabilities() error = %v", err)
	}
	want := []config.Capability{
		config.CapabilityFileInput,
		config.CapabilityPromptCaching,
		config.CapabilityProviderGuardrails,
		config.CapabilityProviderPrompts,
		config.CapabilityReasoning,
		config.CapabilityServiceTier,
		config.CapabilityStructuredOutputs,
		config.CapabilityTools,
		config.CapabilityVision,
		bedrockAdditionalFieldsCapability,
		bedrockRequestMetadataCapability,
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("capabilities = %v, want %v", got, want)
	}
}

func TestBedrockRejectsS3ReferencesAndMissingCapabilities(t *testing.T) {
	t.Parallel()

	var calls atomic.Int64
	upstream := httptest.NewServer(http.HandlerFunc(func(
		http.ResponseWriter,
		*http.Request,
	) {
		calls.Add(1)
	}))
	defer upstream.Close()
	document := bedrockDocument(upstream.URL)
	document.CapabilityDefaults.Unknown = config.UnknownCapabilityReject
	handler, err := NewDataHandler(
		document,
		DataOptions{Credentials: bedrockCredentialSource()},
	)
	if err != nil {
		t.Fatalf("NewDataHandler() error = %v", err)
	}
	for _, test := range []struct {
		name string
		body string
	}{
		{
			name: "missing vision capability",
			body: `{"messages":[{"role":"user","content":[{` +
				`"image":{"format":"png","source":{"bytes":"AA=="}}}]}]}`,
		},
		{
			name: "S3 reference",
			body: `{"messages":[{"role":"user","content":[{` +
				`"document":{"format":"pdf","name":"doc","source":{` +
				`"s3Location":{"uri":"s3://private/doc.pdf"}}}}]}]}`,
		},
	} {
		test := test
		t.Run(test.name, func(t *testing.T) {
			response := serveBedrockRequest(
				handler,
				http.MethodPost,
				"/model/default/converse",
				test.body,
			)
			assertBedrockError(
				t,
				response,
				http.StatusBadRequest,
				"ValidationException",
			)
		})
	}
	if calls.Load() != 0 {
		t.Fatalf("upstream calls = %d, want 0", calls.Load())
	}
}

func TestBedrockRejectsWrongMethodUnknownModelAndPathOwnedBody(t *testing.T) {
	t.Parallel()

	handler, err := NewDataHandler(
		bedrockDocument("https://bedrock.example"),
		DataOptions{Credentials: bedrockCredentialSource()},
	)
	if err != nil {
		t.Fatalf("NewDataHandler() error = %v", err)
	}
	method := serveBedrockRequest(
		handler,
		http.MethodGet,
		"/model/default/converse",
		`{}`,
	)
	assertBedrockError(
		t,
		method,
		http.StatusMethodNotAllowed,
		"ValidationException",
	)
	unknown := serveBedrockRequest(
		handler,
		http.MethodPost,
		"/model/missing/converse",
		`{}`,
	)
	assertBedrockError(
		t,
		unknown,
		http.StatusNotFound,
		"ResourceNotFoundException",
	)
	pathOwned := serveBedrockRequest(
		handler,
		http.MethodPost,
		"/model/default/converse",
		`{"modelId":"other"}`,
	)
	assertBedrockError(
		t,
		pathOwned,
		http.StatusBadRequest,
		"ValidationException",
	)
}

func TestBedrockDerivesRegionalRuntimeEndpoint(t *testing.T) {
	t.Parallel()

	captured := make(chan *http.Request, 1)
	client := &http.Client{Transport: bedrockRoundTripFunc(func(
		request *http.Request,
	) (*http.Response, error) {
		captured <- request.Clone(request.Context())
		return &http.Response{
			StatusCode: http.StatusOK,
			Header: http.Header{
				"Content-Type": []string{"application/json"},
			},
			Body: io.NopCloser(strings.NewReader(`{
				"output":{"message":{"role":"assistant","content":[]}},
				"stopReason":"end_turn",
				"usage":{"inputTokens":1,"outputTokens":1,"totalTokens":2}
			}`)),
			Request: request,
		}, nil
	})}
	document := bedrockDocument("")
	handler, err := NewDataHandler(document, DataOptions{
		Credentials: bedrockCredentialSource(),
		HTTPClient:  client,
	})
	if err != nil {
		t.Fatalf("NewDataHandler() error = %v", err)
	}
	response := serveBedrockRequest(
		handler,
		http.MethodPost,
		"/model/default/converse",
		`{"messages":[{"role":"user","content":[{"text":"hello"}]}]}`,
	)
	if response.Code != http.StatusOK {
		t.Fatalf("response = %d %s", response.Code, response.Body)
	}
	got := <-captured
	if got.URL.Scheme != "https" ||
		got.URL.Host !=
			"bedrock-runtime.us-east-1.amazonaws.com" ||
		got.Header.Get("Authorization") == "" {
		t.Fatalf("derived request URL = %s", got.URL)
	}
}

func TestBedrockProviderHostedContentUsesSingleAttempt(t *testing.T) {
	t.Parallel()

	var firstCalls atomic.Int64
	first := httptest.NewServer(http.HandlerFunc(func(
		w http.ResponseWriter,
		_ *http.Request,
	) {
		firstCalls.Add(1)
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = io.WriteString(w, `{"message":"unavailable"}`)
	}))
	defer first.Close()
	var secondCalls atomic.Int64
	second := httptest.NewServer(http.HandlerFunc(func(
		w http.ResponseWriter,
		_ *http.Request,
	) {
		secondCalls.Add(1)
		writeJSON(w, http.StatusOK, map[string]any{
			"output": map[string]any{
				"message": map[string]any{
					"role": "assistant", "content": []any{},
				},
			},
			"stopReason": "end_turn",
			"usage": map[string]any{
				"inputTokens": 1, "outputTokens": 1,
				"totalTokens": 2,
			},
		})
	}))
	defer second.Close()
	document := bedrockDocument(
		first.URL,
		config.CapabilityProviderTools,
	)
	document.Providers = append(document.Providers, config.Provider{
		Name:    "bedrock-second",
		Type:    "bedrock",
		BaseURL: second.URL,
		Region:  "us-east-1",
		Auth: config.ProviderAuth{
			Type:       config.AuthAWSSigV4,
			Credential: credentials.Ref("workload://aws"),
		},
	})
	document.Deployments = append(
		document.Deployments,
		config.Deployment{
			Name:     "bedrock-second",
			Provider: "bedrock-second",
			Model:    "second-model",
			Capabilities: []config.Capability{
				config.CapabilityProviderTools,
			},
		},
	)
	document.VirtualModels[0].Pools[0].Targets = append(
		document.VirtualModels[0].Pools[0].Targets,
		config.WeightedTarget{
			Deployment: "bedrock-second",
			Weight:     1,
		},
	)
	document.VirtualModels[0].Limits.MaxAttempts = 2
	handler, err := NewDataHandler(document, DataOptions{
		Credentials:   bedrockCredentialSource(),
		RoutingPicker: zeroPicker{},
	})
	if err != nil {
		t.Fatalf("NewDataHandler() error = %v", err)
	}
	response := serveBedrockRequest(
		handler,
		http.MethodPost,
		"/model/default/converse",
		`{"messages":[{"role":"user","content":[{`+
			`"searchResult":{"source":{"type":"web"},`+
			`"title":"result","content":[{"text":"content"}]}}]}]}`,
	)
	if response.Code != http.StatusServiceUnavailable ||
		firstCalls.Load() != 1 ||
		secondCalls.Load() != 0 {
		t.Fatalf(
			"response=%d first=%d second=%d body=%s",
			response.Code,
			firstCalls.Load(),
			secondCalls.Load(),
			response.Body,
		)
	}
}

func bedrockDocument(
	baseURL string,
	capabilities ...config.Capability,
) config.Document {
	return config.Document{
		Providers: []config.Provider{{
			Name:    "bedrock",
			Type:    "bedrock",
			BaseURL: baseURL,
			Region:  "us-east-1",
			Auth: config.ProviderAuth{
				Type:       config.AuthAWSSigV4,
				Credential: credentials.Ref("workload://aws"),
			},
		}},
		Deployments: []config.Deployment{{
			Name:         "bedrock",
			Provider:     "bedrock",
			Model:        "us.anthropic.claude-sonnet-4-20250514-v1:0",
			Capabilities: capabilities,
		}},
		VirtualModels: []config.VirtualModel{{
			Name:    "public",
			Aliases: []string{"default"},
			Pools: []config.RoutingPool{{
				Targets: []config.WeightedTarget{{
					Deployment: "bedrock",
					Weight:     1,
				}},
			}},
		}},
	}
}

func bedrockCredentialSource() fakeCredentialSource {
	raw, _ := json.Marshal(credentialawsworkload.SigningMaterial{
		AccessKeyID:     "AKIDEXAMPLE",
		SecretAccessKey: "secret-key",
		SessionToken:    "session-token",
	})
	return fakeCredentialSource{
		"workload://aws": string(raw),
	}
}

func serveBedrockRequest(
	handler http.Handler,
	method string,
	path string,
	body string,
) *httptest.ResponseRecorder {
	request := httptest.NewRequest(
		method,
		path,
		strings.NewReader(body),
	)
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	return response
}

func bedrockEventHeaders(eventType string) map[string]string {
	return map[string]string{
		":message-type": "event",
		":event-type":   eventType,
		":content-type": "application/json",
	}
}

func readRawEnvelope(
	t *testing.T,
	raw string,
) map[string]json.RawMessage {
	t.Helper()
	var envelope map[string]json.RawMessage
	if err := json.Unmarshal([]byte(raw), &envelope); err != nil {
		t.Fatalf("decode test envelope: %v", err)
	}
	return envelope
}

func assertBedrockUsage(
	t *testing.T,
	usage ledger.TokenUsage,
	input int64,
	output int64,
	total int64,
	cacheRead int64,
	cacheWrite int64,
) {
	t.Helper()
	if usage.InputTokens == nil || *usage.InputTokens != input ||
		usage.OutputTokens == nil || *usage.OutputTokens != output ||
		usage.TotalTokens == nil || *usage.TotalTokens != total {
		t.Fatalf("usage = %#v", usage)
	}
	if cacheRead == 0 {
		if usage.CachedInputTokens != nil {
			t.Fatalf("cached input tokens = %v", usage.CachedInputTokens)
		}
	} else if usage.CachedInputTokens == nil ||
		*usage.CachedInputTokens != cacheRead {
		t.Fatalf("cached input tokens = %v", usage.CachedInputTokens)
	}
	if cacheWrite == 0 {
		if usage.CacheCreationTokens != nil {
			t.Fatalf(
				"cache creation tokens = %v",
				usage.CacheCreationTokens,
			)
		}
	} else if usage.CacheCreationTokens == nil ||
		*usage.CacheCreationTokens != cacheWrite {
		t.Fatalf(
			"cache creation tokens = %v",
			usage.CacheCreationTokens,
		)
	}
	if usage.Completeness != ledger.UsageComplete ||
		usage.NormalizationVersion != bedrockConverseUsageVersion {
		t.Fatalf("usage metadata = %#v", usage)
	}
}

func assertBedrockError(
	t *testing.T,
	response *httptest.ResponseRecorder,
	status int,
	errorType string,
) {
	t.Helper()
	if response.Code != status ||
		response.Header().Get("X-Amzn-Errortype") != errorType {
		t.Fatalf(
			"error = status:%d type:%q body:%s",
			response.Code,
			response.Header().Get("X-Amzn-Errortype"),
			response.Body,
		)
	}
	var envelope bedrockErrorEnvelope
	if err := json.Unmarshal(
		response.Body.Bytes(),
		&envelope,
	); err != nil || envelope.Message == "" {
		t.Fatalf("error body = %s, %v", response.Body, err)
	}
}

type bedrockRoundTripFunc func(
	*http.Request,
) (*http.Response, error)

func (f bedrockRoundTripFunc) RoundTrip(
	request *http.Request,
) (*http.Response, error) {
	return f(request)
}

func TestBedrockCredentialMaterialCanExpire(t *testing.T) {
	t.Parallel()

	raw, _ := json.Marshal(credentialawsworkload.SigningMaterial{
		AccessKeyID:     "AKIDEXAMPLE",
		SecretAccessKey: "secret",
		ExpiresAt:       time.Now().Add(-time.Minute),
	})
	request, _ := http.NewRequest(
		http.MethodPost,
		"https://bedrock-runtime.us-east-1.amazonaws.com/model/x/converse",
		bytes.NewReader([]byte(`{}`)),
	)
	request.Header.Set("Content-Type", "application/json")
	if err := signAWSRequest(
		request,
		raw,
		"us-east-1",
		bedrockAWSService,
		[]byte(`{}`),
		time.Now(),
	); err == nil {
		t.Fatal("signAWSRequest() error = nil for expired material")
	}
}
