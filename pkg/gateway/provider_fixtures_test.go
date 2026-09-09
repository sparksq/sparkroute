// SPDX-FileCopyrightText: 2026 Scitrera LLC
// SPDX-FileCopyrightText: 2026 Fox Engine Ltd.
// SPDX-License-Identifier: AGPL-3.0-only

package gateway

import (
	"bytes"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/sparksq/sparkroute/pkg/config"
)

func TestChatCompletionsOpenAICompatibleProviderFixtures(t *testing.T) {
	t.Parallel()

	for _, test := range []struct {
		name       string
		fixture    string
		extraField string
	}{
		{name: "OpenAI", fixture: "openai.json", extraField: `"service_tier"`},
		{name: "vLLM", fixture: "vllm.json", extraField: `"prompt_logprobs"`},
		{name: "Ollama", fixture: "ollama.json", extraField: `"done_reason"`},
		{name: "llama.cpp", fixture: "llamacpp.json", extraField: `"timings"`},
	} {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			fixture := readChatFixture(t, test.fixture)
			transport := &fixtureTransport{
				contentType: "application/json; charset=utf-8",
				body:        fixture,
				chunkBytes:  1,
			}
			handler, err := NewDataHandler(
				proxyDocument("https://provider.example/v1"),
				DataOptions{HTTPClient: &http.Client{Transport: transport}},
			)
			if err != nil {
				t.Fatalf("NewDataHandler() error = %v", err)
			}
			requestBody := `{"model":"public","messages":[{"role":"user","content":"hello"}]}`
			request := httptestRequestWithOneByteBody(requestBody)
			response := newBufferedResponseWriter()
			handler.ServeHTTP(response, request)
			if response.statusCode() != http.StatusOK {
				t.Fatalf(
					"status = %d; body=%s",
					response.statusCode(),
					response.body.String(),
				)
			}
			got := response.body.String()
			if !strings.Contains(got, `"model":"public"`) ||
				!strings.Contains(got, test.extraField) {
				t.Fatalf("fixture response = %s", got)
			}
			if transport.calls.Load() != 1 {
				t.Fatalf("transport calls = %d, want 1", transport.calls.Load())
			}
		})
	}
}

func TestChatCompletionsStreamingProviderFixturesAcrossByteFragments(t *testing.T) {
	t.Parallel()

	for _, fixtureName := range []string{
		"openai-stream.sse",
		"vllm-stream.sse",
	} {
		fixtureName := fixtureName
		t.Run(fixtureName, func(t *testing.T) {
			t.Parallel()
			transport := &fixtureTransport{
				contentType: "text/event-stream; charset=utf-8",
				body:        readChatFixture(t, fixtureName),
				chunkBytes:  1,
			}
			handler, err := NewDataHandler(
				proxyDocument("https://provider.example/v1"),
				DataOptions{HTTPClient: &http.Client{Transport: transport}},
			)
			if err != nil {
				t.Fatalf("NewDataHandler() error = %v", err)
			}
			response := newBufferedResponseWriter()
			handler.ServeHTTP(
				response,
				httptestRequestWithOneByteBody(
					`{"model":"public","messages":[],"stream":true,"stream_options":{"include_usage":true}}`,
				),
			)
			if response.statusCode() != http.StatusOK {
				t.Fatalf(
					"status = %d; body=%s",
					response.statusCode(),
					response.body.String(),
				)
			}
			got := response.body.String()
			if !strings.Contains(got, `"model":"public"`) ||
				!strings.Contains(got, "data: [DONE]") {
				t.Fatalf("stream fixture response = %q", got)
			}
		})
	}
}

func TestChatCompletionsValidatesUpstreamModeResponseJSON(t *testing.T) {
	t.Parallel()

	transport := &fixtureTransport{
		contentType: "application/json",
		body:        []byte(`{"invalid"`),
		chunkBytes:  1,
	}
	document := proxyDocument("https://provider.example/v1")
	document.VirtualModels[0].ResponseModel = config.ResponseModelUpstream
	handler, err := NewDataHandler(
		document,
		DataOptions{HTTPClient: &http.Client{Transport: transport}},
	)
	if err != nil {
		t.Fatalf("NewDataHandler() error = %v", err)
	}
	response := serveChat(t, handler, `{"model":"public","messages":[]}`)
	assertOpenAIErrorCode(
		t,
		response,
		http.StatusBadGateway,
		"upstream_response_error",
	)
}

func TestAnthropicOfficialWireFixturesAcrossByteFragments(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		fixture     string
		contentType string
		stream      bool
		expected    []string
	}{
		{
			name:        "message",
			fixture:     "message.json",
			contentType: "application/json",
			expected: []string{
				`"model":"public"`,
				`"text":"Hello!"`,
			},
		},
		{
			name:        "stream",
			fixture:     "message-stream.sse",
			contentType: "text/event-stream",
			stream:      true,
			expected: []string{
				"event: message_start",
				`"model":"public"`,
				"event: ping",
				"event: message_stop",
			},
		},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			transport := &fixtureTransport{
				path:        "/v1/messages",
				contentType: test.contentType,
				body:        readFixture(t, "anthropic", test.fixture),
				chunkBytes:  1,
			}
			handler, err := NewDataHandler(
				anthropicDocument("https://provider.example/v1"),
				DataOptions{
					HTTPClient: &http.Client{Transport: transport},
				},
			)
			if err != nil {
				t.Fatalf("NewDataHandler() error = %v", err)
			}
			body := `{"model":"public","max_tokens":32,` +
				`"messages":[{"role":"user","content":"hello"}]`
			if test.stream {
				body += `,"stream":true`
			}
			body += `}`
			request := httptestRequestWithOneByteBodyFor(
				"/v1/messages",
				body,
			)
			request.Header.Set("Anthropic-Version", anthropicAPIVersion)
			response := newBufferedResponseWriter()
			handler.ServeHTTP(response, request)
			if response.statusCode() != http.StatusOK {
				t.Fatalf(
					"status = %d; body=%s",
					response.statusCode(),
					response.body.String(),
				)
			}
			for _, expected := range test.expected {
				if !strings.Contains(response.body.String(), expected) {
					t.Errorf(
						"fixture response missing %q: %s",
						expected,
						response.body.String(),
					)
				}
			}
			if transport.calls.Load() != 1 {
				t.Fatalf(
					"transport calls = %d",
					transport.calls.Load(),
				)
			}
		})
	}
}

func TestGeminiOfficialWireFixturesAcrossByteFragments(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		fixture     string
		path        string
		contentType string
		expected    []string
	}{
		{
			name:        "generateContent",
			fixture:     "generate-content.json",
			path:        "/v1beta/models/gemini-upstream:generateContent",
			contentType: "application/json",
			expected: []string{
				`"modelVersion":"public"`,
				`"text":"Hello from Gemini."`,
				`"responseId":"response-fixture"`,
			},
		},
		{
			name:        "streamGenerateContent",
			fixture:     "stream-generate-content.sse",
			path:        "/v1beta/models/gemini-upstream:streamGenerateContent",
			contentType: "text/event-stream",
			expected: []string{
				`"modelVersion":"public"`,
				`"text":"Hello "`,
				`: keep-alive`,
				`"futureField":{"kept":true}`,
			},
		},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			transport := &fixtureTransport{
				path:        test.path,
				contentType: test.contentType,
				body:        readFixture(t, "gemini", test.fixture),
				chunkBytes:  1,
			}
			handler, err := NewDataHandler(
				geminiDocument("https://provider.example/v1beta"),
				DataOptions{
					HTTPClient: &http.Client{Transport: transport},
				},
			)
			if err != nil {
				t.Fatalf("NewDataHandler() error = %v", err)
			}
			action := "generateContent"
			if strings.HasPrefix(test.name, "stream") {
				action = "streamGenerateContent"
			}
			request := httptestRequestWithOneByteBodyFor(
				"/v1beta/models/public:"+action,
				`{"contents":[{"role":"user",`+
					`"parts":[{"text":"hello"}]}]}`,
			)
			response := newBufferedResponseWriter()
			handler.ServeHTTP(response, request)
			if response.statusCode() != http.StatusOK {
				t.Fatalf(
					"status = %d; body=%s",
					response.statusCode(),
					response.body.String(),
				)
			}
			for _, expected := range test.expected {
				if !strings.Contains(response.body.String(), expected) {
					t.Errorf(
						"fixture response missing %q: %s",
						expected,
						response.body.String(),
					)
				}
			}
			if transport.calls.Load() != 1 {
				t.Fatalf(
					"transport calls = %d",
					transport.calls.Load(),
				)
			}
		})
	}
}

func readChatFixture(t *testing.T, name string) []byte {
	t.Helper()
	return readFixture(t, "chat", name)
}

func readFixture(t *testing.T, family, name string) []byte {
	t.Helper()
	body, err := os.ReadFile(filepath.Join("testdata", family, name))
	if err != nil {
		t.Fatalf("read fixture %q: %v", name, err)
	}
	return body
}

func httptestRequestWithOneByteBody(body string) *http.Request {
	return httptestRequestWithOneByteBodyFor("/v1/chat/completions", body)
}

func httptestRequestWithOneByteBodyFor(
	path string,
	body string,
) *http.Request {
	request, _ := http.NewRequest(
		http.MethodPost,
		"http://gateway.example"+path,
		io.NopCloser(&chunkReader{
			source: bytes.NewReader([]byte(body)),
			max:    1,
		}),
	)
	request.Header.Set("Content-Type", "application/json")
	return request
}

type fixtureTransport struct {
	path        string
	contentType string
	body        []byte
	chunkBytes  int
	calls       atomic.Int64
}

func (t *fixtureTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	t.calls.Add(1)
	path := t.path
	if path == "" {
		path = "/v1/chat/completions"
	}
	if request.URL.Path != path {
		return &http.Response{
			StatusCode: http.StatusNotFound,
			Header:     make(http.Header),
			Body:       http.NoBody,
			Request:    request,
		}, nil
	}
	return &http.Response{
		StatusCode: http.StatusOK,
		Header: http.Header{
			"Content-Type": []string{t.contentType},
		},
		Body: io.NopCloser(&chunkReader{
			source: bytes.NewReader(t.body),
			max:    t.chunkBytes,
		}),
		Request: request,
	}, nil
}

type chunkReader struct {
	source *bytes.Reader
	max    int
}

func (r *chunkReader) Read(destination []byte) (int, error) {
	if r.max > 0 && len(destination) > r.max {
		destination = destination[:r.max]
	}
	return r.source.Read(destination)
}

var _ http.RoundTripper = (*fixtureTransport)(nil)
