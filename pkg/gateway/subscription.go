// SPDX-License-Identifier: AGPL-3.0-only
// Copyright 2026 Scitrera LLC and Fox Engine Ltd.

package gateway

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"

	llmprotocol "github.com/scitrera/go-llm/protocol"
	"github.com/scitrera/go-llm/protocol/codec"
	openaiprovider "github.com/scitrera/go-llm/provider/openai"
	upstreamheaders "github.com/sparksq/sparkroute/pkg/headers"
	"github.com/sparksq/sparkroute/pkg/routing"
	"github.com/sparksq/sparkroute/pkg/version"
)

func prepareSubscriptionBody(body []byte) ([]byte, error) {
	format := codec.OpenAIResponses{}
	policy := llmprotocol.StrictPolicy()
	decoded, err := format.DecodeRequest(body, policy)
	if err != nil {
		return nil, err
	}
	request := decoded.Request
	if request.State.Store != nil && *request.State.Store || request.State.Background != nil && *request.State.Background ||
		request.State.PreviousResponseID != "" || len(request.State.Conversation) > 0 {
		return nil, fmt.Errorf("subscription Responses require full transcript input, store=false, and no background or conversation state")
	}
	request.ClearPreservation()
	openaiprovider.PrepareSubscriptionRequest(&request)
	request.Stream = true // Codex serves SSE; buffered callers are assembled below.
	encoded, err := format.EncodeRequest(request, policy)
	if err != nil {
		return nil, err
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(encoded.Body, &fields); err != nil {
		return nil, err
	}
	if _, ok := fields["instructions"]; !ok {
		fields["instructions"] = json.RawMessage(`""`)
	}
	return json.Marshal(fields)
}

func (h *chatCompletionsHandler) buildSubscriptionRequest(downstream *http.Request, requestID string, selection routing.Selection, body []byte) (*http.Request, error) {
	if h.operation != openAIOperationResponses || h.providerAuth == nil {
		return nil, fmt.Errorf("subscription provider requires Responses and managed sign-in")
	}
	body, err := prepareSubscriptionBody(body)
	if err != nil {
		return nil, err
	}
	request, err := http.NewRequestWithContext(downstream.Context(), http.MethodPost,
		openaiprovider.SubscriptionBaseURL+openaiprovider.SubscriptionPath, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	request.Header, err = upstreamheaders.Resolve(downstream.Context(), selection.Provider.DefaultHeaders,
		selection.Deployment.UpstreamHeaders, upstreamheaders.PurposeInference, h.credentials)
	if err != nil {
		return nil, err
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Accept", "text/event-stream")
	request.Header.Set("User-Agent", version.UserAgent())
	request.Header.Set("X-Request-Id", requestID)
	h.telemetry.Inject(request.Context(), request.Header)
	if err := h.providerAuth.Apply(request.Context(), selection.Provider.SubscriptionProfile, request); err != nil {
		return nil, err
	}
	return request, nil
}

// Authentication recovery is bounded to one replay, before any response reaches
// the caller, and uses the same attempt deadline and immutable request body.
func (h *chatCompletionsHandler) sendUpstream(request *http.Request, selection routing.Selection) (*http.Response, error) {
	response, err := h.client.Do(request)
	if err != nil || selection.Provider.Type != "openai_subscription" || response.StatusCode != http.StatusUnauthorized {
		return response, err
	}
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
	return h.client.Do(retry)
}

// collectSubscriptionResponse retains the provider's complete terminal object,
// including tool calls and usage. Missing/failed terminal events never become a
// successful partial buffered response. Idle and overall deadlines are supplied
// by the existing proxy response path.
func collectSubscriptionResponse(source io.Reader, limit int64) ([]byte, error) {
	terminal := errors.New("terminal subscription response")
	var result json.RawMessage
	limited := &io.LimitedReader{R: source, N: limit + 1}
	err := proxySSEEvents(io.Discard, limited, nil, func(payload []byte) error {
		var event struct {
			Type     string          `json:"type"`
			Response json.RawMessage `json:"response"`
		}
		if err := json.Unmarshal(payload, &event); err != nil {
			return err
		}
		switch event.Type {
		case "response.completed", "response.incomplete":
			if len(event.Response) == 0 || !json.Valid(event.Response) || bytes.Equal(event.Response, []byte("null")) {
				return fmt.Errorf("subscription terminal event omitted its response")
			}
			result = append(json.RawMessage(nil), event.Response...)
			return terminal
		case "error", "response.failed":
			return fmt.Errorf("subscription stream failed")
		}
		return nil
	})
	if limited.N <= 0 {
		return nil, fmt.Errorf("subscription response exceeds the configured size limit")
	}
	if errors.Is(err, terminal) {
		return result, nil
	}
	if err != nil {
		return nil, err
	}
	return nil, fmt.Errorf("subscription stream ended before its terminal response")
}
