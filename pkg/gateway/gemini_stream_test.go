package gateway

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/sparksq/sparkroute/pkg/ledger"
)

func TestGeminiStreamGenerateContentLifecycleAndUsage(t *testing.T) {
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
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(
			w,
			`data: {"candidates":[{"content":{"role":"model",`+
				`"parts":[{"text":"hel"}]},"index":0}],`+
				`"usageMetadata":{"promptTokenCount":4},`+
				`"modelVersion":"gemini-upstream","responseId":"response-1"}`+
				"\n\n",
		)
		_, _ = io.WriteString(
			w,
			`: keep-alive`+"\n\n"+
				`data: {"candidates":[{"content":{"role":"model",`+
				`"parts":[{"text":"lo"}]},"finishReason":"STOP","index":0}],`+
				`"usageMetadata":{"candidatesTokenCount":2,`+
				`"thoughtsTokenCount":1,"totalTokenCount":7},`+
				`"modelVersion":"gemini-upstream","responseId":"response-1",`+
				`"futureField":{"kept":true}}`+"\n\n",
		)
	}))
	defer upstream.Close()

	recorder := &collectingRecorder{}
	handler, err := NewDataHandler(
		geminiDocument(upstream.URL+"/v1beta"),
		DataOptions{Ledger: recorder},
	)
	if err != nil {
		t.Fatalf("NewDataHandler() error = %v", err)
	}
	response := serveGeminiRequest(
		handler,
		http.MethodPost,
		"/v1beta/models/default:streamGenerateContent?alt=sse&key=caller",
		`{"contents":[{"role":"user","parts":[{"text":"hello"}]}]}`,
	)
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d; body=%s", response.Code, response.Body)
	}
	body := response.Body.String()
	for _, expected := range []string{
		`"text":"hel"`,
		`"text":"lo"`,
		`"modelVersion":"public"`,
		`: keep-alive`,
		`"futureField":{"kept":true}`,
	} {
		if !strings.Contains(body, expected) {
			t.Errorf("stream missing %q:\n%s", expected, body)
		}
	}
	if strings.Contains(body, "[DONE]") {
		t.Fatalf("Gemini stream contains fabricated terminal marker: %s", body)
	}
	got := <-captured
	if got.Path !=
		"/v1beta/models/gemini-upstream:streamGenerateContent" ||
		got.RawQuery != "alt=sse" ||
		got.Header.Get("Accept") != "text/event-stream" ||
		len(got.Body) == 0 {
		t.Fatalf("upstream request = %#v", got)
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
		assertUsageValue(t, "input", usage.InputTokens, 4)
		assertUsageValue(t, "output", usage.OutputTokens, 2)
		assertUsageValue(t, "total", usage.TotalTokens, 7)
		assertUsageValue(t, "reasoning", usage.ReasoningTokens, 1)
		if usage.Completeness != ledger.UsageComplete ||
			usage.NormalizationVersion !=
				geminiGenerateContentUsageVersion {
			t.Errorf("usage = %#v", usage)
		}
	}
	if records[0].Attempt.Outcome != ledger.OutcomeSuccess ||
		records[1].Request.Outcome != ledger.OutcomeSuccess ||
		records[1].Request.Operation != "stream_generate_content" {
		t.Fatalf("records = %#v", records)
	}
}

func TestGeminiStreamGenerateContentPost200Error(t *testing.T) {
	t.Parallel()

	upstream := httptest.NewServer(http.HandlerFunc(func(
		w http.ResponseWriter,
		_ *http.Request,
	) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(
			w,
			`data: {"error":{"code":503,"message":"unavailable",`+
				`"status":"UNAVAILABLE"}}`+"\n\n",
		)
	}))
	defer upstream.Close()

	recorder := &collectingRecorder{}
	handler, err := NewDataHandler(
		geminiDocument(upstream.URL+"/v1beta"),
		DataOptions{Ledger: recorder},
	)
	if err != nil {
		t.Fatalf("NewDataHandler() error = %v", err)
	}
	response := serveGeminiRequest(
		handler,
		http.MethodPost,
		"/v1beta/models/public:streamGenerateContent",
		`{"contents":[{"parts":[{"text":"hello"}]}]}`,
	)
	if response.Code != http.StatusOK ||
		!strings.Contains(response.Body.String(), `"status":"UNAVAILABLE"`) {
		t.Fatalf("response = %d %s", response.Code, response.Body)
	}
	records := recorder.snapshot()
	if len(records) != 2 ||
		records[0].Attempt == nil ||
		records[1].Request == nil {
		t.Fatalf("records = %#v", records)
	}
	for _, result := range []struct {
		outcome ledger.Outcome
		failure string
	}{
		{
			records[0].Attempt.Outcome,
			records[0].Attempt.FailureClass,
		},
		{
			records[1].Request.Outcome,
			records[1].Request.FailureClass,
		},
	} {
		if result.outcome != ledger.OutcomeUpstreamError ||
			result.failure != "gemini_stream_error_unavailable" {
			t.Errorf(
				"outcome/failure = %q/%q",
				result.outcome,
				result.failure,
			)
		}
	}
}

func TestGeminiStreamGenerateContentInvalidEvent(t *testing.T) {
	t.Parallel()

	upstream := httptest.NewServer(http.HandlerFunc(func(
		w http.ResponseWriter,
		_ *http.Request,
	) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {invalid-json}\n\n")
	}))
	defer upstream.Close()

	recorder := &collectingRecorder{}
	handler, err := NewDataHandler(
		geminiDocument(upstream.URL+"/v1beta"),
		DataOptions{Ledger: recorder},
	)
	if err != nil {
		t.Fatalf("NewDataHandler() error = %v", err)
	}
	response := serveGeminiRequest(
		handler,
		http.MethodPost,
		"/v1beta/models/public:streamGenerateContent",
		`{"contents":[{"parts":[{"text":"hello"}]}]}`,
	)
	if response.Code != http.StatusOK ||
		!strings.Contains(response.Body.String(), "{invalid-json}") {
		t.Fatalf("response = %d %s", response.Code, response.Body)
	}
	records := recorder.snapshot()
	if len(records) != 2 ||
		records[0].Attempt == nil ||
		records[1].Request == nil {
		t.Fatalf("records = %#v", records)
	}
	for _, result := range []struct {
		outcome ledger.Outcome
		failure string
	}{
		{
			records[0].Attempt.Outcome,
			records[0].Attempt.FailureClass,
		},
		{
			records[1].Request.Outcome,
			records[1].Request.FailureClass,
		},
	} {
		if result.outcome != ledger.OutcomeStreamError ||
			result.failure != "gemini_stream_invalid_event" {
			t.Errorf(
				"outcome/failure = %q/%q",
				result.outcome,
				result.failure,
			)
		}
	}
}
