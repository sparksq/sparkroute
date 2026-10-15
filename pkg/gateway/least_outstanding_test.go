// SPDX-FileCopyrightText: 2026 Scitrera LLC
// SPDX-FileCopyrightText: 2026 Fox Engine Ltd.
// SPDX-License-Identifier: AGPL-3.0-only

package gateway

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sparksq/sparkroute/pkg/config"
)

// TestChatCompletionsLeastOutstandingAvoidsBusyTarget holds one request open on
// the first target and checks where the next request lands. The picker always
// draws the first candidate, so only the in-flight count can move the second
// request off the busy target.
func TestChatCompletionsLeastOutstandingAvoidsBusyTarget(t *testing.T) {
	t.Parallel()

	for _, test := range []struct {
		mode       config.SelectionMode
		wantFirst  int64
		wantSecond int64
	}{
		{mode: config.SelectionLeastOutstanding, wantFirst: 1, wantSecond: 1},
		{mode: config.SelectionWeightedRandom, wantFirst: 2, wantSecond: 0},
	} {
		test := test
		t.Run(string(test.mode), func(t *testing.T) {
			t.Parallel()

			release := make(chan struct{})
			entered := make(chan struct{}, 2)
			var firstCalls, secondCalls atomic.Int64
			reply := func(w http.ResponseWriter, model string) {
				writeJSON(w, http.StatusOK, map[string]any{"id": "chatcmpl", "model": model, "choices": []any{}})
			}
			first := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				firstCalls.Add(1)
				entered <- struct{}{}
				<-release
				reply(w, "upstream")
			}))
			defer first.Close()
			second := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				secondCalls.Add(1)
				reply(w, "upstream-second")
			}))
			defer second.Close()

			document := twoTargetDocument(first.URL+"/v1", second.URL+"/v1")
			document.VirtualModels[0].Selection = config.SelectionPolicy{Mode: test.mode}
			handler, err := NewDataHandler(document, DataOptions{RoutingPicker: zeroPicker{}})
			if err != nil {
				t.Fatalf("NewDataHandler() error = %v", err)
			}
			send := func() *httptest.ResponseRecorder {
				request := httptest.NewRequest(
					http.MethodPost,
					"/v1/chat/completions",
					strings.NewReader(`{"model":"public","messages":[]}`),
				)
				request.Header.Set("Content-Type", "application/json")
				response := httptest.NewRecorder()
				handler.ServeHTTP(response, request)
				return response
			}

			done := make(chan *httptest.ResponseRecorder, 2)
			pending := 2
			go func() { done <- send() }()
			select {
			case <-entered:
			case <-time.After(10 * time.Second):
				t.Fatal("first request never reached the first target")
			}
			go func() { done <- send() }()
			if test.mode == config.SelectionLeastOutstanding {
				// The second request must complete on its own, on the idle target.
				select {
				case response := <-done:
					pending--
					if response.Code != http.StatusOK {
						t.Fatalf("second status = %d; body=%s", response.Code, response.Body)
					}
				case <-time.After(10 * time.Second):
					t.Fatal("second request queued behind the busy target")
				}
			} else {
				select {
				case <-entered:
				case <-time.After(10 * time.Second):
					t.Fatal("second request never reached the first target")
				}
			}
			close(release)
			for ; pending > 0; pending-- {
				select {
				case response := <-done:
					if response.Code != http.StatusOK {
						t.Fatalf("status = %d; body=%s", response.Code, response.Body)
					}
				case <-time.After(10 * time.Second):
					t.Fatal("request did not complete after release")
				}
			}
			if got := firstCalls.Load(); got != test.wantFirst {
				t.Fatalf("first target calls = %d, want %d", got, test.wantFirst)
			}
			if got := secondCalls.Load(); got != test.wantSecond {
				t.Fatalf("second target calls = %d, want %d", got, test.wantSecond)
			}
		})
	}
}
