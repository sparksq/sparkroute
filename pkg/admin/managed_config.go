package admin

import (
	"errors"
	"net/http"
	"net/url"
	"strings"

	"github.com/sparksq/sparkroute/pkg/adminapi"
	"github.com/sparksq/sparkroute/pkg/config"
	"github.com/sparksq/sparkroute/pkg/config/managed"
)

type managedSetMutation struct {
	Document                config.Document `json:"document"`
	ExpectedActiveRevision  *config.Version `json:"expected_active_revision"`
	ExpectedPresetsRevision *int64          `json:"expected_presets_revision,omitempty"`
	Reason                  string          `json:"reason,omitempty"`
}

type managedSetRoutingSimulation struct {
	Document               config.Document     `json:"document"`
	ExpectedActiveRevision *config.Version     `json:"expected_active_revision"`
	RequestedModel         string              `json:"requested_model"`
	RoutingText            string              `json:"routing_text,omitempty"`
	RequiredCapabilities   []config.Capability `json:"required_capabilities,omitempty"`
}

func (h *handler) activeConfiguration(writer http.ResponseWriter, request *http.Request) {
	if request.Method != http.MethodGet {
		methodNotAllowed(writer, http.MethodGet)
		return
	}
	if !requireRole(writer, request, RoleConfigRead) || !strictManagedQuery(writer, request.URL.Query()) {
		return
	}
	writer.Header().Set("Cache-Control", "no-store")
	writeJSON(writer, http.StatusOK, map[string]any{
		"revision": h.revision,
		"document": h.document,
	})
}

func (h *handler) managedSets(writer http.ResponseWriter, request *http.Request) {
	if request.Method != http.MethodGet {
		methodNotAllowed(writer, http.MethodGet)
		return
	}
	principal, ok := principalFromRequest(request)
	if !ok {
		writeError(writer, http.StatusUnauthorized, "authentication_required", "admin authentication is required")
		return
	}
	canReadAll := hasRole(principal, RoleConfigRead)
	canWriteOperator := hasRole(principal, RoleConfigWrite)
	canReconcileSparkrun := hasRole(principal, RoleConfigReconcileSparkrun)
	if !canReadAll && !canWriteOperator && !canReconcileSparkrun {
		writeError(writer, http.StatusForbidden, "forbidden", "the authenticated principal is not authorized for this operation")
		return
	}
	if !strictManagedQuery(writer, request.URL.Query()) {
		return
	}
	sets, err := h.options.ManagedConfig.ListSets(request.Context())
	if err != nil {
		writeManagedConfigError(writer, err)
		return
	}
	if !canReadAll {
		filtered := sets[:0]
		for _, set := range sets {
			if (set.Owner == managed.OwnerOperator && canWriteOperator) ||
				(set.Owner == managed.OwnerSparkrun && canReconcileSparkrun) {
				filtered = append(filtered, set)
			}
		}
		sets = filtered
	}
	_, activeRevision, err := h.options.ManagedConfig.Load(request.Context())
	if err != nil {
		writeManagedConfigError(writer, err)
		return
	}
	response := map[string]any{"active_revision": activeRevision, "managed_sets": sets}
	if store, ok := h.options.ManagedConfig.(managed.PresetStore); ok && (canReadAll || canWriteOperator) {
		catalog, err := store.ListPresets(request.Context())
		if err != nil {
			writeManagedConfigError(writer, err)
			return
		}
		// Use the configuration revision from the same snapshot as the selection.
		response["active_revision"] = catalog.ActiveRevision
		response["presets_revision"] = catalog.PresetsRevision
		response["active_preset"] = catalog.ActivePreset
	}
	writer.Header().Set("Cache-Control", "no-store")
	writeJSON(writer, http.StatusOK, response)
}

func (h *handler) managedSet(writer http.ResponseWriter, request *http.Request) {
	path := strings.TrimPrefix(request.URL.Path, "/v1/config/managed-sets/")
	parts := strings.Split(strings.Trim(path, "/"), "/")
	if len(parts) == 0 || len(parts) > 2 || parts[0] == "" ||
		(len(parts) == 2 && parts[1] != "validate" && parts[1] != "simulate-routing") {
		writeError(writer, http.StatusNotFound, "not_found", "route not found")
		return
	}
	owner := managed.Owner(parts[0])
	if err := owner.Validate(); err != nil {
		writeError(writer, http.StatusNotFound, "managed_set_not_found", "managed configuration set not found")
		return
	}
	if !strictManagedQuery(writer, request.URL.Query()) {
		return
	}
	if len(parts) == 2 {
		if parts[1] == "validate" {
			h.validateManagedSet(writer, request, owner)
		} else {
			h.simulateManagedSetRouting(writer, request, owner)
		}
		return
	}
	switch request.Method {
	case http.MethodGet:
		if !authorizeManagedSetRead(writer, request, owner) {
			return
		}
		set, err := h.options.ManagedConfig.GetSet(request.Context(), owner)
		if err != nil {
			writeManagedConfigError(writer, err)
			return
		}
		writer.Header().Set("Cache-Control", "no-store")
		writeJSON(writer, http.StatusOK, set)
	case http.MethodPut:
		if !authorizeManagedSetWrite(writer, request, owner) {
			return
		}
		input, ok := decodeManagedSetMutation(writer, request)
		if !ok {
			return
		}
		if !h.validateSparkrunCandidate(writer, request, owner, input) {
			return
		}
		principal, _ := principalFromRequest(request)
		result, err := h.options.ManagedConfig.ReplaceSet(
			request.Context(), owner, input.Document, managed.ReplaceOptions{
				ExpectedActive:          *input.ExpectedActiveRevision,
				ExpectedPresetsRevision: input.ExpectedPresetsRevision,
				Actor:                   principal.ID,
				Reason:                  input.Reason,
			},
		)
		if err != nil {
			writeManagedConfigError(writer, err)
			return
		}
		writer.Header().Set("Cache-Control", "no-store")
		writeJSON(writer, http.StatusOK, result)
	default:
		methodNotAllowed(writer, http.MethodGet, http.MethodPut)
	}
}

func (h *handler) simulateManagedSetRouting(
	writer http.ResponseWriter,
	request *http.Request,
	owner managed.Owner,
) {
	if request.Method != http.MethodPost {
		methodNotAllowed(writer, http.MethodPost)
		return
	}
	if !authorizeManagedSetRead(writer, request, owner) {
		return
	}
	var input managedSetRoutingSimulation
	if !decodeRequestJSON(writer, request, &input) {
		return
	}
	if input.ExpectedActiveRevision == nil {
		writeError(writer, http.StatusBadRequest, "expected_revision_required", "expected_active_revision is required")
		return
	}
	if !validManagedRevision(*input.ExpectedActiveRevision) {
		writeError(writer, http.StatusBadRequest, "invalid_revision", "expected_active_revision must be a SHA-256 identifier")
		return
	}
	candidate, _, err := h.options.ManagedConfig.BuildCandidate(
		request.Context(), owner, input.Document, *input.ExpectedActiveRevision,
	)
	if err != nil {
		writeManagedConfigError(writer, err)
		return
	}
	result, err := adminapi.SimulateRoutingWithMetadata(adminapi.RoutingSimulationInput{
		Document:             candidate,
		RequestedModel:       input.RequestedModel,
		RoutingText:          input.RoutingText,
		RequiredCapabilities: input.RequiredCapabilities,
	}, config.UnknownCapabilityTry, discoveredMetadataState(h.options.ModelMetadata))
	if err != nil {
		writeError(writer, http.StatusBadRequest, "routing_simulation_failed", err.Error())
		return
	}
	writer.Header().Set("Cache-Control", "no-store")
	writeJSON(writer, http.StatusOK, result)
}

func (h *handler) validateManagedSet(
	writer http.ResponseWriter,
	request *http.Request,
	owner managed.Owner,
) {
	if request.Method != http.MethodPost {
		methodNotAllowed(writer, http.MethodPost)
		return
	}
	if !authorizeManagedSetWrite(writer, request, owner) {
		return
	}
	input, ok := decodeManagedSetMutation(writer, request)
	if !ok {
		return
	}
	if !h.validateSparkrunCandidate(writer, request, owner, input) {
		return
	}
	validation, err := h.options.ManagedConfig.ValidateSet(
		request.Context(), owner, input.Document, *input.ExpectedActiveRevision,
	)
	if err != nil {
		writeManagedConfigError(writer, err)
		return
	}
	writer.Header().Set("Cache-Control", "no-store")
	writeJSON(writer, http.StatusOK, validation)
}

func decodeManagedSetMutation(
	writer http.ResponseWriter,
	request *http.Request,
) (managedSetMutation, bool) {
	var input managedSetMutation
	if !decodeRequestJSON(writer, request, &input) {
		return input, false
	}
	if input.ExpectedActiveRevision == nil {
		writeError(writer, http.StatusBadRequest, "expected_revision_required", "expected_active_revision is required")
		return input, false
	}
	if !validManagedRevision(*input.ExpectedActiveRevision) {
		writeError(writer, http.StatusBadRequest, "invalid_revision", "expected_active_revision must be a SHA-256 identifier")
		return input, false
	}
	if len(input.Reason) > 4096 {
		writeError(writer, http.StatusBadRequest, "invalid_reason", "reason must not exceed 4096 bytes")
		return input, false
	}
	return input, true
}

func authorizeManagedSetRead(writer http.ResponseWriter, request *http.Request, owner managed.Owner) bool {
	if owner == managed.OwnerSparkrun {
		return requireRole(writer, request, RoleConfigRead, RoleConfigReconcileSparkrun)
	}
	return requireRole(writer, request, RoleConfigRead, RoleConfigWrite)
}

func authorizeManagedSetWrite(writer http.ResponseWriter, request *http.Request, owner managed.Owner) bool {
	if owner == managed.OwnerSparkrun {
		return requireRole(writer, request, RoleConfigReconcileSparkrun)
	}
	return requireRole(writer, request, RoleConfigWrite)
}

func strictManagedQuery(writer http.ResponseWriter, values url.Values, allowed ...string) bool {
	known := make(map[string]struct{}, len(allowed))
	for _, name := range allowed {
		known[name] = struct{}{}
	}
	for name, current := range values {
		if _, ok := known[name]; !ok || len(current) != 1 {
			writeError(writer, http.StatusBadRequest, "invalid_query", "query parameters are invalid or unsupported")
			return false
		}
	}
	return true
}

func validManagedRevision(version config.Version) bool {
	if len(version) != 64 {
		return false
	}
	for _, character := range version {
		if (character < '0' || character > '9') && (character < 'a' || character > 'f') {
			return false
		}
	}
	return true
}

func methodNotAllowed(writer http.ResponseWriter, methods ...string) {
	writer.Header().Set("Allow", strings.Join(methods, ", "))
	writeError(writer, http.StatusMethodNotAllowed, "method_not_allowed", "method not allowed")
}

func writeManagedConfigError(writer http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, managed.ErrRevisionConflict):
		writeError(writer, http.StatusConflict, "revision_conflict", "active configuration revision changed")
	case errors.Is(err, managed.ErrPresetConflict):
		writeError(writer, http.StatusConflict, "preset_conflict", err.Error())
	case errors.Is(err, managed.ErrPresetNotFound):
		writeError(writer, http.StatusNotFound, "preset_not_found", err.Error())
	case errors.Is(err, managed.ErrInvalidPreset):
		writeError(writer, http.StatusBadRequest, "invalid_preset", err.Error())
	case errors.Is(err, managed.ErrSetNotFound):
		writeError(writer, http.StatusNotFound, "managed_set_not_found", "managed configuration set not found")
	case errors.Is(err, managed.ErrInvalidConfiguration):
		writeError(writer, http.StatusBadRequest, "invalid_configuration", err.Error())
	default:
		writeError(writer, http.StatusInternalServerError, "internal_error", "internal server error")
	}
}
