// SPDX-FileCopyrightText: 2026 Scitrera LLC
// SPDX-FileCopyrightText: 2026 Fox Engine Ltd.
// SPDX-License-Identifier: AGPL-3.0-only

package gateway

import (
	"context"
	"encoding/json"
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
	"github.com/sparksq/sparkroute/pkg/identity"
	"github.com/sparksq/sparkroute/pkg/ledger"
	"github.com/sparksq/sparkroute/pkg/responsesstate"
)

type failingConversationBindStore struct {
	*responsesstate.MemoryStore
}

func (failingConversationBindStore) BindResource(
	context.Context,
	responsesstate.ResourceAffinity,
) error {
	return errors.New("state unavailable")
}

func TestConversationsLifecycleAndResponsesConsumption(t *testing.T) {
	t.Parallel()

	captured := make(chan string, 9)
	upstream := httptest.NewServer(http.HandlerFunc(func(
		w http.ResponseWriter,
		request *http.Request,
	) {
		captured <- request.Method + " " + request.URL.RequestURI()
		w.Header().Set("Content-Type", "application/json")
		switch {
		case request.Method == http.MethodPost &&
			request.URL.Path == "/v1/conversations":
			var body map[string]json.RawMessage
			if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
				t.Errorf("decode conversation create: %v", err)
			}
			if string(body["metadata"]) != `{"topic":"demo"}` ||
				!rawActive(body["items"]) {
				t.Errorf("conversation create body = %#v", body)
			}
			writeJSON(w, http.StatusOK, map[string]any{
				"id":         "conv_1",
				"object":     "conversation",
				"created_at": 1,
				"metadata":   map[string]string{"topic": "demo"},
			})
		case request.Method == http.MethodGet &&
			request.URL.Path == "/v1/conversations/conv_1":
			writeJSON(w, http.StatusOK, map[string]any{
				"id": "conv_1", "object": "conversation",
				"metadata": map[string]string{"topic": "demo"},
			})
		case request.Method == http.MethodPost &&
			request.URL.Path == "/v1/conversations/conv_1":
			writeJSON(w, http.StatusOK, map[string]any{
				"id": "conv_1", "object": "conversation",
				"metadata": map[string]string{"topic": "updated"},
			})
		case request.Method == http.MethodPost &&
			request.URL.Path == "/v1/conversations/conv_1/items":
			writeJSON(w, http.StatusOK, map[string]any{
				"object": "list",
				"data": []any{map[string]any{
					"id": "msg_1", "type": "message", "role": "user",
					"content": "Next turn",
				}},
				"has_more": false,
			})
		case request.Method == http.MethodGet &&
			request.URL.Path == "/v1/conversations/conv_1/items" &&
			request.URL.Query().Get("limit") == "10":
			writeJSON(w, http.StatusOK, map[string]any{
				"object": "list",
				"data": []any{map[string]any{
					"id": "msg_1", "type": "message", "role": "user",
					"content": "Next turn",
				}},
				"has_more": false,
			})
		case request.Method == http.MethodGet &&
			request.URL.Path == "/v1/conversations/conv_1/items/msg_1":
			writeJSON(w, http.StatusOK, map[string]any{
				"id": "msg_1", "type": "message", "role": "user",
				"content": "Next turn",
			})
		case request.Method == http.MethodDelete &&
			request.URL.Path == "/v1/conversations/conv_1/items/msg_1":
			writeJSON(w, http.StatusOK, map[string]any{
				"id": "msg_1", "object": "conversation.item.deleted", "deleted": true,
			})
		case request.Method == http.MethodPost &&
			request.URL.Path == "/v1/responses":
			var body map[string]json.RawMessage
			if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
				t.Errorf("decode response create: %v", err)
			}
			if string(body["conversation"]) != `"conv_1"` ||
				string(body["model"]) != `"upstream-model"` ||
				string(body["store"]) != "false" {
				t.Errorf("response create body = %#v", body)
			}
			writeJSON(w, http.StatusOK, map[string]any{
				"id": "resp_conversation", "object": "response",
				"model": "upstream-model", "status": "completed", "output": []any{},
			})
		case request.Method == http.MethodDelete &&
			request.URL.Path == "/v1/conversations/conv_1":
			writeJSON(w, http.StatusOK, map[string]any{
				"id": "conv_1", "object": "conversation.deleted", "deleted": true,
			})
		default:
			t.Errorf("unexpected upstream request: %s %s", request.Method, request.URL)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer upstream.Close()

	document := responsesDocument(upstream.URL + "/v1")
	document.VirtualModels[0].Visibility = config.ModelVisibilityHidden
	document.Deployments[0].Capabilities = append(
		document.Deployments[0].Capabilities,
		config.CapabilityConversations,
		config.CapabilityStoredCompletion,
	)
	state := responsesstate.NewMemoryStore(responsesstate.MemoryOptions{})
	recorder := &collectingRecorder{}
	handler, err := NewDataHandler(document, DataOptions{
		ResponsesState: state,
		Ledger:         recorder,
	})
	if err != nil {
		t.Fatalf("NewDataHandler() error = %v", err)
	}

	requests := []struct {
		method string
		target string
		body   string
	}{
		{
			method: http.MethodPost,
			target: "/v1/conversations",
			body: `{"metadata":{"topic":"demo"},"items":[` +
				`{"type":"message","role":"user","content":"Hello"}]}`,
		},
		{method: http.MethodGet, target: "/v1/conversations/conv_1"},
		{
			method: http.MethodPost,
			target: "/v1/conversations/conv_1",
			body:   `{"metadata":{"topic":"updated"}}`,
		},
		{
			method: http.MethodPost,
			target: "/v1/conversations/conv_1/items",
			body: `{"items":[` +
				`{"type":"message","role":"user","content":"Next turn"}]}`,
		},
		{
			method: http.MethodGet,
			target: "/v1/conversations/conv_1/items?limit=10&order=asc",
		},
		{
			method: http.MethodGet,
			target: "/v1/conversations/conv_1/items/msg_1",
		},
		{
			method: http.MethodDelete,
			target: "/v1/conversations/conv_1/items/msg_1",
		},
		{
			method: http.MethodPost,
			target: "/v1/responses",
			body: `{"model":"default","conversation":"conv_1",` +
				`"input":"Continue","store":false}`,
		},
		{method: http.MethodDelete, target: "/v1/conversations/conv_1"},
	}
	for _, request := range requests {
		response := serveResponsesRequest(
			t,
			handler,
			request.method,
			request.target,
			request.body,
			identity.Identity{},
		)
		if response.Code != http.StatusOK {
			t.Fatalf(
				"%s %s = status:%d body:%s",
				request.method,
				request.target,
				response.Code,
				response.Body,
			)
		}
	}

	resourceKey := responsesstate.ResourceKey{
		Scope:      responsesCallerScope(identity.Identity{}),
		Kind:       responsesstate.ResourceConversation,
		ResourceID: "conv_1",
	}
	affinity, found, err := state.ResolveResource(context.Background(), resourceKey)
	if err != nil || !found || !affinity.Deleted() || !affinity.ExpiresAt.After(affinity.DeletedAt) {
		t.Fatalf("conversation affinity = %#v, %v, %v", affinity, found, err)
	}
	responseAffinity, found, err := state.ResolveResource(
		context.Background(),
		responsesstate.ResourceKey{
			Scope:      responsesCallerScope(identity.Identity{}),
			Kind:       responsesstate.ResourceResponse,
			ResourceID: "resp_conversation",
		},
	)
	if err != nil || !found || responseAffinity.Deleted() ||
		!responseAffinity.ExpiresAt.IsZero() {
		t.Fatalf("response affinity = %#v, found = %v, error = %v", responseAffinity, found, err)
	}
	itemAffinity, found, err := state.ResolveResource(
		context.Background(),
		responsesstate.ResourceKey{
			Scope:      responsesCallerScope(identity.Identity{}),
			Kind:       responsesstate.ResourceItem,
			ResourceID: "msg_1",
		},
	)
	if err != nil || !found || !itemAffinity.Deleted() ||
		!itemAffinity.ExpiresAt.After(itemAffinity.DeletedAt) {
		t.Fatalf("item affinity = %#v, found = %v, error = %v", itemAffinity, found, err)
	}

	afterDelete := serveResponsesRequest(
		t,
		handler,
		http.MethodGet,
		"/v1/conversations/conv_1",
		"",
		identity.Identity{},
	)
	if afterDelete.Code != http.StatusNotFound {
		t.Fatalf("post-delete retrieve = status:%d body:%s", afterDelete.Code, afterDelete.Body)
	}

	wantUpstream := []string{
		"POST /v1/conversations",
		"GET /v1/conversations/conv_1",
		"POST /v1/conversations/conv_1",
		"POST /v1/conversations/conv_1/items",
		"GET /v1/conversations/conv_1/items?limit=10&order=asc",
		"GET /v1/conversations/conv_1/items/msg_1",
		"DELETE /v1/conversations/conv_1/items/msg_1",
		"POST /v1/responses",
		"DELETE /v1/conversations/conv_1",
	}
	for _, want := range wantUpstream {
		if got := <-captured; got != want {
			t.Errorf("upstream request = %q, want %q", got, want)
		}
	}
	for _, record := range recorder.snapshot() {
		if record.Request == nil ||
			!strings.HasPrefix(record.Request.Operation, "conversation") ||
			record.Request.Outcome != ledger.OutcomeSuccess {
			continue
		}
		if record.Request.AttemptCount != 1 ||
			record.Request.Usage.Completeness != ledger.UsageMissing {
			t.Errorf("conversation request record = %#v", record.Request)
		}
	}
}

func TestConversationResponseDeleteLeavesBoundedTombstone(t *testing.T) {
	t.Parallel()

	var calls atomic.Int64
	upstream := httptest.NewServer(http.HandlerFunc(func(
		w http.ResponseWriter,
		request *http.Request,
	) {
		calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		switch request.Method {
		case http.MethodDelete:
			writeJSON(w, http.StatusOK, map[string]any{
				"id": "resp_durable", "object": "response.deleted", "deleted": true,
			})
		case http.MethodGet:
			writeJSON(w, http.StatusNotFound, map[string]any{
				"error": map[string]string{"message": "deleted"},
			})
		default:
			t.Errorf("unexpected method %s", request.Method)
			w.WriteHeader(http.StatusMethodNotAllowed)
		}
	}))
	defer upstream.Close()
	document := responsesDocument(upstream.URL + "/v1")
	document.Deployments[0].Capabilities = append(
		document.Deployments[0].Capabilities,
		config.CapabilityStoredCompletion,
	)
	state := responsesstate.NewMemoryStore(responsesstate.MemoryOptions{})
	key := responsesstate.ResourceKey{
		Scope:      responsesCallerScope(identity.Identity{}),
		Kind:       responsesstate.ResourceResponse,
		ResourceID: "resp_durable",
	}
	if err := state.BindResource(
		context.Background(),
		responsesstate.ResourceAffinity{
			ResourceKey:   key,
			VirtualModel:  "public",
			Provider:      "provider",
			Deployment:    "deployment",
			UpstreamModel: "upstream-model",
			BoundAt:       time.Now().UTC(),
		},
	); err != nil {
		t.Fatalf("BindResource() error = %v", err)
	}
	handler, err := NewDataHandler(document, DataOptions{
		ResponsesState: state,
	})
	if err != nil {
		t.Fatalf("NewDataHandler() error = %v", err)
	}
	deleted := serveResponsesRequest(
		t,
		handler,
		http.MethodDelete,
		"/v1/responses/resp_durable",
		"",
		identity.Identity{},
	)
	if deleted.Code != http.StatusOK {
		t.Fatalf("delete = status:%d body:%s", deleted.Code, deleted.Body)
	}
	affinity, found, err := state.ResolveResource(context.Background(), key)
	if err != nil || !found || !affinity.Deleted() ||
		!affinity.ExpiresAt.After(affinity.DeletedAt) {
		t.Fatalf("response affinity = %#v, found = %v, error = %v", affinity, found, err)
	}
	retrieve := serveResponsesRequest(
		t,
		handler,
		http.MethodGet,
		"/v1/responses/resp_durable",
		"",
		identity.Identity{},
	)
	if retrieve.Code != http.StatusNotFound || calls.Load() != 2 {
		t.Fatalf(
			"retrieve = status:%d body:%s calls:%d",
			retrieve.Code,
			retrieve.Body,
			calls.Load(),
		)
	}
}

func TestConversationCreateRequiresDefaultAlias(t *testing.T) {
	t.Parallel()

	var calls atomic.Int64
	upstream := httptest.NewServer(http.HandlerFunc(func(
		w http.ResponseWriter,
		_ *http.Request,
	) {
		calls.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()
	document := responsesDocument(upstream.URL + "/v1")
	document.VirtualModels[0].Aliases = nil
	document.Deployments[0].Capabilities = append(
		document.Deployments[0].Capabilities,
		config.CapabilityConversations,
	)
	handler, err := NewDataHandler(document, DataOptions{})
	if err != nil {
		t.Fatalf("NewDataHandler() error = %v", err)
	}
	response := serveResponsesRequest(
		t,
		handler,
		http.MethodPost,
		"/v1/conversations",
		"",
		identity.Identity{},
	)
	if response.Code != http.StatusServiceUnavailable || calls.Load() != 0 {
		t.Fatalf("response = status:%d body:%s calls:%d", response.Code, response.Body, calls.Load())
	}
	var body openAIErrorEnvelope
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil ||
		body.Error.Code != "default_model_not_found" {
		t.Fatalf("error = %#v, %v", body, err)
	}
}

func TestConversationCreateRejectsInternalDefaultAlias(t *testing.T) {
	t.Parallel()

	var calls atomic.Int64
	upstream := httptest.NewServer(http.HandlerFunc(func(
		w http.ResponseWriter,
		_ *http.Request,
	) {
		calls.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()
	document := responsesDocument(upstream.URL + "/v1")
	document.VirtualModels[0].Visibility = config.ModelVisibilityInternal
	document.Deployments[0].Capabilities = append(
		document.Deployments[0].Capabilities,
		config.CapabilityConversations,
	)
	handler, err := NewDataHandler(document, DataOptions{})
	if err != nil {
		t.Fatalf("NewDataHandler() error = %v", err)
	}
	response := serveResponsesRequest(
		t,
		handler,
		http.MethodPost,
		"/v1/conversations",
		"",
		identity.Identity{},
	)
	if response.Code != http.StatusServiceUnavailable || calls.Load() != 0 {
		t.Fatalf("response = status:%d body:%s calls:%d", response.Code, response.Body, calls.Load())
	}
	var body openAIErrorEnvelope
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil ||
		body.Error.Code != "default_model_not_found" {
		t.Fatalf("error = %#v, %v", body, err)
	}
}

func TestConversationCreateDoesNotExposeIDBeforeAffinityBinding(t *testing.T) {
	t.Parallel()

	upstream := httptest.NewServer(http.HandlerFunc(func(
		w http.ResponseWriter,
		_ *http.Request,
	) {
		writeJSON(w, http.StatusOK, map[string]any{
			"id": "conv_must_not_leak", "object": "conversation",
		})
	}))
	defer upstream.Close()
	document := responsesDocument(upstream.URL + "/v1")
	document.Deployments[0].Capabilities = append(
		document.Deployments[0].Capabilities,
		config.CapabilityConversations,
	)
	handler, err := NewDataHandler(document, DataOptions{
		ResponsesState: failingConversationBindStore{
			MemoryStore: responsesstate.NewMemoryStore(responsesstate.MemoryOptions{}),
		},
	})
	if err != nil {
		t.Fatalf("NewDataHandler() error = %v", err)
	}
	response := serveResponsesRequest(
		t,
		handler,
		http.MethodPost,
		"/v1/conversations",
		"",
		identity.Identity{},
	)
	if response.Code != http.StatusServiceUnavailable {
		t.Fatalf("response = status:%d body:%s", response.Code, response.Body)
	}
	if strings.Contains(response.Body.String(), "conv_must_not_leak") {
		t.Fatalf("unbound conversation ID leaked: %s", response.Body)
	}
	var body openAIErrorEnvelope
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil ||
		body.Error.Code != "state_affinity_unavailable" {
		t.Fatalf("error = %#v, %v", body, err)
	}
}

func TestConversationAffinityIsCallerScoped(t *testing.T) {
	t.Parallel()

	var calls atomic.Int64
	upstream := httptest.NewServer(http.HandlerFunc(func(
		w http.ResponseWriter,
		_ *http.Request,
	) {
		calls.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()
	document := responsesDocument(upstream.URL + "/v1")
	document.Deployments[0].Capabilities = append(
		document.Deployments[0].Capabilities,
		config.CapabilityConversations,
	)
	state := responsesstate.NewMemoryStore(responsesstate.MemoryOptions{})
	owner := identity.Identity{Principal: identity.Principal{
		ID: "owner",
	}}
	other := identity.Identity{Principal: identity.Principal{
		ID: "other",
	}}
	if err := state.BindResource(
		context.Background(),
		responsesstate.ResourceAffinity{
			ResourceKey: responsesstate.ResourceKey{
				Scope:      responsesCallerScope(owner),
				Kind:       responsesstate.ResourceConversation,
				ResourceID: "conv_private",
			},
			VirtualModel:  "public",
			Provider:      "provider",
			Deployment:    "deployment",
			UpstreamModel: "upstream-model",
			BoundAt:       time.Now().UTC(),
		},
	); err != nil {
		t.Fatalf("BindResource() error = %v", err)
	}
	handler, err := NewDataHandler(document, DataOptions{
		ResponsesState: state,
	})
	if err != nil {
		t.Fatalf("NewDataHandler() error = %v", err)
	}
	response := serveResponsesRequest(
		t,
		handler,
		http.MethodGet,
		"/v1/conversations/conv_private",
		"",
		other,
	)
	if response.Code != http.StatusNotFound || calls.Load() != 0 {
		t.Fatalf(
			"response = status:%d body:%s calls:%d",
			response.Code,
			response.Body,
			calls.Load(),
		)
	}
}

func TestConversationCreateDoesNotRetryAnotherTarget(t *testing.T) {
	t.Parallel()

	var firstCalls, secondCalls atomic.Int64
	first := httptest.NewServer(http.HandlerFunc(func(
		w http.ResponseWriter,
		_ *http.Request,
	) {
		firstCalls.Add(1)
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{
			"error": map[string]string{"message": "temporarily unavailable"},
		})
	}))
	defer first.Close()
	second := httptest.NewServer(http.HandlerFunc(func(
		w http.ResponseWriter,
		_ *http.Request,
	) {
		secondCalls.Add(1)
		writeJSON(w, http.StatusOK, map[string]any{
			"id": "conv_wrong_target", "object": "conversation",
		})
	}))
	defer second.Close()
	document := twoTargetDocument(first.URL+"/v1", second.URL+"/v1")
	for index := range document.Deployments {
		document.Deployments[index].Capabilities = append(
			document.Deployments[index].Capabilities,
			config.CapabilityResponses,
			config.CapabilityConversations,
		)
	}
	handler, err := NewDataHandler(document, DataOptions{
		ResponsesState: responsesstate.NewMemoryStore(responsesstate.MemoryOptions{}),
		RoutingPicker:  zeroPicker{},
	})
	if err != nil {
		t.Fatalf("NewDataHandler() error = %v", err)
	}
	response := serveResponsesRequest(
		t,
		handler,
		http.MethodPost,
		"/v1/conversations",
		"",
		identity.Identity{},
	)
	if response.Code != http.StatusServiceUnavailable ||
		firstCalls.Load() != 1 ||
		secondCalls.Load() != 0 {
		t.Fatalf(
			"response = status:%d body:%s calls:(%d,%d)",
			response.Code,
			response.Body,
			firstCalls.Load(),
			secondCalls.Load(),
		)
	}
}

func TestConversationItemsRequireTargetCapabilities(t *testing.T) {
	t.Parallel()

	var calls atomic.Int64
	upstream := httptest.NewServer(http.HandlerFunc(func(
		w http.ResponseWriter,
		_ *http.Request,
	) {
		calls.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()
	document := responsesDocument(upstream.URL + "/v1")
	document.CapabilityDefaults.Unknown = config.UnknownCapabilityReject
	document.Deployments[0].Capabilities = append(
		document.Deployments[0].Capabilities,
		config.CapabilityConversations,
	)
	handler, err := NewDataHandler(document, DataOptions{})
	if err != nil {
		t.Fatalf("NewDataHandler() error = %v", err)
	}
	response := serveResponsesRequest(
		t,
		handler,
		http.MethodPost,
		"/v1/conversations",
		`{"items":[{"type":"message","role":"user","content":[`+
			`{"type":"input_image","image_url":"https://example.com/image.png"}]}]}`,
		identity.Identity{},
	)
	if response.Code != http.StatusBadRequest || calls.Load() != 0 {
		t.Fatalf("response = status:%d body:%s calls:%d", response.Code, response.Body, calls.Load())
	}
}

func TestResponsesConversationAffinityPinsTarget(t *testing.T) {
	t.Parallel()

	var firstCalls, secondCalls atomic.Int64
	first := httptest.NewServer(http.HandlerFunc(func(
		w http.ResponseWriter,
		_ *http.Request,
	) {
		firstCalls.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	defer first.Close()
	second := httptest.NewServer(http.HandlerFunc(func(
		w http.ResponseWriter,
		request *http.Request,
	) {
		secondCalls.Add(1)
		raw, _ := io.ReadAll(request.Body)
		if !strings.Contains(string(raw), `"model":"upstream-second"`) {
			t.Errorf("upstream body = %s", raw)
		}
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer second.Close()
	document := twoTargetDocument(first.URL+"/v1", second.URL+"/v1")
	for index := range document.Deployments {
		document.Deployments[index].Capabilities = append(
			document.Deployments[index].Capabilities,
			config.CapabilityResponses,
			config.CapabilityStoredCompletion,
			config.CapabilityConversations,
		)
	}
	state := responsesstate.NewMemoryStore(responsesstate.MemoryOptions{})
	now := time.Now().UTC()
	if err := state.BindResource(
		context.Background(),
		responsesstate.ResourceAffinity{
			ResourceKey: responsesstate.ResourceKey{
				Scope:      responsesCallerScope(identity.Identity{}),
				Kind:       responsesstate.ResourceConversation,
				ResourceID: "conv_pinned",
			},
			VirtualModel:  "public",
			Provider:      "provider-second",
			Deployment:    "deployment-second",
			UpstreamModel: "upstream-second",
			BoundAt:       now,
		},
	); err != nil {
		t.Fatalf("BindResource() error = %v", err)
	}
	handler, err := NewDataHandler(document, DataOptions{
		ResponsesState: state,
		RoutingPicker:  zeroPicker{},
	})
	if err != nil {
		t.Fatalf("NewDataHandler() error = %v", err)
	}
	response := serveResponsesRequest(
		t,
		handler,
		http.MethodPost,
		"/v1/responses",
		`{"model":"public","conversation":{"id":"conv_pinned"},`+
			`"input":"hello","store":false}`,
		identity.Identity{},
	)
	if response.Code != http.StatusServiceUnavailable ||
		firstCalls.Load() != 0 ||
		secondCalls.Load() != 1 {
		t.Fatalf(
			"response = status:%d body:%s calls:(%d,%d)",
			response.Code,
			response.Body,
			firstCalls.Load(),
			secondCalls.Load(),
		)
	}
}

func TestResponsesConversationValidation(t *testing.T) {
	t.Parallel()

	tests := []struct {
		raw  string
		want string
	}{
		{
			raw:  `{"model":"public","conversation":{"id":"conv_1"},"input":"hello"}`,
			want: "conv_1",
		},
		{
			raw: `{"model":"public","conversation":"conv_1",` +
				`"previous_response_id":"resp_1","input":"hello"}`,
		},
	}
	for _, test := range tests {
		envelope, _, _, err := decodeResponsesRequest([]byte(test.raw))
		if test.want == "" {
			if err == nil {
				t.Errorf("decodeResponsesRequest(%s) error = nil", test.raw)
			}
			continue
		}
		if err != nil {
			t.Errorf("decodeResponsesRequest(%s) error = %v", test.raw, err)
			continue
		}
		got, err := responsesConversationID(envelope["conversation"])
		if err != nil || got != test.want {
			t.Errorf("conversation ID = %q, %v; want %q", got, err, test.want)
		}
	}
}

func TestConversationCapabilities(t *testing.T) {
	t.Parallel()

	var envelope map[string]json.RawMessage
	if err := json.Unmarshal([]byte(`{"items":[{
		"type":"message","role":"developer","content":[
			{"type":"input_image","image_url":"https://example.com/image.png"}
		]
	}]}`), &envelope); err != nil {
		t.Fatalf("decode fixture: %v", err)
	}
	got, err := conversationCapabilities(envelope, true)
	if err != nil {
		t.Fatalf("conversationCapabilities() error = %v", err)
	}
	want := []config.Capability{
		config.CapabilityConversations,
		config.CapabilityDeveloperMessages,
		config.CapabilityVision,
	}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("capabilities = %v, want %v", got, want)
	}
}
