// SPDX-FileCopyrightText: 2026 Scitrera LLC
// SPDX-FileCopyrightText: 2026 Fox Engine Ltd.
// SPDX-License-Identifier: AGPL-3.0-only

package gateway

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestNamedProfileOverridesCallerAndKeepsDeploymentModel(t *testing.T) {
	captured := make(chan map[string]json.RawMessage, 1)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]json.RawMessage
		_ = json.NewDecoder(r.Body).Decode(&body)
		captured <- body
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"test","model":"upstream-model","choices":[]}`)
	}))
	defer upstream.Close()
	document := proxyDocument(upstream.URL + "/v1")
	profile := document.VirtualModels[0]
	profile.Name = "coding:xhigh"
	profile.Aliases = []string{"code:xhigh"}
	profile.RequestOverrides = map[string]map[string]json.RawMessage{"chat_completions": {"reasoning_effort": json.RawMessage(`"xhigh"`), "chat_template_kwargs": json.RawMessage(`{"enable_thinking":true}`)}}
	document.VirtualModels = append(document.VirtualModels, profile)
	handler, err := NewDataHandler(document, DataOptions{})
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"code:xhigh","messages":[{"role":"user","content":"hello"}],"reasoning_effort":"low","chat_template_kwargs":{"enable_thinking":false,"other":true}}`))
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != 200 {
		t.Fatalf("%d: %s", response.Code, response.Body.String())
	}
	body := <-captured
	if string(body["reasoning_effort"]) != `"xhigh"` || string(body["model"]) != `"upstream-model"` {
		t.Fatalf("profile/model not applied: %s", body)
	}
	var nested map[string]bool
	_ = json.Unmarshal(body["chat_template_kwargs"], &nested)
	if !nested["enable_thinking"] || !nested["other"] {
		t.Fatal("nested caller fields were lost", nested)
	}
}

func TestRequestProfilesCannotChangeRoutingOrRequestContents(t *testing.T) {
	for _, key := range []string{"model", "messages", "input", "stream", "tools", "contents", "system", "previous_response_id"} {
		document := proxyDocument("http://localhost:8000")
		document.VirtualModels[0].RequestOverrides = map[string]map[string]json.RawMessage{"chat_completions": {key: json.RawMessage(`true`)}}
		if err := document.Validate(); err == nil {
			t.Fatalf("accepted %s override", key)
		}
	}
}
