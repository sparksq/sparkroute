// SPDX-FileCopyrightText: 2026 Scitrera LLC
// SPDX-FileCopyrightText: 2026 Fox Engine Ltd.
// SPDX-License-Identifier: AGPL-3.0-only

package gateway

import (
	"errors"
	"net/http"
	"strings"
)

const maxModalContinuations = 8

// A 303 means Modal accepted the inference already. Failure to retrieve its
// result must never cause the routing retry loop to submit that inference again.
var errUpstreamContinuation = errors.New("upstream result continuation failed")

func (h *chatCompletionsHandler) followModalContinuation(original *http.Request, response *http.Response) (*http.Response, error) {
	if response.StatusCode != http.StatusSeeOther || original.Header.Get("Modal-Key") == "" || original.Header.Get("Modal-Secret") == "" {
		return response, nil
	}
	for hop := 0; response.StatusCode == http.StatusSeeOther; hop++ {
		next, err := response.Location()
		_ = response.Body.Close()
		if err != nil || hop >= maxModalContinuations || original.URL.Scheme != "https" ||
			next.Scheme != "https" || !strings.EqualFold(next.Host, original.URL.Host) || next.User != nil || next.Fragment != "" {
			return nil, errUpstreamContinuation
		}
		// NewRequest does not copy the POST body, GetBody, or its replay semantics.
		request, err := http.NewRequestWithContext(original.Context(), http.MethodGet, next.String(), nil)
		if err != nil {
			return nil, errUpstreamContinuation
		}
		request.Header = original.Header.Clone()
		for _, name := range []string{"Content-Type", "Content-Length", "Content-Encoding", "Transfer-Encoding"} {
			request.Header.Del(name)
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
