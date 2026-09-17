// SPDX-FileCopyrightText: 2026 Scitrera LLC
// SPDX-FileCopyrightText: 2026 Fox Engine Ltd.
// SPDX-License-Identifier: AGPL-3.0-only

package gateway

import (
	"errors"
	"net/http"
	"strings"

	"github.com/sparksq/sparkroute/pkg/config"
	"github.com/sparksq/sparkroute/pkg/routing"
)

const maxUpstreamContinuations = 8

// Once result retrieval starts, the inference may already have executed.
// Retrieval failure must never cause routing to resubmit the inference.
var errUpstreamContinuation = errors.New("upstream result continuation failed")

func continuationEnabled(request *http.Request, provider config.Provider) bool {
	switch provider.Continuations {
	case config.ContinuationsSameOrigin303:
		return true
	case config.ContinuationsDefault:
		// Compatibility with configurations predating the explicit policy.
		return provider.Type == "openai_compatible" && request.Header.Get("Modal-Key") != "" && request.Header.Get("Modal-Secret") != ""
	default:
		return false
	}
}

func (h *chatCompletionsHandler) followUpstreamContinuation(original *http.Request, response *http.Response, selection routing.Selection) (*http.Response, error) {
	if response.StatusCode != http.StatusSeeOther || !continuationEnabled(original, selection.Provider) {
		return response, nil
	}
	for hop := 0; response.StatusCode == http.StatusSeeOther; hop++ {
		next, err := response.Location()
		_ = response.Body.Close()
		if err != nil || hop >= maxUpstreamContinuations || original.URL.Scheme != "https" ||
			next.Scheme != "https" || !strings.EqualFold(next.Host, original.URL.Host) || next.User != nil || next.Fragment != "" {
			return nil, errUpstreamContinuation
		}
		// NewRequest does not copy the POST body, GetBody, or its replay semantics.
		request, err := http.NewRequestWithContext(original.Context(), http.MethodGet, next.String(), nil)
		if err != nil {
			return nil, errUpstreamContinuation
		}
		request.Header = original.Header.Clone()
		for _, name := range []string{"Content-Type", "Content-Length", "Content-Encoding", "Content-Language", "Content-Location", "Transfer-Encoding", "Trailer", "Digest", "Content-Digest", "Repr-Digest", "Content-MD5", "Last-Modified"} {
			request.Header.Del(name)
		}
		if selection.Provider.Auth.Type == config.AuthAWSSigV4 {
			// A POST signature cannot authenticate a different URL and GET body.
			if err := h.applyUpstreamAuthentication(request, selection, nil); err != nil {
				return nil, errUpstreamContinuation
			}
		}
		response, err = h.client.Do(request)
		if err != nil {
			// Do not put a continuation URL (which can contain a signed token) in errors.
			if response != nil {
				_ = response.Body.Close()
			}
			return nil, errUpstreamContinuation
		}
	}
	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		_ = response.Body.Close()
		return nil, errUpstreamContinuation
	}
	return response, nil
}
