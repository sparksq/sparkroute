// SPDX-FileCopyrightText: 2026 Scitrera LLC
// SPDX-FileCopyrightText: 2026 Fox Engine Ltd.
// SPDX-License-Identifier: AGPL-3.0-only

package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sparksq/sparkroute/pkg/config"
	"github.com/sparksq/sparkroute/pkg/identity"
	"github.com/sparksq/sparkroute/pkg/responsesstate"
)

type failingItemBindStore struct {
	*responsesstate.MemoryStore
}

func (s failingItemBindStore) BindResource(
	ctx context.Context,
	affinity responsesstate.ResourceAffinity,
) error {
	if affinity.Kind == responsesstate.ResourceItem {
		return errors.New("item state unavailable")
	}
	return s.MemoryStore.BindResource(ctx, affinity)
}

func TestResponsesItemAffinityPinsBareAndHostedInputs(t *testing.T) {
	t.Parallel()

	var firstCalls atomic.Int64
	first := httptest.NewServer(http.HandlerFunc(func(
		http.ResponseWriter,
		*http.Request,
	) {
		firstCalls.Add(1)
	}))
	defer first.Close()
	var secondCalls atomic.Int64
	second := httptest.NewServer(http.HandlerFunc(func(
		w http.ResponseWriter,
		request *http.Request,
	) {
		secondCalls.Add(1)
		raw, _ := io.ReadAll(request.Body)
		if !strings.Contains(string(raw), `"model":"upstream-second"`) {
			t.Errorf("upstream body = %s", raw)
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"id": "resp_item", "object": "response",
			"model": "upstream-second", "status": "completed", "output": []any{},
		})
	}))
	defer second.Close()

	document := twoTargetDocument(first.URL+"/v1", second.URL+"/v1")
	for index := range document.Deployments {
		document.Deployments[index].Capabilities = append(
			document.Deployments[index].Capabilities,
			config.CapabilityResponses,
			config.CapabilityStoredCompletion,
			config.CapabilityProviderTools,
		)
	}
	state := responsesstate.NewMemoryStore(responsesstate.MemoryOptions{})
	for _, itemID := range []string{"msg_second", "ws_second"} {
		bindTestItemAffinity(
			t,
			state,
			identity.Identity{},
			itemID,
			"public",
			"provider-second",
			"deployment-second",
			"upstream-second",
		)
	}
	handler, err := NewDataHandler(document, DataOptions{
		ResponsesState: state,
		RoutingPicker:  zeroPicker{},
	})
	if err != nil {
		t.Fatalf("NewDataHandler() error = %v", err)
	}
	for _, body := range []string{
		`{"model":"public","store":false,"input":[{"id":"msg_second"}]}`,
		`{"model":"public","store":false,"input":[{"type":"web_search_call",` +
			`"id":"ws_second","status":"completed","action":{"type":"search","query":"x"}}]}`,
	} {
		response := serveResponsesRequest(
			t,
			handler,
			http.MethodPost,
			"/v1/responses",
			body,
			identity.Identity{},
		)
		if response.Code != http.StatusOK {
			t.Fatalf("response = status:%d body:%s", response.Code, response.Body)
		}
	}
	if firstCalls.Load() != 0 || secondCalls.Load() != 2 {
		t.Fatalf(
			"upstream calls = first:%d second:%d",
			firstCalls.Load(),
			secondCalls.Load(),
		)
	}
}

func TestResponsesItemAffinityFailsClosed(t *testing.T) {
	t.Parallel()

	var calls atomic.Int64
	first := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		calls.Add(1)
	}))
	defer first.Close()
	second := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		calls.Add(1)
	}))
	defer second.Close()
	document := twoTargetDocument(first.URL+"/v1", second.URL+"/v1")
	for index := range document.Deployments {
		document.Deployments[index].Capabilities = append(
			document.Deployments[index].Capabilities,
			config.CapabilityResponses,
			config.CapabilityStoredCompletion,
		)
	}
	state := responsesstate.NewMemoryStore(responsesstate.MemoryOptions{})
	bindTestItemAffinity(
		t, state, identity.Identity{}, "item_first", "public",
		"provider", "deployment", "upstream-model",
	)
	bindTestItemAffinity(
		t, state, identity.Identity{}, "item_second", "public",
		"provider-second", "deployment-second", "upstream-second",
	)
	bindTestItemAffinity(
		t, state, identity.Identity{}, "item_other_model", "other",
		"provider", "deployment", "upstream-model",
	)
	handler, err := NewDataHandler(document, DataOptions{ResponsesState: state})
	if err != nil {
		t.Fatalf("NewDataHandler() error = %v", err)
	}
	tests := []struct {
		body     string
		identity identity.Identity
		code     string
	}{
		{
			body: `{"model":"public","store":false,"input":[` +
				`{"type":"item_reference","id":"item_unknown"}]}`,
			code: "state_affinity_not_found",
		},
		{
			body: `{"model":"public","store":false,"input":[` +
				`{"type":"item_reference","id":"item_first"},` +
				`{"type":"item_reference","id":"item_second"}]}`,
			code: "state_affinity_conflict",
		},
		{
			body: `{"model":"public","store":false,"input":[` +
				`{"type":"item_reference","id":"item_other_model"}]}`,
			code: "state_affinity_model_mismatch",
		},
		{
			body: `{"model":"public","store":false,"input":[` +
				`{"type":"item_reference","id":"item_first"}]}`,
			identity: identity.Identity{Principal: identity.Principal{ID: "other"}},
			code:     "state_affinity_not_found",
		},
	}
	for _, test := range tests {
		response := serveResponsesRequest(
			t,
			handler,
			http.MethodPost,
			"/v1/responses",
			test.body,
			test.identity,
		)
		if response.Code != http.StatusConflict {
			t.Errorf("response = status:%d body:%s", response.Code, response.Body)
			continue
		}
		var body openAIErrorEnvelope
		if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil ||
			body.Error.Code != test.code {
			t.Errorf("error = %#v, %v; want %q", body, err, test.code)
		}
	}
	if calls.Load() != 0 {
		t.Fatalf("upstream calls = %d, want zero", calls.Load())
	}
}

func TestResponsesOutputItemsBindIDsAndCallIDs(t *testing.T) {
	t.Parallel()

	var calls atomic.Int64
	upstream := httptest.NewServer(http.HandlerFunc(func(
		w http.ResponseWriter,
		request *http.Request,
	) {
		call := calls.Add(1)
		raw, _ := io.ReadAll(request.Body)
		switch call {
		case 1:
			writeJSON(w, http.StatusOK, map[string]any{
				"id": "resp_stateless", "object": "response",
				"model": "upstream-model", "status": "completed",
				"output": []any{
					map[string]any{
						"id": "comp_1", "call_id": "call_1",
						"type": "computer_call", "status": "completed",
					},
					map[string]any{
						"id": "msg_1", "type": "message", "role": "assistant",
						"status": "completed", "content": []any{},
					},
				},
			})
		case 2:
			if !strings.Contains(string(raw), `"call_id":"call_1"`) {
				t.Errorf("computer output body = %s", raw)
			}
			writeJSON(w, http.StatusOK, map[string]any{
				"id": "resp_computer", "object": "response",
				"model": "upstream-model", "status": "completed", "output": []any{},
			})
		case 3:
			if !strings.Contains(string(raw), `"id":"msg_1"`) {
				t.Errorf("item reference body = %s", raw)
			}
			writeJSON(w, http.StatusOK, map[string]any{
				"id": "resp_reference", "object": "response",
				"model": "upstream-model", "status": "completed", "output": []any{},
			})
		default:
			t.Errorf("unexpected upstream call %d", call)
			w.WriteHeader(http.StatusInternalServerError)
		}
	}))
	defer upstream.Close()
	document := responsesDocument(upstream.URL + "/v1")
	document.Deployments[0].Capabilities = append(
		document.Deployments[0].Capabilities,
		config.CapabilityStoredCompletion,
		config.CapabilityProviderTools,
	)
	state := responsesstate.NewMemoryStore(responsesstate.MemoryOptions{})
	handler, err := NewDataHandler(document, DataOptions{ResponsesState: state})
	if err != nil {
		t.Fatalf("NewDataHandler() error = %v", err)
	}
	requests := []string{
		`{"model":"public","store":false,"input":"start"}`,
		`{"model":"public","store":false,"input":[{"type":"computer_call_output",` +
			`"call_id":"call_1","output":{"type":"computer_screenshot","image_url":"data:x"}}]}`,
		`{"model":"public","store":false,"input":[{"type":"item_reference","id":"msg_1"}]}`,
	}
	for _, body := range requests {
		response := serveResponsesRequest(
			t,
			handler,
			http.MethodPost,
			"/v1/responses",
			body,
			identity.Identity{},
		)
		if response.Code != http.StatusOK {
			t.Fatalf("response = status:%d body:%s", response.Code, response.Body)
		}
	}
	for _, itemID := range []string{"comp_1", "call_1", "msg_1"} {
		affinity, found, err := state.ResolveResource(
			context.Background(),
			responsesstate.ResourceKey{
				Scope:      responsesCallerScope(identity.Identity{}),
				Kind:       responsesstate.ResourceItem,
				ResourceID: itemID,
			},
		)
		if err != nil || !found || affinity.Deleted() ||
			!affinity.ExpiresAt.After(affinity.BoundAt) {
			t.Fatalf("item %q affinity = %#v, %v, %v", itemID, affinity, found, err)
		}
	}
}

func TestConversationItemInputMustMatchConversationTarget(t *testing.T) {
	t.Parallel()

	var calls atomic.Int64
	first := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		calls.Add(1)
	}))
	defer first.Close()
	second := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		calls.Add(1)
	}))
	defer second.Close()
	document := twoTargetDocument(first.URL+"/v1", second.URL+"/v1")
	for index := range document.Deployments {
		document.Deployments[index].Capabilities = append(
			document.Deployments[index].Capabilities,
			config.CapabilityConversations,
			config.CapabilityStoredCompletion,
		)
	}
	state := responsesstate.NewMemoryStore(responsesstate.MemoryOptions{})
	if err := state.BindResource(
		context.Background(),
		responsesstate.ResourceAffinity{
			ResourceKey: responsesstate.ResourceKey{
				Scope:      responsesCallerScope(identity.Identity{}),
				Kind:       responsesstate.ResourceConversation,
				ResourceID: "conv_second",
			},
			VirtualModel:  "public",
			Provider:      "provider-second",
			Deployment:    "deployment-second",
			UpstreamModel: "upstream-second",
			BoundAt:       time.Now().UTC(),
		},
	); err != nil {
		t.Fatalf("BindResource() conversation error = %v", err)
	}
	bindTestItemAffinity(
		t, state, identity.Identity{}, "item_first", "public",
		"provider", "deployment", "upstream-model",
	)
	handler, err := NewDataHandler(document, DataOptions{ResponsesState: state})
	if err != nil {
		t.Fatalf("NewDataHandler() error = %v", err)
	}
	response := serveResponsesRequest(
		t,
		handler,
		http.MethodPost,
		"/v1/conversations/conv_second/items",
		`{"items":[{"type":"item_reference","id":"item_first"}]}`,
		identity.Identity{},
	)
	if response.Code != http.StatusConflict {
		t.Fatalf("response = status:%d body:%s", response.Code, response.Body)
	}
	if calls.Load() != 0 {
		t.Fatalf("upstream calls = %d, want zero", calls.Load())
	}
}

func TestResponsesDoesNotExposeItemBeforeAffinityBinding(t *testing.T) {
	t.Parallel()

	upstream := httptest.NewServer(http.HandlerFunc(func(
		w http.ResponseWriter,
		_ *http.Request,
	) {
		writeJSON(w, http.StatusOK, map[string]any{
			"id": "resp_unstored", "object": "response",
			"model": "upstream-model", "status": "completed",
			"output": []any{map[string]any{
				"id": "msg_must_not_leak", "type": "message",
				"role": "assistant", "content": []any{},
			}},
		})
	}))
	defer upstream.Close()
	handler, err := NewDataHandler(
		responsesDocument(upstream.URL+"/v1"),
		DataOptions{ResponsesState: failingItemBindStore{
			MemoryStore: responsesstate.NewMemoryStore(responsesstate.MemoryOptions{}),
		}},
	)
	if err != nil {
		t.Fatalf("NewDataHandler() error = %v", err)
	}
	response := serveResponsesRequest(
		t,
		handler,
		http.MethodPost,
		"/v1/responses",
		`{"model":"public","store":false,"input":"hello"}`,
		identity.Identity{},
	)
	if response.Code != http.StatusServiceUnavailable {
		t.Fatalf("response = status:%d body:%s", response.Code, response.Body)
	}
	if strings.Contains(response.Body.String(), "msg_must_not_leak") {
		t.Fatalf("unbound item ID leaked: %s", response.Body)
	}
}

func TestResponsesStreamBindsItemBeforeExposure(t *testing.T) {
	t.Parallel()

	upstream := httptest.NewServer(http.HandlerFunc(func(
		w http.ResponseWriter,
		_ *http.Request,
	) {
		w.Header().Set("Content-Type", "text/event-stream")
		events := []string{
			`{"type":"response.created","response":{"id":"resp_stream_item",` +
				`"object":"response","model":"upstream-model","status":"in_progress","output":[]}}`,
			`{"type":"response.output_item.added","output_index":0,"item":{` +
				`"id":"msg_stream_item","type":"message","role":"assistant",` +
				`"status":"in_progress","content":[]}}`,
			`{"type":"response.completed","response":{"id":"resp_stream_item",` +
				`"object":"response","model":"upstream-model","status":"completed","output":[]}}`,
		}
		for _, event := range events {
			_, _ = io.WriteString(w, "data: "+event+"\n\n")
		}
	}))
	defer upstream.Close()
	state := responsesstate.NewMemoryStore(responsesstate.MemoryOptions{})
	handler, err := NewDataHandler(
		responsesDocument(upstream.URL+"/v1"),
		DataOptions{ResponsesState: state},
	)
	if err != nil {
		t.Fatalf("NewDataHandler() error = %v", err)
	}
	response := serveResponsesRequest(
		t,
		handler,
		http.MethodPost,
		"/v1/responses",
		`{"model":"public","store":false,"stream":true,"input":"hello"}`,
		identity.Identity{},
	)
	if response.Code != http.StatusOK ||
		!strings.Contains(response.Body.String(), "msg_stream_item") {
		t.Fatalf("response = status:%d body:%s", response.Code, response.Body)
	}
	affinity, found, err := state.ResolveResource(
		context.Background(),
		responsesstate.ResourceKey{
			Scope:      responsesCallerScope(identity.Identity{}),
			Kind:       responsesstate.ResourceItem,
			ResourceID: "msg_stream_item",
		},
	)
	if err != nil || !found || affinity.Deleted() {
		t.Fatalf("stream item affinity = %#v, %v, %v", affinity, found, err)
	}
}

func bindTestItemAffinity(
	t *testing.T,
	store responsesstate.ResourceStore,
	caller identity.Identity,
	itemID string,
	virtualModel string,
	provider string,
	deployment string,
	upstreamModel string,
) {
	t.Helper()
	if err := store.BindResource(
		context.Background(),
		responsesstate.ResourceAffinity{
			ResourceKey: responsesstate.ResourceKey{
				Scope:      responsesCallerScope(caller),
				Kind:       responsesstate.ResourceItem,
				ResourceID: itemID,
			},
			VirtualModel:  virtualModel,
			Provider:      provider,
			Deployment:    deployment,
			UpstreamModel: upstreamModel,
			BoundAt:       time.Now().UTC(),
		},
	); err != nil {
		t.Fatalf("BindResource(%q) error = %v", itemID, err)
	}
}
