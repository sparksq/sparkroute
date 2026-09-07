package gateway

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"

	"github.com/sparksq/sparkroute/pkg/config"
	"github.com/sparksq/sparkroute/pkg/identity"
	"github.com/sparksq/sparkroute/pkg/privacy"
)

var piiEmailTokenPattern = regexp.MustCompile(
	`\[\[SPARKROUTE_PII_EMAIL_[A-Z2-7]+\]\]`,
)

func TestPIIRequestTraversalCoversNativeProtocolText(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		operation openAIOperation
		body      string
	}{
		"openai chat": {
			operation: openAIOperationChatCompletions,
			body: `{"model":"public","messages":[{"role":"user","content":"drew@example.com",` +
				`"tool_calls":[{"function":{"arguments":"{\"email\":\"drew@example.com\"}"}}]}]}`,
		},
		"openai responses": {
			operation: openAIOperationResponses,
			body: `{"model":"public","instructions":"contact drew@example.com",` +
				`"input":[{"role":"user","content":[{"type":"input_text","text":"drew@example.com"}]}]}`,
		},
		"openai embeddings": {
			operation: openAIOperationEmbeddings,
			body:      `{"model":"public","input":["drew@example.com"]}`,
		},
		"anthropic messages": {
			operation: anthropicOperationMessages,
			body: `{"model":"public","system":"drew@example.com",` +
				`"messages":[{"role":"user","content":[{"type":"text","text":"drew@example.com"}]}]}`,
		},
		"gemini generate": {
			operation: geminiOperationGenerateContent,
			body: `{"systemInstruction":{"parts":[{"text":"drew@example.com"}]},` +
				`"contents":[{"role":"user","parts":[{"text":"drew@example.com"}]}]}`,
		},
		"gemini embedding": {
			operation: geminiOperationEmbedContent,
			body:      `{"content":{"parts":[{"text":"drew@example.com"}]}}`,
		},
		"bedrock converse": {
			operation: bedrockOperationConverse,
			body: `{"system":[{"text":"drew@example.com"}],` +
				`"messages":[{"role":"user","content":[{"text":"drew@example.com"}]}]}`,
		},
	}
	for name, test := range tests {
		name, test := name, test
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			session := deterministicPIISession(t)
			transformed, err := test.operation.substitutePIIRequest(
				context.Background(), []byte(test.body), session,
			)
			if err != nil {
				t.Fatalf("substitutePIIRequest() error = %v", err)
			}
			if strings.Contains(string(transformed), "drew@example.com") ||
				!strings.Contains(string(transformed), "[[SPARKROUTE_PII_EMAIL_TOKEN]]") {
				t.Fatalf("transformed request = %s", transformed)
			}
		})
	}
}

func TestPIIResponseTraversalRestoresKnownAndRedactsNovelText(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		operation openAIOperation
		body      string
	}{
		"openai chat": {
			operation: openAIOperationChatCompletions,
			body: `{"choices":[{"message":{"content":"known [[SPARKROUTE_PII_EMAIL_TOKEN]] ` +
				`novel other@example.com"}}]}`,
		},
		"openai responses": {
			operation: openAIOperationResponses,
			body: `{"output":[{"content":[{"type":"output_text","text":` +
				`"known [[SPARKROUTE_PII_EMAIL_TOKEN]] novel other@example.com"}]}]}`,
		},
		"anthropic messages": {
			operation: anthropicOperationMessages,
			body: `{"content":[{"type":"text","text":` +
				`"known [[SPARKROUTE_PII_EMAIL_TOKEN]] novel other@example.com"}]}`,
		},
		"gemini generate": {
			operation: geminiOperationGenerateContent,
			body: `{"candidates":[{"content":{"parts":[{"text":` +
				`"known [[SPARKROUTE_PII_EMAIL_TOKEN]] novel other@example.com"}]}}]}`,
		},
		"bedrock converse": {
			operation: bedrockOperationConverse,
			body: `{"output":{"message":{"content":[{"text":` +
				`"known [[SPARKROUTE_PII_EMAIL_TOKEN]] novel other@example.com"}]}}}`,
		},
	}
	for name, test := range tests {
		name, test := name, test
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			session := deterministicPIISession(t)
			if _, err := session.Substitute(context.Background(), "drew@example.com"); err != nil {
				t.Fatal(err)
			}
			masked, err := test.operation.redactPIIResponse(
				context.Background(), []byte(test.body), session,
			)
			if err != nil {
				t.Fatalf("redactPIIResponse() error = %v", err)
			}
			restored, err := test.operation.restorePIIResponse(
				context.Background(), masked, session,
			)
			if err != nil {
				t.Fatalf("restorePIIResponse() error = %v", err)
			}
			got := string(restored)
			if !strings.Contains(got, "drew@example.com") ||
				strings.Contains(got, "other@example.com") ||
				strings.Contains(got, "[[SPARKROUTE_PII_EMAIL_TOKEN]]") ||
				!strings.Contains(got, "[[SPARKROUTE_PII_EMAIL_REDACTED]]") {
				t.Fatalf("restored response = %s", restored)
			}
		})
	}
}

func deterministicPIISession(t *testing.T) privacy.Session {
	t.Helper()
	return newTestPrivacySession()
}

func TestNewPIISessionUsesConfiguredProviderForDynamicCatalogPolicy(t *testing.T) {
	t.Parallel()

	policy := (&config.PIIPolicy{
		Entities: []config.PIIEntity{config.PIIEntityEmail},
	}).Effective()
	session, err := newPIISession(
		t.Context(), testPrivacyProvider{}, policy, identity.Identity{},
	)
	if err != nil {
		t.Fatalf("newPIISession() error = %v", err)
	}
	masked, err := session.Substitute(context.Background(), "drew@example.com")
	if err != nil || !piiEmailTokenPattern.MatchString(masked) {
		t.Fatalf("Substitute() = %q, %v", masked, err)
	}
}

func TestPIIConversationScopeUsesOnlyAuthenticatedIdentityAndTrustedAttribution(t *testing.T) {
	t.Parallel()
	scope, err := piiConversationScope(identity.Identity{
		Principal:   identity.Principal{ID: "principal-a", Tenant: "tenant-a"},
		Attribution: identity.Attribution{identity.AttributeThreadID: "thread-a"},
	})
	if err != nil || scope.Tenant != "tenant-a" || scope.Principal != "principal-a" ||
		scope.Conversation != "thread-a" {
		t.Fatalf("piiConversationScope() = %#v, %v", scope, err)
	}
	if _, err := piiConversationScope(identity.Identity{
		Principal: identity.Principal{ID: "principal-a"},
	}); err == nil || !strings.Contains(err.Error(), "thread_id or session") {
		t.Fatalf("missing attribution error = %v", err)
	}
	if _, err := piiConversationScope(identity.Identity{
		Attribution: identity.Attribution{identity.AttributeThreadID: "thread-a"},
	}); err == nil || !strings.Contains(err.Error(), "authenticated principal") {
		t.Fatalf("missing principal error = %v", err)
	}
}

func TestDataPlaneRejectsPIIPolicyWithoutProvider(t *testing.T) {
	t.Parallel()
	document := proxyDocument("https://provider.example/v1")
	document.VirtualModels[0].Privacy = &config.PrivacyPolicy{PII: &config.PIIPolicy{
		Scope: config.PIIScopeConversation,
	}}
	if _, err := NewDataHandler(document, DataOptions{}); err == nil ||
		!strings.Contains(err.Error(), "requires a privacy provider") {
		t.Fatalf("NewDataHandler() error = %v", err)
	}
}

func TestChatCompletionsPIISubstitutionRestorationAndMaskedTrace(t *testing.T) {
	t.Parallel()

	const requestEmail = "drew@example.com"
	const novelResponseEmail = "other@example.com"
	var upstreamBody string
	var upstreamToken string
	upstream := httptest.NewServer(http.HandlerFunc(func(
		writer http.ResponseWriter,
		request *http.Request,
	) {
		body, readErr := io.ReadAll(request.Body)
		if readErr != nil {
			http.Error(writer, readErr.Error(), http.StatusBadRequest)
			return
		}
		upstreamBody = string(body)
		upstreamToken = piiEmailTokenPattern.FindString(upstreamBody)
		if upstreamToken == "" || strings.Contains(upstreamBody, requestEmail) {
			http.Error(writer, "request was not PII-substituted", http.StatusBadRequest)
			return
		}
		writer.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintf(
			writer,
			`{"id":"chat-pii","model":"upstream-model","choices":[{"message":{"role":"assistant","content":"known %s; novel %s"}}]}`,
			upstreamToken,
			novelResponseEmail,
		)
	}))
	defer upstream.Close()

	document := proxyDocument(upstream.URL)
	document.VirtualModels[0].Privacy = &config.PrivacyPolicy{PII: &config.PIIPolicy{
		Entities: []config.PIIEntity{config.PIIEntityEmail},
	}}
	recorder := &savedTraceCollector{}
	handler, err := NewDataHandler(document, DataOptions{
		SavedTraces: recorder, Privacy: testPrivacyProvider{},
	})
	if err != nil {
		t.Fatalf("NewDataHandler() error = %v", err)
	}
	request := httptest.NewRequest(
		http.MethodPost,
		"/v1/chat/completions",
		strings.NewReader(
			`{"model":"default","messages":[{"role":"user","content":"email drew@example.com"}]}`,
		),
	)
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)

	if response.Code != http.StatusOK {
		t.Fatalf("status = %d; body = %s; upstream = %s", response.Code, response.Body, upstreamBody)
	}
	callerBody := response.Body.String()
	if !strings.Contains(callerBody, requestEmail) ||
		!strings.Contains(callerBody, "[[SPARKROUTE_PII_EMAIL_REDACTED]]") ||
		strings.Contains(callerBody, novelResponseEmail) ||
		strings.Contains(callerBody, upstreamToken) {
		t.Fatalf("caller response = %s", callerBody)
	}

	records := recorder.snapshot()
	if len(records) != 1 {
		t.Fatalf("saved trace count = %d, want 1", len(records))
	}
	record := records[0]
	if strings.Contains(record.Request.Body, requestEmail) ||
		!strings.Contains(record.Request.Body, upstreamToken) {
		t.Fatalf("trace request = %s", record.Request.Body)
	}
	if strings.Contains(record.Response.Body, requestEmail) ||
		strings.Contains(record.Response.Body, novelResponseEmail) ||
		!strings.Contains(record.Response.Body, upstreamToken) ||
		!strings.Contains(record.Response.Body, "[[SPARKROUTE_PII_EMAIL_REDACTED]]") {
		t.Fatalf("trace response = %s", record.Response.Body)
	}
	if record.Metadata["sparkroute.pii.state"] != "substituted" ||
		record.Metadata["sparkroute.pii.mapping_count"] != "1" {
		t.Fatalf("trace metadata = %#v", record.Metadata)
	}
}

func TestChatCompletionsPIIStreamingRestoresCallerAndMasksTrace(t *testing.T) {
	t.Parallel()

	var upstreamToken string
	upstream := httptest.NewServer(http.HandlerFunc(func(
		writer http.ResponseWriter,
		request *http.Request,
	) {
		body, _ := io.ReadAll(request.Body)
		upstreamToken = piiEmailTokenPattern.FindString(string(body))
		writer.Header().Set("Content-Type", "text/event-stream")
		_, _ = fmt.Fprintf(writer,
			`data: {"choices":[{"index":0,"delta":{"content":"known %s novel other@"}}]}`+"\n\n",
			upstreamToken,
		)
		_, _ = io.WriteString(writer,
			`data: {"choices":[{"index":0,"delta":{"content":"example.com"},"finish_reason":"stop"}]}`+"\n\n"+
				"data: [DONE]\n\n",
		)
	}))
	defer upstream.Close()
	document := proxyDocument(upstream.URL)
	document.VirtualModels[0].Privacy = &config.PrivacyPolicy{PII: &config.PIIPolicy{
		Entities: []config.PIIEntity{config.PIIEntityEmail},
	}}
	recorder := &savedTraceCollector{}
	handler, err := NewDataHandler(document, DataOptions{
		SavedTraces: recorder, Privacy: testPrivacyProvider{},
	})
	if err != nil {
		t.Fatalf("NewDataHandler() error = %v", err)
	}
	request := httptest.NewRequest(
		http.MethodPost,
		"/v1/chat/completions",
		strings.NewReader(
			`{"model":"default","stream":true,"messages":[{"role":"user","content":"drew@example.com"}]}`,
		),
	)
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)

	if response.Code != http.StatusOK || upstreamToken == "" {
		t.Fatalf("status = %d; token = %q; body = %s", response.Code, upstreamToken, response.Body)
	}
	if got := chatStreamText(t, response.Body.String()); got !=
		"known drew@example.com novel [[SPARKROUTE_PII_EMAIL_REDACTED]]" {
		t.Fatalf("caller stream text = %q; wire = %s", got, response.Body)
	}
	records := recorder.snapshot()
	if len(records) != 1 ||
		strings.Contains(records[0].Request.Body, "drew@example.com") ||
		strings.Contains(records[0].Response.Body, "drew@example.com") ||
		strings.Contains(records[0].Response.Body, "other@example.com") ||
		!strings.Contains(records[0].Response.Body, upstreamToken) ||
		!strings.Contains(records[0].Response.Body, "[[SPARKROUTE_PII_EMAIL_REDACTED]]") {
		t.Fatalf("saved traces = %#v", records)
	}
}

func TestChatCompletionsPIIStreamingGuardrailSeesOnlyMaskedWindows(t *testing.T) {
	t.Parallel()

	var upstreamToken string
	primary := httptest.NewServer(http.HandlerFunc(func(
		writer http.ResponseWriter,
		request *http.Request,
	) {
		body, _ := io.ReadAll(request.Body)
		upstreamToken = piiEmailTokenPattern.FindString(string(body))
		writer.Header().Set("Content-Type", "text/event-stream")
		_, _ = fmt.Fprintf(writer,
			`data: {"choices":[{"index":0,"delta":{"content":"known %s novel other@"}}]}`+"\n\n",
			upstreamToken,
		)
		_, _ = io.WriteString(writer,
			`data: {"choices":[{"index":0,"delta":{"content":"example.com"},"finish_reason":"stop"}]}`+"\n\n"+
				"data: [DONE]\n\n",
		)
	}))
	defer primary.Close()

	guardInputs := make(chan string, 8)
	guard := httptest.NewServer(http.HandlerFunc(func(
		writer http.ResponseWriter,
		request *http.Request,
	) {
		guardInputs <- readGuardrailUpstreamRequest(t, request).LastContent
		writeGuardrailVerdict(writer, "upstream-guard", `{"action":"allow"}`)
	}))
	defer guard.Close()

	document := guardrailDocument(primary.URL+"/v1", guard.URL+"/v1")
	document.VirtualModels[0].Privacy = &config.PrivacyPolicy{PII: &config.PIIPolicy{
		Entities: []config.PIIEntity{config.PIIEntityEmail},
	}}
	document.VirtualModels[0].Guardrails = config.GuardrailPolicy{
		Post: []config.Guardrail{{Name: "response-safety", Model: "safety"}},
		Stream: &config.GuardrailStreamPolicy{
			WindowBytes: 1 << 10, ContextBytes: 4 << 10,
		},
	}
	handler, err := NewDataHandler(document, DataOptions{Privacy: testPrivacyProvider{}})
	if err != nil {
		t.Fatalf("NewDataHandler() error = %v", err)
	}
	response := serveChat(
		t,
		handler,
		`{"model":"public","stream":true,"messages":[{"role":"user","content":"drew@example.com"}]}`,
	)
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d; body = %s", response.Code, response.Body)
	}
	if got := chatStreamText(t, response.Body.String()); got !=
		"known drew@example.com novel [[SPARKROUTE_PII_EMAIL_REDACTED]]" {
		t.Fatalf("caller stream text = %q", got)
	}
	close(guardInputs)
	inputCount := 0
	maskedNovel := false
	for input := range guardInputs {
		inputCount++
		if strings.Contains(input, "drew@example.com") ||
			strings.Contains(input, "other@example.com") ||
			strings.Contains(input, "other@") {
			t.Fatalf("guardrail received raw PII: %s", input)
		}
		if strings.Contains(input, "[[SPARKROUTE_PII_EMAIL_REDACTED]]") {
			maskedNovel = true
		}
	}
	if inputCount == 0 || !maskedNovel || upstreamToken == "" {
		t.Fatalf("guardrail inputs = %d; masked novel = %v; token = %q", inputCount, maskedNovel, upstreamToken)
	}
}

func TestChatCompletionsPIIStreamingMasksJSONUpstreamError(t *testing.T) {
	t.Parallel()

	var upstreamToken string
	upstream := httptest.NewServer(http.HandlerFunc(func(
		writer http.ResponseWriter,
		request *http.Request,
	) {
		body, _ := io.ReadAll(request.Body)
		upstreamToken = piiEmailTokenPattern.FindString(string(body))
		writer.Header().Set("Content-Type", "application/json")
		writer.WriteHeader(http.StatusBadRequest)
		_, _ = fmt.Fprintf(
			writer,
			`{"error":{"message":"known %s novel other@example.com"}}`,
			upstreamToken,
		)
	}))
	defer upstream.Close()
	document := proxyDocument(upstream.URL)
	document.VirtualModels[0].Privacy = &config.PrivacyPolicy{PII: &config.PIIPolicy{
		Entities: []config.PIIEntity{config.PIIEntityEmail},
	}}
	recorder := &savedTraceCollector{}
	handler, err := NewDataHandler(document, DataOptions{
		SavedTraces: recorder, Privacy: testPrivacyProvider{},
	})
	if err != nil {
		t.Fatal(err)
	}
	response := serveChat(
		t,
		handler,
		`{"model":"public","stream":true,"messages":[{"role":"user","content":"drew@example.com"}]}`,
	)
	if response.Code != http.StatusBadRequest ||
		!strings.Contains(response.Body.String(), "drew@example.com") ||
		strings.Contains(response.Body.String(), "other@example.com") ||
		!strings.Contains(response.Body.String(), "[[SPARKROUTE_PII_EMAIL_REDACTED]]") {
		t.Fatalf("response = %d %s", response.Code, response.Body)
	}
	records := recorder.snapshot()
	if len(records) != 1 ||
		strings.Contains(records[0].Response.Body, "drew@example.com") ||
		strings.Contains(records[0].Response.Body, "other@example.com") ||
		!strings.Contains(records[0].Response.Body, upstreamToken) {
		t.Fatalf("saved traces = %#v", records)
	}
}

func TestPIIBuiltinDetectorRejectsUnsupportedConfiguredEntity(t *testing.T) {
	t.Parallel()

	document := proxyDocument("https://example.com/v1")
	document.VirtualModels[0].Privacy = &config.PrivacyPolicy{PII: &config.PIIPolicy{
		Entities: []config.PIIEntity{config.PIIEntityPerson},
	}}
	_, err := NewDataHandler(document, DataOptions{Privacy: testPrivacyProvider{}})
	if err == nil || !strings.Contains(err.Error(), `does not support configured entity "person"`) {
		t.Fatalf("NewDataHandler() error = %v", err)
	}
}
