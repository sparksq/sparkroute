package admin

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/sparksq/sparkroute/pkg/config"
	"github.com/sparksq/sparkroute/pkg/config/managed"
	"github.com/sparksq/sparkroute/pkg/sparkrun"
)

func (h *handler) sparkrunCatalog(writer http.ResponseWriter, request *http.Request) {
	if request.Method != http.MethodPost {
		methodNotAllowed(writer, http.MethodPost)
		return
	}
	if !strictManagedQuery(writer, request.URL.Query()) {
		return
	}
	var input struct {
		Operation string         `json:"operation"`
		Arguments map[string]any `json:"arguments"`
	}
	if !decodeRequestJSON(writer, request, &input) {
		return
	}
	switch input.Operation {
	case "catalog_search", "catalog_resolve", "catalog_registries", "catalog_clusters", "operation_status":
		if !requireRole(writer, request, RoleConfigRead) {
			return
		}
	case "catalog_import", "catalog_refresh":
		if !requireRole(writer, request, RoleConfigWrite) {
			return
		}
	default:
		writeError(writer, http.StatusBadRequest, "invalid_operation", "Unsupported catalog operation")
		return
	}
	ctx, cancel := context.WithTimeout(request.Context(), 45*time.Second)
	defer cancel()
	var result json.RawMessage
	if err := h.options.SparkrunCatalog.Catalog(ctx, input.Operation, input.Arguments, &result); err != nil {
		writeCatalogError(writer, err)
		return
	}
	writer.Header().Set("Cache-Control", "no-store")
	writeJSON(writer, http.StatusOK, result)
}

func (h *handler) sparkrunRecipeDraft(writer http.ResponseWriter, request *http.Request) {
	if request.Method != http.MethodPost {
		methodNotAllowed(writer, http.MethodPost)
		return
	}
	if !requireRole(writer, request, RoleConfigWrite) || !strictManagedQuery(writer, request.URL.Query()) {
		return
	}
	var input struct {
		Document               config.Document      `json:"document"`
		ExpectedActiveRevision config.Version       `json:"expected_active_revision"`
		Recipe                 sparkrun.RecipeDraft `json:"recipe"`
	}
	if !decodeRequestJSON(writer, request, &input) {
		return
	}
	if !validManagedRevision(input.ExpectedActiveRevision) {
		writeError(writer, 400, "invalid_revision", "An active revision is required")
		return
	}
	_, active, err := h.options.ManagedConfig.Load(request.Context())
	if err != nil {
		writeManagedConfigError(writer, err)
		return
	}
	if active != input.ExpectedActiveRevision {
		writeManagedConfigError(writer, managed.ErrRevisionConflict)
		return
	}
	generated := managed.EmptyDocument()
	set, err := h.options.ManagedConfig.GetSet(request.Context(), managed.OwnerSparkrun)
	if err == nil {
		generated = set.Document
	} else if !errors.Is(err, managed.ErrSetNotFound) {
		writeManagedConfigError(writer, err)
		return
	}
	ctx, cancel := context.WithTimeout(request.Context(), 45*time.Second)
	defer cancel()
	document, deployment, reused, err := sparkrun.PrepareRecipeDraft(ctx, h.options.SparkrunCatalog, input.Document, generated, input.Recipe)
	if err != nil {
		writeCatalogError(writer, err)
		return
	}
	if _, err := h.options.ManagedConfig.ValidateSet(ctx, managed.OwnerOperator, document, input.ExpectedActiveRevision); err != nil {
		writeManagedConfigError(writer, err)
		return
	}
	writer.Header().Set("Cache-Control", "no-store")
	writeJSON(writer, 200, map[string]any{"document": document, "deployment": deployment, "reused": reused})
}

func (h *handler) validateSparkrunCandidate(writer http.ResponseWriter, request *http.Request, owner managed.Owner, input managedSetMutation) bool {
	if owner != managed.OwnerOperator {
		return true
	}
	candidate, _, err := h.options.ManagedConfig.BuildCandidate(request.Context(), owner, input.Document, *input.ExpectedActiveRevision)
	if err != nil {
		writeManagedConfigError(writer, err)
		return false
	}
	ctx, cancel := context.WithTimeout(request.Context(), 45*time.Second)
	defer cancel()
	if err := sparkrun.ValidateRecipeBindings(ctx, h.options.SparkrunCatalog, candidate); err != nil {
		writeCatalogError(writer, err)
		return false
	}
	if request.Method == http.MethodPut {
		if err := sparkrun.RetainRecipeImports(ctx, h.options.SparkrunCatalog, input.Document); err != nil {
			writeCatalogError(writer, err)
			return false
		}
	}
	return true
}

func writeCatalogError(writer http.ResponseWriter, err error) {
	var bridgeError *sparkrun.BridgeError
	if errors.As(err, &bridgeError) {
		writeError(writer, http.StatusUnprocessableEntity, bridgeError.Code, bridgeError.Message)
		return
	}
	// Expected validation errors are produced locally with bounded recipe
	// diagnostics. Subprocess failures contain no command output or secrets.
	message := err.Error()
	if strings.Contains(message, "sparkrun bridge") || strings.Contains(message, "gateway bridge") {
		message = "sparkrun could not be reached on this control node. Install or update sparkrun with the SparkRoute plugin and enable gateway.sparkroute."
	}
	if len(message) > 2048 {
		message = "sparkrun catalog request failed"
	}
	writeError(writer, http.StatusUnprocessableEntity, "sparkrun_unavailable", message)
}

func (h *handler) lifecycleStatus(writer http.ResponseWriter, request *http.Request) {
	if request.Method != http.MethodGet {
		methodNotAllowed(writer, http.MethodGet)
		return
	}
	if !requireRole(writer, request, RoleStatusRead) || !strictManagedQuery(writer, request.URL.Query()) {
		return
	}
	snapshot, err := h.options.Lifecycle.Snapshot(request.Context())
	if err != nil {
		writeError(writer, http.StatusServiceUnavailable, "runtime_unavailable", "Runtime status is unavailable")
		return
	}
	writer.Header().Set("Cache-Control", "no-store")
	writeJSON(writer, http.StatusOK, snapshot)
}
