// SPDX-FileCopyrightText: 2026 Scitrera LLC
// SPDX-FileCopyrightText: 2026 Fox Engine Ltd.
// SPDX-License-Identifier: AGPL-3.0-only

package gateway

import (
	"bytes"
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/sparksq/sparkroute/pkg/identity"
	"github.com/sparksq/sparkroute/pkg/ledger"
	"github.com/sparksq/sparkroute/pkg/savedtrace"
)

func TestSavedTraceCapturesCallerVisiblePayloadAndMetadata(t *testing.T) {
	t.Parallel()

	upstream := httptest.NewServer(http.HandlerFunc(func(
		writer http.ResponseWriter,
		_ *http.Request,
	) {
		writer.Header().Set("Content-Type", "application/json")
		_, _ = writer.Write([]byte(
			`{"id":"chat-1","model":"upstream-model","choices":[{"message":{"role":"assistant","content":"answer"}}]}`,
		))
	}))
	defer upstream.Close()
	recorder := &savedTraceCollector{}
	handler, err := NewDataHandler(proxyDocument(upstream.URL), DataOptions{
		SavedTraces:        recorder,
		MaxSavedTraceBytes: 1 << 20,
	})
	if err != nil {
		t.Fatalf("NewDataHandler() error = %v", err)
	}
	body := `{"model":"default","messages":[{"role":"user","content":"prompt"}]}`
	request := httptest.NewRequest(
		http.MethodPost,
		"/v1/chat/completions",
		strings.NewReader(body),
	)
	request.Header.Set("Content-Type", "application/json")
	request = request.WithContext(identity.WithContext(request.Context(), identity.Identity{
		Principal: identity.Principal{
			ID:      "principal-a",
			Type:    "workload",
			Tenant:  "tenant-a",
			Subject: "subject-a",
		},
		Attribution: identity.Attribution{
			identity.AttributeWorkspace: "workspace-a",
			identity.AttributeThreadID:  "conversation-a",
			identity.AttributeSession:   "session-a",
		},
	}))
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d; body=%s", response.Code, response.Body)
	}
	records := recorder.snapshot()
	if len(records) != 1 {
		t.Fatalf("saved trace count = %d", len(records))
	}
	record := records[0]
	if record.RequestID == "" ||
		record.TenantID != "tenant-a" ||
		record.PrincipalID != "principal-a" ||
		record.Metadata[identity.AttributeWorkspace] != "workspace-a" ||
		record.Request.Body != body ||
		record.Response.Body != response.Body.String() ||
		record.VirtualModel != "public" ||
		record.FinalDeployment != "deployment" ||
		record.Outcome != "success" {
		t.Fatalf("saved trace = %#v", record)
	}
	if record.ConversationID != "conversation-a" || record.ResponseID != "chat-1" ||
		record.SessionID != "session-a" || record.CaptureOutcome != savedtrace.CaptureComplete {
		t.Fatalf("saved trace correlation = %#v", record)
	}
	if strings.Contains(record.Response.Body, `"model":"upstream-model"`) {
		t.Fatalf("saved response is not caller-visible: %s", record.Response.Body)
	}
}

func TestSavedTraceCaptureMarksTruncationWithoutBreakingUTF8(t *testing.T) {
	t.Parallel()

	recorder := &savedTraceCollector{}
	response := httptest.NewRecorder()
	writer := newSavedTraceResponseWriter(response, recorder, 4)
	_, _ = writer.Write([]byte("a€b"))
	payload := writer.payload()
	if payload.Body != "a€" || !payload.Truncated {
		t.Fatalf("payload = %#v", payload)
	}
}

func TestSavedTraceCapturesCallerVisibleSSE(t *testing.T) {
	t.Parallel()

	upstreamBody := "data: " +
		`{"id":"chat-1","model":"upstream-model","choices":[{"delta":{"content":"answer"}}]}` +
		"\n\ndata: [DONE]\n\n"
	upstream := httptest.NewServer(http.HandlerFunc(func(
		writer http.ResponseWriter,
		_ *http.Request,
	) {
		writer.Header().Set("Content-Type", "text/event-stream")
		_, _ = writer.Write([]byte(upstreamBody))
	}))
	defer upstream.Close()
	recorder := &savedTraceCollector{}
	handler, err := NewDataHandler(proxyDocument(upstream.URL), DataOptions{
		SavedTraces: recorder,
	})
	if err != nil {
		t.Fatalf("NewDataHandler() error = %v", err)
	}
	request := httptest.NewRequest(
		http.MethodPost,
		"/v1/chat/completions",
		strings.NewReader(
			`{"model":"default","stream":true,"messages":[{"role":"user","content":"prompt"}]}`,
		),
	)
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	records := recorder.snapshot()
	if response.Code != http.StatusOK ||
		len(records) != 1 ||
		!records[0].Stream ||
		records[0].Response.ContentType != "text/event-stream" ||
		records[0].Response.Body != response.Body.String() ||
		!strings.Contains(records[0].Response.Body, `"model":"public"`) {
		t.Fatalf("response=%d %q trace=%#v", response.Code, response.Body.String(), records)
	}
	if records[0].ResponseID != "chat-1" || records[0].CaptureOutcome != savedtrace.CaptureComplete {
		t.Fatalf("stream capture correlation = %#v", records[0])
	}
}

func TestSavedTraceCaptureMarksSuccessfulSSEWithoutTerminalEventIncomplete(t *testing.T) {
	t.Parallel()
	if got := savedTraceCaptureOutcome(true, ledger.OutcomeSuccess, savedtrace.Payload{}, savedtrace.Payload{
		ContentType: "text/event-stream", Body: `data: {"type":"response.output_text.delta"}\n\n`,
	}); got != savedtrace.CaptureIncomplete {
		t.Fatalf("capture outcome = %q", got)
	}
}

func TestSavedTraceGeminiPreservesActualPathModelWireBody(t *testing.T) {
	t.Parallel()

	upstream := httptest.NewServer(http.HandlerFunc(func(
		writer http.ResponseWriter,
		_ *http.Request,
	) {
		writer.Header().Set("Content-Type", "application/json")
		_, _ = writer.Write([]byte(`{
			"candidates":[{"content":{"role":"model","parts":[{"text":"answer"}]}}],
			"modelVersion":"gemini-upstream",
			"responseId":"response-trace"
		}`))
	}))
	defer upstream.Close()
	recorder := &savedTraceCollector{}
	handler, err := NewDataHandler(
		geminiDocument(upstream.URL+"/v1beta"),
		DataOptions{SavedTraces: recorder},
	)
	if err != nil {
		t.Fatalf("NewDataHandler() error = %v", err)
	}
	body := `{"contents":[{"role":"user","parts":[{"text":"prompt"}]}]}`
	response := serveGeminiRequest(
		handler,
		http.MethodPost,
		"/v1beta/models/default:generateContent",
		body,
	)
	records := recorder.snapshot()
	if response.Code != http.StatusOK || len(records) != 1 {
		t.Fatalf(
			"response=%d %q traces=%#v",
			response.Code,
			response.Body.String(),
			records,
		)
	}
	record := records[0]
	if record.Protocol != "gemini" ||
		record.Operation != "generate_content" ||
		record.RequestedModel != "default" ||
		record.Request.Body != body ||
		strings.Contains(record.Request.Body, `"model"`) ||
		record.Response.Body != response.Body.String() ||
		!strings.Contains(record.Response.Body, `"modelVersion":"public"`) {
		t.Fatalf("saved trace = %#v", record)
	}
}

func TestSavedTraceBedrockStreamUsesLosslessBase64Body(t *testing.T) {
	t.Parallel()

	stream := bytes.Join([][]byte{
		encodeAWSEventMessage(
			t,
			bedrockEventHeaders("messageStop"),
			[]byte(`{"stopReason":"end_turn"}`),
		),
		encodeAWSEventMessage(
			t,
			bedrockEventHeaders("metadata"),
			[]byte(`{"usage":{"inputTokens":1,`+
				`"outputTokens":1,"totalTokens":2}}`),
		),
	}, nil)
	upstream := httptest.NewServer(http.HandlerFunc(func(
		writer http.ResponseWriter,
		_ *http.Request,
	) {
		writer.Header().Set(
			"Content-Type",
			"application/vnd.amazon.eventstream",
		)
		_, _ = writer.Write(stream)
	}))
	defer upstream.Close()
	recorder := &savedTraceCollector{}
	handler, err := NewDataHandler(
		bedrockDocument(upstream.URL),
		DataOptions{
			Credentials: bedrockCredentialSource(),
			SavedTraces: recorder,
		},
	)
	if err != nil {
		t.Fatalf("NewDataHandler() error = %v", err)
	}
	body := `{"messages":[{"role":"user","content":[{"text":"prompt"}]}]}`
	response := serveBedrockRequest(
		handler,
		http.MethodPost,
		"/model/default/converse-stream",
		body,
	)
	records := recorder.snapshot()
	if response.Code != http.StatusOK || len(records) != 1 {
		t.Fatalf(
			"response=%d traces=%#v",
			response.Code,
			records,
		)
	}
	record := records[0]
	if record.Protocol != "bedrock" ||
		record.Operation != "converse_stream" ||
		record.Request.Body != body ||
		record.Response.ContentType !=
			"application/vnd.amazon.eventstream" ||
		!strings.HasPrefix(
			record.Response.Body,
			savedtrace.Base64BodyPrefix,
		) {
		t.Fatalf("saved trace = %#v", record)
	}
	decoded, err := base64.StdEncoding.DecodeString(
		strings.TrimPrefix(
			record.Response.Body,
			savedtrace.Base64BodyPrefix,
		),
	)
	if err != nil || !bytes.Equal(decoded, stream) {
		t.Fatalf(
			"decoded trace equals stream = %v, error = %v",
			bytes.Equal(decoded, stream),
			err,
		)
	}
}

type savedTraceCollector struct {
	mu      sync.Mutex
	records []savedtrace.Record
}

func (r *savedTraceCollector) Record(record savedtrace.Record) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.records = append(r.records, record)
}

func (r *savedTraceCollector) snapshot() []savedtrace.Record {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]savedtrace.Record(nil), r.records...)
}

var _ savedtrace.Recorder = (*savedTraceCollector)(nil)
