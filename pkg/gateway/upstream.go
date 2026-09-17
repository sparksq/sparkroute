// SPDX-FileCopyrightText: 2026 Scitrera LLC
// SPDX-FileCopyrightText: 2026 Fox Engine Ltd.
// SPDX-License-Identifier: AGPL-3.0-only

package gateway

import (
	"fmt"
	"net/http"
	"time"

	"github.com/sparksq/sparkroute/pkg/config"
	upstreamheaders "github.com/sparksq/sparkroute/pkg/headers"
	"github.com/sparksq/sparkroute/pkg/routing"
)

// sendUpstream retrieves the final inference response before protocol decoding.
// Authentication recovery may replay only an initially rejected request, never
// one whose result is already being retrieved through a continuation.
func (h *chatCompletionsHandler) sendUpstream(request *http.Request, selection routing.Selection) (*http.Response, error) {
	response, err := h.client.Do(request)
	if err != nil {
		return response, err
	}
	if selection.Provider.Type == "openai_subscription" && response.StatusCode == http.StatusUnauthorized {
		_ = response.Body.Close()
		profile := selection.Provider.SubscriptionProfile
		if err := h.providerAuth.RecoverUnauthorized(request.Context(), profile); err != nil {
			return nil, err
		}
		retry := request.Clone(request.Context())
		retry.Body, err = request.GetBody()
		if err != nil {
			return nil, err
		}
		if err := h.providerAuth.Apply(retry.Context(), profile, retry); err != nil {
			_ = retry.Body.Close()
			return nil, err
		}
		response, err = h.client.Do(retry)
		if err != nil {
			return response, err
		}
		request = retry
	}
	return h.followUpstreamContinuation(request, response, selection)
}

func (h *chatCompletionsHandler) applyUpstreamAuthentication(request *http.Request, selection routing.Selection, body []byte) error {
	if selection.Provider.Auth.Type != config.AuthAWSSigV4 {
		return upstreamheaders.ApplyAuthentication(request.Context(), request.Header,
			selection.Provider.Auth, selection.Deployment.Credential, h.credentials)
	}
	ref := selection.Deployment.Credential
	if ref == "" {
		ref = selection.Provider.Auth.Credential
	}
	if h.credentials == nil {
		return fmt.Errorf("AWS workload credential source is required")
	}
	material, err := h.credentials.Resolve(request.Context(), ref)
	if err != nil {
		return fmt.Errorf("resolve AWS workload credential: %w", err)
	}
	if err := signAWSRequest(request, material.Value, selection.Provider.Region,
		bedrockAWSService, body, time.Now()); err != nil {
		return fmt.Errorf("sign Bedrock request: %w", err)
	}
	return nil
}
