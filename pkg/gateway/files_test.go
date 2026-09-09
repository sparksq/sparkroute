// SPDX-FileCopyrightText: 2026 Scitrera LLC
// SPDX-FileCopyrightText: 2026 Fox Engine Ltd.
// SPDX-License-Identifier: AGPL-3.0-only

package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"mime/multipart"
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

type resourceOnlyStore struct {
	store *responsesstate.MemoryStore
}

func (s resourceOnlyStore) Resolve(
	ctx context.Context,
	scope string,
	responseID string,
) (responsesstate.Affinity, bool, error) {
	return s.store.Resolve(ctx, scope, responseID)
}

func (s resourceOnlyStore) Bind(
	ctx context.Context,
	affinity responsesstate.Affinity,
) error {
	return s.store.Bind(ctx, affinity)
}

func (s resourceOnlyStore) ResolveResource(
	ctx context.Context,
	key responsesstate.ResourceKey,
) (responsesstate.ResourceAffinity, bool, error) {
	return s.store.ResolveResource(ctx, key)
}

func (s resourceOnlyStore) BindResource(
	ctx context.Context,
	affinity responsesstate.ResourceAffinity,
) error {
	return s.store.BindResource(ctx, affinity)
}

func (s resourceOnlyStore) TombstoneResource(
	ctx context.Context,
	key responsesstate.ResourceKey,
	deletedAt time.Time,
	expiresAt time.Time,
) error {
	return s.store.TombstoneResource(ctx, key, deletedAt, expiresAt)
}

func TestFilesLifecycleAndResponsesConsumption(t *testing.T) {
	t.Parallel()

	captured := make(chan string, 5)
	upstream := httptest.NewServer(http.HandlerFunc(func(
		w http.ResponseWriter,
		request *http.Request,
	) {
		captured <- request.Method + " " + request.URL.RequestURI()
		switch {
		case request.Method == http.MethodPost &&
			request.URL.Path == "/v1/files":
			if err := request.ParseMultipartForm(1 << 20); err != nil {
				t.Errorf("ParseMultipartForm() error = %v", err)
			}
			file, header, err := request.FormFile("file")
			if err != nil {
				t.Errorf("FormFile() error = %v", err)
			} else {
				defer func() { _ = file.Close() }()
				body, readErr := io.ReadAll(file)
				if readErr != nil {
					t.Errorf("read uploaded file: %v", readErr)
				}
				if header.Filename != "notes.txt" || string(body) != "training context" {
					t.Errorf("uploaded file = %q %q", header.Filename, body)
				}
			}
			if got := request.FormValue("purpose"); got != "user_data" {
				t.Errorf("purpose = %q", got)
			}
			writeJSON(w, http.StatusOK, map[string]any{
				"id":         "file_1",
				"object":     "file",
				"bytes":      16,
				"created_at": 1,
				"filename":   "notes.txt",
				"purpose":    "user_data",
			})
		case request.Method == http.MethodGet &&
			request.URL.Path == "/v1/files/file_1":
			writeJSON(w, http.StatusOK, map[string]any{
				"id": "file_1", "object": "file", "filename": "notes.txt",
			})
		case request.Method == http.MethodGet &&
			request.URL.Path == "/v1/files/file_1/content":
			w.Header().Set("Content-Type", "text/plain")
			w.Header().Set("X-Upstream-File", "true")
			_, _ = io.WriteString(w, "training context")
		case request.Method == http.MethodPost &&
			request.URL.Path == "/v1/responses":
			var body map[string]json.RawMessage
			if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
				t.Errorf("decode response request: %v", err)
			}
			if string(body["model"]) != `"upstream-model"` ||
				!strings.Contains(string(body["input"]), `"file_id":"file_1"`) {
				t.Errorf("response request = %#v", body)
			}
			writeJSON(w, http.StatusOK, map[string]any{
				"id": "resp_file", "object": "response",
				"model": "upstream-model", "status": "completed", "output": []any{},
			})
		case request.Method == http.MethodDelete &&
			request.URL.Path == "/v1/files/file_1":
			writeJSON(w, http.StatusOK, map[string]any{
				"id": "file_1", "object": "file", "deleted": true,
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
		config.CapabilityFileInput,
		config.CapabilityFiles,
	)
	state := responsesstate.NewMemoryStore(responsesstate.MemoryOptions{})
	handler, err := NewDataHandler(document, DataOptions{ResponsesState: state})
	if err != nil {
		t.Fatalf("NewDataHandler() error = %v", err)
	}

	var upload bytes.Buffer
	multipartWriter := multipart.NewWriter(&upload)
	if err := multipartWriter.WriteField("purpose", "user_data"); err != nil {
		t.Fatalf("WriteField() error = %v", err)
	}
	fileWriter, err := multipartWriter.CreateFormFile("file", "notes.txt")
	if err != nil {
		t.Fatalf("CreateFormFile() error = %v", err)
	}
	if _, err := io.WriteString(fileWriter, "training context"); err != nil {
		t.Fatalf("write multipart file: %v", err)
	}
	if err := multipartWriter.Close(); err != nil {
		t.Fatalf("multipart Close() error = %v", err)
	}

	request := httptest.NewRequest(http.MethodPost, "/v1/files", &upload)
	request.Header.Set("Content-Type", multipartWriter.FormDataContentType())
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("upload status = %d; body=%s", response.Code, response.Body)
	}
	affinity, found, err := state.ResolveResource(
		request.Context(),
		responsesstate.ResourceKey{
			Scope:      responsesCallerScope(identity.Identity{}),
			Kind:       responsesstate.ResourceFile,
			ResourceID: "file_1",
		},
	)
	if err != nil || !found || affinity.Deployment != "deployment" {
		t.Fatalf("file affinity = %#v, %v, %v", affinity, found, err)
	}
	response = serveGatewayRequest(t, handler, http.MethodGet, "/v1/files", "", "")
	if response.Code != http.StatusOK {
		t.Fatalf("list status = %d; body=%s", response.Code, response.Body)
	}
	var listed fileListEnvelope
	if err := json.Unmarshal(response.Body.Bytes(), &listed); err != nil {
		t.Fatalf("decode list response: %v", err)
	}
	if listed.Object != "list" || listed.HasMore ||
		len(listed.Data) != 1 || listed.Data[0].ID != "file_1" ||
		listed.FirstID != "file_1" || listed.LastID != "file_1" {
		t.Fatalf("list response = %#v", listed)
	}

	response = serveGatewayRequest(t, handler, http.MethodGet, "/v1/files/file_1", "", "")
	if response.Code != http.StatusOK {
		t.Fatalf("retrieve status = %d; body=%s", response.Code, response.Body)
	}
	response = serveGatewayRequest(
		t,
		handler,
		http.MethodGet,
		"/v1/files/file_1/content",
		"",
		"",
	)
	if response.Code != http.StatusOK ||
		response.Body.String() != "training context" ||
		response.Header().Get("X-Upstream-File") != "true" {
		t.Fatalf(
			"content response = %d headers=%v body=%q",
			response.Code,
			response.Header(),
			response.Body.String(),
		)
	}
	response = serveGatewayRequest(
		t,
		handler,
		http.MethodPost,
		"/v1/responses",
		`{"model":"public","store":false,"input":[{"role":"user","content":[{"type":"input_file","file_id":"file_1"},{"type":"input_text","text":"summarize"}]}]}`,
		"application/json",
	)
	if response.Code != http.StatusOK {
		t.Fatalf("Responses status = %d; body=%s", response.Code, response.Body)
	}
	response = serveGatewayRequest(
		t,
		handler,
		http.MethodDelete,
		"/v1/files/file_1",
		"",
		"",
	)
	if response.Code != http.StatusOK {
		t.Fatalf("delete status = %d; body=%s", response.Code, response.Body)
	}
	response = serveGatewayRequest(t, handler, http.MethodGet, "/v1/files/file_1", "", "")
	if response.Code != http.StatusNotFound {
		t.Fatalf("post-delete retrieve status = %d; body=%s", response.Code, response.Body)
	}
	response = serveGatewayRequest(t, handler, http.MethodGet, "/v1/files", "", "")
	if response.Code != http.StatusOK {
		t.Fatalf("post-delete list status = %d; body=%s", response.Code, response.Body)
	}
	listed = fileListEnvelope{}
	if err := json.Unmarshal(response.Body.Bytes(), &listed); err != nil {
		t.Fatalf("decode post-delete list response: %v", err)
	}
	if len(listed.Data) != 0 || listed.HasMore || listed.FirstID != "" || listed.LastID != "" {
		t.Fatalf("post-delete list response = %#v", listed)
	}

	close(captured)
	gotCalls := make([]string, 0, 5)
	for call := range captured {
		gotCalls = append(gotCalls, call)
	}
	wantCalls := []string{
		"POST /v1/files",
		"GET /v1/files/file_1",
		"GET /v1/files/file_1/content",
		"POST /v1/responses",
		"DELETE /v1/files/file_1",
	}
	if strings.Join(gotCalls, "\n") != strings.Join(wantCalls, "\n") {
		t.Fatalf("upstream calls = %v, want %v", gotCalls, wantCalls)
	}
}

func TestFileAffinityIsCallerScoped(t *testing.T) {
	t.Parallel()

	var calls atomic.Int64
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		writeJSON(w, http.StatusOK, map[string]any{"id": "file_private", "object": "file"})
	}))
	defer upstream.Close()

	document := responsesDocument(upstream.URL + "/v1")
	document.Deployments[0].Capabilities = append(
		document.Deployments[0].Capabilities,
		config.CapabilityFileInput,
		config.CapabilityFiles,
	)
	owner := identity.Identity{Principal: identity.Principal{
		ID: "client-a", Tenant: "tenant-a", Type: "machine", Subject: "client-a",
	}}
	state := responsesstate.NewMemoryStore(responsesstate.MemoryOptions{})
	if err := state.BindResource(
		t.Context(),
		responsesstate.ResourceAffinity{
			ResourceKey: responsesstate.ResourceKey{
				Scope:      responsesCallerScope(owner),
				Kind:       responsesstate.ResourceFile,
				ResourceID: "file_private",
			},
			VirtualModel: "public", Provider: "provider",
			Deployment: "deployment", UpstreamModel: "upstream-model",
			BoundAt: time.Now().UTC(),
		},
	); err != nil {
		t.Fatalf("BindResource() error = %v", err)
	}
	handler, err := NewDataHandler(document, DataOptions{ResponsesState: state})
	if err != nil {
		t.Fatalf("NewDataHandler() error = %v", err)
	}
	request := httptest.NewRequest(http.MethodGet, "/v1/files/file_private", nil)
	request = request.WithContext(identity.WithContext(request.Context(), identity.Identity{
		Principal: identity.Principal{
			ID: "client-b", Tenant: "tenant-a", Type: "machine", Subject: "client-b",
		},
	}))
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusNotFound {
		t.Fatalf("status = %d; body=%s", response.Code, response.Body)
	}
	if calls.Load() != 0 {
		t.Fatalf("upstream calls = %d, want 0", calls.Load())
	}
}

func TestChatFileIDPinsOwningTarget(t *testing.T) {
	t.Parallel()

	var firstCalls atomic.Int64
	first := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		firstCalls.Add(1)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer first.Close()
	var secondCalls atomic.Int64
	second := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		secondCalls.Add(1)
		writeJSON(w, http.StatusOK, map[string]any{
			"id": "chat_1", "object": "chat.completion", "model": "upstream-second",
			"choices": []any{}, "usage": map[string]int{"total_tokens": 0},
		})
	}))
	defer second.Close()

	document := twoTargetDocument(first.URL+"/v1", second.URL+"/v1")
	for index := range document.Deployments {
		document.Deployments[index].Capabilities = append(
			document.Deployments[index].Capabilities,
			config.CapabilityFileInput,
			config.CapabilityFiles,
		)
	}
	state := responsesstate.NewMemoryStore(responsesstate.MemoryOptions{})
	if err := state.BindResource(
		t.Context(),
		responsesstate.ResourceAffinity{
			ResourceKey: responsesstate.ResourceKey{
				Scope:      responsesCallerScope(identity.Identity{}),
				Kind:       responsesstate.ResourceFile,
				ResourceID: "file_second",
			},
			VirtualModel: "public", Provider: "provider-second",
			Deployment: "deployment-second", UpstreamModel: "upstream-second",
			BoundAt: time.Now().UTC(),
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
	response := serveGatewayRequest(
		t,
		handler,
		http.MethodPost,
		"/v1/chat/completions",
		`{"model":"public","messages":[{"role":"user","content":[{"type":"file","file":{"file_id":"file_second"}}]}]}`,
		"application/json",
	)
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d; body=%s", response.Code, response.Body)
	}
	if firstCalls.Load() != 0 || secondCalls.Load() != 1 {
		t.Fatalf(
			"upstream calls = first %d, second %d",
			firstCalls.Load(),
			secondCalls.Load(),
		)
	}
}

func TestFileInputsFailClosedForUnknownOrCrossTargetAffinity(t *testing.T) {
	t.Parallel()

	var firstCalls atomic.Int64
	first := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		firstCalls.Add(1)
	}))
	defer first.Close()
	var secondCalls atomic.Int64
	second := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		secondCalls.Add(1)
	}))
	defer second.Close()

	document := twoTargetDocument(first.URL+"/v1", second.URL+"/v1")
	for index := range document.Deployments {
		document.Deployments[index].Capabilities = append(
			document.Deployments[index].Capabilities,
			config.CapabilityResponses,
			config.CapabilityFileInput,
			config.CapabilityFiles,
		)
	}
	state := responsesstate.NewMemoryStore(responsesstate.MemoryOptions{})
	for _, affinity := range []responsesstate.ResourceAffinity{
		{
			ResourceKey: responsesstate.ResourceKey{
				Scope:      responsesCallerScope(identity.Identity{}),
				Kind:       responsesstate.ResourceFile,
				ResourceID: "file_first",
			},
			VirtualModel: "public", Provider: "provider",
			Deployment: "deployment", UpstreamModel: "upstream-model",
			BoundAt: time.Now().UTC(),
		},
		{
			ResourceKey: responsesstate.ResourceKey{
				Scope:      responsesCallerScope(identity.Identity{}),
				Kind:       responsesstate.ResourceFile,
				ResourceID: "file_second",
			},
			VirtualModel: "public", Provider: "provider-second",
			Deployment: "deployment-second", UpstreamModel: "upstream-second",
			BoundAt: time.Now().UTC(),
		},
	} {
		if err := state.BindResource(t.Context(), affinity); err != nil {
			t.Fatalf("BindResource() error = %v", err)
		}
	}
	handler, err := NewDataHandler(document, DataOptions{ResponsesState: state})
	if err != nil {
		t.Fatalf("NewDataHandler() error = %v", err)
	}
	requests := []string{
		`{"model":"public","store":false,"input":[{"type":"input_file","file_id":"file_unknown"}]}`,
		`{"model":"public","store":false,"input":[{"type":"input_file","file_id":"file_first"},{"type":"input_file","file_id":"file_second"}]}`,
	}
	for _, raw := range requests {
		response := serveGatewayRequest(
			t,
			handler,
			http.MethodPost,
			"/v1/responses",
			raw,
			"application/json",
		)
		if response.Code != http.StatusConflict {
			t.Errorf("request %s status = %d; body=%s", raw, response.Code, response.Body)
		}
	}
	if firstCalls.Load() != 0 || secondCalls.Load() != 0 {
		t.Fatalf(
			"upstream calls = first %d, second %d; want zero",
			firstCalls.Load(),
			secondCalls.Load(),
		)
	}
}

func TestFilesListUsesGatewayIndexWithoutProviderTraffic(t *testing.T) {
	t.Parallel()

	var calls atomic.Int64
	upstream := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		calls.Add(1)
	}))
	defer upstream.Close()
	handler, err := NewDataHandler(proxyDocument(upstream.URL+"/v1"), DataOptions{})
	if err != nil {
		t.Fatalf("NewDataHandler() error = %v", err)
	}
	response := serveGatewayRequest(t, handler, http.MethodGet, "/v1/files", "", "")
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d; body=%s", response.Code, response.Body)
	}
	if response.Body.String() != `{"object":"list","data":[],"has_more":false}`+"\n" {
		t.Fatalf("body = %s", response.Body)
	}
	if calls.Load() != 0 {
		t.Fatalf("upstream calls = %d, want 0", calls.Load())
	}
}

func TestFilesUploadRejectsMissingIndexBeforeProviderTraffic(t *testing.T) {
	t.Parallel()

	var calls atomic.Int64
	upstream := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		calls.Add(1)
	}))
	defer upstream.Close()
	document := responsesDocument(upstream.URL + "/v1")
	document.Deployments[0].Capabilities = append(
		document.Deployments[0].Capabilities,
		config.CapabilityFileInput,
		config.CapabilityFiles,
	)
	handler, err := NewDataHandler(document, DataOptions{
		ResponsesState: resourceOnlyStore{
			store: responsesstate.NewMemoryStore(responsesstate.MemoryOptions{}),
		},
	})
	if err != nil {
		t.Fatalf("NewDataHandler() error = %v", err)
	}
	var upload bytes.Buffer
	writer := multipart.NewWriter(&upload)
	if err := writer.Close(); err != nil {
		t.Fatalf("multipart Close() error = %v", err)
	}
	request := httptest.NewRequest(http.MethodPost, "/v1/files", &upload)
	request.Header.Set("Content-Type", writer.FormDataContentType())
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusServiceUnavailable ||
		!strings.Contains(response.Body.String(), `"code":"file_index_unavailable"`) {
		t.Fatalf("response = %d %s", response.Code, response.Body)
	}
	if calls.Load() != 0 {
		t.Fatalf("upstream calls = %d, want 0", calls.Load())
	}
}

func TestFilesListPaginationFilteringAndCallerScope(t *testing.T) {
	t.Parallel()

	state := responsesstate.NewMemoryStore(responsesstate.MemoryOptions{})
	owner := identity.Identity{Principal: identity.Principal{
		ID: "client-a", Tenant: "tenant-a", Type: "machine", Subject: "client-a",
	}}
	other := identity.Identity{Principal: identity.Principal{
		ID: "client-b", Tenant: "tenant-a", Type: "machine", Subject: "client-b",
	}}
	bind := func(caller identity.Identity, id, purpose string, createdAt int64) {
		t.Helper()
		scope := responsesCallerScope(caller)
		if err := state.BindFile(
			t.Context(),
			responsesstate.ResourceAffinity{
				ResourceKey: responsesstate.ResourceKey{
					Scope: scope, Kind: responsesstate.ResourceFile, ResourceID: id,
				},
				VirtualModel: "public", Provider: "provider",
				Deployment: "deployment", UpstreamModel: "upstream-model",
				BoundAt: time.Now().UTC(),
			},
			responsesstate.FileRecord{
				Scope: scope, ID: id, Object: "file", Bytes: 1,
				CreatedAt: createdAt, Filename: id + ".txt", Purpose: purpose,
			},
		); err != nil {
			t.Fatalf("BindFile(%q) error = %v", id, err)
		}
	}
	bind(owner, "file_a", "user_data", 1)
	bind(owner, "file_b", "fine-tune", 2)
	bind(owner, "file_c", "user_data", 2)
	bind(other, "file_other", "user_data", 3)

	handler, err := NewDataHandler(proxyDocument("http://unused.invalid/v1"), DataOptions{
		ResponsesState: state,
	})
	if err != nil {
		t.Fatalf("NewDataHandler() error = %v", err)
	}
	serve := func(caller identity.Identity, target string) *httptest.ResponseRecorder {
		t.Helper()
		request := httptest.NewRequest(http.MethodGet, target, nil)
		request = request.WithContext(identity.WithContext(request.Context(), caller))
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		return response
	}
	decode := func(response *httptest.ResponseRecorder) fileListEnvelope {
		t.Helper()
		if response.Code != http.StatusOK {
			t.Fatalf("list status = %d; body=%s", response.Code, response.Body)
		}
		var page fileListEnvelope
		if err := json.Unmarshal(response.Body.Bytes(), &page); err != nil {
			t.Fatalf("decode list response: %v", err)
		}
		return page
	}

	page := decode(serve(owner, "/v1/files?limit=2"))
	if !page.HasMore || len(page.Data) != 2 ||
		page.Data[0].ID != "file_c" || page.Data[1].ID != "file_b" ||
		page.FirstID != "file_c" || page.LastID != "file_b" {
		t.Fatalf("first page = %#v", page)
	}
	page = decode(serve(owner, "/v1/files?limit=2&after=file_b"))
	if page.HasMore || len(page.Data) != 1 || page.Data[0].ID != "file_a" {
		t.Fatalf("second page = %#v", page)
	}
	page = decode(serve(owner, "/v1/files?purpose=user_data&order=asc"))
	if len(page.Data) != 2 || page.Data[0].ID != "file_a" || page.Data[1].ID != "file_c" {
		t.Fatalf("filtered page = %#v", page)
	}
	page = decode(serve(other, "/v1/files"))
	if len(page.Data) != 1 || page.Data[0].ID != "file_other" {
		t.Fatalf("other caller page = %#v", page)
	}
	response := serve(owner, "/v1/files?after=file_other")
	if response.Code != http.StatusBadRequest ||
		!strings.Contains(response.Body.String(), `"code":"invalid_after"`) {
		t.Fatalf("cross-caller cursor response = %d %s", response.Code, response.Body)
	}
	response = serve(owner, "/v1/files?limit=1001")
	if response.Code != http.StatusBadRequest ||
		!strings.Contains(response.Body.String(), `"code":"invalid_query"`) {
		t.Fatalf("oversized page response = %d %s", response.Code, response.Body)
	}
	response = serve(owner, "/v1/files?after=bad%20id")
	if response.Code != http.StatusBadRequest ||
		!strings.Contains(response.Body.String(), `"code":"invalid_after"`) {
		t.Fatalf("malformed cursor response = %d %s", response.Code, response.Body)
	}
}

func serveGatewayRequest(
	t *testing.T,
	handler http.Handler,
	method string,
	target string,
	body string,
	contentType string,
) *httptest.ResponseRecorder {
	t.Helper()
	request := httptest.NewRequest(method, target, strings.NewReader(body))
	if contentType != "" {
		request.Header.Set("Content-Type", contentType)
	}
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	return response
}
