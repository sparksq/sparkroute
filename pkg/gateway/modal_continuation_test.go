// SPDX-FileCopyrightText: 2026 Scitrera LLC
// SPDX-FileCopyrightText: 2026 Fox Engine Ltd.
// SPDX-License-Identifier: AGPL-3.0-only

package gateway

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sparksq/sparkroute/pkg/config"
	"github.com/sparksq/sparkroute/pkg/routing"
)

func modalTestHandler(t *testing.T, upstream *httptest.Server) http.Handler {
	t.Helper()
	document := twoTargetDocument(upstream.URL, upstream.URL)
	for i := range document.Providers {
		document.Providers[i].DefaultHeaders = map[string]config.HeaderValue{
			"Modal-Key": {ValueFrom: "env://MODAL_KEY"}, "Modal-Secret": {ValueFrom: "env://MODAL_SECRET"},
		}
		document.Providers[i].Auth = config.ProviderAuth{Type: config.AuthBearer, Credential: "env://UPSTREAM_TOKEN"}
	}
	document.VirtualModels[0].Limits.MaxAttempts = 3
	handler, err := NewDataHandler(document, DataOptions{HTTPClient: upstream.Client(), Credentials: fakeCredentialSource{
		"env://MODAL_KEY": "test-key", "env://MODAL_SECRET": "test-secret", "env://UPSTREAM_TOKEN": "test-upstream",
	}})
	if err != nil {
		t.Fatal(err)
	}
	return handler
}

func TestModalContinuationPreservesResultWithoutReplayingPOST(t *testing.T) {
	t.Parallel()
	for _, stream := range []bool{false, true} {
		t.Run(fmt.Sprint(stream), func(t *testing.T) {
			var posts, gets atomic.Int64
			upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Header.Get("Modal-Key") != "test-key" || r.Header.Get("Modal-Secret") != "test-secret" || r.Header.Get("Authorization") != "Bearer test-upstream" {
					t.Error("configured credentials missing from upstream request")
				}
				body, _ := io.ReadAll(r.Body)
				if r.Method == http.MethodPost {
					posts.Add(1)
					if len(body) == 0 {
						t.Error("inference body missing")
					}
					w.Header().Set("Location", "/result?attempt=1")
					w.WriteHeader(http.StatusSeeOther)
					return
				}
				if r.Method != http.MethodGet || len(body) != 0 || r.Header.Get("Content-Type") != "" {
					t.Error("continuation replayed request content")
				}
				if gets.Add(1) == 1 {
					w.Header().Set("Location", "?attempt=2")
					w.WriteHeader(http.StatusSeeOther)
					return
				}
				if stream {
					w.Header().Set("Content-Type", "text/event-stream")
					_, _ = io.WriteString(w, "data: {\"id\":\"test\",\"model\":\"upstream-model\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"READY\"}}]}\n\ndata: [DONE]\n\n")
				} else {
					w.Header().Set("Content-Type", "application/json")
					_, _ = io.WriteString(w, `{"id":"test","model":"upstream-model","choices":[{"index":0,"message":{"role":"assistant","content":"READY"}}]}`)
				}
			}))
			defer upstream.Close()
			handler := modalTestHandler(t, upstream)
			request := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(fmt.Sprintf(`{"model":"public","messages":[],"stream":%t}`, stream)))
			request.Header.Set("Content-Type", "application/json")
			out := httptest.NewRecorder()
			handler.ServeHTTP(out, request)
			if out.Code != http.StatusOK || !strings.Contains(out.Body.String(), "READY") || posts.Load() != 1 || gets.Load() != 2 {
				t.Fatalf("status=%d posts=%d gets=%d body=%s", out.Code, posts.Load(), gets.Load(), out.Body)
			}
			if out.Header().Get("Location") != "" {
				t.Fatal("continuation token exposed downstream")
			}
		})
	}
}

func TestModalContinuationFailuresAreTerminalAndDoNotLeakCredentials(t *testing.T) {
	t.Parallel()
	for _, mode := range []string{"foreign", "downgrade", "userinfo", "missing", "loop", "unavailable", "replay-redirect"} {
		t.Run(mode, func(t *testing.T) {
			var posts, gets, foreignCalls atomic.Int64
			foreign := httptest.NewTLSServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { foreignCalls.Add(1) }))
			defer foreign.Close()
			upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method == http.MethodPost {
					posts.Add(1)
				} else {
					gets.Add(1)
				}
				if r.Method == http.MethodGet && mode == "unavailable" {
					w.WriteHeader(http.StatusServiceUnavailable)
					return
				}
				if r.Method == http.MethodGet && mode == "replay-redirect" {
					w.Header().Set("Location", "/v1/chat/completions")
					w.WriteHeader(http.StatusTemporaryRedirect)
					return
				}
				location := "/result?private-signed-token=secret"
				switch mode {
				case "foreign":
					location = foreign.URL + location
				case "downgrade":
					location = "http://" + r.Host + location
				case "userinfo":
					location = "https://user@" + r.Host + location
				case "missing":
					location = ""
				}
				if location != "" {
					w.Header().Set("Location", location)
				}
				w.WriteHeader(http.StatusSeeOther)
			}))
			defer upstream.Close()
			out := httptest.NewRecorder()
			request := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"public","messages":[]}`))
			request.Header.Set("Content-Type", "application/json")
			modalTestHandler(t, upstream).ServeHTTP(out, request)
			if out.Code != http.StatusBadGateway || posts.Load() != 1 || foreignCalls.Load() != 0 || gets.Load() > maxModalContinuations {
				t.Fatalf("status=%d posts=%d gets=%d foreign=%d", out.Code, posts.Load(), gets.Load(), foreignCalls.Load())
			}
			if out.Header().Get("Location") != "" || strings.Contains(out.Body.String(), "private-signed-token") {
				t.Fatal("signed URL leaked")
			}
		})
	}
}

func TestModalContinuationRetainsRequestDeadline(t *testing.T) {
	t.Parallel()
	var posts atomic.Int64
	upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			posts.Add(1)
			w.Header().Set("Location", "/result")
			w.WriteHeader(http.StatusSeeOther)
			return
		}
		<-r.Context().Done()
	}))
	defer upstream.Close()
	client := upstream.Client()
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	h := &chatCompletionsHandler{client: client}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, upstream.URL, strings.NewReader("body"))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Modal-Key", "test-key")
	request.Header.Set("Modal-Secret", "test-secret")
	_, err = h.sendUpstream(request, routing.Selection{Provider: config.Provider{Type: "openai_compatible"}})
	if !errors.Is(err, errUpstreamContinuation) || ctx.Err() == nil || posts.Load() != 1 {
		t.Fatalf("err=%v posts=%d", err, posts.Load())
	}
}
