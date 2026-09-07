package gateway

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sparksq/sparkroute/pkg/config"
	"github.com/sparksq/sparkroute/pkg/credentials"
	"github.com/sparksq/sparkroute/pkg/identity"
	"github.com/sparksq/sparkroute/pkg/ledger"
	"github.com/sparksq/sparkroute/pkg/routing"
)

type fakeCredentialSource map[credentials.Ref]string

func (s fakeCredentialSource) Resolve(
	_ context.Context,
	ref credentials.Ref,
) (credentials.Material, error) {
	value, exists := s[ref]
	if !exists {
		return credentials.Material{}, fmt.Errorf("missing credential %s", ref)
	}
	return credentials.Material{Value: []byte(value)}, nil
}

type zeroPicker struct{}

func (zeroPicker) Pick(int64) (int64, error) {
	return 0, nil
}

var _ routing.WeightedPicker = zeroPicker{}

type recordingRetrySleeper struct {
	mu     sync.Mutex
	delays []time.Duration
}

func (s *recordingRetrySleeper) Sleep(
	ctx context.Context,
	delay time.Duration,
) error {
	s.mu.Lock()
	s.delays = append(s.delays, delay)
	s.mu.Unlock()
	select {
	case <-ctx.Done():
		return ctx.Err()
	default:
		return nil
	}
}

func (s *recordingRetrySleeper) snapshot() []time.Duration {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]time.Duration(nil), s.delays...)
}

type blockingFirstRetrySleeper struct {
	calls   atomic.Int64
	entered chan struct{}
	release chan struct{}
}

func (s *blockingFirstRetrySleeper) Sleep(
	ctx context.Context,
	_ time.Duration,
) error {
	if s.calls.Add(1) != 1 {
		return nil
	}
	close(s.entered)
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-s.release:
		return nil
	}
}

type collectingRecorder struct {
	mu      sync.Mutex
	records []ledger.Record
}

func (r *collectingRecorder) Record(record ledger.Record) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.records = append(r.records, record)
}

func (r *collectingRecorder) snapshot() []ledger.Record {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]ledger.Record(nil), r.records...)
}

type capturedUpstreamRequest struct {
	Path   string
	Header http.Header
	Body   map[string]json.RawMessage
}

func TestChatCompletionsPassthrough(t *testing.T) {
	t.Parallel()

	captured := make(chan capturedUpstreamRequest, 1)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		defer request.Body.Close()
		raw, err := io.ReadAll(request.Body)
		if err != nil {
			t.Errorf("ReadAll() error = %v", err)
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		var body map[string]json.RawMessage
		if err := json.Unmarshal(raw, &body); err != nil {
			t.Errorf("Unmarshal() error = %v", err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		captured <- capturedUpstreamRequest{
			Path:   request.URL.Path,
			Header: request.Header.Clone(),
			Body:   body,
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("X-Upstream-Request-Id", "upstream-1")
		w.WriteHeader(http.StatusCreated)
		_, _ = io.WriteString(w, `{"id":"chatcmpl-1","model":"upstream-model","choices":[]}`)
	}))
	defer upstream.Close()

	document := proxyDocument(upstream.URL + "/v1")
	document.Providers[0].Auth = config.ProviderAuth{
		Type:       config.AuthBearer,
		Credential: "env://UPSTREAM_TOKEN",
	}
	document.Providers[0].DefaultHeaders = map[string]config.HeaderValue{
		"X-Profile": {Value: "provider"},
	}
	document.Providers[0].ExtraBody = map[string]json.RawMessage{
		"temperature":  json.RawMessage(`1`),
		"service_tier": json.RawMessage(`"auto"`),
	}
	document.Deployments[0].UpstreamHeaders = map[string]config.HeaderValue{
		"x-profile":          {Value: "deployment"},
		"X-Deployment-Token": {ValueFrom: "env://DEPLOYMENT_TOKEN"},
	}
	document.Deployments[0].ExtraBody = map[string]json.RawMessage{
		"service_tier": json.RawMessage(`"priority"`),
		"top_k":        json.RawMessage(`40`),
	}
	handler, err := NewDataHandler(document, DataOptions{
		Credentials: fakeCredentialSource{
			"env://UPSTREAM_TOKEN":   "upstream-secret",
			"env://DEPLOYMENT_TOKEN": "deployment-secret",
		},
		RequestIDHeader: "X-Test-Request-Id",
	})
	if err != nil {
		t.Fatalf("NewDataHandler() error = %v", err)
	}

	body := `{
		"model":"default",
		"messages":[{"role":"user","content":"hello"}],
		"seed":42,
		"temperature":0,
		"stream":false
	}`
	request := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Authorization", "Bearer caller-secret")
	request.Header.Set("Traceparent", "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)

	if response.Code != http.StatusCreated {
		t.Fatalf("status = %d, want %d; body=%s", response.Code, http.StatusCreated, response.Body)
	}
	if response.Header().Get("X-Test-Request-Id") == "" {
		t.Fatal("missing gateway request ID")
	}
	if response.Header().Get("X-Upstream-Request-Id") != "upstream-1" {
		t.Fatalf("upstream request ID = %q", response.Header().Get("X-Upstream-Request-Id"))
	}
	if got := response.Body.String(); !strings.Contains(got, `"id":"chatcmpl-1"`) {
		t.Fatalf("response body = %s", got)
	}
	var downstreamBody map[string]json.RawMessage
	if err := json.Unmarshal(response.Body.Bytes(), &downstreamBody); err != nil {
		t.Fatalf("decode downstream body: %v", err)
	}
	var downstreamModel string
	if err := json.Unmarshal(downstreamBody["model"], &downstreamModel); err != nil {
		t.Fatalf("decode downstream model: %v", err)
	}
	if downstreamModel != "public" {
		t.Fatalf("downstream model = %q, want virtual model public", downstreamModel)
	}

	upstreamRequest := <-captured
	if upstreamRequest.Path != "/v1/chat/completions" {
		t.Fatalf("upstream path = %q", upstreamRequest.Path)
	}
	var model string
	if err := json.Unmarshal(upstreamRequest.Body["model"], &model); err != nil {
		t.Fatalf("decode upstream model: %v", err)
	}
	if model != "upstream-model" {
		t.Fatalf("upstream model = %q", model)
	}
	if string(upstreamRequest.Body["seed"]) != "42" {
		t.Fatalf("unknown field seed = %s", upstreamRequest.Body["seed"])
	}
	if string(upstreamRequest.Body["temperature"]) != "0" ||
		string(upstreamRequest.Body["service_tier"]) != `"priority"` ||
		string(upstreamRequest.Body["top_k"]) != "40" {
		t.Fatalf("merged upstream defaults = %#v", upstreamRequest.Body)
	}
	if got := upstreamRequest.Header.Get("Authorization"); got != "Bearer upstream-secret" {
		t.Fatalf("Authorization = %q", got)
	}
	if got := upstreamRequest.Header.Get("X-Profile"); got != "deployment" {
		t.Fatalf("X-Profile = %q", got)
	}
	if got := upstreamRequest.Header.Get("X-Deployment-Token"); got != "deployment-secret" {
		t.Fatalf("X-Deployment-Token = %q", got)
	}
	if got := upstreamRequest.Header.Get("Traceparent"); got != request.Header.Get("Traceparent") {
		t.Fatalf("Traceparent = %q", got)
	}
}

func TestChatCompletionsStreamsBeforeUpstreamCompletes(t *testing.T) {
	t.Parallel()

	release := make(chan struct{})
	defer func() {
		select {
		case <-release:
		default:
			close(release)
		}
	}()
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, `data: {"model":"upstream-model","part":"first"}`+"\n\n")
		w.(http.Flusher).Flush()
		<-release
		_, _ = io.WriteString(w, `data: {"model":"upstream-model","part":"second"}`+"\n\n")
		w.(http.Flusher).Flush()
	}))
	defer upstream.Close()

	handler, err := NewDataHandler(proxyDocument(upstream.URL+"/v1"), DataOptions{})
	if err != nil {
		t.Fatalf("NewDataHandler() error = %v", err)
	}
	gatewayServer := httptest.NewServer(handler)
	defer gatewayServer.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	request, err := http.NewRequestWithContext(
		ctx,
		http.MethodPost,
		gatewayServer.URL+"/v1/chat/completions",
		strings.NewReader(`{"model":"public","messages":[],"stream":true}`),
	)
	if err != nil {
		t.Fatalf("NewRequestWithContext() error = %v", err)
	}
	request.Header.Set("Content-Type", "application/json")
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatalf("Do() error = %v", err)
	}
	defer response.Body.Close()

	reader := bufio.NewReader(response.Body)
	first, err := reader.ReadString('\n')
	if err != nil {
		t.Fatalf("read first streamed line: %v", err)
	}
	if !strings.Contains(first, `"model":"public"`) || !strings.Contains(first, `"part":"first"`) {
		t.Fatalf("first streamed line = %q", first)
	}
	close(release)
	remaining, err := io.ReadAll(reader)
	if err != nil {
		t.Fatalf("read remaining stream: %v", err)
	}
	if !bytes.Contains(remaining, []byte(`"model":"public"`)) ||
		!bytes.Contains(remaining, []byte(`"part":"second"`)) {
		t.Fatalf("remaining stream = %q", remaining)
	}
}

func TestChatCompletionsTerminatesIdleStream(t *testing.T) {
	t.Parallel()

	upstream := httptest.NewServer(http.HandlerFunc(func(
		w http.ResponseWriter,
		request *http.Request,
	) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(
			w,
			`data: {"model":"upstream-model","part":"first"}`+"\n\n",
		)
		w.(http.Flusher).Flush()
		<-request.Context().Done()
	}))
	defer upstream.Close()

	document := proxyDocument(upstream.URL + "/v1")
	document.VirtualModels[0].Limits = config.ModelLimits{
		MaxAttempts:       1,
		OverallTimeout:    config.Duration(2 * time.Second),
		PerTryTimeout:     config.Duration(2 * time.Second),
		StreamIdleTimeout: config.Duration(40 * time.Millisecond),
	}
	recorder := &collectingRecorder{}
	handler, err := NewDataHandler(document, DataOptions{Ledger: recorder})
	if err != nil {
		t.Fatalf("NewDataHandler() error = %v", err)
	}
	request := httptest.NewRequest(
		http.MethodPost,
		"/v1/chat/completions",
		strings.NewReader(`{"model":"public","messages":[],"stream":true}`),
	)
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	started := time.Now()
	handler.ServeHTTP(response, request)
	if elapsed := time.Since(started); elapsed >= time.Second {
		t.Fatalf("stream elapsed = %s, want idle timeout", elapsed)
	}
	if response.Code != http.StatusOK ||
		!strings.Contains(response.Body.String(), `"part":"first"`) {
		t.Fatalf("response = status:%d body:%s", response.Code, response.Body)
	}
	records := recorder.snapshot()
	if len(records) != 2 ||
		records[0].Attempt == nil ||
		records[1].Request == nil {
		t.Fatalf("records = %#v", records)
	}
	if records[0].Attempt.Outcome != ledger.OutcomeStreamError ||
		records[0].Attempt.FailureClass != "stream_idle_timeout" ||
		records[1].Request.Outcome != ledger.OutcomeStreamError ||
		records[1].Request.FailureClass != "stream_idle_timeout" {
		t.Fatalf("idle timeout records = %#v", records)
	}
}

func TestChatCompletionsResponseModelModes(t *testing.T) {
	t.Parallel()

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{
			"id":      "chatcmpl-mode",
			"model":   "upstream-model",
			"choices": []any{},
		})
	}))
	defer upstream.Close()

	tests := []struct {
		name string
		mode config.ResponseModelMode
		want string
	}{
		{name: "default virtual", want: "public"},
		{name: "virtual", mode: config.ResponseModelVirtual, want: "public"},
		{name: "requested alias", mode: config.ResponseModelRequested, want: "default"},
		{name: "upstream", mode: config.ResponseModelUpstream, want: "upstream-model"},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			document := proxyDocument(upstream.URL + "/v1")
			document.VirtualModels[0].ResponseModel = test.mode
			handler, err := NewDataHandler(document, DataOptions{})
			if err != nil {
				t.Fatalf("NewDataHandler() error = %v", err)
			}
			request := httptest.NewRequest(
				http.MethodPost,
				"/v1/chat/completions",
				strings.NewReader(`{"model":"default","messages":[]}`),
			)
			request.Header.Set("Content-Type", "application/json")
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, request)

			var body struct {
				Model string `json:"model"`
			}
			if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
				t.Fatalf("decode response: %v; body=%s", err, response.Body)
			}
			if body.Model != test.want {
				t.Fatalf(
					"status = %d, model = %q, want %q; body=%s",
					response.Code,
					body.Model,
					test.want,
					response.Body,
				)
			}
		})
	}
}

func TestChatCompletionsRecordsNormalizedUsage(t *testing.T) {
	t.Parallel()

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{
			"id":      "chatcmpl-usage",
			"model":   "upstream-model",
			"choices": []any{},
			"usage": map[string]any{
				"prompt_tokens":     21,
				"completion_tokens": 8,
				"total_tokens":      29,
				"prompt_tokens_details": map[string]any{
					"cached_tokens": 5,
				},
			},
		})
	}))
	defer upstream.Close()

	recorder := &collectingRecorder{}
	handler, err := NewDataHandler(proxyDocument(upstream.URL+"/v1"), DataOptions{
		Ledger:         recorder,
		ConfigRevision: "revision-1",
	})
	if err != nil {
		t.Fatalf("NewDataHandler() error = %v", err)
	}
	request := httptest.NewRequest(
		http.MethodPost,
		"/v1/chat/completions",
		strings.NewReader(`{"model":"default","messages":[]}`),
	)
	request.Header.Set("Content-Type", "application/json")
	request = request.WithContext(identity.WithContext(
		request.Context(),
		identity.Identity{
			Principal: identity.Principal{
				ID:      "client-a",
				Type:    "machine",
				Tenant:  "tenant-a",
				Subject: "service-account:a",
				Roles:   []string{"inference"},
			},
			Attribution: identity.Attribution{
				identity.AttributeTenant:    "tenant-a",
				identity.AttributeWorkspace: "workspace-a",
			},
		},
	))
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)

	records := recorder.snapshot()
	if len(records) != 2 || records[0].Attempt == nil || records[1].Request == nil {
		t.Fatalf("records = %#v, want attempt then request", records)
	}
	attempt := records[0].Attempt
	requestRecord := records[1].Request
	if attempt.RequestID != requestRecord.RequestID ||
		requestRecord.RequestID != response.Header().Get("X-Request-Id") {
		t.Fatalf(
			"request IDs = attempt:%q request:%q response:%q",
			attempt.RequestID,
			requestRecord.RequestID,
			response.Header().Get("X-Request-Id"),
		)
	}
	if requestRecord.RequestedModel != "default" ||
		requestRecord.VirtualModel != "public" ||
		requestRecord.ResponsePresentedModel != "public" {
		t.Fatalf("request model identity = %#v", requestRecord)
	}
	if requestRecord.ConfigRevision != "revision-1" ||
		requestRecord.FinalProvider != "provider" ||
		requestRecord.FinalDeployment != "deployment" ||
		requestRecord.AttemptCount != 1 ||
		requestRecord.Outcome != ledger.OutcomeSuccess {
		t.Fatalf("request record = %#v", requestRecord)
	}
	if requestRecord.PrincipalID != "client-a" ||
		requestRecord.PrincipalType != "machine" ||
		requestRecord.PrincipalSubject != "service-account:a" ||
		requestRecord.TenantID != "tenant-a" ||
		len(requestRecord.PrincipalRoles) != 1 ||
		requestRecord.PrincipalRoles[0] != "inference" ||
		requestRecord.Attribution[identity.AttributeWorkspace] != "workspace-a" {
		t.Fatalf("request identity = %#v", requestRecord)
	}
	assertTokenValue(t, "request input", requestRecord.Usage.InputTokens, 21)
	assertTokenValue(t, "attempt output", attempt.Usage.OutputTokens, 8)
	assertTokenValue(t, "request cached", requestRecord.Usage.CachedInputTokens, 5)
}

func TestChatCompletionsRecordsStreamingUsage(t *testing.T) {
	t.Parallel()

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(
			w,
			`data: {"id":"chunk-1","model":"upstream-model","choices":[{"delta":{"content":"hi"}}]}`+"\n\n",
		)
		_, _ = io.WriteString(
			w,
			`data: {"id":"chunk-2","model":"upstream-model","choices":[],"usage":{"prompt_tokens":4,"completion_tokens":2,"total_tokens":6}}`+"\n\n",
		)
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
	}))
	defer upstream.Close()

	recorder := &collectingRecorder{}
	handler, err := NewDataHandler(
		proxyDocument(upstream.URL+"/v1"),
		DataOptions{Ledger: recorder},
	)
	if err != nil {
		t.Fatalf("NewDataHandler() error = %v", err)
	}
	request := httptest.NewRequest(
		http.MethodPost,
		"/v1/chat/completions",
		strings.NewReader(`{"model":"public","messages":[],"stream":true,"stream_options":{"include_usage":true}}`),
	)
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)

	records := recorder.snapshot()
	if len(records) != 2 {
		t.Fatalf("record count = %d, want 2", len(records))
	}
	assertTokenValue(t, "stream input", records[0].Attempt.Usage.InputTokens, 4)
	assertTokenValue(t, "stream output", records[1].Request.Usage.OutputTokens, 2)
	if !strings.Contains(response.Body.String(), `"model":"public"`) {
		t.Fatalf("stream response = %s", response.Body)
	}
}

func TestChatCompletionsUnknownModelDoesNotCallUpstream(t *testing.T) {
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
		"/v1/chat/completions",
		strings.NewReader(`{"model":"missing","messages":[]}`),
	)
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)

	if response.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want %d", response.Code, http.StatusNotFound)
	}
	if calls.Load() != 0 {
		t.Fatalf("upstream calls = %d, want 0", calls.Load())
	}
}

func TestChatCompletionsRequestLimit(t *testing.T) {
	t.Parallel()

	handler, err := NewDataHandler(proxyDocument("https://example.com/v1"), DataOptions{
		MaxRequestBytes: 32,
	})
	if err != nil {
		t.Fatalf("NewDataHandler() error = %v", err)
	}
	request := httptest.NewRequest(
		http.MethodPost,
		"/v1/chat/completions",
		strings.NewReader(`{"model":"public","padding":"this body is deliberately too large"}`),
	)
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)

	if response.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, want %d; body=%s", response.Code, http.StatusRequestEntityTooLarge, response.Body)
	}
}

func TestChatCompletionsRetriesAnotherWeightedTarget(t *testing.T) {
	t.Parallel()

	var firstCalls atomic.Int64
	first := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		firstCalls.Add(1)
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "temporarily unavailable"})
	}))
	defer first.Close()

	var secondCalls atomic.Int64
	second := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		secondCalls.Add(1)
		var body map[string]json.RawMessage
		if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
			t.Errorf("decode second request: %v", err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		var model string
		if err := json.Unmarshal(body["model"], &model); err != nil {
			t.Errorf("decode second model: %v", err)
		}
		if model != "upstream-second" {
			t.Errorf("second upstream model = %q", model)
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"id":      "chatcmpl-fallback",
			"model":   model,
			"choices": []any{},
		})
	}))
	defer second.Close()

	document := twoTargetDocument(first.URL+"/v1", second.URL+"/v1")
	recorder := &collectingRecorder{}
	handler, err := NewDataHandler(document, DataOptions{
		RoutingPicker: zeroPicker{},
		Ledger:        recorder,
	})
	if err != nil {
		t.Fatalf("NewDataHandler() error = %v", err)
	}
	request := httptest.NewRequest(
		http.MethodPost,
		"/v1/chat/completions",
		strings.NewReader(`{"model":"public","messages":[]}`),
	)
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)

	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", response.Code, response.Body)
	}
	if firstCalls.Load() != 1 || secondCalls.Load() != 1 {
		t.Fatalf("upstream calls = (%d, %d), want (1, 1)", firstCalls.Load(), secondCalls.Load())
	}
	if got := response.Header().Get("X-SparkRoute-Attempt-Count"); got != "2" {
		t.Fatalf("attempt count = %q, want 2", got)
	}
	records := recorder.snapshot()
	if len(records) != 3 ||
		records[0].Attempt == nil ||
		records[1].Attempt == nil ||
		records[2].Request == nil {
		t.Fatalf("ledger records = %#v, want two attempts and one request", records)
	}
	if !records[0].Attempt.Retried ||
		records[0].Attempt.HTTPStatus != http.StatusServiceUnavailable ||
		records[0].Attempt.Outcome != ledger.OutcomeUpstreamError {
		t.Fatalf("first attempt = %#v", records[0].Attempt)
	}
	if records[1].Attempt.Retried ||
		records[1].Attempt.Outcome != ledger.OutcomeSuccess ||
		records[2].Request.AttemptCount != 2 {
		t.Fatalf("final records = %#v, %#v", records[1], records[2])
	}
}

func TestChatCompletionsRetriesAfterPerTryTimeout(t *testing.T) {
	t.Parallel()

	var slowCalls atomic.Int64
	slowRelease := make(chan struct{})
	slow := httptest.NewServer(http.HandlerFunc(func(
		_ http.ResponseWriter,
		request *http.Request,
	) {
		slowCalls.Add(1)
		select {
		case <-request.Context().Done():
		case <-slowRelease:
		}
	}))
	defer slow.Close()
	var healthyCalls atomic.Int64
	healthy := httptest.NewServer(http.HandlerFunc(func(
		w http.ResponseWriter,
		_ *http.Request,
	) {
		healthyCalls.Add(1)
		writeJSON(w, http.StatusOK, map[string]any{
			"id":      "chatcmpl-after-timeout",
			"model":   "upstream-second",
			"choices": []any{},
		})
	}))
	defer healthy.Close()

	document := twoTargetDocument(slow.URL+"/v1", healthy.URL+"/v1")
	document.VirtualModels[0].Limits.OverallTimeout = config.Duration(time.Second)
	document.VirtualModels[0].Limits.PerTryTimeout = config.Duration(20 * time.Millisecond)
	document.VirtualModels[0].Retry.BaseBackoff = config.Duration(time.Millisecond)
	document.VirtualModels[0].Retry.MaxBackoff = config.Duration(time.Millisecond)
	recorder := &collectingRecorder{}
	handler, err := NewDataHandler(document, DataOptions{
		RoutingPicker:    zeroPicker{},
		RetryDelayPicker: zeroPicker{},
		Ledger:           recorder,
	})
	if err != nil {
		t.Fatalf("NewDataHandler() error = %v", err)
	}
	request := httptest.NewRequest(
		http.MethodPost,
		"/v1/chat/completions",
		strings.NewReader(`{"model":"public","messages":[]}`),
	)
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	close(slowRelease)

	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", response.Code, response.Body)
	}
	if slowCalls.Load() != 1 || healthyCalls.Load() != 1 {
		t.Fatalf(
			"upstream calls = (%d, %d), want (1, 1)",
			slowCalls.Load(),
			healthyCalls.Load(),
		)
	}
	records := recorder.snapshot()
	if len(records) != 3 ||
		records[0].Attempt == nil ||
		records[0].Attempt.FailureClass != "per_try_timeout" ||
		!records[0].Attempt.Retried {
		t.Fatalf("ledger records = %#v", records)
	}
}

func TestChatCompletionsOverallTimeoutStopsRequest(t *testing.T) {
	t.Parallel()

	release := make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(
		_ http.ResponseWriter,
		request *http.Request,
	) {
		select {
		case <-request.Context().Done():
		case <-release:
		}
	}))
	defer upstream.Close()
	document := proxyDocument(upstream.URL + "/v1")
	document.VirtualModels[0].Limits.OverallTimeout = config.Duration(25 * time.Millisecond)
	document.VirtualModels[0].Limits.PerTryTimeout = config.Duration(time.Second)
	recorder := &collectingRecorder{}
	handler, err := NewDataHandler(document, DataOptions{Ledger: recorder})
	if err != nil {
		t.Fatalf("NewDataHandler() error = %v", err)
	}
	request := httptest.NewRequest(
		http.MethodPost,
		"/v1/chat/completions",
		strings.NewReader(`{"model":"public","messages":[]}`),
	)
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	close(release)

	if response.Code != http.StatusGatewayTimeout {
		t.Fatalf("status = %d, want 504; body=%s", response.Code, response.Body)
	}
	var failure openAIErrorEnvelope
	if err := json.Unmarshal(response.Body.Bytes(), &failure); err != nil {
		t.Fatalf("decode failure: %v", err)
	}
	if failure.Error.Code != "request_timeout" {
		t.Fatalf("error code = %q, want request_timeout", failure.Error.Code)
	}
	records := recorder.snapshot()
	if len(records) != 2 ||
		records[0].Attempt == nil ||
		records[0].Attempt.FailureClass != "overall_timeout" ||
		records[0].Attempt.Retried {
		t.Fatalf("ledger records = %#v", records)
	}
}

func TestChatCompletionsRespectsCappedRetryAfter(t *testing.T) {
	t.Parallel()

	first := httptest.NewServer(http.HandlerFunc(func(
		w http.ResponseWriter,
		_ *http.Request,
	) {
		w.Header().Set("Retry-After", "60")
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer first.Close()
	second := httptest.NewServer(http.HandlerFunc(func(
		w http.ResponseWriter,
		_ *http.Request,
	) {
		writeJSON(w, http.StatusOK, map[string]any{
			"id":      "chatcmpl-after-retry-after",
			"model":   "upstream-second",
			"choices": []any{},
		})
	}))
	defer second.Close()

	document := twoTargetDocument(first.URL+"/v1", second.URL+"/v1")
	document.VirtualModels[0].Retry.BaseBackoff = config.Duration(time.Millisecond)
	document.VirtualModels[0].Retry.MaxBackoff = config.Duration(time.Millisecond)
	document.VirtualModels[0].Retry.MaxRetryAfter = config.Duration(2 * time.Second)
	sleeper := &recordingRetrySleeper{}
	handler, err := NewDataHandler(document, DataOptions{
		RoutingPicker:    zeroPicker{},
		RetryDelayPicker: zeroPicker{},
		RetrySleeper:     sleeper,
	})
	if err != nil {
		t.Fatalf("NewDataHandler() error = %v", err)
	}
	request := httptest.NewRequest(
		http.MethodPost,
		"/v1/chat/completions",
		strings.NewReader(`{"model":"public","messages":[]}`),
	)
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)

	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", response.Code, response.Body)
	}
	delays := sleeper.snapshot()
	if len(delays) != 1 || delays[0] != 2*time.Second {
		t.Fatalf("retry delays = %v, want [2s]", delays)
	}
}

func TestChatCompletionsSharedRetryBudgetRejectsConcurrentAmplification(t *testing.T) {
	t.Parallel()

	var failingCalls atomic.Int64
	failing := httptest.NewServer(http.HandlerFunc(func(
		w http.ResponseWriter,
		_ *http.Request,
	) {
		failingCalls.Add(1)
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer failing.Close()
	var fallbackCalls atomic.Int64
	fallback := httptest.NewServer(http.HandlerFunc(func(
		w http.ResponseWriter,
		_ *http.Request,
	) {
		fallbackCalls.Add(1)
		writeJSON(w, http.StatusOK, map[string]any{
			"id":      "chatcmpl-budget-fallback",
			"model":   "upstream-second",
			"choices": []any{},
		})
	}))
	defer fallback.Close()

	document := twoTargetDocument(failing.URL+"/v1", fallback.URL+"/v1")
	document.VirtualModels[0].Retry.BaseBackoff = config.Duration(time.Millisecond)
	document.VirtualModels[0].Retry.MaxBackoff = config.Duration(time.Millisecond)
	document.VirtualModels[0].Retry.Budget = config.RetryBudgetPolicy{
		Ratio:          0.01,
		MinConcurrency: 1,
	}
	sleeper := &blockingFirstRetrySleeper{
		entered: make(chan struct{}),
		release: make(chan struct{}),
	}
	handler, err := NewDataHandler(document, DataOptions{
		RoutingPicker:    zeroPicker{},
		RetryDelayPicker: zeroPicker{},
		RetrySleeper:     sleeper,
	})
	if err != nil {
		t.Fatalf("NewDataHandler() error = %v", err)
	}
	firstDone := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		request := httptest.NewRequest(
			http.MethodPost,
			"/v1/chat/completions",
			strings.NewReader(`{"model":"public","messages":[]}`),
		)
		request.Header.Set("Content-Type", "application/json")
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		firstDone <- response
	}()
	select {
	case <-sleeper.entered:
	case <-time.After(5 * time.Second):
		close(sleeper.release)
		t.Fatal("timed out waiting for the first admitted retry")
	}

	secondRequest := httptest.NewRequest(
		http.MethodPost,
		"/v1/chat/completions",
		strings.NewReader(`{"model":"public","messages":[]}`),
	)
	secondRequest.Header.Set("Content-Type", "application/json")
	secondResponse := httptest.NewRecorder()
	handler.ServeHTTP(secondResponse, secondRequest)
	if secondResponse.Code != http.StatusServiceUnavailable {
		close(sleeper.release)
		t.Fatalf(
			"second status = %d, want 503; body=%s",
			secondResponse.Code,
			secondResponse.Body,
		)
	}

	close(sleeper.release)
	select {
	case firstResponse := <-firstDone:
		if firstResponse.Code != http.StatusOK {
			t.Fatalf(
				"first status = %d, want 200; body=%s",
				firstResponse.Code,
				firstResponse.Body,
			)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for the admitted retry")
	}
	if failingCalls.Load() != 2 || fallbackCalls.Load() != 1 {
		t.Fatalf(
			"upstream calls = (%d, %d), want (2, 1)",
			failingCalls.Load(),
			fallbackCalls.Load(),
		)
	}
}

func TestChatCompletionsRoutesOnlyToCapabilityCompatibleTarget(t *testing.T) {
	t.Parallel()

	var plainCalls atomic.Int64
	plain := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		plainCalls.Add(1)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer plain.Close()
	var toolCalls atomic.Int64
	tools := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		toolCalls.Add(1)
		writeJSON(w, http.StatusOK, map[string]any{
			"id":      "chatcmpl-tools",
			"model":   "upstream-second",
			"choices": []any{},
		})
	}))
	defer tools.Close()

	document := twoTargetDocument(plain.URL+"/v1", tools.URL+"/v1")
	document.Deployments[1].Capabilities = append(
		document.Deployments[1].Capabilities,
		config.CapabilityTools,
	)
	handler, err := NewDataHandler(document, DataOptions{RoutingPicker: zeroPicker{}})
	if err != nil {
		t.Fatalf("NewDataHandler() error = %v", err)
	}
	request := httptest.NewRequest(
		http.MethodPost,
		"/v1/chat/completions",
		strings.NewReader(`{
			"model":"public",
			"messages":[{"role":"user","content":"use a tool"}],
			"tools":[{"type":"function","function":{"name":"lookup"}}]
		}`),
	)
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)

	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", response.Code, response.Body)
	}
	if plainCalls.Load() != 0 || toolCalls.Load() != 1 {
		t.Fatalf(
			"upstream calls = (%d, %d), want (0, 1)",
			plainCalls.Load(),
			toolCalls.Load(),
		)
	}
}

func TestChatCompletionsRejectsUnsupportedCapabilityBeforeUpstream(t *testing.T) {
	t.Parallel()

	var calls atomic.Int64
	upstream := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		calls.Add(1)
	}))
	defer upstream.Close()

	document := proxyDocument(upstream.URL + "/v1")
	document.CapabilityDefaults.Unknown = config.UnknownCapabilityReject
	handler, err := NewDataHandler(document, DataOptions{})
	if err != nil {
		t.Fatalf("NewDataHandler() error = %v", err)
	}
	request := httptest.NewRequest(
		http.MethodPost,
		"/v1/chat/completions",
		strings.NewReader(`{
			"model":"public",
			"messages":[],
			"tools":[{"type":"function","function":{"name":"lookup"}}]
		}`),
	)
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)

	if response.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body=%s", response.Code, response.Body)
	}
	var failure openAIErrorEnvelope
	if err := json.Unmarshal(response.Body.Bytes(), &failure); err != nil {
		t.Fatalf("decode failure: %v", err)
	}
	if failure.Error.Code != "unsupported_feature" {
		t.Fatalf("error code = %q, want unsupported_feature", failure.Error.Code)
	}
	if calls.Load() != 0 {
		t.Fatalf("upstream calls = %d, want 0", calls.Load())
	}
}

func TestChatCompletionsTriesSafeUnknownCapabilityWhenPermissive(t *testing.T) {
	t.Parallel()

	var calls atomic.Int64
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		writeJSON(w, http.StatusOK, map[string]any{
			"id": "chatcmpl-permissive", "model": "upstream-model", "choices": []any{},
		})
	}))
	defer upstream.Close()

	document := proxyDocument(upstream.URL + "/v1")
	document.Deployments[0].CapabilityPolicy.Unknown = config.UnknownCapabilityTry
	handler, err := NewDataHandler(document, DataOptions{})
	if err != nil {
		t.Fatalf("NewDataHandler() error = %v", err)
	}
	request := httptest.NewRequest(
		http.MethodPost,
		"/v1/chat/completions",
		strings.NewReader(`{
			"model":"public",
			"messages":[],
			"tools":[{"type":"function","function":{"name":"lookup"}}]
		}`),
	)
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)

	if response.Code != http.StatusOK || calls.Load() != 1 {
		t.Fatalf(
			"permissive request = status %d, calls %d; body=%s",
			response.Code,
			calls.Load(),
			response.Body,
		)
	}
}

func TestChatCompletionsEjectsFailedTargetAndFallsBack(t *testing.T) {
	t.Parallel()

	var failedCalls atomic.Int64
	failed := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		failedCalls.Add(1)
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer failed.Close()
	var healthyCalls atomic.Int64
	healthy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		healthyCalls.Add(1)
		writeJSON(w, http.StatusOK, map[string]any{
			"id":      "chatcmpl-healthy",
			"model":   "upstream-second",
			"choices": []any{},
		})
	}))
	defer healthy.Close()

	document := twoTargetDocument(failed.URL+"/v1", healthy.URL+"/v1")
	document.Deployments[0].Circuit.ConsecutiveFailures = 1
	targets, err := routing.NewTargetManager(
		document.Deployments,
		routing.TargetManagerOptions{},
	)
	if err != nil {
		t.Fatalf("NewTargetManager() error = %v", err)
	}
	handler, err := NewDataHandler(document, DataOptions{
		RoutingPicker: zeroPicker{},
		TargetManager: targets,
	})
	if err != nil {
		t.Fatalf("NewDataHandler() error = %v", err)
	}

	for requestNumber := 1; requestNumber <= 2; requestNumber++ {
		request := httptest.NewRequest(
			http.MethodPost,
			"/v1/chat/completions",
			strings.NewReader(`{"model":"public","messages":[]}`),
		)
		request.Header.Set("Content-Type", "application/json")
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		if response.Code != http.StatusOK {
			t.Fatalf(
				"request %d status = %d, want 200; body=%s",
				requestNumber,
				response.Code,
				response.Body,
			)
		}
	}

	if failedCalls.Load() != 1 || healthyCalls.Load() != 2 {
		t.Fatalf(
			"upstream calls = (%d, %d), want (1, 2)",
			failedCalls.Load(),
			healthyCalls.Load(),
		)
	}
	status, exists := targets.Status("deployment")
	if !exists || status.CircuitState != routing.CircuitOpen {
		t.Fatalf("failed target status = %#v, exists = %t", status, exists)
	}
	if len(status.RecentFailures) != 1 ||
		status.RecentFailures[0].FailureClass != "upstream_http_503" ||
		status.RecentFailures[0].OccurredAt.IsZero() {
		t.Fatalf("failed target recent failures = %#v", status.RecentFailures)
	}
}

func TestChatCompletionsMaxConcurrencyRejectsExcessRequest(t *testing.T) {
	t.Parallel()

	entered := make(chan struct{}, 1)
	release := make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		entered <- struct{}{}
		<-release
		writeJSON(w, http.StatusOK, map[string]any{
			"id":      "chatcmpl-capacity",
			"model":   "upstream-model",
			"choices": []any{},
		})
	}))
	defer upstream.Close()

	document := proxyDocument(upstream.URL + "/v1")
	document.Deployments[0].MaxConcurrency = 1
	handler, err := NewDataHandler(document, DataOptions{})
	if err != nil {
		t.Fatalf("NewDataHandler() error = %v", err)
	}
	firstDone := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		request := httptest.NewRequest(
			http.MethodPost,
			"/v1/chat/completions",
			strings.NewReader(`{"model":"public","messages":[]}`),
		)
		request.Header.Set("Content-Type", "application/json")
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		firstDone <- response
	}()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		close(release)
		t.Fatal("timed out waiting for first upstream request")
	}

	secondRequest := httptest.NewRequest(
		http.MethodPost,
		"/v1/chat/completions",
		strings.NewReader(`{"model":"public","messages":[]}`),
	)
	secondRequest.Header.Set("Content-Type", "application/json")
	secondResponse := httptest.NewRecorder()
	handler.ServeHTTP(secondResponse, secondRequest)
	if secondResponse.Code != http.StatusServiceUnavailable {
		close(release)
		t.Fatalf(
			"second status = %d, want 503; body=%s",
			secondResponse.Code,
			secondResponse.Body,
		)
	}

	close(release)
	select {
	case firstResponse := <-firstDone:
		if firstResponse.Code != http.StatusOK {
			t.Fatalf(
				"first status = %d, want 200; body=%s",
				firstResponse.Code,
				firstResponse.Body,
			)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for first gateway request")
	}
}

func TestChatCompletionsDoesNotRetryCallerError(t *testing.T) {
	t.Parallel()

	var firstCalls atomic.Int64
	first := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		firstCalls.Add(1)
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid request"})
	}))
	defer first.Close()
	var secondCalls atomic.Int64
	second := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		secondCalls.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	defer second.Close()

	handler, err := NewDataHandler(
		twoTargetDocument(first.URL+"/v1", second.URL+"/v1"),
		DataOptions{RoutingPicker: zeroPicker{}},
	)
	if err != nil {
		t.Fatalf("NewDataHandler() error = %v", err)
	}
	request := httptest.NewRequest(
		http.MethodPost,
		"/v1/chat/completions",
		strings.NewReader(`{"model":"public","messages":[]}`),
	)
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)

	if response.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", response.Code)
	}
	if firstCalls.Load() != 1 || secondCalls.Load() != 0 {
		t.Fatalf("upstream calls = (%d, %d), want (1, 0)", firstCalls.Load(), secondCalls.Load())
	}
}

func TestChatCompletionsRespectsMaxAttempts(t *testing.T) {
	t.Parallel()

	var secondCalls atomic.Int64
	first := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer first.Close()
	second := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		secondCalls.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	defer second.Close()

	document := twoTargetDocument(first.URL+"/v1", second.URL+"/v1")
	document.VirtualModels[0].Limits.MaxAttempts = 1
	handler, err := NewDataHandler(document, DataOptions{RoutingPicker: zeroPicker{}})
	if err != nil {
		t.Fatalf("NewDataHandler() error = %v", err)
	}
	request := httptest.NewRequest(
		http.MethodPost,
		"/v1/chat/completions",
		strings.NewReader(`{"model":"public","messages":[]}`),
	)
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)

	if response.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", response.Code)
	}
	if secondCalls.Load() != 0 {
		t.Fatalf("second upstream calls = %d, want 0", secondCalls.Load())
	}
	if got := response.Header().Get("X-SparkRoute-Attempt-Count"); got != "1" {
		t.Fatalf("attempt count = %q, want 1", got)
	}
}

func TestChatCompletionsDoesNotFollowRedirectWithCredential(t *testing.T) {
	t.Parallel()

	var redirectedCalls atomic.Int64
	redirectTarget := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		redirectedCalls.Add(1)
	}))
	defer redirectTarget.Close()
	redirector := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Location", redirectTarget.URL)
		w.WriteHeader(http.StatusTemporaryRedirect)
	}))
	defer redirector.Close()

	document := proxyDocument(redirector.URL)
	document.Providers[0].Auth = config.ProviderAuth{
		Type:       config.AuthBearer,
		Credential: "env://UPSTREAM_TOKEN",
	}
	handler, err := NewDataHandler(document, DataOptions{
		Credentials: fakeCredentialSource{"env://UPSTREAM_TOKEN": "secret"},
	})
	if err != nil {
		t.Fatalf("NewDataHandler() error = %v", err)
	}
	request := httptest.NewRequest(
		http.MethodPost,
		"/v1/chat/completions",
		strings.NewReader(`{"model":"public","messages":[]}`),
	)
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)

	if response.Code != http.StatusTemporaryRedirect {
		t.Fatalf("status = %d, want %d", response.Code, http.StatusTemporaryRedirect)
	}
	if redirectedCalls.Load() != 0 {
		t.Fatalf("redirect target calls = %d, want 0", redirectedCalls.Load())
	}
}

func TestChatCompletionsRejectsIncompatibleProviderType(t *testing.T) {
	t.Parallel()

	document := proxyDocument("https://example.com/v1")
	document.Providers[0].Type = "custom"
	handler, err := NewDataHandler(document, DataOptions{})
	if err != nil {
		t.Fatalf("NewDataHandler() error = %v", err)
	}
	request := httptest.NewRequest(
		http.MethodPost,
		"/v1/chat/completions",
		strings.NewReader(`{"model":"public","messages":[]}`),
	)
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)

	if response.Code != http.StatusServiceUnavailable {
		t.Fatalf(
			"status = %d, want %d",
			response.Code,
			http.StatusServiceUnavailable,
		)
	}
}

func TestCopyResponseHeadersHonorsConnectionTokens(t *testing.T) {
	t.Parallel()

	source := make(http.Header)
	source.Set("Connection", "X-Internal")
	source.Set("X-Internal", "must-not-forward")
	source.Set("X-Public", "forward")
	source.Set("X-MM-Projection", "spoofed")
	target := make(http.Header)
	copyResponseHeaders(target, source)

	if got := target.Get("X-Internal"); got != "" {
		t.Fatalf("X-Internal = %q", got)
	}
	if got := target.Get("X-Public"); got != "forward" {
		t.Fatalf("X-Public = %q", got)
	}
	if got := target.Get("X-MM-Projection"); got != "" {
		t.Fatalf("X-MM-Projection = %q", got)
	}
}

func proxyDocument(baseURL string) config.Document {
	return config.Document{
		Providers: []config.Provider{{
			Name:    "provider",
			Type:    "openai_compatible",
			BaseURL: baseURL,
		}},
		Deployments: []config.Deployment{{
			Name:     "deployment",
			Provider: "provider",
			Model:    "upstream-model",
			Capabilities: []config.Capability{
				config.CapabilitySeed,
				config.CapabilityStreamUsage,
			},
		}},
		VirtualModels: []config.VirtualModel{{
			Name:    "public",
			Aliases: []string{"default"},
			Pools: []config.RoutingPool{{
				Priority: 0,
				Targets: []config.WeightedTarget{{
					Deployment: "deployment",
					Weight:     100,
				}},
			}},
		}},
	}
}

func twoTargetDocument(firstBaseURL, secondBaseURL string) config.Document {
	document := proxyDocument(firstBaseURL)
	document.Providers = append(document.Providers, config.Provider{
		Name:    "provider-second",
		Type:    "openai_compatible",
		BaseURL: secondBaseURL,
	})
	document.Deployments = append(document.Deployments, config.Deployment{
		Name:     "deployment-second",
		Provider: "provider-second",
		Model:    "upstream-second",
		Capabilities: append(
			[]config.Capability(nil),
			document.Deployments[0].Capabilities...,
		),
	})
	document.VirtualModels[0].Pools[0].Targets = append(
		document.VirtualModels[0].Pools[0].Targets,
		config.WeightedTarget{
			Deployment: "deployment-second",
			Weight:     100,
		},
	)
	return document
}
