// SPDX-FileCopyrightText: 2026 Scitrera LLC
// SPDX-FileCopyrightText: 2026 Fox Engine Ltd.
// SPDX-License-Identifier: AGPL-3.0-only

package gateway

import (
	"bytes"
	"container/list"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/sparksq/sparkroute/pkg/config"
	"github.com/sparksq/sparkroute/pkg/identity"
	"github.com/sparksq/sparkroute/pkg/modelcatalog"
	"github.com/sparksq/sparkroute/pkg/modelrouter"
)

type catalogPlaneKey struct {
	tenant string
	model  string
}

type modelCatalogRequestStartKey struct{}

func withModelCatalogRequestStartedAt(ctx context.Context, startedAt time.Time) context.Context {
	return context.WithValue(ctx, modelCatalogRequestStartKey{}, startedAt)
}

func modelCatalogRequestStartedAt(ctx context.Context) time.Time {
	startedAt, _ := ctx.Value(modelCatalogRequestStartKey{}).(time.Time)
	return startedAt
}

type catalogPlane struct {
	key        catalogPlaneKey
	sourceID   string
	revision   string
	digest     config.Version
	cacheUntil time.Time
	handler    http.Handler
	element    *list.Element
}

// modelCatalogHandler performs one exact lookup before entering an isolated
// ordinary data plane. Authentication remains outside this handler, so tenant
// identity is already authoritative and the child retains every normal
// protocol, routing, telemetry, ledger, and saved-trace path.
type modelCatalogHandler struct {
	static          config.Document
	base            http.Handler
	directory       *modelcatalog.Directory
	options         DataOptions
	maxRequestBytes int64
	maxPlanes       int
	routerModels    map[string]struct{}

	mu     sync.Mutex
	planes map[catalogPlaneKey]*catalogPlane
	order  *list.List
}

func newModelCatalogHandler(
	document config.Document,
	base http.Handler,
	options DataOptions,
) http.Handler {
	maximum := options.MaxRequestBytes
	if maximum <= 0 {
		maximum = defaultMaxRequestBytes
	}
	routerModels := make(map[string]struct{})
	if publisher, ok := options.ModelRouter.(modelrouter.ModelPublisher); ok {
		for _, model := range publisher.PublicModels() {
			routerModels[model] = struct{}{}
		}
	}
	return &modelCatalogHandler{
		static:          document,
		base:            base,
		directory:       options.ModelCatalog,
		options:         options,
		maxRequestBytes: maximum,
		maxPlanes:       options.ModelCatalog.MaxEntries(),
		routerModels:    routerModels,
		planes:          make(map[catalogPlaneKey]*catalogPlane),
		order:           list.New(),
	}
}

func (h *modelCatalogHandler) ServeHTTP(w http.ResponseWriter, request *http.Request) {
	startedAt := time.Now()
	requested, candidate := catalogRequestedModel(request, h.maxRequestBytes)
	if !candidate {
		h.base.ServeHTTP(w, request)
		return
	}
	if _, exists := h.static.CanonicalModel(requested); exists {
		h.base.ServeHTTP(w, request)
		return
	}
	if _, exists := h.routerModels[requested]; exists {
		h.base.ServeHTTP(w, request)
		return
	}
	request = request.WithContext(withModelCatalogRequestStartedAt(request.Context(), startedAt))
	caller, exists := identity.FromContext(request.Context())
	if !exists || caller.Principal.Tenant == "" {
		// Catalog lookup never manufactures a tenant from request data. The base
		// handler preserves the ordinary model-not-found behavior.
		h.base.ServeHTTP(w, request)
		return
	}
	resolution, err := h.directory.Lookup(request.Context(), modelcatalog.Request{
		TenantID:       caller.Principal.Tenant,
		RequestedModel: requested,
	})
	if err != nil {
		if errors.Is(err, modelcatalog.ErrNotFound) {
			h.base.ServeHTTP(w, request)
			return
		}
		if request.Context().Err() != nil {
			return
		}
		writeModelCatalogError(w, request, err)
		return
	}
	handler, err := h.planeHandler(caller.Principal.Tenant, requested, resolution.Entry)
	if err != nil {
		writeModelCatalogError(w, request, fmt.Errorf("%w: %v", modelcatalog.ErrInvalidEntry, err))
		return
	}
	handler.ServeHTTP(w, request)
}

func (h *modelCatalogHandler) planeHandler(
	tenant string,
	requested string,
	entry modelcatalog.ResolvedEntry,
) (http.Handler, error) {
	key := catalogPlaneKey{tenant: tenant, model: requested}
	h.mu.Lock()
	defer h.mu.Unlock()
	if current := h.planes[key]; current != nil &&
		current.sourceID == entry.SourceID &&
		current.revision == entry.Revision &&
		current.digest == entry.Digest {
		current.cacheUntil = entry.CacheUntil
		h.order.MoveToFront(current.element)
		return current.handler, nil
	}
	document, err := entry.Document()
	if err != nil {
		return nil, err
	}
	if documentUsesCatalogPromptCache(document) &&
		(h.options.PromptCache == nil || h.options.PromptFingerprinter == nil) {
		return nil, fmt.Errorf("catalog prompt-cache affinity is not configured")
	}
	childOptions := h.options
	childOptions.ConfigRevision = entry.ConfigRevision()
	if childOptions.ModelCatalogCredentials != nil {
		childOptions.Credentials = childOptions.ModelCatalogCredentials
	}
	childOptions.TargetManager = nil
	childOptions.Lifecycle = nil
	childOptions.RetryBudget = nil
	childOptions.ModelCatalog = nil
	childOptions.OTLPTraceHandler = nil
	plane, err := NewDataPlane(document, childOptions)
	if err != nil {
		return nil, err
	}
	if previous := h.planes[key]; previous != nil {
		h.order.Remove(previous.element)
		delete(h.planes, key)
	}
	created := &catalogPlane{
		key:        key,
		sourceID:   entry.SourceID,
		revision:   entry.Revision,
		digest:     entry.Digest,
		cacheUntil: entry.CacheUntil,
		handler:    plane.Handler,
	}
	created.element = h.order.PushFront(created)
	h.planes[key] = created
	for len(h.planes) > h.maxPlanes {
		oldest, _ := h.order.Back().Value.(*catalogPlane)
		delete(h.planes, oldest.key)
		h.order.Remove(oldest.element)
	}
	return created.handler, nil
}

func catalogRequestedModel(request *http.Request, maximum int64) (string, bool) {
	if request.Method == http.MethodGet && strings.HasPrefix(request.URL.Path, "/v1/models/") {
		model := strings.TrimPrefix(request.URL.Path, "/v1/models/")
		return model, validCatalogModelName(model)
	}
	if request.Method != http.MethodPost {
		return "", false
	}
	if strings.HasPrefix(request.URL.Path, geminiModelsPathPrefix) {
		model, _, err := parseGeminiRoute(request.URL.Path)
		return model, err == nil
	}
	if strings.HasPrefix(request.URL.Path, bedrockModelsPathPrefix) {
		model, _, err := parseBedrockRoute(request.URL.Path)
		return model, err == nil
	}
	switch request.URL.Path {
	case "/v1/chat/completions",
		"/v1/responses",
		"/v1/responses/compact",
		"/v1/embeddings",
		"/v1/messages",
		"/v1/messages/count_tokens":
	default:
		return "", false
	}
	if request.Body == nil || request.ContentLength > maximum {
		return "", false
	}
	original := request.Body
	raw, err := io.ReadAll(io.LimitReader(original, maximum+1))
	if err != nil || int64(len(raw)) > maximum {
		request.Body = &catalogReplayBody{
			Reader: io.MultiReader(bytes.NewReader(raw), original),
			closer: original,
		}
		return "", false
	}
	request.Body = &catalogReplayBody{Reader: bytes.NewReader(raw), closer: original}
	var envelope struct {
		Model json.RawMessage `json:"model"`
	}
	if json.Unmarshal(raw, &envelope) != nil || len(envelope.Model) == 0 {
		return "", false
	}
	var model string
	if json.Unmarshal(envelope.Model, &model) != nil || !validCatalogModelName(model) {
		return "", false
	}
	return model, true
}

type catalogReplayBody struct {
	io.Reader
	closer io.Closer
}

func (b *catalogReplayBody) Close() error {
	if b.closer == nil {
		return nil
	}
	return b.closer.Close()
}

func validCatalogModelName(model string) bool {
	if model == "" || len(model) > maxRequestedModelBytes || strings.TrimSpace(model) != model {
		return false
	}
	for _, ch := range model {
		if ch <= 0x20 || ch == 0x7f {
			return false
		}
	}
	return true
}

func documentUsesCatalogPromptCache(document config.Document) bool {
	for _, model := range document.VirtualModels {
		if model.Selection.PromptCacheAffinity.Enabled {
			return true
		}
	}
	return false
}

func writeModelCatalogError(w http.ResponseWriter, request *http.Request, err error) {
	code := "model_catalog_unavailable"
	message := "the tenant model catalog is temporarily unavailable"
	if errors.Is(err, modelcatalog.ErrInvalidEntry) {
		code = "model_catalog_invalid"
		message = "the tenant model definition was rejected"
	}
	w.Header().Set("Retry-After", "1")
	switch {
	case strings.HasPrefix(request.URL.Path, geminiModelsPathPrefix):
		writeGeminiError(w, http.StatusServiceUnavailable, message)
	case strings.HasPrefix(request.URL.Path, bedrockModelsPathPrefix):
		writeBedrockError(w, http.StatusServiceUnavailable, code, message)
	case request.URL.Path == "/v1/messages" || request.URL.Path == "/v1/messages/count_tokens":
		anthropicOperationMessages.writeError(
			w, http.StatusServiceUnavailable, code, message, "",
		)
	default:
		writeOpenAIError(w, http.StatusServiceUnavailable, code, message)
	}
}
