// SPDX-License-Identifier: AGPL-3.0-only
// Copyright 2026 Scitrera LLC
// Copyright 2026 Fox Engine Ltd.

package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"

	"github.com/sparksq/sparkroute/pkg/config"
	"github.com/sparksq/sparkroute/pkg/mmprojection"
	"github.com/sparksq/sparkroute/pkg/modelrouter"
)

type mmProjectionAnalyzerContextKey struct{}

func withMMProjectionAnalyzerInvocation(ctx context.Context) context.Context {
	return context.WithValue(ctx, mmProjectionAnalyzerContextKey{}, true)
}

func isMMProjectionAnalyzerInvocation(ctx context.Context) bool {
	value, _ := ctx.Value(mmProjectionAnalyzerContextKey{}).(bool)
	return value
}

func (h *chatCompletionsHandler) projectionOperation() (mmprojection.Operation, bool) {
	switch h.operation {
	case openAIOperationChatCompletions:
		return mmprojection.OperationChat, true
	case openAIOperationResponses:
		return mmprojection.OperationResponses, true
	default:
		return "", false
	}
}

func projectionFailureMode(policy *modelrouter.MMProjectionPolicy) string {
	if policy != nil && strings.TrimSpace(policy.FailureMode) != "" {
		return strings.TrimSpace(policy.FailureMode)
	}
	return "fallback"
}

func failedProjectionTrace(
	policy *modelrouter.MMProjectionPolicy,
	message string,
) *modelrouter.MMProjectionTrace {
	return &modelrouter.MMProjectionTrace{
		ProviderID:  mmprojection.ProviderID,
		State:       "failed",
		FailureMode: projectionFailureMode(policy),
		Error:       compactProjectionError(message),
	}
}

func compactProjectionError(message string) string {
	message = strings.TrimSpace(strings.ReplaceAll(message, "\n", " "))
	if len(message) > 500 {
		return message[:497] + "..."
	}
	return message
}

func withoutCapabilities(
	required []config.Capability,
	projected []string,
) []config.Capability {
	if len(projected) == 0 {
		return required
	}
	remove := make(map[string]struct{}, len(projected))
	for _, capability := range projected {
		remove[capability] = struct{}{}
	}
	result := make([]config.Capability, 0, len(required))
	for _, capability := range required {
		if _, exists := remove[string(capability)]; !exists {
			result = append(result, capability)
		}
	}
	return result
}

func applyMMProjectionObservation(
	header http.Header,
	observation *requestObservation,
	trace *modelrouter.MMProjectionTrace,
) {
	if trace == nil {
		return
	}
	header.Set("X-MM-Projection", trace.State)
	header.Set("X-MM-Projection-Provider", trace.ProviderID)
	observation.setMMProjection(trace)
}

// WrapMMProjectionAnalyzer exposes MMBridge's protected analyzer callback
// outside ordinary caller authentication while retaining the raw data-plane
// handler for direct logical-model delivery. Non-internal paths always use the
// public, caller-authenticated handler.
func WrapMMProjectionAnalyzer(
	publicHandler http.Handler,
	internalHandler http.Handler,
	client *mmprojection.Client,
	router modelrouter.Router,
) http.Handler {
	if client == nil {
		return publicHandler
	}
	allowed := map[string]struct{}{}
	if model := strings.TrimSpace(client.DefaultAnalyzerModel()); model != "" {
		allowed[model] = struct{}{}
	}
	if inspector, ok := router.(modelrouter.ProjectionPolicyInspector); ok {
		for _, model := range inspector.ProjectionAnalyzerModels() {
			if model = strings.TrimSpace(model); model != "" {
				allowed[model] = struct{}{}
			}
		}
	}
	return http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		upstreamPath := ""
		switch request.URL.Path {
		case mmprojection.AnalyzerChatPath:
			upstreamPath = "/v1/chat/completions"
		case mmprojection.AnalyzerResponsesPath:
			upstreamPath = "/v1/responses"
		default:
			if strings.HasPrefix(request.URL.Path, "/internal/mmprojection-analyzer/") {
				http.NotFound(w, request)
				return
			}
			publicHandler.ServeHTTP(w, request)
			return
		}
		if request.Method != http.MethodPost {
			w.Header().Set("Allow", http.MethodPost)
			writeOpenAIError(w, http.StatusMethodNotAllowed, "method_not_allowed", "method not allowed")
			return
		}
		authorized, err := client.Authenticate(
			request.Context(), request.Header.Get("Authorization"),
		)
		if err != nil {
			writeOpenAIError(w, http.StatusServiceUnavailable, "projection_auth_unavailable", "MM projection authentication is unavailable")
			return
		}
		if !authorized {
			w.Header().Set("WWW-Authenticate", "Bearer")
			writeOpenAIError(w, http.StatusUnauthorized, "unauthorized", "unauthorized")
			return
		}
		body, err := io.ReadAll(io.LimitReader(request.Body, defaultMaxRequestBytes+1))
		if err != nil {
			writeOpenAIError(w, http.StatusBadRequest, "invalid_request_body", "could not read request body")
			return
		}
		if int64(len(body)) > defaultMaxRequestBytes {
			writeOpenAIError(w, http.StatusRequestEntityTooLarge, "request_too_large", "request body is too large")
			return
		}
		var envelope map[string]json.RawMessage
		var model string
		if json.Unmarshal(body, &envelope) != nil ||
			json.Unmarshal(envelope["model"], &model) != nil ||
			strings.TrimSpace(model) == "" {
			writeOpenAIError(w, http.StatusBadRequest, "invalid_request_body", "model is required")
			return
		}
		if len(allowed) != 0 {
			if _, exists := allowed[model]; !exists {
				writeOpenAIError(w, http.StatusNotFound, "model_not_found", "requested analyzer model was not found")
				return
			}
		}
		clone := request.Clone(withMMProjectionAnalyzerInvocation(request.Context()))
		urlClone := *request.URL
		urlClone.Path = upstreamPath
		urlClone.RawPath = ""
		clone.URL = &urlClone
		clone.Body = io.NopCloser(bytes.NewReader(body))
		clone.ContentLength = int64(len(body))
		clone.Header = request.Header.Clone()
		for _, name := range []string{
			"Authorization", "Proxy-Authorization", "X-Api-Key", "Api-Key",
			"X-Goog-Api-Key", "Cookie",
		} {
			clone.Header.Del(name)
		}
		clone.Header.Del(mmprojection.HopHeader)
		clone.Header.Del(mmprojection.AnalyzerModelHeader)
		internalHandler.ServeHTTP(w, clone)
	})
}
