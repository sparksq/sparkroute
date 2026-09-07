package gateway

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
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

type responsesResourceHandler struct {
	core *chatCompletionsHandler
}

type responsesResourceRoute struct {
	operation            string
	method               string
	allowed              string
	suffix               string
	requiredCapabilities []config.Capability
	streaming            bool
}

func (h *responsesResourceHandler) ServeHTTP(
	w http.ResponseWriter,
	request *http.Request,
) {
	callerContext := request.Context()
	callerIdentity, _ := identity.FromContext(callerContext)
	route, responseID, routeStatus, routeCode, routeMessage := matchResponsesResourceRoute(
		request,
	)
	operation := route.operation
	if operation == "" {
		operation = "responses_resource"
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
	ownerScope := responsesCallerScope(callerIdentity)
	affinity, found, durableAffinity, err := resolveResponsesAffinity(
		request.Context(),
		h.core.responsesState,
		ownerScope,
		responseID,
	)
	if err != nil {
		fail(
			http.StatusServiceUnavailable,
			ledger.OutcomeConfigError,
			"state_affinity_unavailable",
			"provider-owned response state could not be resolved",
		)
		return
	}
	if !found {
		fail(
			http.StatusNotFound,
			ledger.OutcomeRejected,
			"state_affinity_not_found",
			"response is unknown or expired at this gateway",
		)
		return
	}

	plan, err := h.core.snapshot.BuildPlanFor(
		affinity.VirtualModel,
		routing.PlanOptions{
			RequiredCapabilities: route.requiredCapabilities,
			ProtocolResolver:     nativeProtocolResolver(config.ProtocolOpenAI),
			PinnedDeployment:     affinity.Deployment,
			PinnedProvider:       affinity.Provider,
			SingleAttempt:        true,
			Eligibility:          h.core.targets,
			Picker:               h.core.picker,
		},
	)
	if err != nil {
		if errors.Is(err, routing.ErrModelNotFound) {
			fail(
				http.StatusServiceUnavailable,
				ledger.OutcomeConfigError,
				"state_affinity_model_unavailable",
				"the response's virtual model is no longer configured",
			)
			return
		}
		if errors.Is(err, routing.ErrUnsupportedCapabilities) {
			fail(
				http.StatusServiceUnavailable,
				ledger.OutcomeConfigError,
				"state_affinity_capability_unavailable",
				"the response's target no longer supports this resource operation",
			)
			return
		}
		fail(
			http.StatusServiceUnavailable,
			ledger.OutcomeExhausted,
			"state_affinity_target_unavailable",
			"the provider-owned response state target is unavailable",
		)
		return
	}
	if plan.Candidates[0].Deployment.Model != affinity.UpstreamModel {
		fail(
			http.StatusServiceUnavailable,
			ledger.OutcomeConfigError,
			"state_affinity_target_changed",
			"the provider-owned response state target changed upstream model",
		)
		return
	}
	observation.resolve(plan, route.streaming)
	selection := plan.Candidates[0]
	lease, _ := h.core.targets.Acquire(selection.Deployment.Name)
	if lease == nil {
		fail(
			http.StatusServiceUnavailable,
			ledger.OutcomeExhausted,
			"state_affinity_target_unavailable",
			"the provider-owned response state target has no capacity available",
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
	overallContext, cancelOverall := context.WithTimeout(
		request.Context(),
		plan.Limits.OverallTimeout,
	)
	defer cancelOverall()
	attemptTimeout := plan.Limits.PerTryTimeout
	if attemptTimeout > plan.Limits.OverallTimeout {
		attemptTimeout = plan.Limits.OverallTimeout
	}
	attemptExecutionContext, cancelAttempt := context.WithTimeout(
		attemptContext,
		attemptTimeout,
	)
	defer cancelAttempt()
	request = request.WithContext(overallContext)
	upstreamRequest, err := h.buildUpstreamRequest(
		request.WithContext(attemptExecutionContext),
		requestID,
		selection,
		route,
		responseID,
	)
	if err != nil {
		outcome := ledger.OutcomeConfigError
		failureClass := "upstream_configuration_error"
		targetResult := routing.TargetNeutral
		status := http.StatusInternalServerError
		code := "upstream_configuration_error"
		message := "could not prepare the provider-owned response request"
		if callerContext.Err() != nil {
			outcome = ledger.OutcomeClientCancelled
			failureClass = "client_cancelled"
			status = 0
		} else if errors.Is(attemptExecutionContext.Err(), context.DeadlineExceeded) {
			outcome = ledger.OutcomeTransportError
			failureClass = "per_try_timeout"
			targetResult = routing.TargetFailure
			status = http.StatusGatewayTimeout
			code = "upstream_timeout"
			message = "the upstream request exceeded its deadline"
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
		fail(status, outcome, code, message)
		return
	}
	response, err := h.core.client.Do(upstreamRequest)
	if err != nil {
		outcome := ledger.OutcomeTransportError
		failureClass := "upstream_transport_error"
		targetResult := routing.TargetFailure
		status := http.StatusBadGateway
		code := "upstream_transport_error"
		message := "the provider-owned response request failed"
		if callerContext.Err() != nil {
			outcome = ledger.OutcomeClientCancelled
			failureClass = "client_cancelled"
			targetResult = routing.TargetNeutral
			status = 0
		} else if errors.Is(attemptExecutionContext.Err(), context.DeadlineExceeded) ||
			errors.Is(overallContext.Err(), context.DeadlineExceeded) {
			failureClass = "upstream_timeout"
			status = http.StatusGatewayTimeout
			code = "upstream_timeout"
			message = "the upstream request exceeded its deadline"
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
		fail(status, outcome, code, message)
		return
	}

	firstByte := time.Now()
	observation.selectFinal(selection, attempt, firstByte)
	if durableAffinity &&
		route.method == http.MethodDelete &&
		response.StatusCode >= http.StatusOK &&
		response.StatusCode < http.StatusMultipleChoices {
		deletedAt := time.Now().UTC()
		resourceStore := h.core.responsesState.(responsesstate.ResourceStore)
		stateErr := resourceStore.TombstoneResource(
			request.Context(),
			responsesstate.ResourceKey{
				Scope:      ownerScope,
				Kind:       responsesstate.ResourceResponse,
				ResourceID: responseID,
			},
			deletedAt,
			deletedAt.Add(responsesstate.DefaultTombstoneTTL),
		)
		if stateErr != nil {
			_ = response.Body.Close()
			h.core.setAttemptCount(w.Header(), attempt)
			writeOpenAIError(
				w,
				http.StatusServiceUnavailable,
				"state_affinity_unavailable",
				"deleted response routing state could not be retained",
			)
			result := proxyResult{
				gatewayStatus: http.StatusServiceUnavailable,
				outcome:       ledger.OutcomeUpstreamError,
				failureClass:  "responses_state_tombstone_failed",
				usage:         missingUsage(),
			}
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
			return
		}
	}
	result := h.core.proxyResponse(
		w,
		request,
		response,
		requestID,
		attempt,
		selection,
		route.streaming,
		plan.Limits.StreamIdleTimeout,
		responsesRequestState{
			ownerScope:    ownerScope,
			itemExpiresAt: affinity.ExpiresAt,
			durableItems:  durableAffinity,
		},
		config.GuardrailPolicy{},
		nil,
		streamPIIContext{},
	)
	// Resource reads can include the original generation's usage. They are not
	// new token consumption and must not be counted a second time.
	result.usage = missingUsage()
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

func matchResponsesResourceRoute(
	request *http.Request,
) (responsesResourceRoute, string, int, string, string) {
	const prefix = "/v1/responses/"
	if !strings.HasPrefix(request.URL.Path, prefix) {
		return responsesResourceRoute{}, "", http.StatusNotFound, "route_not_found", "route not found"
	}
	remainder := strings.TrimPrefix(request.URL.Path, prefix)
	parts := strings.Split(remainder, "/")
	if len(parts) < 1 || len(parts) > 2 || parts[0] == "" {
		return responsesResourceRoute{}, "", http.StatusNotFound, "route_not_found", "route not found"
	}
	responseID := parts[0]
	if len(parts) == 1 &&
		(responseID == "compact" || responseID == "input_tokens") {
		return responsesResourceRoute{}, "", http.StatusNotFound, "route_not_found", "route not found"
	}
	if responseID == "." || responseID == ".." || strings.Contains(responseID, "\\") {
		return responsesResourceRoute{}, "", http.StatusBadRequest, "invalid_response_id", "response ID is invalid"
	}
	if err := responsesstate.ValidateResponseID(responseID); err != nil {
		return responsesResourceRoute{}, "", http.StatusBadRequest, "invalid_response_id", err.Error()
	}
	query, err := url.ParseQuery(request.URL.RawQuery)
	if err != nil {
		return responsesResourceRoute{}, "", http.StatusBadRequest, "invalid_query", "query parameters are invalid"
	}
	commonCapabilities := []config.Capability{
		config.CapabilityResponses,
		config.CapabilityStoredCompletion,
	}
	if len(parts) == 1 {
		switch request.Method {
		case http.MethodGet:
			streaming := false
			if values, exists := query["stream"]; exists {
				if len(values) != 1 || values[0] != "true" && values[0] != "false" {
					return responsesResourceRoute{
						operation: "responses_retrieve",
						allowed:   http.MethodGet + ", " + http.MethodDelete,
					}, "", http.StatusBadRequest, "invalid_query", "stream must be true or false"
				}
				streaming = values[0] == "true"
			}
			requiredCapabilities := append(
				[]config.Capability(nil),
				commonCapabilities...,
			)
			if streaming {
				requiredCapabilities = append(
					requiredCapabilities,
					config.CapabilityBackgroundResponses,
				)
			}
			return responsesResourceRoute{
				operation:            "responses_retrieve",
				method:               http.MethodGet,
				allowed:              http.MethodGet + ", " + http.MethodDelete,
				requiredCapabilities: requiredCapabilities,
				streaming:            streaming,
			}, responseID, 0, "", ""
		case http.MethodDelete:
			return responsesResourceRoute{
				operation:            "responses_delete",
				method:               http.MethodDelete,
				allowed:              http.MethodGet + ", " + http.MethodDelete,
				requiredCapabilities: commonCapabilities,
			}, responseID, 0, "", ""
		default:
			return responsesResourceRoute{
				allowed: http.MethodGet + ", " + http.MethodDelete,
			}, "", http.StatusMethodNotAllowed, "method_not_allowed", "method not allowed"
		}
	}
	switch parts[1] {
	case "cancel":
		route := responsesResourceRoute{
			operation: "responses_cancel",
			method:    http.MethodPost,
			allowed:   http.MethodPost,
			suffix:    "/cancel",
			requiredCapabilities: []config.Capability{
				config.CapabilityResponses,
				config.CapabilityStoredCompletion,
				config.CapabilityBackgroundResponses,
			},
		}
		if request.Method != route.method {
			return route, "", http.StatusMethodNotAllowed, "method_not_allowed", "method not allowed"
		}
		return route, responseID, 0, "", ""
	case "input_items":
		route := responsesResourceRoute{
			operation:            "responses_input_items",
			method:               http.MethodGet,
			allowed:              http.MethodGet,
			suffix:               "/input_items",
			requiredCapabilities: commonCapabilities,
		}
		if request.Method != route.method {
			return route, "", http.StatusMethodNotAllowed, "method_not_allowed", "method not allowed"
		}
		return route, responseID, 0, "", ""
	default:
		return responsesResourceRoute{}, "", http.StatusNotFound, "route_not_found", "route not found"
	}
}

func (h *responsesResourceHandler) buildUpstreamRequest(
	downstream *http.Request,
	requestID string,
	selection routing.Selection,
	route responsesResourceRoute,
	responseID string,
) (*http.Request, error) {
	if selectionUpstreamProtocol(selection) != config.ProtocolOpenAI ||
		!selection.Deployment.SupportsNativeProtocol(
			selection.Provider,
			config.ProtocolOpenAI,
		) {
		return nil, fmt.Errorf(
			"deployment %q does not natively support OpenAI Responses resources",
			selection.Deployment.Name,
		)
	}
	endpoint, err := responsesResourceURL(
		selection.Provider.BaseURL,
		responseID,
		route.suffix,
		downstream.URL.RawQuery,
	)
	if err != nil {
		return nil, err
	}
	upstreamRequest, err := http.NewRequestWithContext(
		downstream.Context(),
		route.method,
		endpoint,
		nil,
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
	if route.streaming {
		upstreamRequest.Header.Set("Accept", "text/event-stream")
	} else {
		upstreamRequest.Header.Set("Accept", "application/json")
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

func responsesResourceURL(
	baseURL string,
	responseID string,
	suffix string,
	rawQuery string,
) (string, error) {
	if err := responsesstate.ValidateResponseID(responseID); err != nil {
		return "", err
	}
	parsed, err := url.Parse(baseURL)
	if err != nil {
		return "", err
	}
	parsed.Path = strings.TrimRight(parsed.Path, "/") +
		"/responses/" + responseID + suffix
	parsed.RawPath = ""
	parsed.RawQuery = rawQuery
	parsed.Fragment = ""
	return parsed.String(), nil
}

func responsesResourceTargetResult(
	status int,
	result proxyResult,
) routing.TargetResult {
	switch result.outcome {
	case ledger.OutcomeSuccess:
		return routing.TargetSuccess
	case ledger.OutcomeClientCancelled:
		return routing.TargetNeutral
	case ledger.OutcomeStreamError:
		return routing.TargetFailure
	}
	if status == http.StatusRequestTimeout ||
		status == http.StatusTooManyRequests ||
		status >= http.StatusInternalServerError ||
		status >= http.StatusOK && status < http.StatusMultipleChoices {
		return routing.TargetFailure
	}
	return routing.TargetNeutral
}
