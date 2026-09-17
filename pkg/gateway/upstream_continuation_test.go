// SPDX-FileCopyrightText: 2026 Scitrera LLC
// SPDX-FileCopyrightText: 2026 Fox Engine Ltd.
// SPDX-License-Identifier: AGPL-3.0-only

package gateway

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sparksq/sparkroute/pkg/config"
	"github.com/sparksq/sparkroute/pkg/ledger"
	"github.com/sparksq/sparkroute/pkg/routing"
)

func TestUpstreamContinuationsAcrossProtocols(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, provider, path, request, response, fixture string
		stream                                           bool
	}{
		{"chat", "openai", "/v1/chat/completions", `{"model":"public","messages":[]}`, `{"model":"upstream-model","choices":[{"message":{"role":"assistant","content":"READY"}}]}`, "", false},
		{"responses", "openai_responses", "/v1/responses", `{"model":"public","input":"hello","store":false}`, `{"id":"resp_ready","object":"response","status":"completed","model":"upstream-model","output":[]}`, "", false},
		{"responses-stream", "openai_responses", "/v1/responses", `{"model":"public","input":"hello","store":false,"stream":true}`, "event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_ready\",\"object\":\"response\",\"status\":\"completed\",\"model\":\"upstream-model\",\"output\":[]}}\n\n", "", true},
		{"compact", "openai_responses", "/v1/responses/compact", `{"model":"public","input":"hello"}`, `{"id":"resp_compact","object":"response.compaction","created_at":1770000000,"output":[{"id":"cmp_ready","type":"compaction","encrypted_content":"opaque-state"}],"usage":{"input_tokens":1,"output_tokens":1,"total_tokens":2}}`, "", false},
		{"embeddings", "openai_compatible", "/v1/embeddings", `{"model":"public","input":"hello"}`, `{"object":"list","model":"upstream-model","data":[{"object":"embedding","index":0,"embedding":[0.25]}]}`, "", false},
		{"anthropic", "anthropic", "/v1/messages", `{"model":"public","max_tokens":20,"messages":[{"role":"user","content":"hello"}]}`, "", "testdata/anthropic/message.json", false},
		{"anthropic-stream", "anthropic", "/v1/messages", `{"model":"public","max_tokens":20,"messages":[{"role":"user","content":"hello"}],"stream":true}`, "", "testdata/anthropic/message-stream.sse", true},
		{"gemini", "gemini", "/v1beta/models/public:generateContent", `{"contents":[{"role":"user","parts":[{"text":"hello"}]}]}`, "", "testdata/gemini/generate-content.json", false},
		{"gemini-stream", "gemini", "/v1beta/models/public:streamGenerateContent", `{"contents":[{"role":"user","parts":[{"text":"hello"}]}]}`, "", "testdata/gemini/stream-generate-content.sse", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			payload := tc.response
			if tc.fixture != "" {
				raw, err := os.ReadFile(tc.fixture)
				if err != nil {
					t.Fatal(err)
				}
				payload = string(raw)
			}
			for _, status := range []int{http.StatusOK, http.StatusServiceUnavailable, http.StatusUnauthorized} {
				t.Run(fmt.Sprint(status), func(t *testing.T) {
					var posts, gets atomic.Int32
					upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
						raw, _ := io.ReadAll(r.Body)
						if tc.provider == "openai_compatible" && r.Header.Get("Authorization") != "Bearer test-token" {
							t.Error("bearer credential lost")
						}
						if tc.provider == "anthropic" && (r.Header.Get("X-Api-Key") != "test-token" || r.Header.Get("Anthropic-Version") != anthropicAPIVersion) {
							t.Error("Anthropic authentication or protocol header lost")
						}
						if r.Method == http.MethodPost {
							posts.Add(1)
							if len(raw) == 0 {
								t.Error("missing inference body")
							}
							w.Header().Set("Location", "/result?private-token=1")
							w.WriteHeader(http.StatusSeeOther)
							return
						}
						if r.Method != http.MethodGet || len(raw) != 0 || r.Header.Get("Content-Type") != "" || r.Header.Get("Digest") != "" || r.Header.Get("Content-Language") != "" {
							t.Error("continuation retained inference content")
						}
						if gets.Add(1) == 1 {
							w.Header().Set("Location", "?private-token=2")
							w.WriteHeader(http.StatusSeeOther)
							return
						}
						if r.URL.Path != "/result" || r.URL.Query().Get("private-token") != "2" {
							t.Error("relative continuation was not resolved against the preceding GET")
						}
						if tc.stream {
							w.Header().Set("Content-Type", "text/event-stream")
						} else {
							w.Header().Set("Content-Type", "application/json")
						}
						w.WriteHeader(status)
						_, _ = io.WriteString(w, payload)
					}))
					defer upstream.Close()
					doc := twoTargetDocument(upstream.URL+"/v1", upstream.URL+"/v1")
					for i := range doc.Providers {
						p := &doc.Providers[i]
						p.Type, p.Continuations = tc.provider, config.ContinuationsSameOrigin303
						p.DefaultHeaders = map[string]config.HeaderValue{"Digest": {Value: "old-body"}, "Content-Language": {Value: "en"}}
						switch tc.provider {
						case "openai_compatible":
							p.Auth = config.ProviderAuth{Type: config.AuthBearer, Credential: "env://TOKEN"}
						case "anthropic":
							p.Auth = config.ProviderAuth{Type: config.AuthHeader, Header: "X-Api-Key", Credential: "env://TOKEN"}
						}
					}
					for i := range doc.Deployments {
						doc.Deployments[i].Capabilities = []config.Capability{config.CapabilityResponses, config.CapabilityResponsesCompact, config.CapabilitySingleVectorEmbedding}
					}
					doc.VirtualModels[0].Limits.MaxAttempts = 3
					handler, err := NewDataHandler(doc, DataOptions{HTTPClient: upstream.Client(), Credentials: fakeCredentialSource{"env://TOKEN": "test-token"}})
					if err != nil {
						t.Fatal(err)
					}
					request := httptest.NewRequest(http.MethodPost, tc.path, strings.NewReader(tc.request))
					request.Header.Set("Content-Type", "application/json")
					request.Header.Set("Anthropic-Version", anthropicAPIVersion)
					out := httptest.NewRecorder()
					handler.ServeHTTP(out, request)
					want := http.StatusOK
					if status != http.StatusOK {
						want = http.StatusBadGateway
					}
					if out.Code != want || posts.Load() != 1 || gets.Load() != 2 {
						t.Fatalf("status=%d posts=%d gets=%d body=%s", out.Code, posts.Load(), gets.Load(), out.Body)
					}
					if out.Header().Get("Location") != "" || strings.Contains(out.Body.String(), "private-token") {
						t.Fatal("continuation URL exposed downstream")
					}
					if status == http.StatusOK && out.Body.Len() == 0 {
						t.Fatal("result lost")
					}
					if status == http.StatusOK && tc.stream && !strings.Contains(out.Header().Get("Content-Type"), "text/event-stream") {
						t.Fatal("stream framing lost")
					}
				})
			}
		})
	}
}

func TestUpstreamContinuationPolicyAndRedirectBoundaries(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, provider string
		policy         config.ContinuationPolicy
		modalHeaders   int
		status         int
		follow         bool
	}{
		{"omitted", "openai", "", 0, 303, false},
		{"legacy-modal", "openai_compatible", "", 2, 303, true},
		{"incomplete-modal-auth", "openai_compatible", "", 1, 303, false},
		{"legacy-other-provider", "anthropic", "", 2, 303, false},
		{"disable-modal", "openai_compatible", config.ContinuationsNone, 2, 303, false},
		{"public", "anthropic", config.ContinuationsSameOrigin303, 0, 303, true},
		{"301", "openai", config.ContinuationsSameOrigin303, 0, 301, false},
		{"302", "openai", config.ContinuationsSameOrigin303, 0, 302, false},
		{"307", "openai", config.ContinuationsSameOrigin303, 0, 307, false},
		{"308", "openai", config.ContinuationsSameOrigin303, 0, 308, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var posts, gets atomic.Int32
			upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method == http.MethodPost {
					posts.Add(1)
					w.Header().Set("Location", "/result")
					w.WriteHeader(tc.status)
				} else {
					gets.Add(1)
					w.WriteHeader(http.StatusOK)
				}
			}))
			defer upstream.Close()
			client := upstream.Client()
			client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
			h := &chatCompletionsHandler{client: client}
			request, _ := http.NewRequestWithContext(t.Context(), http.MethodPost, upstream.URL, strings.NewReader("inference"))
			if tc.modalHeaders >= 1 {
				request.Header.Set("Modal-Key", "key")
			}
			if tc.modalHeaders == 2 {
				request.Header.Set("Modal-Secret", "secret")
			}
			response, err := h.sendUpstream(request, routing.Selection{Provider: config.Provider{Type: tc.provider, Continuations: tc.policy}})
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = response.Body.Close() }()
			wantStatus, wantGets := tc.status, int32(0)
			if tc.follow {
				wantStatus, wantGets = http.StatusOK, 1
			}
			if response.StatusCode != wantStatus || posts.Load() != 1 || gets.Load() != wantGets {
				t.Fatalf("status=%d posts=%d gets=%d", response.StatusCode, posts.Load(), gets.Load())
			}
		})
	}
}

func TestUpstreamContinuationSignsEachBedrockGET(t *testing.T) {
	t.Parallel()
	var posts, gets atomic.Int32
	var previousSignature atomic.Value
	upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		signature := r.Header.Get("Authorization")
		if !strings.Contains(signature, "Credential=AKIDEXAMPLE/") || signature == previousSignature.Load() {
			t.Error("missing or reused request signature")
		}
		previousSignature.Store(signature)
		if r.Method == http.MethodPost {
			posts.Add(1)
			if len(body) == 0 {
				t.Error("missing inference body")
			}
			w.Header().Set("Location", "/result?hop=1")
			w.WriteHeader(http.StatusSeeOther)
			return
		}
		emptyHash := sha256.Sum256(nil)
		if r.Method != http.MethodGet || len(body) != 0 || r.Header.Get("X-Amz-Content-Sha256") != hex.EncodeToString(emptyHash[:]) || r.Header.Get("Content-Type") != "" || strings.Contains(signature, "content-type;") {
			t.Error("GET signature retained POST content")
		}
		if gets.Add(1) == 1 {
			w.Header().Set("Location", "?hop=2")
			w.WriteHeader(http.StatusSeeOther)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"output":{"message":{"role":"assistant","content":[{"text":"READY"}]}},"stopReason":"end_turn","usage":{"inputTokens":1,"outputTokens":1,"totalTokens":2},"metrics":{"latencyMs":1}}`)
	}))
	defer upstream.Close()
	doc := bedrockDocument(upstream.URL)
	doc.Providers[0].Continuations = config.ContinuationsSameOrigin303
	// A deployment credential must also be used to sign retrieval requests.
	doc.Providers[0].Auth.Credential = "workload://aws/default"
	doc.Deployments[0].Credential = "workload://aws"
	handler, err := NewDataHandler(doc, DataOptions{HTTPClient: upstream.Client(), Credentials: bedrockCredentialSource()})
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodPost, "/model/public/converse", strings.NewReader(`{"messages":[{"role":"user","content":[{"text":"hello"}]}]}`))
	request.Header.Set("Content-Type", "application/json")
	out := httptest.NewRecorder()
	handler.ServeHTTP(out, request)
	if out.Code != http.StatusOK || !strings.Contains(out.Body.String(), "READY") || posts.Load() != 1 || gets.Load() != 2 {
		t.Fatalf("status=%d posts=%d gets=%d body=%s", out.Code, posts.Load(), gets.Load(), out.Body)
	}
}

func TestUpstreamContinuationDeadlineAndTransportFailures(t *testing.T) {
	t.Parallel()
	for _, mode := range []string{"deadline", "transport", "signing", "fragment", "insecure", "foreign", "loop", "missing", "userinfo", "downgrade", "replay-redirect", "subscription-unauthorized"} {
		t.Run(mode, func(t *testing.T) {
			var posts, gets int
			ctx, cancel := context.WithTimeout(t.Context(), time.Second)
			defer cancel()
			transport := bedrockRoundTripFunc(func(r *http.Request) (*http.Response, error) {
				if r.Context() != ctx {
					t.Error("attempt context changed")
				}
				location := "/result?private-token=secret"
				status := http.StatusSeeOther
				if r.Method == http.MethodPost {
					posts++
					switch mode {
					case "fragment":
						location += "#fragment"
					case "foreign":
						location = "https://foreign.example/result"
					case "missing":
						location = ""
					case "userinfo":
						location = "https://user@upstream.example/result"
					case "downgrade":
						location = "http://upstream.example/result"
					}
				} else {
					gets++
					switch mode {
					case "deadline":
						<-r.Context().Done()
						return nil, r.Context().Err()
					case "transport":
						return nil, fmt.Errorf("failed %s", r.URL)
					case "replay-redirect":
						status = http.StatusTemporaryRedirect
					case "subscription-unauthorized":
						status = http.StatusUnauthorized
					}
				}
				return &http.Response{StatusCode: status, Header: http.Header{"Location": {location}}, Body: io.NopCloser(strings.NewReader("")), Request: r}, nil
			})
			h := &chatCompletionsHandler{client: &http.Client{Transport: transport, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}
			endpoint := "https://upstream.example"
			if mode == "insecure" {
				endpoint = "http://upstream.example"
			}
			request, _ := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader("inference"))
			selection := routing.Selection{Provider: config.Provider{Continuations: config.ContinuationsSameOrigin303}}
			if mode == "signing" {
				selection.Provider.Auth.Type = config.AuthAWSSigV4
			}
			if mode == "subscription-unauthorized" {
				selection.Provider.Type = "openai_subscription"
			}
			response, err := h.sendUpstream(request, selection)
			if response != nil || !errors.Is(err, errUpstreamContinuation) || strings.Contains(err.Error(), "private-token") || posts != 1 || gets > maxUpstreamContinuations {
				t.Fatalf("response=%v err=%v posts=%d gets=%d", response, err, posts, gets)
			}
			wantGets := 0
			switch mode {
			case "loop":
				wantGets = maxUpstreamContinuations
			case "deadline", "transport", "replay-redirect", "subscription-unauthorized":
				wantGets = 1
			}
			if gets != wantGets {
				t.Fatalf("retrieval calls=%d want=%d", gets, wantGets)
			}
		})
	}
}

func TestUpstreamContinuationBrokenBodiesDoNotRetry(t *testing.T) {
	t.Parallel()
	for _, stream := range []bool{false, true} {
		t.Run(fmt.Sprint(stream), func(t *testing.T) {
			var posts, gets atomic.Int32
			upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method == http.MethodPost {
					posts.Add(1)
					w.Header().Set("Location", "/result")
					w.WriteHeader(http.StatusSeeOther)
					return
				}
				gets.Add(1)
				w.Header().Set("Content-Length", "1000") // Force premature EOF during retrieval.
				if stream {
					w.Header().Set("Content-Type", "text/event-stream")
					_, _ = io.WriteString(w, "data: {\"model\":\"upstream-model\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"partial\"}}]}\n\n")
				} else {
					w.Header().Set("Content-Type", "application/json")
					_, _ = io.WriteString(w, "{}")
				}
			}))
			defer upstream.Close()
			doc := twoTargetDocument(upstream.URL, upstream.URL)
			for i := range doc.Providers {
				doc.Providers[i].Continuations = config.ContinuationsSameOrigin303
			}
			doc.VirtualModels[0].Limits.MaxAttempts = 3
			recorder := &collectingRecorder{}
			handler, err := NewDataHandler(doc, DataOptions{HTTPClient: upstream.Client(), Ledger: recorder})
			if err != nil {
				t.Fatal(err)
			}
			request := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(fmt.Sprintf(`{"model":"public","messages":[],"stream":%t}`, stream)))
			request.Header.Set("Content-Type", "application/json")
			out := httptest.NewRecorder()
			handler.ServeHTTP(out, request)
			if posts.Load() != 1 || gets.Load() != 1 {
				t.Fatalf("posts=%d gets=%d", posts.Load(), gets.Load())
			}
			if !stream && out.Code != http.StatusBadGateway {
				t.Fatalf("status=%d body=%s", out.Code, out.Body)
			}
			records := recorder.snapshot()
			if len(records) != 2 || records[0].Attempt == nil || records[0].Attempt.Outcome == ledger.OutcomeSuccess || records[0].Attempt.Retried {
				t.Fatalf("broken result not recorded as terminal failure: %#v", records)
			}
		})
	}
}
