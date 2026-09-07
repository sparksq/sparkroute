// SPDX-License-Identifier: AGPL-3.0-only
// Copyright 2026 Scitrera LLC and Fox Engine Ltd.

package admin

import (
	"net/http"
	"net/url"
	"strings"

	"github.com/sparksq/sparkroute/pkg/config"
)

func (h *handler) providerAuth(writer http.ResponseWriter, request *http.Request) {
	writer.Header().Set("Cache-Control", "no-store")
	if !requireRole(writer, request, RoleConfigWrite) {
		return
	}
	if request.URL.RawQuery != "" {
		writeError(writer, http.StatusBadRequest, "invalid_query", "query parameters are not supported")
		return
	}
	parts := strings.Split(strings.TrimPrefix(request.URL.Path, "/v1/provider-auth/openai/"), "/")
	if len(parts) > 2 || config.ValidateSubscriptionProfile(parts[0]) != nil {
		writeError(writer, http.StatusNotFound, "not_found", "profile route not found")
		return
	}
	principal, _ := principalFromRequest(request)
	actor := principal.Tenant + "\x00" + principal.ID
	name := parts[0]
	if len(parts) == 1 && request.Method == http.MethodGet {
		status, err := h.options.ProviderAuth.Status(request.Context(), name, actor)
		if err != nil {
			writeError(writer, http.StatusServiceUnavailable, "provider_auth_unavailable", "could not read provider sign-in status")
			return
		}
		writeJSON(writer, http.StatusOK, status)
		return
	}
	if len(parts) != 2 || request.Method != http.MethodPost {
		methodNotAllowed(writer, http.MethodGet, http.MethodPost)
		return
	}
	if request.Header.Get("Sec-Fetch-Site") == "cross-site" {
		writeError(writer, http.StatusForbidden, "forbidden", "cross-site sign-in requests are not allowed")
		return
	}
	if origin := request.Header.Get("Origin"); origin != "" {
		parsed, err := url.Parse(origin)
		if err != nil || parsed.Host != request.Host || (parsed.Scheme != "https" && parsed.Scheme != "http") {
			writeError(writer, http.StatusForbidden, "forbidden", "cross-origin sign-in requests are not allowed")
			return
		}
	}
	var input struct{}
	if !decodeRequestJSON(writer, request, &input) {
		return
	}
	switch parts[1] {
	case "login":
		status, err := h.options.ProviderAuth.Begin(request.Context(), name, actor)
		if err != nil {
			writeError(writer, http.StatusConflict, "sign_in_conflict", err.Error())
			return
		}
		writeJSON(writer, http.StatusAccepted, status)
	case "cancel", "logout":
		if err := h.options.ProviderAuth.Cancel(request.Context(), name, actor, parts[1] == "logout"); err != nil {
			writeError(writer, http.StatusConflict, "sign_out_incomplete", err.Error())
			return
		}
		status, err := h.options.ProviderAuth.Status(request.Context(), name, actor)
		if err != nil {
			writeError(writer, http.StatusServiceUnavailable, "provider_auth_unavailable", "could not read provider sign-in status")
			return
		}
		writeJSON(writer, http.StatusOK, status)
	default:
		writeError(writer, http.StatusNotFound, "not_found", "sign-in action not found")
	}
}
