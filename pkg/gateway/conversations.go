package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/sparksq/sparkroute/pkg/config"
	upstreamheaders "github.com/sparksq/sparkroute/pkg/headers"
	"github.com/sparksq/sparkroute/pkg/identity"
	"github.com/sparksq/sparkroute/pkg/ledger"
	"github.com/sparksq/sparkroute/pkg/responsesstate"
	"github.com/sparksq/sparkroute/pkg/routing"
	"github.com/sparksq/sparkroute/pkg/telemetry"
	"github.com/sparksq/sparkroute/pkg/version"
)

const defaultConversationModel = "default"

type conversationsHandler struct {
	core *chatCompletionsHandler
}

type conversationRoute struct {
	operation      string
	method         string
	allowed        string
	conversationID string
	itemID         string
	bodyRequired   bool
	inspectItems   bool
	createResource bool
	deleteResource bool
	upstreamSuffix string
}

func newConversationsHandler(
	snapshot *routing.Snapshot,
	targets *routing.TargetManager,
	retryBudget routing.RetryBudget,
	options DataOptions,
	instrumentation *telemetry.Instrumentation,
) http.Handler {
	return &conversationsHandler{core: newOpenAIHandler(
		openAIOperationResponses,
		snapshot,
		targets,
		retryBudget,
		options,
		instrumentation,
	)}
}

func (h *conversationsHandler) ServeHTTP(
	w http.ResponseWriter,
	request *http.Request,
) {
	callerContext := request.Context()
	callerIdentity, _ := identity.FromContext(callerContext)
	route, routeStatus, routeCode, routeMessage := matchConversationRoute(request)
	operation := route.operation
	if operation == "" {
		operation = "conversations_resource"
	}
	requestID, err := newRequestID()
	if err != nil {
		writeOpenAIError(
			w,
			http.StatusInternalServerError,
			"request_id_error",
			"could not create request ID",
		)
		return
	}
	requestContext, telemetryRequest := h.core.telemetry.StartRequest(
		request.Context(),
		request.Header,
		telemetry.RequestStart{
			RequestID:      requestID,
			Protocol:       "openai",
			Operation:      operation,
			ConfigRevision: h.core.configRevision,
			Identity:       callerIdentity,
		},
	)
	request = request.WithContext(requestContext)
	traceWriter := newSavedTraceResponseWriter(
		w,
		h.core.savedTraces,
		h.core.maxSavedTraceBytes,
	)
	if traceWriter != nil {
		w = traceWriter
	}
	observation := newRequestObservation(
		h.core.ledger,
		requestID,
		h.core.configRevision,
		operation,
		callerIdentity,
		telemetryRequest,
	)
	observation.enableSavedTrace(
		h.core.savedTraces,
		traceWriter,
		h.core.maxSavedTraceBytes,
		request.Header.Get("Content-Type"),
	)
	observation.setSavedTraceConversationID(route.conversationID)
	defer observation.finish()
	fail := func(status int, outcome ledger.Outcome, code, message string) {
		observation.fail(status, outcome, code)
		writeOpenAIError(w, status, code, message)
	}

	w.Header().Set(h.core.requestIDHeader, requestID)
	if routeStatus != 0 {
		if routeStatus == http.StatusMethodNotAllowed && route.allowed != "" {
			w.Header().Set("Allow", route.allowed)
		}
		fail(routeStatus, ledger.OutcomeRejected, routeCode, routeMessage)
		return
	}
	resourceStore, supportsResources := h.core.responsesState.(responsesstate.ResourceStore)
	if !supportsResources {
		fail(
			http.StatusServiceUnavailable,
			ledger.OutcomeConfigError,
			"state_affinity_unavailable",
			"provider-owned conversation state storage is not configured",
		)
		return
	}
	if route.method == http.MethodPost &&
		!isJSONContentType(request.Header.Get("Content-Type")) {
		fail(
			http.StatusUnsupportedMediaType,
			ledger.OutcomeRejected,
			"unsupported_content_type",
			"Content-Type must be application/json",
		)
		return
	}

	body, envelope, err := h.readRequestBody(w, request, route)
	if err != nil {
		var maxBytesError *http.MaxBytesError
		if errors.As(err, &maxBytesError) {
			fail(
				http.StatusRequestEntityTooLarge,
				ledger.OutcomeRejected,
				"request_too_large",
				"request body is too large",
			)
			return
		}
		fail(
			http.StatusBadRequest,
			ledger.OutcomeRejected,
			requestValidationCode(err),
			err.Error(),
		)
		return
	}
	observation.captureSavedTraceRequest(body)
	requiredCapabilities, err := conversationCapabilities(envelope, route.inspectItems)
	if err != nil {
		fail(
			http.StatusBadRequest,
			ledger.OutcomeRejected,
			requestValidationCode(err),
			err.Error(),
		)
		return
	}
	var itemIDs []string
	if route.inspectItems {
		itemIDs, err = collectResponsesItemAffinityIDs(envelope["items"])
		if err != nil {
			fail(
				http.StatusBadRequest,
				ledger.OutcomeRejected,
				"invalid_request_body",
				err.Error(),
			)
			return
		}
	}
	observation.telemetry.SetRequiredCapabilities(
		capabilityStrings(requiredCapabilities),
	)

	ownerScope := responsesCallerScope(callerIdentity)
	requestedModel := defaultConversationModel
	var affinity responsesstate.ResourceAffinity
	if route.conversationID != "" {
		key := responsesstate.ResourceKey{
			Scope:      ownerScope,
			Kind:       responsesstate.ResourceConversation,
			ResourceID: route.conversationID,
		}
		var found bool
		affinity, found, err = resourceStore.ResolveResource(request.Context(), key)
		if err != nil {
			fail(
				http.StatusServiceUnavailable,
				ledger.OutcomeConfigError,
				"state_affinity_unavailable",
				"provider-owned conversation state could not be resolved",
			)
			return
		}
		if !found || affinity.Deleted() {
			fail(
				http.StatusNotFound,
				ledger.OutcomeRejected,
				"state_affinity_not_found",
				"conversation is unknown or deleted at this gateway",
			)
			return
		}
		requestedModel = affinity.VirtualModel
	}
	var itemAffinity responsesstate.ResourceAffinity
	if len(itemIDs) != 0 {
		model, resolveModelErr := h.core.snapshot.ResolveModel(requestedModel, false)
		if resolveModelErr != nil {
			code := "state_affinity_model_unavailable"
			message := "the conversation's virtual model is no longer configured"
			if route.createResource {
				code = "default_model_not_found"
				message = "conversation creation requires an externally addressable default virtual model"
			}
			fail(
				http.StatusServiceUnavailable,
				ledger.OutcomeConfigError,
				code,
				message,
			)
			return
		}
		var affinityResult itemAffinityResult
		itemAffinity, affinityResult, err = resolveItemAffinities(
			request.Context(),
			resourceStore,
			ownerScope,
			model.Name,
			itemIDs,
		)
		if err != nil {
			fail(
				http.StatusServiceUnavailable,
				ledger.OutcomeConfigError,
				"state_affinity_unavailable",
				"provider-owned item routing state could not be resolved",
			)
			return
		}
		switch affinityResult {
		case itemAffinityNotFound:
			fail(
				http.StatusConflict,
				ledger.OutcomeRejected,
				"state_affinity_not_found",
				"provider-owned item is unknown or deleted at this gateway",
			)
			return
		case itemAffinityModelMismatch:
			fail(
				http.StatusConflict,
				ledger.OutcomeRejected,
				"state_affinity_model_mismatch",
				"provider-owned item belongs to a different virtual model",
			)
			return
		case itemAffinityTargetMismatch:
			fail(
				http.StatusConflict,
				ledger.OutcomeRejected,
				"state_affinity_conflict",
				"provider-owned items belong to different provider targets",
			)
			return
		}
		if route.conversationID != "" &&
			(affinity.Deployment != itemAffinity.Deployment ||
				affinity.Provider != itemAffinity.Provider ||
				affinity.UpstreamModel != itemAffinity.UpstreamModel) {
			fail(
				http.StatusConflict,
				ledger.OutcomeRejected,
				"state_affinity_conflict",
				"provider-owned item and conversation belong to different targets",
			)
			return
		}
	}
	observation.record.RequestedModel = requestedModel
	planOptions := routing.PlanOptions{
		RequiredCapabilities: requiredCapabilities,
		ProtocolResolver:     nativeProtocolResolver(config.ProtocolOpenAI),
		SingleAttempt:        true,
		Eligibility:          h.core.targets,
		Picker:               h.core.picker,
	}
	if route.conversationID != "" {
		planOptions.PinnedDeployment = affinity.Deployment
		planOptions.PinnedProvider = affinity.Provider
	} else if len(itemIDs) != 0 {
		planOptions.PinnedDeployment = itemAffinity.Deployment
		planOptions.PinnedProvider = itemAffinity.Provider
	}
	plan, err := h.core.snapshot.BuildPlanFor(requestedModel, planOptions)
	if err != nil {
		switch {
		case errors.Is(err, routing.ErrModelNotFound) && route.createResource:
			fail(
				http.StatusServiceUnavailable,
				ledger.OutcomeConfigError,
				"default_model_not_found",
				"conversation creation requires an externally addressable default virtual model",
			)
		case errors.Is(err, routing.ErrModelNotFound):
			fail(
				http.StatusServiceUnavailable,
				ledger.OutcomeConfigError,
				"state_affinity_model_unavailable",
				"the conversation's virtual model is no longer configured",
			)
		case errors.Is(err, routing.ErrUnsupportedCapabilities) &&
			route.conversationID != "":
			fail(
				http.StatusServiceUnavailable,
				ledger.OutcomeConfigError,
				"state_affinity_capability_unavailable",
				"the conversation's target no longer supports this operation",
			)
		case errors.Is(err, routing.ErrUnsupportedCapabilities):
			fail(
				http.StatusBadRequest,
				ledger.OutcomeRejected,
				"unsupported_feature",
				"no configured default target supports the conversation request",
			)
		default:
			code := "default_model_unavailable"
			message := "the default virtual model has no available conversation target"
			if route.conversationID != "" {
				code = "state_affinity_target_unavailable"
				message = "the provider-owned conversation state target is unavailable"
			}
			fail(
				http.StatusServiceUnavailable,
				ledger.OutcomeExhausted,
				code,
				message,
			)
		}
		return
	}
	pinnedUpstreamModel := ""
	if route.conversationID != "" {
		pinnedUpstreamModel = affinity.UpstreamModel
	} else if len(itemIDs) != 0 {
		pinnedUpstreamModel = itemAffinity.UpstreamModel
	}
	if pinnedUpstreamModel != "" &&
		plan.Candidates[0].Deployment.Model != pinnedUpstreamModel {
		fail(
			http.StatusServiceUnavailable,
			ledger.OutcomeConfigError,
			"state_affinity_target_changed",
			"the provider-owned conversation state target changed upstream model",
		)
		return
	}
	observation.resolve(plan, false)
	selection := plan.Candidates[0]
	lease, _ := h.core.targets.Acquire(selection.Deployment.Name)
	if lease == nil {
		code := "default_model_unavailable"
		message := "the default conversation target has no capacity available"
		if route.conversationID != "" {
			code = "state_affinity_target_unavailable"
			message = "the provider-owned conversation state target has no capacity available"
		}
		fail(
			http.StatusServiceUnavailable,
			ledger.OutcomeExhausted,
			code,
			message,
		)
		return
	}

	attempt := 1
	attemptStarted := time.Now()
	attemptContext, telemetryAttempt := observation.startAttempt(
		selection,
		attempt,
		lease,
		plan.RequiredCapabilities,
		"",
		0,
	)
	attemptTimeout := plan.Limits.PerTryTimeout
	if attemptTimeout > plan.Limits.OverallTimeout {
		attemptTimeout = plan.Limits.OverallTimeout
	}
	attemptExecutionContext, cancelAttempt := context.WithTimeout(
		attemptContext,
		attemptTimeout,
	)
	defer cancelAttempt()
	upstreamRequest, err := h.buildUpstreamRequest(
		request.WithContext(attemptExecutionContext),
		requestID,
		selection,
		route,
		body,
	)
	if err != nil {
		h.recordConversationFailure(
			w,
			callerContext,
			attemptExecutionContext,
			observation,
			telemetryAttempt,
			lease,
			requestID,
			attempt,
			attemptStarted,
			selection,
			err,
		)
		return
	}
	response, err := h.core.client.Do(upstreamRequest)
	if err != nil {
		h.recordConversationFailure(
			w,
			callerContext,
			attemptExecutionContext,
			observation,
			telemetryAttempt,
			lease,
			requestID,
			attempt,
			attemptStarted,
			selection,
			err,
		)
		return
	}

	firstByte := time.Now()
	observation.selectFinal(selection, attempt, firstByte)
	result := h.proxyResponse(
		w,
		request,
		response,
		requestID,
		attempt,
		selection,
		route,
		ownerScope,
		resourceStore,
	)
	recordAttempt(
		h.core.ledger,
		telemetryAttempt,
		h.core.targets,
		lease,
		responsesResourceTargetResult(response.StatusCode, result),
		requestID,
		attempt,
		attemptStarted,
		&firstByte,
		selection,
		response.StatusCode,
		result.outcome,
		result.failureClass,
		false,
		upstreamRequestID(response.Header),
		missingUsage(),
	)
	observation.completeResponse(result)
}

func (h *conversationsHandler) readRequestBody(
	w http.ResponseWriter,
	request *http.Request,
	route conversationRoute,
) ([]byte, map[string]json.RawMessage, error) {
	if route.method != http.MethodPost {
		return nil, nil, nil
	}
	raw, err := readBoundedBody(w, request, h.core.maxRequestBytes)
	if err != nil {
		return nil, nil, err
	}
	if len(bytes.TrimSpace(raw)) == 0 {
		if route.bodyRequired {
			return nil, nil, fmt.Errorf("request body is required")
		}
		raw = []byte("{}")
	}
	var envelope map[string]json.RawMessage
	if err := json.Unmarshal(raw, &envelope); err != nil || envelope == nil {
		return nil, nil, fmt.Errorf("request body must be a JSON object")
	}
	return raw, envelope, nil
}

func conversationCapabilities(
	envelope map[string]json.RawMessage,
	inspectItems bool,
) ([]config.Capability, error) {
	required := map[config.Capability]struct{}{
		config.CapabilityConversations: {},
	}
	if inspectItems && rawActive(envelope["items"]) {
		add := func(capabilities ...config.Capability) {
			for _, capability := range capabilities {
				required[capability] = struct{}{}
			}
		}
		if err := inspectResponsesInput(envelope["items"], add); err != nil {
			return nil, fmt.Errorf("items: %w", err)
		}
	}
	result := make([]config.Capability, 0, len(required))
	for capability := range required {
		result = append(result, capability)
	}
	sort.Slice(result, func(i, j int) bool {
		return result[i] < result[j]
	})
	return result, nil
}

func matchConversationRoute(
	request *http.Request,
) (conversationRoute, int, string, string) {
	if _, err := url.ParseQuery(request.URL.RawQuery); err != nil {
		return conversationRoute{}, http.StatusBadRequest, "invalid_query", "query parameters are invalid"
	}
	if request.URL.Path == "/v1/conversations" {
		route := conversationRoute{
			operation:      "conversations_create",
			method:         http.MethodPost,
			allowed:        http.MethodPost,
			inspectItems:   true,
			createResource: true,
		}
		if request.Method != route.method {
			return route, http.StatusMethodNotAllowed, "method_not_allowed", "method not allowed"
		}
		return route, 0, "", ""
	}
	const prefix = "/v1/conversations/"
	if !strings.HasPrefix(request.URL.Path, prefix) {
		return conversationRoute{}, http.StatusNotFound, "route_not_found", "route not found"
	}
	parts := strings.Split(strings.TrimPrefix(request.URL.Path, prefix), "/")
	if len(parts) < 1 || len(parts) > 3 || parts[0] == "" {
		return conversationRoute{}, http.StatusNotFound, "route_not_found", "route not found"
	}
	conversationID := parts[0]
	if invalidConversationPathID(conversationID) {
		return conversationRoute{}, http.StatusBadRequest, "invalid_conversation_id", "conversation ID is invalid"
	}
	if len(parts) == 1 {
		switch request.Method {
		case http.MethodGet:
			return conversationRoute{
				operation:      "conversations_retrieve",
				method:         http.MethodGet,
				allowed:        http.MethodGet + ", " + http.MethodPost + ", " + http.MethodDelete,
				conversationID: conversationID,
			}, 0, "", ""
		case http.MethodPost:
			return conversationRoute{
				operation:      "conversations_update",
				method:         http.MethodPost,
				allowed:        http.MethodGet + ", " + http.MethodPost + ", " + http.MethodDelete,
				conversationID: conversationID,
				bodyRequired:   true,
			}, 0, "", ""
		case http.MethodDelete:
			return conversationRoute{
				operation:      "conversations_delete",
				method:         http.MethodDelete,
				allowed:        http.MethodGet + ", " + http.MethodPost + ", " + http.MethodDelete,
				conversationID: conversationID,
				deleteResource: true,
			}, 0, "", ""
		default:
			return conversationRoute{
				allowed: http.MethodGet + ", " + http.MethodPost + ", " + http.MethodDelete,
			}, http.StatusMethodNotAllowed, "method_not_allowed", "method not allowed"
		}
	}
	if parts[1] != "items" {
		return conversationRoute{}, http.StatusNotFound, "route_not_found", "route not found"
	}
	if len(parts) == 2 {
		switch request.Method {
		case http.MethodGet:
			return conversationRoute{
				operation:      "conversation_items_list",
				method:         http.MethodGet,
				allowed:        http.MethodGet + ", " + http.MethodPost,
				conversationID: conversationID,
				upstreamSuffix: "/items",
			}, 0, "", ""
		case http.MethodPost:
			return conversationRoute{
				operation:      "conversation_items_create",
				method:         http.MethodPost,
				allowed:        http.MethodGet + ", " + http.MethodPost,
				conversationID: conversationID,
				bodyRequired:   true,
				inspectItems:   true,
				upstreamSuffix: "/items",
			}, 0, "", ""
		default:
			return conversationRoute{
				allowed: http.MethodGet + ", " + http.MethodPost,
			}, http.StatusMethodNotAllowed, "method_not_allowed", "method not allowed"
		}
	}
	itemID := parts[2]
	if itemID == "" || invalidConversationPathID(itemID) {
		return conversationRoute{}, http.StatusBadRequest, "invalid_item_id", "item ID is invalid"
	}
	switch request.Method {
	case http.MethodGet:
		return conversationRoute{
			operation:      "conversation_items_retrieve",
			method:         http.MethodGet,
			allowed:        http.MethodGet + ", " + http.MethodDelete,
			conversationID: conversationID,
			itemID:         itemID,
			upstreamSuffix: "/items/" + itemID,
		}, 0, "", ""
	case http.MethodDelete:
		return conversationRoute{
			operation:      "conversation_items_delete",
			method:         http.MethodDelete,
			allowed:        http.MethodGet + ", " + http.MethodDelete,
			conversationID: conversationID,
			itemID:         itemID,
			upstreamSuffix: "/items/" + itemID,
		}, 0, "", ""
	default:
		return conversationRoute{
			allowed: http.MethodGet + ", " + http.MethodDelete,
		}, http.StatusMethodNotAllowed, "method_not_allowed", "method not allowed"
	}
}

func invalidConversationPathID(value string) bool {
	return value == "." ||
		value == ".." ||
		strings.Contains(value, "\\") ||
		responsesstate.ValidateResourceID(value) != nil
}

func (h *conversationsHandler) buildUpstreamRequest(
	downstream *http.Request,
	requestID string,
	selection routing.Selection,
	route conversationRoute,
	body []byte,
) (*http.Request, error) {
	if selectionUpstreamProtocol(selection) != config.ProtocolOpenAI ||
		!selection.Deployment.SupportsNativeProtocol(
			selection.Provider,
			config.ProtocolOpenAI,
		) {
		return nil, fmt.Errorf(
			"deployment %q does not natively support OpenAI Conversations",
			selection.Deployment.Name,
		)
	}
	endpoint, err := conversationURL(
		selection.Provider.BaseURL,
		route,
		downstream.URL.RawQuery,
	)
	if err != nil {
		return nil, err
	}
	var source io.Reader
	if body != nil {
		source = bytes.NewReader(body)
	}
	upstreamRequest, err := http.NewRequestWithContext(
		downstream.Context(),
		route.method,
		endpoint,
		source,
	)
	if err != nil {
		return nil, err
	}
	resolvedHeaders, err := upstreamheaders.Resolve(
		downstream.Context(),
		selection.Provider.DefaultHeaders,
		selection.Deployment.UpstreamHeaders,
		upstreamheaders.PurposeInference,
		h.core.credentials,
	)
	if err != nil {
		return nil, err
	}
	upstreamRequest.Header = resolvedHeaders
	upstreamRequest.Header.Set("Accept", "application/json")
	if body != nil {
		upstreamRequest.Header.Set("Content-Type", "application/json")
	}
	upstreamRequest.Header.Set("User-Agent", version.UserAgent())
	upstreamRequest.Header.Set("X-Request-Id", requestID)
	if err := upstreamheaders.ApplyAuthentication(
		downstream.Context(),
		upstreamRequest.Header,
		selection.Provider.Auth,
		selection.Deployment.Credential,
		h.core.credentials,
	); err != nil {
		return nil, err
	}
	h.core.telemetry.Inject(upstreamRequest.Context(), upstreamRequest.Header)
	return upstreamRequest, nil
}

func conversationURL(
	baseURL string,
	route conversationRoute,
	rawQuery string,
) (string, error) {
	parsed, err := url.Parse(baseURL)
	if err != nil {
		return "", err
	}
	path := strings.TrimRight(parsed.Path, "/") + "/conversations"
	if route.conversationID != "" {
		path += "/" + route.conversationID + route.upstreamSuffix
	}
	parsed.Path = path
	parsed.RawPath = ""
	parsed.RawQuery = rawQuery
	parsed.Fragment = ""
	return parsed.String(), nil
}

func (h *conversationsHandler) proxyResponse(
	w http.ResponseWriter,
	request *http.Request,
	response *http.Response,
	requestID string,
	attempt int,
	selection routing.Selection,
	route conversationRoute,
	ownerScope string,
	resourceStore responsesstate.ResourceStore,
) proxyResult {
	defer func() { _ = response.Body.Close() }()
	result := proxyResult{
		gatewayStatus: response.StatusCode,
		outcome:       ledger.OutcomeUpstreamError,
		failureClass:  fmt.Sprintf("upstream_http_%d", response.StatusCode),
		usage:         missingUsage(),
	}
	success := response.StatusCode >= http.StatusOK &&
		response.StatusCode < http.StatusMultipleChoices
	if success {
		result.outcome = ledger.OutcomeSuccess
		result.failureClass = ""
	}
	body, err := readBoundedResponse(response.Body, h.core.maxResponseBytes)
	if err != nil {
		h.core.setAttemptCount(w.Header(), attempt)
		writeOpenAIError(
			w,
			http.StatusBadGateway,
			"upstream_response_error",
			"upstream response was invalid or exceeded the configured limit",
		)
		result.gatewayStatus = http.StatusBadGateway
		result.outcome = ledger.OutcomeUpstreamError
		result.failureClass = "upstream_response_limit"
		return result
	}
	if success {
		if err := validateJSONResponse(body); err != nil {
			h.core.setAttemptCount(w.Header(), attempt)
			writeOpenAIError(
				w,
				http.StatusBadGateway,
				"upstream_response_error",
				"upstream returned an invalid Conversations response",
			)
			result.gatewayStatus = http.StatusBadGateway
			result.outcome = ledger.OutcomeUpstreamError
			result.failureClass = "upstream_invalid_response"
			return result
		}
		if route.createResource {
			conversationID, stateErr := conversationIDFromPayload(body)
			if stateErr != nil {
				h.core.setAttemptCount(w.Header(), attempt)
				writeOpenAIError(
					w,
					http.StatusBadGateway,
					"upstream_response_error",
					"upstream returned a conversation without a valid ID",
				)
				result.gatewayStatus = http.StatusBadGateway
				result.outcome = ledger.OutcomeUpstreamError
				result.failureClass = "conversation_state_invalid"
				return result
			}
			stateErr = resourceStore.BindResource(
				request.Context(),
				responsesstate.ResourceAffinity{
					ResourceKey: responsesstate.ResourceKey{
						Scope:      ownerScope,
						Kind:       responsesstate.ResourceConversation,
						ResourceID: conversationID,
					},
					VirtualModel:  selection.VirtualModel,
					Provider:      selection.Provider.Name,
					Deployment:    selection.Deployment.Name,
					UpstreamModel: selection.Deployment.Model,
					BoundAt:       time.Now().UTC(),
				},
			)
			if stateErr != nil {
				status := http.StatusServiceUnavailable
				code := "state_affinity_unavailable"
				message := "provider-owned conversation state could not be persisted"
				result.failureClass = "conversation_state_bind_failed"
				if errors.Is(stateErr, responsesstate.ErrConflict) {
					status = http.StatusBadGateway
					code = "upstream_response_error"
					message = "upstream returned a conversation ID with conflicting affinity"
					result.failureClass = "conversation_state_conflict"
				}
				h.core.setAttemptCount(w.Header(), attempt)
				writeOpenAIError(w, status, code, message)
				result.gatewayStatus = status
				result.outcome = ledger.OutcomeUpstreamError
				return result
			}
		}
		itemRoute := route.upstreamSuffix == "/items" ||
			(route.itemID != "" && route.method == http.MethodGet)
		if itemRoute {
			itemIDs, stateErr := conversationProviderItemIDs(
				body,
				route.itemID != "",
			)
			invalidItemState := stateErr != nil
			if stateErr == nil && route.itemID != "" {
				foundRouteItem := false
				for _, itemID := range itemIDs {
					if itemID == route.itemID {
						foundRouteItem = true
						break
					}
				}
				if !foundRouteItem {
					stateErr = fmt.Errorf(
						"upstream returned a different conversation item",
					)
					invalidItemState = true
				}
			}
			if stateErr == nil {
				stateErr = bindProviderItemAffinities(
					request.Context(),
					resourceStore,
					ownerScope,
					selection,
					itemIDs,
					time.Time{},
				)
			}
			if stateErr != nil {
				status := http.StatusServiceUnavailable
				code := "state_affinity_unavailable"
				message := "provider-owned item routing state could not be persisted"
				result.failureClass = "conversation_item_state_bind_failed"
				if errors.Is(stateErr, responsesstate.ErrConflict) {
					status = http.StatusBadGateway
					code = "upstream_response_error"
					message = "upstream returned an item ID with conflicting affinity"
					result.failureClass = "conversation_item_state_conflict"
				} else if invalidItemState {
					status = http.StatusBadGateway
					code = "upstream_response_error"
					message = "upstream returned invalid conversation item state"
					result.failureClass = "conversation_item_state_invalid"
				}
				h.core.setAttemptCount(w.Header(), attempt)
				writeOpenAIError(w, status, code, message)
				result.gatewayStatus = status
				result.outcome = ledger.OutcomeUpstreamError
				return result
			}
		}
		if route.itemID != "" && route.method == http.MethodDelete {
			stateErr := bindProviderItemAffinities(
				request.Context(),
				resourceStore,
				ownerScope,
				selection,
				[]string{route.itemID},
				time.Time{},
			)
			if stateErr == nil {
				deletedAt := time.Now().UTC()
				stateErr = resourceStore.TombstoneResource(
					request.Context(),
					responsesstate.ResourceKey{
						Scope:      ownerScope,
						Kind:       responsesstate.ResourceItem,
						ResourceID: route.itemID,
					},
					deletedAt,
					deletedAt.Add(responsesstate.DefaultTombstoneTTL),
				)
			}
			if stateErr != nil {
				h.core.setAttemptCount(w.Header(), attempt)
				writeOpenAIError(
					w,
					http.StatusServiceUnavailable,
					"state_affinity_unavailable",
					"deleted conversation item routing state could not be retained",
				)
				result.gatewayStatus = http.StatusServiceUnavailable
				result.outcome = ledger.OutcomeUpstreamError
				result.failureClass = "conversation_item_state_tombstone_failed"
				return result
			}
		}
		if route.deleteResource {
			deletedAt := time.Now().UTC()
			stateErr := resourceStore.TombstoneResource(
				request.Context(),
				responsesstate.ResourceKey{
					Scope:      ownerScope,
					Kind:       responsesstate.ResourceConversation,
					ResourceID: route.conversationID,
				},
				deletedAt,
				deletedAt.Add(responsesstate.DefaultTombstoneTTL),
			)
			if stateErr != nil {
				h.core.setAttemptCount(w.Header(), attempt)
				writeOpenAIError(
					w,
					http.StatusServiceUnavailable,
					"state_affinity_unavailable",
					"deleted conversation routing state could not be retained",
				)
				result.gatewayStatus = http.StatusServiceUnavailable
				result.outcome = ledger.OutcomeUpstreamError
				result.failureClass = "conversation_state_tombstone_failed"
				return result
			}
		}
	}
	copyResponseHeaders(w.Header(), response.Header)
	w.Header().Set(h.core.requestIDHeader, requestID)
	h.core.setAttemptCount(w.Header(), attempt)
	w.Header().Del("Content-Length")
	w.Header().Set("Content-Length", strconv.Itoa(len(body)))
	w.WriteHeader(response.StatusCode)
	if _, err := w.Write(body); err != nil {
		result.outcome, result.failureClass = streamFailure(request.Context().Err(), false)
	}
	return result
}

func conversationIDFromPayload(payload []byte) (string, error) {
	var envelope map[string]json.RawMessage
	if err := json.Unmarshal(payload, &envelope); err != nil || envelope == nil {
		return "", fmt.Errorf("conversation response must be an object")
	}
	var conversationID string
	if err := json.Unmarshal(envelope["id"], &conversationID); err != nil {
		return "", fmt.Errorf("conversation id must be a string")
	}
	if err := responsesstate.ValidateResourceID(conversationID); err != nil {
		return "", err
	}
	return conversationID, nil
}

func (h *conversationsHandler) recordConversationFailure(
	w http.ResponseWriter,
	callerContext context.Context,
	attemptContext context.Context,
	observation *requestObservation,
	telemetryAttempt *telemetry.Attempt,
	lease *routing.TargetLease,
	requestID string,
	attempt int,
	attemptStarted time.Time,
	selection routing.Selection,
	failure error,
) {
	outcome := ledger.OutcomeTransportError
	failureClass := "upstream_transport_error"
	targetResult := routing.TargetFailure
	status := http.StatusBadGateway
	code := "upstream_transport_error"
	message := "the provider-owned conversation request failed"
	if callerContext.Err() != nil {
		outcome = ledger.OutcomeClientCancelled
		failureClass = "client_cancelled"
		targetResult = routing.TargetNeutral
		status = 0
	} else if errors.Is(attemptContext.Err(), context.DeadlineExceeded) {
		failureClass = "upstream_timeout"
		status = http.StatusGatewayTimeout
		code = "upstream_timeout"
		message = "the upstream request exceeded its deadline"
	} else if failure != nil {
		var urlError *url.Error
		if !errors.As(failure, &urlError) {
			outcome = ledger.OutcomeConfigError
			failureClass = "upstream_configuration_error"
			targetResult = routing.TargetNeutral
			status = http.StatusInternalServerError
			code = "upstream_configuration_error"
			message = "could not prepare the provider-owned conversation request"
		}
	}
	recordAttempt(
		h.core.ledger,
		telemetryAttempt,
		h.core.targets,
		lease,
		targetResult,
		requestID,
		attempt,
		attemptStarted,
		nil,
		selection,
		0,
		outcome,
		failureClass,
		false,
		"",
		missingUsage(),
	)
	observation.record.AttemptCount = attempt
	h.core.setAttemptCount(w.Header(), attempt)
	if status == 0 {
		observation.fail(0, outcome, failureClass)
		return
	}
	observation.fail(status, outcome, code)
	writeOpenAIError(w, status, code, message)
}
