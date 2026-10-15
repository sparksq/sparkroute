// SPDX-FileCopyrightText: 2026 Scitrera LLC
// SPDX-FileCopyrightText: 2026 Fox Engine Ltd.
// SPDX-License-Identifier: AGPL-3.0-only

package admin

import (
	"net/http"
	"strings"

	"github.com/sparksq/sparkroute/pkg/config"
	"github.com/sparksq/sparkroute/pkg/config/managed"
)

func (h *handler) configurationPresets(writer http.ResponseWriter, request *http.Request) {
	store, ok := h.options.ManagedConfig.(managed.PresetStore)
	if !ok {
		writeError(writer, http.StatusNotFound, "not_found", "configuration presets are unavailable")
		return
	}
	if !strictManagedQuery(writer, request.URL.Query()) {
		return
	}
	if request.Method == http.MethodGet {
		if !requireRole(writer, request, RoleConfigRead, RoleConfigWrite) {
			return
		}
		if request.URL.Path != "/v1/config/presets" {
			writeError(writer, http.StatusNotFound, "not_found", "route not found")
			return
		}
		catalog, err := store.ListPresets(request.Context())
		if err != nil {
			writeManagedConfigError(writer, err)
			return
		}
		writer.Header().Set("Cache-Control", "no-store")
		writeJSON(writer, http.StatusOK, catalog)
		return
	}
	if request.Method != http.MethodPost {
		methodNotAllowed(writer, http.MethodGet, http.MethodPost)
		return
	}
	if !requireRole(writer, request, RoleConfigWrite) {
		return
	}
	operation := strings.TrimPrefix(request.URL.Path, "/v1/config/presets/")
	if operation != "save" && operation != "activate" && operation != "rename" && operation != "delete" {
		writeError(writer, http.StatusNotFound, "not_found", "route not found")
		return
	}
	var input struct {
		ID              string         `json:"id"`
		Name            string         `json:"name"`
		ExpectedActive  config.Version `json:"expected_active_revision"`
		ExpectedPresets *int64         `json:"expected_presets_revision"`
	}
	if !decodeRequestJSON(writer, request, &input) {
		return
	}
	if !validManagedRevision(input.ExpectedActive) || input.ExpectedPresets == nil || *input.ExpectedPresets < 1 {
		writeError(writer, http.StatusBadRequest, "expected_revision_required", "configuration and presets revisions are required")
		return
	}
	principal, _ := principalFromRequest(request)
	options := managed.ReplaceOptions{ExpectedActive: input.ExpectedActive, ExpectedPresetsRevision: input.ExpectedPresets, Actor: principal.ID, Reason: "preset " + operation}
	var result any
	var err error
	switch operation {
	case "save":
		result, err = store.SavePreset(request.Context(), input.Name, options)
	case "rename":
		err = store.RenamePreset(request.Context(), input.ID, input.Name, options)
	case "delete":
		err = store.DeletePreset(request.Context(), input.ID, options)
	case "activate":
		var preset managed.Preset
		preset, err = store.GetPreset(request.Context(), input.ID)
		if err == nil && !h.validateSparkrunCandidate(writer, request, managed.OwnerOperator, managedSetMutation{
			Document: preset.Document, ExpectedActiveRevision: &input.ExpectedActive,
		}, true) {
			return
		}

		if err == nil {
			result, err = store.ActivatePreset(request.Context(), input.ID, options)
		}
	}
	if err != nil {
		writeManagedConfigError(writer, err)
		return
	}
	if result == nil {
		result = map[string]bool{"ok": true}
	}
	writer.Header().Set("Cache-Control", "no-store")
	writeJSON(writer, http.StatusOK, result)
}
