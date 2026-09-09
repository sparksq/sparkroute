// SPDX-FileCopyrightText: 2026 Scitrera LLC
// SPDX-FileCopyrightText: 2026 Fox Engine Ltd.
// SPDX-License-Identifier: AGPL-3.0-only

package gateway

import (
	"context"
	"encoding/json"
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

func TestResponsesBackgroundResourceLifecycle(t *testing.T) {
	t.Parallel()

	captured := make(chan string, 5)
	upstream := httptest.NewServer(http.HandlerFunc(func(
		w http.ResponseWriter,
		request *http.Request,
	) {
		captured <- request.Method + " " + request.URL.RequestURI()
		w.Header().Set("Content-Type", "application/json")
		switch {
		case request.Method == http.MethodPost &&
			request.URL.Path == "/v1/responses":
			defer func() { _ = request.Body.Close() }()
			var body map[string]json.RawMessage
			if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
				t.Errorf("decode create body: %v", err)
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			if string(body["background"]) != "true" ||
				string(body["store"]) != "false" ||
				string(body["model"]) != `"upstream-model"` {
				t.Errorf("create body = %#v", body)
			}
			writeJSON(w, http.StatusOK, map[string]any{
				"id":     "resp_background",
				"object": "response",
				"model":  "upstream-model",
				"status": "queued",
				"usage": map[string]any{
					"input_tokens":  3,
					"output_tokens": 0,
					"total_tokens":  3,
				},
			})
		case request.Method == http.MethodGet &&
			request.URL.Path == "/v1/responses/resp_background":
			writeJSON(w, http.StatusOK, map[string]any{
				"id":     "resp_background",
				"object": "response",
				"model":  "upstream-model",
				"status": "completed",
				"usage": map[string]any{
					"input_tokens":  3,
					"output_tokens": 2,
					"total_tokens":  5,
				},
			})
		case request.Method == http.MethodGet &&
			request.URL.Path == "/v1/responses/resp_background/input_items":
			writeJSON(w, http.StatusOK, map[string]any{
				"object": "list",
				"data": []any{map[string]any{
					"id": "msg_background_input", "type": "message",
					"role": "user", "content": []any{},
				}},
				"has_more": false,
			})
		case request.Method == http.MethodPost &&
			request.URL.Path == "/v1/responses/resp_background/cancel":
			writeJSON(w, http.StatusOK, map[string]any{
				"id":     "resp_background",
				"object": "response",
				"model":  "upstream-model",
				"status": "cancelled",
			})
		case request.Method == http.MethodDelete &&
			request.URL.Path == "/v1/responses/resp_background":
			writeJSON(w, http.StatusOK, map[string]any{
				"id":      "resp_background",
				"object":  "response.deleted",
				"deleted": true,
			})
		default:
			t.Errorf("unexpected upstream request: %s %s", request.Method, request.URL)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer upstream.Close()

	document := responsesDocument(upstream.URL + "/v1")
	document.Deployments[0].Capabilities = append(
		document.Deployments[0].Capabilities,
		config.CapabilityStoredCompletion,
		config.CapabilityBackgroundResponses,
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

	create := serveResponsesRequest(
		t,
		handler,
		http.MethodPost,
		"/v1/responses",
		`{"model":"default","input":"hello","store":false,"background":true}`,
		identity.Identity{},
	)
	if create.Code != http.StatusOK ||
		!strings.Contains(create.Body.String(), `"model":"public"`) {
		t.Fatalf("create = status:%d body:%s", create.Code, create.Body)
	}
	affinity, found, err := state.Resolve(
		context.Background(),
		responsesCallerScope(identity.Identity{}),
		"resp_background",
	)
	if err != nil || !found || affinity.Deployment != "deployment" {
		t.Fatalf("background affinity = %#v, %v, %v", affinity, found, err)
	}

	retrieve := serveResponsesRequest(
		t,
		handler,
		http.MethodGet,
		"/v1/responses/resp_background?include=output_text&stream=false",
		"",
		identity.Identity{},
	)
	if retrieve.Code != http.StatusOK ||
		!strings.Contains(retrieve.Body.String(), `"model":"public"`) {
		t.Fatalf("retrieve = status:%d body:%s", retrieve.Code, retrieve.Body)
	}
	inputItems := serveResponsesRequest(
		t,
		handler,
		http.MethodGet,
		"/v1/responses/resp_background/input_items?limit=10&order=asc",
		"",
		identity.Identity{},
	)
	if inputItems.Code != http.StatusOK {
		t.Fatalf("input items = status:%d body:%s", inputItems.Code, inputItems.Body)
	}
	itemAffinity, found, err := state.ResolveResource(
		context.Background(),
		responsesstate.ResourceKey{
			Scope:      responsesCallerScope(identity.Identity{}),
			Kind:       responsesstate.ResourceItem,
			ResourceID: "msg_background_input",
		},
	)
	if err != nil || !found ||
		itemAffinity.ExpiresAt.IsZero() ||
		!itemAffinity.ExpiresAt.Equal(affinity.ExpiresAt) {
		t.Fatalf("input item affinity = %#v, %v, %v", itemAffinity, found, err)
	}
	cancel := serveResponsesRequest(
		t,
		handler,
		http.MethodPost,
		"/v1/responses/resp_background/cancel",
		"",
		identity.Identity{},
	)
	if cancel.Code != http.StatusOK {
		t.Fatalf("cancel = status:%d body:%s", cancel.Code, cancel.Body)
	}
	deleted := serveResponsesRequest(
		t,
		handler,
		http.MethodDelete,
		"/v1/responses/resp_background",
		"",
		identity.Identity{},
	)
	if deleted.Code != http.StatusOK ||
		!strings.Contains(deleted.Body.String(), `"deleted":true`) {
		t.Fatalf("delete = status:%d body:%s", deleted.Code, deleted.Body)
	}
	// A successful provider delete leaves a routing tombstone until normal
	// expiry so the same ID cannot be rebound to another provider.
	if _, found, err := state.Resolve(
		context.Background(),
		responsesCallerScope(identity.Identity{}),
		"resp_background",
	); err != nil || !found {
		t.Fatalf("post-delete affinity found = %v, error = %v", found, err)
	}

	wantRequests := []string{
		"POST /v1/responses",
		"GET /v1/responses/resp_background?include=output_text&stream=false",
		"GET /v1/responses/resp_background/input_items?limit=10&order=asc",
		"POST /v1/responses/resp_background/cancel",
		"DELETE /v1/responses/resp_background",
	}
	for _, want := range wantRequests {
		if got := <-captured; got != want {
			t.Errorf("upstream request = %q, want %q", got, want)
		}
	}

	resourceOperations := map[string]bool{
		"responses_retrieve":    true,
		"responses_input_items": true,
		"responses_cancel":      true,
		"responses_delete":      true,
	}
	seen := make(map[string]bool)
	for _, record := range recorder.snapshot() {
		if record.Request == nil || !resourceOperations[record.Request.Operation] {
			continue
		}
		seen[record.Request.Operation] = true
		if record.Request.Usage.Completeness != ledger.UsageMissing {
			t.Errorf(
				"%s usage completeness = %q",
				record.Request.Operation,
				record.Request.Usage.Completeness,
			)
		}
		if record.Request.AttemptCount != 1 {
			t.Errorf("%s attempts = %d", record.Request.Operation, record.Request.AttemptCount)
		}
	}
	if fmt.Sprint(seen) == "map[]" || len(seen) != len(resourceOperations) {
		t.Fatalf("resource operations seen = %v", seen)
	}
}

func TestResponsesBackgroundStreamResume(t *testing.T) {
	t.Parallel()

	var calls atomic.Int64
	upstream := httptest.NewServer(http.HandlerFunc(func(
		w http.ResponseWriter,
		request *http.Request,
	) {
		calls.Add(1)
		if request.Method != http.MethodGet ||
			request.URL.RequestURI() !=
				"/v1/responses/resp_stream_resume?stream=true&starting_after=7" {
			t.Errorf("upstream request = %s %s", request.Method, request.URL.RequestURI())
		}
		if request.Header.Get("Accept") != "text/event-stream" {
			t.Errorf("Accept = %q", request.Header.Get("Accept"))
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(
			w,
			`data: {"type":"response.created","sequence_number":8,"response":{"id":"resp_stream_resume","model":"upstream-model","status":"in_progress"}}`+"\n\n",
		)
		_, _ = io.WriteString(
			w,
			`data: {"type":"response.completed","sequence_number":9,"response":{"id":"resp_stream_resume","model":"upstream-model","status":"completed","usage":{"input_tokens":4,"output_tokens":2,"total_tokens":6}}}`+"\n\n",
		)
	}))
	defer upstream.Close()

	document := responsesDocument(upstream.URL + "/v1")
	document.Deployments[0].Capabilities = append(
		document.Deployments[0].Capabilities,
		config.CapabilityStoredCompletion,
		config.CapabilityBackgroundResponses,
	)
	state := responsesstate.NewMemoryStore(responsesstate.MemoryOptions{})
	bindTestResponseAffinity(t, state, identity.Identity{}, "resp_stream_resume")
	recorder := &collectingRecorder{}
	handler, err := NewDataHandler(document, DataOptions{
		ResponsesState: state,
		Ledger:         recorder,
	})
	if err != nil {
		t.Fatalf("NewDataHandler() error = %v", err)
	}
	response := serveResponsesRequest(
		t,
		handler,
		http.MethodGet,
		"/v1/responses/resp_stream_resume?stream=true&starting_after=7",
		"",
		identity.Identity{},
	)
	if response.Code != http.StatusOK ||
		!strings.Contains(response.Body.String(), `"model":"public"`) ||
		calls.Load() != 1 {
		t.Fatalf("response = status:%d body:%s calls:%d", response.Code, response.Body, calls.Load())
	}
	records := recorder.snapshot()
	if len(records) != 2 ||
		records[1].Request == nil ||
		records[1].Request.Operation != "responses_retrieve" ||
		!records[1].Request.Stream ||
		records[1].Request.Usage.Completeness != ledger.UsageMissing {
		t.Fatalf("records = %#v", records)
	}
}

func TestResponsesResourceAffinityIsCallerScoped(t *testing.T) {
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
		config.CapabilityStoredCompletion,
	)
	state := responsesstate.NewMemoryStore(responsesstate.MemoryOptions{})
	owner := identity.Identity{Principal: identity.Principal{
		ID: "owner", Tenant: "tenant-a", Type: "machine", Subject: "owner",
	}}
	bindTestResponseAffinity(t, state, owner, "resp_private_resource")
	handler, err := NewDataHandler(document, DataOptions{ResponsesState: state})
	if err != nil {
		t.Fatalf("NewDataHandler() error = %v", err)
	}
	other := identity.Identity{Principal: identity.Principal{
		ID: "other", Tenant: "tenant-a", Type: "machine", Subject: "other",
	}}
	response := serveResponsesRequest(
		t,
		handler,
		http.MethodGet,
		"/v1/responses/resp_private_resource",
		"",
		other,
	)
	if response.Code != http.StatusNotFound || calls.Load() != 0 {
		t.Fatalf("response = status:%d body:%s calls:%d", response.Code, response.Body, calls.Load())
	}
	var body openAIErrorEnvelope
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil ||
		body.Error.Code != "state_affinity_not_found" {
		t.Fatalf("error = %#v, %v", body, err)
	}
}

func TestResponsesResourcePinsOwningTargetWithoutRetry(t *testing.T) {
	t.Parallel()

	var firstCalls, secondCalls atomic.Int64
	first := httptest.NewServer(http.HandlerFunc(func(
		w http.ResponseWriter,
		_ *http.Request,
	) {
		firstCalls.Add(1)
		writeJSON(w, http.StatusOK, map[string]any{
			"id": "resp_pinned", "object": "response", "model": "upstream-model",
		})
	}))
	defer first.Close()
	second := httptest.NewServer(http.HandlerFunc(func(
		w http.ResponseWriter,
		_ *http.Request,
	) {
		secondCalls.Add(1)
		w.WriteHeader(http.StatusServiceUnavailable)
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
	now := time.Now().UTC()
	if err := state.Bind(context.Background(), responsesstate.Affinity{
		Scope:         responsesCallerScope(identity.Identity{}),
		ResponseID:    "resp_pinned",
		VirtualModel:  "public",
		Provider:      "provider-second",
		Deployment:    "deployment-second",
		UpstreamModel: "upstream-second",
		BoundAt:       now,
		ExpiresAt:     now.Add(time.Hour),
	}); err != nil {
		t.Fatalf("Bind() error = %v", err)
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
		http.MethodGet,
		"/v1/responses/resp_pinned",
		"",
		identity.Identity{},
	)
	if response.Code != http.StatusServiceUnavailable ||
		response.Header().Get("X-SparkRoute-Attempt-Count") != "1" {
		t.Fatalf("response = status:%d headers:%v body:%s", response.Code, response.Header(), response.Body)
	}
	if firstCalls.Load() != 0 || secondCalls.Load() != 1 {
		t.Fatalf(
			"upstream calls = (%d, %d), want (0, 1)",
			firstCalls.Load(),
			secondCalls.Load(),
		)
	}
}

func TestResponsesResourceRejectsChangedUpstreamModel(t *testing.T) {
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
		config.CapabilityStoredCompletion,
	)
	state := responsesstate.NewMemoryStore(responsesstate.MemoryOptions{})
	now := time.Now().UTC()
	if err := state.Bind(context.Background(), responsesstate.Affinity{
		Scope:         responsesCallerScope(identity.Identity{}),
		ResponseID:    "resp_old_model",
		VirtualModel:  "public",
		Provider:      "provider",
		Deployment:    "deployment",
		UpstreamModel: "previous-upstream-model",
		BoundAt:       now,
		ExpiresAt:     now.Add(time.Hour),
	}); err != nil {
		t.Fatalf("Bind() error = %v", err)
	}
	handler, err := NewDataHandler(document, DataOptions{ResponsesState: state})
	if err != nil {
		t.Fatalf("NewDataHandler() error = %v", err)
	}
	response := serveResponsesRequest(
		t,
		handler,
		http.MethodGet,
		"/v1/responses/resp_old_model",
		"",
		identity.Identity{},
	)
	if response.Code != http.StatusServiceUnavailable || calls.Load() != 0 {
		t.Fatalf("response = status:%d body:%s calls:%d", response.Code, response.Body, calls.Load())
	}
	var body openAIErrorEnvelope
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil ||
		body.Error.Code != "state_affinity_target_changed" {
		t.Fatalf("error = %#v, %v", body, err)
	}
}

func TestResponsesResourceStreamRequiresBackgroundCapability(t *testing.T) {
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
		config.CapabilityStoredCompletion,
	)
	state := responsesstate.NewMemoryStore(responsesstate.MemoryOptions{})
	bindTestResponseAffinity(t, state, identity.Identity{}, "resp_no_background")
	handler, err := NewDataHandler(document, DataOptions{ResponsesState: state})
	if err != nil {
		t.Fatalf("NewDataHandler() error = %v", err)
	}
	response := serveResponsesRequest(
		t,
		handler,
		http.MethodGet,
		"/v1/responses/resp_no_background?stream=true",
		"",
		identity.Identity{},
	)
	if response.Code != http.StatusServiceUnavailable || calls.Load() != 0 {
		t.Fatalf("response = status:%d body:%s calls:%d", response.Code, response.Body, calls.Load())
	}
	var body openAIErrorEnvelope
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil ||
		body.Error.Code != "state_affinity_capability_unavailable" {
		t.Fatalf("error = %#v, %v", body, err)
	}
}

func TestDetectBackgroundResponsesCapabilities(t *testing.T) {
	t.Parallel()

	var envelope map[string]json.RawMessage
	if err := json.Unmarshal(
		[]byte(`{"model":"public","input":"hello","store":false,"background":true}`),
		&envelope,
	); err != nil {
		t.Fatalf("decode fixture: %v", err)
	}
	got, err := detectResponsesCapabilities(envelope, false)
	if err != nil {
		t.Fatalf("detectResponsesCapabilities() error = %v", err)
	}
	want := []config.Capability{
		config.CapabilityBackgroundResponses,
		config.CapabilityResponses,
		config.CapabilityStoredCompletion,
	}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("capabilities = %v, want %v", got, want)
	}
}

func serveResponsesRequest(
	t *testing.T,
	handler http.Handler,
	method string,
	target string,
	body string,
	caller identity.Identity,
) *httptest.ResponseRecorder {
	t.Helper()
	request := httptest.NewRequest(method, target, strings.NewReader(body))
	request = request.WithContext(identity.WithContext(request.Context(), caller))
	if body != "" {
		request.Header.Set("Content-Type", "application/json")
	}
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	return response
}

func bindTestResponseAffinity(
	t *testing.T,
	state responsesstate.Store,
	caller identity.Identity,
	responseID string,
) {
	t.Helper()
	now := time.Now().UTC()
	if err := state.Bind(context.Background(), responsesstate.Affinity{
		Scope:         responsesCallerScope(caller),
		ResponseID:    responseID,
		VirtualModel:  "public",
		Provider:      "provider",
		Deployment:    "deployment",
		UpstreamModel: "upstream-model",
		BoundAt:       now,
		ExpiresAt:     now.Add(time.Hour),
	}); err != nil {
		t.Fatalf("Bind() error = %v", err)
	}
}
