package gateway

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/sparksq/sparkroute/pkg/config"
	"github.com/sparksq/sparkroute/pkg/identity"
	"github.com/sparksq/sparkroute/pkg/ledger"
	"github.com/sparksq/sparkroute/pkg/responsesstate"
)

func TestResponsesCompactPassthroughUsageTraceAndItemBinding(
	t *testing.T,
) {
	t.Parallel()

	captured := make(chan capturedUpstreamRequest, 1)
	upstream := httptest.NewServer(http.HandlerFunc(func(
		w http.ResponseWriter,
		request *http.Request,
	) {
		captured <- capturedUpstreamRequest{
			Path: request.URL.Path,
			Body: readJSONEnvelope(t, request.Body),
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("ETag", `"upstream-representation"`)
		_, _ = io.WriteString(w, `{
			"id":"resp_compact_1",
			"object":"response.compaction",
			"created_at":1770000000,
			"output":[
				{
					"id":"msg_retained_1",
					"type":"message",
					"status":"completed",
					"role":"user",
					"content":[{"type":"input_text","text":"hello"}]
				},
				{
					"id":"cmp_1",
					"type":"compaction",
					"encrypted_content":"opaque-state"
				}
			],
			"usage":{
				"input_tokens":139,
				"input_tokens_details":{
					"cached_tokens":7,
					"cache_write_tokens":3
				},
				"output_tokens":42,
				"output_tokens_details":{"reasoning_tokens":11},
				"total_tokens":181
			},
			"future_response_field":{"preserved":true}
		}`)
	}))
	defer upstream.Close()

	usageRecorder := &collectingRecorder{}
	traceRecorder := &savedTraceCollector{}
	state := responsesstate.NewMemoryStore(
		responsesstate.MemoryOptions{},
	)
	handler, err := NewDataHandler(
		responsesCompactDocument(upstream.URL+"/v1"),
		DataOptions{
			Ledger:         usageRecorder,
			ResponsesState: state,
			SavedTraces:    traceRecorder,
		},
	)
	if err != nil {
		t.Fatalf("NewDataHandler() error = %v", err)
	}
	rawRequest := `{
		"model":"default",
		"input":[{"role":"user","content":"hello"}],
		"instructions":"Preserve durable facts.",
		"future_request_field":{"preserved":true}
	}`
	request := httptest.NewRequest(
		http.MethodPost,
		"/v1/responses/compact",
		strings.NewReader(rawRequest),
	)
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)

	if response.Code != http.StatusOK {
		t.Fatalf(
			"response=%d body=%s",
			response.Code,
			response.Body,
		)
	}
	upstreamRequest := <-captured
	if upstreamRequest.Path != "/v1/responses/compact" ||
		string(upstreamRequest.Body["model"]) !=
			`"upstream-model"` ||
		string(upstreamRequest.Body["instructions"]) !=
			`"Preserve durable facts."` ||
		string(upstreamRequest.Body["future_request_field"]) !=
			`{"preserved":true}` ||
		rawNonNull(upstreamRequest.Body["store"]) {
		t.Fatalf("upstream request = %#v", upstreamRequest)
	}
	var compactResponse map[string]json.RawMessage
	if json.Unmarshal(
		response.Body.Bytes(),
		&compactResponse,
	) != nil ||
		string(compactResponse["object"]) !=
			`"response.compaction"` ||
		string(compactResponse["future_response_field"]) !=
			`{"preserved":true}` ||
		!strings.Contains(
			string(compactResponse["output"]),
			`"encrypted_content":"opaque-state"`,
		) {
		t.Fatalf("compact response = %s", response.Body)
	}
	if response.Header().Get("ETag") != "" {
		t.Fatalf("response headers = %#v", response.Header())
	}

	records := usageRecorder.snapshot()
	if len(records) != 2 ||
		records[0].Attempt == nil ||
		records[1].Request == nil ||
		records[1].Request.Operation !=
			openAIOperationResponsesCompact.String() ||
		records[1].Request.Outcome != ledger.OutcomeSuccess {
		t.Fatalf("ledger records = %#v", records)
	}
	usage := records[1].Request.Usage
	assertTokenValue(t, "compact input", usage.InputTokens, 139)
	assertTokenValue(t, "compact output", usage.OutputTokens, 42)
	assertTokenValue(t, "compact total", usage.TotalTokens, 181)
	assertTokenValue(t, "compact cached", usage.CachedInputTokens, 7)
	assertTokenValue(
		t,
		"compact cache write",
		usage.CacheCreationTokens,
		3,
	)
	assertTokenValue(t, "compact reasoning", usage.ReasoningTokens, 11)

	traces := traceRecorder.snapshot()
	if len(traces) != 1 ||
		traces[0].Operation !=
			openAIOperationResponsesCompact.String() ||
		traces[0].Request.Body != rawRequest ||
		traces[0].Response.Body != response.Body.String() ||
		traces[0].Outcome != string(ledger.OutcomeSuccess) {
		t.Fatalf("saved traces = %#v", traces)
	}

	for _, itemID := range []string{"msg_retained_1", "cmp_1"} {
		affinity, found, resolveErr := state.ResolveResource(
			context.Background(),
			responsesstate.ResourceKey{
				Scope:      responsesCallerScope(identity.Identity{}),
				Kind:       responsesstate.ResourceItem,
				ResourceID: itemID,
			},
		)
		if resolveErr != nil ||
			!found ||
			affinity.Deployment != "deployment" {
			t.Fatalf(
				"item %s affinity = %#v, %v, %v",
				itemID,
				affinity,
				found,
				resolveErr,
			)
		}
	}
}

func TestResponsesCompactRequiresExplicitCapability(t *testing.T) {
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
		responsesDocument(upstream.URL+"/v1"),
		DataOptions{},
	)
	if err != nil {
		t.Fatalf("NewDataHandler() error = %v", err)
	}
	response := serveResponsesCompact(
		handler,
		`{"model":"public","input":"hello"}`,
	)
	var failure openAIErrorEnvelope
	if response.Code != http.StatusBadRequest ||
		json.Unmarshal(response.Body.Bytes(), &failure) != nil ||
		failure.Error.Code != "unsupported_feature" ||
		!strings.Contains(
			failure.Error.Message,
			string(config.CapabilityResponsesCompact),
		) {
		t.Fatalf(
			"response=%d body=%s failure=%#v",
			response.Code,
			response.Body,
			failure,
		)
	}
	if calls.Load() != 0 {
		t.Fatalf("upstream calls = %d, want 0", calls.Load())
	}
}

func TestResponsesCompactRejectsInvalidOrStatefulRequestsBeforeTraffic(
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
		responsesCompactDocument(upstream.URL+"/v1"),
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
			name: "missing input",
			body: `{"model":"public"}`,
			code: "invalid_request_body",
		},
		{
			name: "stream",
			body: `{
				"model":"public",
				"input":"hello",
				"stream":false
			}`,
			code: "unsupported_feature",
		},
		{
			name: "stored",
			body: `{
				"model":"public",
				"input":"hello",
				"store":false
			}`,
			code: "unsupported_feature",
		},
		{
			name: "background",
			body: `{
				"model":"public",
				"input":"hello",
				"background":true
			}`,
			code: "unsupported_feature",
		},
		{
			name: "conversation",
			body: `{
				"model":"public",
				"input":"hello",
				"conversation":"conv_1"
			}`,
			code: "unsupported_feature",
		},
		{
			name: "prompt resource",
			body: `{
				"model":"public",
				"input":"hello",
				"prompt":{"id":"pmpt_1"}
			}`,
			code: "unsupported_feature",
		},
		{
			name: "tools",
			body: `{
				"model":"public",
				"input":"hello",
				"tools":[{"type":"function","name":"lookup"}]
			}`,
			code: "unsupported_feature",
		},
		{
			name: "instructions type",
			body: `{
				"model":"public",
				"input":"hello",
				"instructions":[{"role":"developer","content":"x"}]
			}`,
			code: "invalid_request_body",
		},
	} {
		test := test
		t.Run(test.name, func(t *testing.T) {
			response := serveResponsesCompact(handler, test.body)
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

func TestResponsesCompactRejectsUnknownPreviousResponseBeforeTraffic(
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
	document := responsesCompactDocument(upstream.URL + "/v1")
	document.Deployments[0].Capabilities = append(
		document.Deployments[0].Capabilities,
		config.CapabilityStoredCompletion,
	)
	handler, err := NewDataHandler(document, DataOptions{})
	if err != nil {
		t.Fatalf("NewDataHandler() error = %v", err)
	}
	response := serveResponsesCompact(
		handler,
		`{
			"model":"public",
			"input":"hello",
			"previous_response_id":"resp_unknown"
		}`,
	)
	var failure openAIErrorEnvelope
	if response.Code != http.StatusConflict ||
		json.Unmarshal(response.Body.Bytes(), &failure) != nil ||
		failure.Error.Code != "state_affinity_not_found" {
		t.Fatalf(
			"response=%d body=%s failure=%#v",
			response.Code,
			response.Body,
			failure,
		)
	}
	if calls.Load() != 0 {
		t.Fatalf("upstream calls = %d, want 0", calls.Load())
	}
}

func TestResponsesCompactRejectsInvalidUpstreamResponses(
	t *testing.T,
) {
	t.Parallel()

	for _, test := range []struct {
		name string
		body string
	}{
		{
			name: "wrong object",
			body: `{
				"id":"resp_1",
				"object":"response",
				"created_at":1,
				"output":[{
					"id":"cmp_1",
					"type":"compaction",
					"encrypted_content":"secret"
				}],
				"usage":{
					"input_tokens":1,
					"output_tokens":1,
					"total_tokens":2
				}
			}`,
		},
		{
			name: "missing compaction item",
			body: `{
				"id":"resp_1",
				"object":"response.compaction",
				"created_at":1,
				"output":[{
					"id":"msg_1",
					"type":"message"
				}],
				"usage":{
					"input_tokens":1,
					"output_tokens":1,
					"total_tokens":2
				}
			}`,
		},
		{
			name: "missing encrypted content",
			body: `{
				"id":"resp_1",
				"object":"response.compaction",
				"created_at":1,
				"output":[{
					"id":"cmp_1",
					"type":"compaction"
				}],
				"usage":{
					"input_tokens":1,
					"output_tokens":1,
					"total_tokens":2
				}
			}`,
		},
		{
			name: "multiple compaction items",
			body: `{
				"id":"resp_1",
				"object":"response.compaction",
				"created_at":1,
				"output":[
					{
						"id":"cmp_1",
						"type":"compaction",
						"encrypted_content":"secret-one"
					},
					{
						"id":"cmp_2",
						"type":"compaction",
						"encrypted_content":"secret-two"
					}
				],
				"usage":{
					"input_tokens":1,
					"output_tokens":1,
					"total_tokens":2
				}
			}`,
		},
		{
			name: "missing usage",
			body: `{
				"id":"resp_1",
				"object":"response.compaction",
				"created_at":1,
				"output":[{
					"id":"cmp_1",
					"type":"compaction",
					"encrypted_content":"secret"
				}]
			}`,
		},
	} {
		test := test
		t.Run(test.name, func(t *testing.T) {
			upstream := httptest.NewServer(http.HandlerFunc(func(
				w http.ResponseWriter,
				_ *http.Request,
			) {
				w.Header().Set(
					"Content-Type",
					"application/json",
				)
				_, _ = io.WriteString(w, test.body)
			}))
			defer upstream.Close()
			handler, err := NewDataHandler(
				responsesCompactDocument(
					upstream.URL+"/v1",
				),
				DataOptions{},
			)
			if err != nil {
				t.Fatalf("NewDataHandler() error = %v", err)
			}
			response := serveResponsesCompact(
				handler,
				`{"model":"public","input":"hello"}`,
			)
			var failure openAIErrorEnvelope
			if response.Code != http.StatusBadGateway ||
				json.Unmarshal(
					response.Body.Bytes(),
					&failure,
				) != nil ||
				failure.Error.Code !=
					"upstream_response_error" ||
				strings.Contains(
					response.Body.String(),
					"secret",
				) {
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

func TestResponsesCompactStatelessRequestRetriesAnotherTarget(
	t *testing.T,
) {
	t.Parallel()

	var firstCalls atomic.Int64
	first := httptest.NewServer(http.HandlerFunc(func(
		w http.ResponseWriter,
		request *http.Request,
	) {
		firstCalls.Add(1)
		if request.URL.Path != "/v1/responses/compact" {
			t.Errorf("first path = %q", request.URL.Path)
		}
		writeJSON(
			w,
			http.StatusServiceUnavailable,
			map[string]string{"error": "temporarily unavailable"},
		)
	}))
	defer first.Close()

	var secondCalls atomic.Int64
	second := httptest.NewServer(http.HandlerFunc(func(
		w http.ResponseWriter,
		request *http.Request,
	) {
		secondCalls.Add(1)
		body := readJSONEnvelope(t, request.Body)
		if request.URL.Path != "/v1/responses/compact" ||
			string(body["model"]) != `"upstream-second"` {
			t.Errorf(
				"second path=%q body=%#v",
				request.URL.Path,
				body,
			)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{
			"id":"resp_compact_retry",
			"object":"response.compaction",
			"created_at":1770000000,
			"output":[{
				"id":"cmp_retry",
				"type":"compaction",
				"encrypted_content":"retried-state"
			}],
			"usage":{
				"input_tokens":10,
				"output_tokens":2,
				"total_tokens":12
			}
		}`)
	}))
	defer second.Close()

	document := twoTargetDocument(
		first.URL+"/v1",
		second.URL+"/v1",
	)
	for index := range document.Deployments {
		document.Deployments[index].Capabilities = append(
			document.Deployments[index].Capabilities,
			config.CapabilityResponses,
			config.CapabilityResponsesCompact,
		)
	}
	sleeper := &recordingRetrySleeper{}
	handler, err := NewDataHandler(document, DataOptions{
		RoutingPicker: zeroPicker{},
		RetrySleeper:  sleeper,
	})
	if err != nil {
		t.Fatalf("NewDataHandler() error = %v", err)
	}
	response := serveResponsesCompact(
		handler,
		`{"model":"public","input":"long context"}`,
	)
	if response.Code != http.StatusOK {
		t.Fatalf(
			"response=%d body=%s",
			response.Code,
			response.Body,
		)
	}
	if firstCalls.Load() != 1 || secondCalls.Load() != 1 {
		t.Fatalf(
			"upstream calls = (%d, %d), want (1, 1)",
			firstCalls.Load(),
			secondCalls.Load(),
		)
	}
	if got := response.Header().Get(
		"X-SparkRoute-Attempt-Count",
	); got != "2" {
		t.Fatalf("attempt count = %q, want 2", got)
	}
	if got := len(sleeper.snapshot()); got != 1 {
		t.Fatalf("retry sleeps = %d, want 1", got)
	}
}

func TestResponsesCompactItemPinsNextResponsesRequest(t *testing.T) {
	t.Parallel()

	var firstCalls, secondCalls atomic.Int64
	first := httptest.NewServer(http.HandlerFunc(func(
		w http.ResponseWriter,
		_ *http.Request,
	) {
		firstCalls.Add(1)
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer first.Close()
	second := httptest.NewServer(http.HandlerFunc(func(
		w http.ResponseWriter,
		request *http.Request,
	) {
		call := secondCalls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		switch request.URL.Path {
		case "/v1/responses/compact":
			if call != 1 {
				t.Errorf("compact call = %d", call)
			}
			_, _ = io.WriteString(w, `{
				"id":"resp_compact_bound",
				"object":"response.compaction",
				"created_at":1770000000,
				"output":[{
					"id":"cmp_bound",
					"type":"compaction",
					"encrypted_content":"target-bound-state"
				}],
				"usage":{
					"input_tokens":10,
					"output_tokens":2,
					"total_tokens":12
				}
			}`)
		case "/v1/responses":
			body := readJSONEnvelope(t, request.Body)
			if call != 2 ||
				!strings.Contains(
					string(body["input"]),
					`"id":"cmp_bound"`,
				) ||
				!strings.Contains(
					string(body["input"]),
					`"encrypted_content":"target-bound-state"`,
				) {
				t.Errorf(
					"follow-up call=%d body=%#v",
					call,
					body,
				)
			}
			_, _ = io.WriteString(w, `{
				"id":"resp_after_compaction",
				"object":"response",
				"model":"second-upstream",
				"status":"completed",
				"output":[],
				"usage":{
					"input_tokens":3,
					"output_tokens":1,
					"total_tokens":4
				}
			}`)
		default:
			t.Errorf("unexpected path = %q", request.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer second.Close()

	document := twoTargetDocument(
		first.URL+"/v1",
		second.URL+"/v1",
	)
	for index := range document.Deployments {
		document.Deployments[index].Capabilities = append(
			document.Deployments[index].Capabilities,
			config.CapabilityResponses,
			config.CapabilityResponsesCompact,
			config.CapabilityStoredCompletion,
		)
	}
	state := responsesstate.NewMemoryStore(
		responsesstate.MemoryOptions{},
	)
	handler, err := NewDataHandler(document, DataOptions{
		RoutingPicker: &responseSequencePicker{
			values: []int64{150, 0},
		},
		ResponsesState: state,
	})
	if err != nil {
		t.Fatalf("NewDataHandler() error = %v", err)
	}
	compact := serveResponsesCompact(
		handler,
		`{"model":"public","input":"long context"}`,
	)
	if compact.Code != http.StatusOK {
		t.Fatalf(
			"compact=%d body=%s",
			compact.Code,
			compact.Body,
		)
	}
	var compactBody map[string]json.RawMessage
	if json.Unmarshal(
		compact.Body.Bytes(),
		&compactBody,
	) != nil {
		t.Fatalf("compact response = %s", compact.Body)
	}
	followUpRequest, err := json.Marshal(map[string]any{
		"model": "public",
		"store": false,
		"input": json.RawMessage(
			compactBody["output"],
		),
	})
	if err != nil {
		t.Fatalf("json.Marshal() error = %v", err)
	}
	request := httptest.NewRequest(
		http.MethodPost,
		"/v1/responses",
		strings.NewReader(string(followUpRequest)),
	)
	request.Header.Set("Content-Type", "application/json")
	followUp := httptest.NewRecorder()
	handler.ServeHTTP(followUp, request)
	if followUp.Code != http.StatusOK {
		t.Fatalf(
			"follow-up=%d body=%s",
			followUp.Code,
			followUp.Body,
		)
	}
	if firstCalls.Load() != 0 ||
		secondCalls.Load() != 2 {
		t.Fatalf(
			"upstream calls = (%d, %d), want (0, 2)",
			firstCalls.Load(),
			secondCalls.Load(),
		)
	}
}

func TestDetectResponsesCompactCapabilities(t *testing.T) {
	t.Parallel()

	var envelope map[string]json.RawMessage
	if err := json.Unmarshal([]byte(`{
		"input":[{
			"id":"cmp_1",
			"type":"compaction",
			"encrypted_content":"opaque"
		}],
		"prompt_cache_key":"workflow",
		"service_tier":"priority"
	}`), &envelope); err != nil {
		t.Fatalf("decode fixture: %v", err)
	}
	got, err := detectResponsesCompactCapabilities(
		envelope,
		false,
	)
	if err != nil {
		t.Fatalf(
			"detectResponsesCompactCapabilities() error = %v",
			err,
		)
	}
	want := []config.Capability{
		config.CapabilityPromptCaching,
		config.CapabilityResponses,
		config.CapabilityResponsesCompact,
		config.CapabilityServiceTier,
		config.CapabilityStoredCompletion,
	}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("capabilities = %v, want %v", got, want)
	}
}

func TestResponsesCompactGuardrailsUseCanonicalProtocolPayloads(
	t *testing.T,
) {
	t.Parallel()

	captured := make(chan map[string]json.RawMessage, 1)
	primary := httptest.NewServer(http.HandlerFunc(func(
		w http.ResponseWriter,
		request *http.Request,
	) {
		captured <- readJSONEnvelope(t, request.Body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{
			"id":"resp_guarded_compact",
			"object":"response.compaction",
			"created_at":1770000000,
			"output":[{
				"id":"cmp_guarded",
				"type":"compaction",
				"encrypted_content":"canonical-output"
			}],
			"usage":{
				"input_tokens":3,
				"output_tokens":1,
				"total_tokens":4
			}
		}`)
	}))
	defer primary.Close()

	var guardCalls atomic.Int64
	guard := httptest.NewServer(http.HandlerFunc(func(
		w http.ResponseWriter,
		request *http.Request,
	) {
		guardCalls.Add(1)
		input := readGuardrailInput(t, request)
		if input.Protocol != "openai" ||
			input.Operation !=
				openAIOperationResponsesCompact {
			t.Errorf("guardrail input = %#v", input)
		}
		switch input.Phase {
		case guardrailPhasePre:
			writeGuardrailVerdict(
				w,
				"upstream-guard",
				`{"action":"replace","replacement":{`+
					`"model":"public",`+
					`"input":"redacted context"}}`,
			)
		case guardrailPhasePost:
			if !strings.Contains(
				string(input.Response),
				`"encrypted_content":"canonical-output"`,
			) {
				t.Errorf(
					"post guardrail response = %s",
					input.Response,
				)
			}
			writeGuardrailVerdict(
				w,
				"upstream-guard",
				`{"action":"allow"}`,
			)
		default:
			t.Errorf(
				"unexpected guardrail phase = %q",
				input.Phase,
			)
			writeGuardrailVerdict(
				w,
				"upstream-guard",
				`{"action":"block"}`,
			)
		}
	}))
	defer guard.Close()

	document := guardrailDocument(
		primary.URL+"/v1",
		guard.URL+"/v1",
	)
	document.Deployments[0].Capabilities = append(
		document.Deployments[0].Capabilities,
		config.CapabilityResponses,
		config.CapabilityResponsesCompact,
	)
	preGuardrail := config.Guardrail{
		Name:             "compact-safety",
		Model:            "safety",
		AllowReplacement: true,
	}
	document.VirtualModels[0].Guardrails.Pre =
		[]config.Guardrail{preGuardrail}
	document.VirtualModels[0].Guardrails.Post =
		[]config.Guardrail{{
			Name:             "compact-output-safety",
			Model:            "safety",
			AllowReplacement: true,
		}}
	handler, err := NewDataHandler(document, DataOptions{})
	if err != nil {
		t.Fatalf("NewDataHandler() error = %v", err)
	}
	response := serveResponsesCompact(
		handler,
		`{"model":"public","input":"sensitive context"}`,
	)
	if response.Code != http.StatusOK ||
		!strings.Contains(
			response.Body.String(),
			`"encrypted_content":"canonical-output"`,
		) {
		t.Fatalf(
			"response=%d body=%s",
			response.Code,
			response.Body,
		)
	}
	upstreamRequest := <-captured
	if string(upstreamRequest["input"]) !=
		`"redacted context"` {
		t.Fatalf(
			"upstream request = %#v",
			upstreamRequest,
		)
	}
	if guardCalls.Load() != 2 {
		t.Fatalf(
			"guardrail calls = %d, want 2",
			guardCalls.Load(),
		)
	}
}

func TestResponsesCompactPreGuardrailPreservesProviderState(
	t *testing.T,
) {
	t.Parallel()

	original := json.RawMessage(`{
		"model":"public",
		"previous_response_id":"resp_prior",
		"input":[
			{
				"id":"cmp_prior",
				"type":"compaction",
				"encrypted_content":"opaque-state"
			},
			{
				"type":"input_file",
				"file_id":"file_prior"
			},
			{
				"type":"message",
				"role":"user",
				"content":"sensitive"
			}
		]
	}`)
	for _, test := range []struct {
		name        string
		replacement json.RawMessage
		wantError   bool
	}{
		{
			name: "redacts ordinary input",
			replacement: json.RawMessage(`{
				"model":"public",
				"previous_response_id":"resp_prior",
				"input":[
					{
						"id":"cmp_prior",
						"type":"compaction",
						"encrypted_content":"opaque-state"
					},
					{
						"type":"input_file",
						"file_id":"file_prior"
					},
					{
						"type":"message",
						"role":"user",
						"content":"redacted"
					}
				]
			}`),
		},
		{
			name: "changes prior response",
			replacement: json.RawMessage(`{
				"model":"public",
				"previous_response_id":"resp_other",
				"input":[
					{
						"id":"cmp_prior",
						"type":"compaction",
						"encrypted_content":"opaque-state"
					},
					{
						"type":"input_file",
						"file_id":"file_prior"
					}
				]
			}`),
			wantError: true,
		},
		{
			name: "changes compact item",
			replacement: json.RawMessage(`{
				"model":"public",
				"previous_response_id":"resp_prior",
				"input":[
					{
						"id":"cmp_other",
						"type":"compaction",
						"encrypted_content":"other-state"
					},
					{
						"type":"input_file",
						"file_id":"file_prior"
					}
				]
			}`),
			wantError: true,
		},
		{
			name: "changes encrypted compact state",
			replacement: json.RawMessage(`{
				"model":"public",
				"previous_response_id":"resp_prior",
				"input":[
					{
						"id":"cmp_prior",
						"type":"compaction",
						"encrypted_content":"other-state"
					},
					{
						"type":"input_file",
						"file_id":"file_prior"
					}
				]
			}`),
			wantError: true,
		},
		{
			name: "changes file",
			replacement: json.RawMessage(`{
				"model":"public",
				"previous_response_id":"resp_prior",
				"input":[
					{
						"id":"cmp_prior",
						"type":"compaction",
						"encrypted_content":"opaque-state"
					},
					{
						"type":"input_file",
						"file_id":"file_other"
					}
				]
			}`),
			wantError: true,
		},
	} {
		test := test
		t.Run(test.name, func(t *testing.T) {
			_, err := validatePreGuardrailReplacement(
				openAIOperationResponsesCompact,
				original,
				test.replacement,
				"public",
				false,
			)
			if test.wantError &&
				(err == nil ||
					!strings.Contains(
						err.Error(),
						"provider-state controls",
					)) {
				t.Fatalf(
					"validatePreGuardrailReplacement() error = %v",
					err,
				)
			}
			if !test.wantError && err != nil {
				t.Fatalf(
					"validatePreGuardrailReplacement() error = %v",
					err,
				)
			}
		})
	}
}

func TestResponsesCompactPostGuardrailCannotReplaceCanonicalOutput(
	t *testing.T,
) {
	t.Parallel()

	_, err := mergePostGuardrailReplacement(
		openAIOperationResponsesCompact,
		[]byte(`{
			"id":"resp_1",
			"object":"response.compaction",
			"output":[]
		}`),
		json.RawMessage(`{"output":[]}`),
	)
	if err == nil ||
		!strings.Contains(err.Error(), "canonical output") {
		t.Fatalf(
			"mergePostGuardrailReplacement() error = %v",
			err,
		)
	}
}

func responsesCompactDocument(baseURL string) config.Document {
	document := responsesDocument(baseURL)
	document.Deployments[0].Capabilities = append(
		document.Deployments[0].Capabilities,
		config.CapabilityResponsesCompact,
	)
	return document
}

func serveResponsesCompact(
	handler http.Handler,
	body string,
) *httptest.ResponseRecorder {
	request := httptest.NewRequest(
		http.MethodPost,
		"/v1/responses/compact",
		strings.NewReader(body),
	)
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	return response
}
