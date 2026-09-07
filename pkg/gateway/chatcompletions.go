package gateway

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/sparksq/sparkroute/pkg/config"
	"github.com/sparksq/sparkroute/pkg/credentials"
	upstreamheaders "github.com/sparksq/sparkroute/pkg/headers"
	"github.com/sparksq/sparkroute/pkg/identity"
	"github.com/sparksq/sparkroute/pkg/ledger"
	"github.com/sparksq/sparkroute/pkg/lifecycle"
	"github.com/sparksq/sparkroute/pkg/mmprojection"
	"github.com/sparksq/sparkroute/pkg/modelrouter"
	"github.com/sparksq/sparkroute/pkg/privacy"
	"github.com/sparksq/sparkroute/pkg/promptcache"
	"github.com/sparksq/sparkroute/pkg/responsesstate"
	"github.com/sparksq/sparkroute/pkg/routing"
	"github.com/sparksq/sparkroute/pkg/savedtrace"
	"github.com/sparksq/sparkroute/pkg/telemetry"
	"github.com/sparksq/sparkroute/pkg/version"
)

const (
	defaultMaxRequestBytes  = 32 << 20
	defaultMaxResponseBytes = 64 << 20
	maxRequestedModelBytes  = 256
)

var hopByHopHeaders = map[string]struct{}{
	"connection":          {},
	"keep-alive":          {},
	"proxy-authenticate":  {},
	"proxy-authorization": {},
	"proxy-connection":    {},
	"te":                  {},
	"trailer":             {},
	"transfer-encoding":   {},
	"upgrade":             {},
}

var gatewayOwnedResponseHeaders = map[string]struct{}{
	"x-mm-projection":          {},
	"x-mm-projection-provider": {},
}

type chatCompletionsHandler struct {
	operation           openAIOperation
	snapshot            *routing.Snapshot
	credentials         credentials.Source
	client              *http.Client
	maxRequestBytes     int64
	maxResponseBytes    int64
	requestIDHeader     string
	attemptHeader       string
	picker              routing.WeightedPicker
	ledger              ledger.Recorder
	configRevision      string
	telemetry           *telemetry.Instrumentation
	targets             *routing.TargetManager
	lifecycle           *lifecycle.AdmissionCoordinator
	retryBudget         routing.RetryBudget
	retryDelayPicker    routing.WeightedPicker
	retrySleeper        RetrySleeper
	responsesState      responsesstate.Store
	savedTraces         savedtrace.Recorder
	maxSavedTraceBytes  int64
	promptCache         *promptcache.Directory
	promptFingerprinter *promptcache.Fingerprinter
	modelRouter         modelrouter.Router
	mmProjection        *mmprojection.Client
	privacy             privacy.Provider
}

func newChatCompletionsHandler(
	snapshot *routing.Snapshot,
	targets *routing.TargetManager,
	retryBudget routing.RetryBudget,
	options DataOptions,
	instrumentation *telemetry.Instrumentation,
) http.Handler {
	return newOpenAIHandler(
		openAIOperationChatCompletions,
		snapshot,
		targets,
		retryBudget,
		options,
		instrumentation,
	)
}

func newOpenAIHandler(
	operation openAIOperation,
	snapshot *routing.Snapshot,
	targets *routing.TargetManager,
	retryBudget routing.RetryBudget,
	options DataOptions,
	instrumentation *telemetry.Instrumentation,
) *chatCompletionsHandler {
	client := options.HTTPClient
	if client == nil {
		client = defaultUpstreamHTTPClient()
	}
	clientCopy := *client
	clientCopy.CheckRedirect = func(_ *http.Request, _ []*http.Request) error {
		return http.ErrUseLastResponse
	}
	maxRequestBytes := options.MaxRequestBytes
	if maxRequestBytes <= 0 {
		maxRequestBytes = defaultMaxRequestBytes
	}
	maxResponseBytes := options.MaxResponseBytes
	if maxResponseBytes <= 0 {
		maxResponseBytes = defaultMaxResponseBytes
	}
	requestIDHeader := options.RequestIDHeader
	if requestIDHeader == "" {
		requestIDHeader = "X-Request-Id"
	}
	attemptHeader := options.AttemptCountHeader
	if attemptHeader == "" {
		attemptHeader = "X-SparkRoute-Attempt-Count"
	}
	recorder := options.Ledger
	if recorder == nil {
		recorder = ledger.DiscardRecorder{}
	}
	retrySleeper := options.RetrySleeper
	if retrySleeper == nil {
		retrySleeper = timerRetrySleeper{}
	}
	return &chatCompletionsHandler{
		operation:           operation,
		snapshot:            snapshot,
		credentials:         options.Credentials,
		client:              &clientCopy,
		maxRequestBytes:     maxRequestBytes,
		maxResponseBytes:    maxResponseBytes,
		requestIDHeader:     requestIDHeader,
		attemptHeader:       attemptHeader,
		picker:              options.RoutingPicker,
		ledger:              recorder,
		configRevision:      options.ConfigRevision,
		telemetry:           instrumentation,
		targets:             targets,
		lifecycle:           options.Lifecycle,
		retryBudget:         retryBudget,
		retryDelayPicker:    options.RetryDelayPicker,
		retrySleeper:        retrySleeper,
		responsesState:      options.ResponsesState,
		savedTraces:         options.SavedTraces,
		maxSavedTraceBytes:  effectiveMaxSavedTraceBytes(options.MaxSavedTraceBytes),
		promptCache:         options.PromptCache,
		promptFingerprinter: options.PromptFingerprinter,
		modelRouter:         options.ModelRouter,
		mmProjection:        options.MMProjection,
		privacy:             options.Privacy,
	}
}

func (h *chatCompletionsHandler) ServeHTTP(w http.ResponseWriter, request *http.Request) {
	callerContext := request.Context()
	requestStartedAt := modelCatalogRequestStartedAt(callerContext)
	callerIdentity, _ := identity.FromContext(callerContext)
	internalInvocation := isGuardrailInvocation(request.Context()) ||
		isMMProjectionAnalyzerInvocation(request.Context())
	requestID, err := newRequestID()
	if err != nil {
		h.operation.writeError(
			w,
			http.StatusInternalServerError,
			"request_id_error",
			"could not create request ID",
			"",
		)
		return
	}
	requestContext, telemetryRequest := h.telemetry.StartRequest(
		request.Context(),
		request.Header,
		telemetry.RequestStart{
			RequestID:      requestID,
			Protocol:       h.operation.protocol(),
			Operation:      h.operation.String(),
			ConfigRevision: h.configRevision,
			Identity:       callerIdentity,
			StartedAt:      requestStartedAt,
		},
	)
	request = request.WithContext(requestContext)
	traceRecorder := h.savedTraces
	if internalInvocation {
		traceRecorder = nil
	}
	traceWriter := newSavedTraceResponseWriter(
		w,
		traceRecorder,
		h.maxSavedTraceBytes,
	)
	if traceWriter != nil {
		w = traceWriter
	}
	observation := newRequestObservation(
		h.ledger,
		requestID,
		h.configRevision,
		h.operation.String(),
		callerIdentity,
		telemetryRequest,
	)
	if !requestStartedAt.IsZero() {
		observation.record.StartedAt = requestStartedAt
	}
	observation.record.Protocol = h.operation.protocol()
	observation.enableSavedTrace(
		traceRecorder,
		traceWriter,
		h.maxSavedTraceBytes,
		request.Header.Get("Content-Type"),
	)
	defer observation.finish()
	fail := func(status int, outcome ledger.Outcome, code, message string) {
		observation.fail(status, outcome, code)
		h.operation.writeError(w, status, code, message, requestID)
	}

	h.operation.setRequestIDHeaders(
		w.Header(),
		h.requestIDHeader,
		requestID,
	)
	if request.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		fail(
			http.StatusMethodNotAllowed,
			ledger.OutcomeRejected,
			"method_not_allowed",
			"method not allowed",
		)
		return
	}
	if !isJSONContentType(request.Header.Get("Content-Type")) {
		fail(
			http.StatusUnsupportedMediaType,
			ledger.OutcomeRejected,
			"unsupported_content_type",
			"Content-Type must be application/json",
		)
		return
	}
	if strings.TrimSpace(request.Header.Get(mmprojection.HopHeader)) != "" {
		fail(
			http.StatusLoopDetected,
			ledger.OutcomeRejected,
			"projection_loop_detected",
			"MM projection loop detected",
		)
		return
	}
	protocolHeaderCapabilities, err :=
		h.operation.protocolHeaderCapabilities(request.Header)
	if err != nil {
		fail(
			http.StatusBadRequest,
			ledger.OutcomeRejected,
			requestValidationCode(err),
			err.Error(),
		)
		return
	}

	raw, err := readBoundedBody(w, request, h.maxRequestBytes)
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
			"invalid_request_body",
			"could not read request body",
		)
		return
	}
	observation.captureSavedTraceRequest(raw)
	pathModel := h.operation.pathModel(request.Context())
	envelope, requestedModel, streaming, err := h.operation.decodeRequest(
		raw,
		pathModel,
	)
	if err != nil {
		fail(http.StatusBadRequest, ledger.OutcomeRejected, requestValidationCode(err), err.Error())
		return
	}
	publicRequestedModel := requestedModel
	selectorRequest := false
	stateAffinityKind := ""
	var providerPriority []string
	var projectionPolicy *modelrouter.MMProjectionPolicy
	if h.modelRouter != nil && !internalInvocation {
		routingCapabilities, capabilityErr := h.operation.detectCapabilities(envelope, streaming)
		if capabilityErr != nil {
			fail(
				http.StatusBadRequest,
				ledger.OutcomeRejected,
				requestValidationCode(capabilityErr),
				capabilityErr.Error(),
			)
			return
		}
		for _, capability := range protocolHeaderCapabilities {
			routingCapabilities = addRequiredCapability(routingCapabilities, capability)
		}
		resolvedVirtualModel := ""
		if resolved, resolveErr := h.snapshot.ResolveModel(requestedModel, false); resolveErr == nil {
			resolvedVirtualModel = resolved.Name
		}
		configured := h.snapshot.ModelCandidates(false)
		selectorOwned := resolvedVirtualModel == ""
		if namespace, ok := h.modelRouter.(modelrouter.NamespaceInspector); ok {
			selectorOwned = namespace.OwnsVirtualRequest(publicRequestedModel)
		}
		var providerStatePin *modelrouter.ProviderStatePin
		if selectorOwned {
			var stateFailure *providerStateSelectorFailure
			providerStatePin, stateFailure = resolveProviderStateSelectorPin(
				request.Context(),
				h.responsesState,
				h.operation,
				envelope,
				responsesCallerScope(callerIdentity),
			)
			if stateFailure != nil {
				fail(
					stateFailure.status,
					stateFailure.outcome,
					stateFailure.code,
					stateFailure.message,
				)
				return
			}
		}
		candidates := make([]modelrouter.Candidate, 0, len(configured))
		for _, candidate := range configured {
			capabilities := make([]string, 0, len(candidate.RequiredCapabilities))
			for _, capability := range candidate.RequiredCapabilities {
				capabilities = append(capabilities, string(capability))
			}
			candidates = append(candidates, modelrouter.Candidate{
				Name: candidate.Name, Capabilities: capabilities,
			})
		}
		normalized, normalizeErr := modelrouter.NormalizeRoutingRequest(raw)
		if normalizeErr != nil {
			fail(
				http.StatusBadRequest,
				ledger.OutcomeRejected,
				"model_routing_request_invalid",
				"request could not be normalized for model routing",
			)
			return
		}
		routingInput := modelrouter.Input{
			RequestedModel:       requestedModel,
			ResolvedVirtualModel: resolvedVirtualModel,
			ProviderStatePin:     providerStatePin,
			Candidates:           candidates,
			Features: modelrouter.RequestFeatures{
				IngressProtocol: h.operation.protocol(),
				Operation:       h.operation.String(),
				Streaming:       streaming,
				RoutingText:     normalized.Text,
				StageHistory:    normalized.StageHistory,
			},
			RoutingIdentity: cloneAttribution(callerIdentity.Attribution),
		}
		if selectorOwned && providerStatePin == nil {
			projectable := map[string][]string{}
			if _, supported := h.projectionOperation(); supported {
				if provider, ok := h.modelRouter.(modelrouter.ProjectionCapabilityProvider); ok {
					projectable, err = provider.ProjectionCapabilities(
						request.Context(), routingInput,
					)
					if err != nil {
						fail(
							http.StatusServiceUnavailable,
							ledger.OutcomeRejected,
							"model_routing_failed",
							"model router could not resolve projection capability policy",
						)
						return
					}
				}
			}
			filtered := candidates[:0]
			for _, candidate := range candidates {
				required := routingCapabilities
				if projected := projectable[candidate.Name]; len(projected) != 0 {
					required = withoutCapabilities(required, projected)
				}
				if h.snapshot.ModelSupportsCapabilities(candidate.Name, required, false) {
					filtered = append(filtered, candidate)
				}
			}
			routingInput.Candidates = filtered
		}
		routeStarted := time.Now()
		decision, routeErr := h.modelRouter.Route(request.Context(), routingInput)
		if routeErr != nil {
			status := http.StatusNotFound
			outcome := ledger.OutcomeRejected
			code := "model_routing_failed"
			message := "model router could not select an available virtual model"
			if namespace, ok := h.modelRouter.(modelrouter.NamespaceInspector); ok &&
				namespace.OwnsVirtualRequest(publicRequestedModel) {
				status = http.StatusServiceUnavailable
				code = "model_unavailable"
				message = "model selector has no capability-compatible virtual model"
			}
			if providerStatePin != nil {
				status = http.StatusServiceUnavailable
				outcome = ledger.OutcomeConfigError
				code = "state_affinity_model_unavailable"
				message = "the provider-owned state's virtual model is no longer configured"
			}
			fail(
				status,
				outcome,
				code,
				message,
			)
			return
		}
		if providerStatePin != nil &&
			decision.VirtualModel != providerStatePin.VirtualModel {
			fail(
				http.StatusServiceUnavailable,
				ledger.OutcomeConfigError,
				"state_affinity_model_unavailable",
				"model router did not preserve the provider-owned state's virtual model",
			)
			return
		}
		modelRoutingTelemetry := telemetry.ModelRoutingSelection{
			Router: decision.Router, Strategy: decision.Annotations["strategy"],
			SelectedVirtualModel: decision.VirtualModel,
			DecisionSource:       decision.Annotations["decision_source"],
			Tier:                 decision.Annotations["routing_tier"], Duration: time.Since(routeStarted),
		}
		if decision.Stage != nil {
			modelRoutingTelemetry.Stage = true
			modelRoutingTelemetry.Score = decision.Stage.Score
			modelRoutingTelemetry.Confidence = decision.Stage.Confidence
			modelRoutingTelemetry.Severity = decision.Stage.Dimensions.Severity
			modelRoutingTelemetry.Spinning = decision.Stage.Dimensions.Spinning
			modelRoutingTelemetry.Exploring = decision.Stage.Dimensions.Exploring
			modelRoutingTelemetry.ProductionIntensity = decision.Stage.Dimensions.ProductionIntensity
		}
		observation.telemetry.SetModelRouting(modelRoutingTelemetry)
		requestedModel = decision.VirtualModel
		providerPriority = append([]string(nil), decision.ProviderPriority...)
		if decision.MMProjection != nil {
			clone := *decision.MMProjection
			projectionPolicy = &clone
		}
		selectorRequest = decision.Annotations["selector"] != "" ||
			providerStatePin != nil
		stateAffinityKind = decision.Annotations["state_affinity_kind"]
		if providerStatePin != nil {
			stateAffinityKind = providerStatePin.Kind
		}
		if selectorRequest {
			if observer, ok := h.modelRouter.(modelrouter.RouteObserver); ok {
				selectedModel := requestedModel
				selectedAt := time.Now()
				defer func() {
					observer.ObserveRoute(
						selectedModel,
						time.Since(selectedAt),
						observation.record.HTTPStatus,
					)
				}()
			}
		}
	}
	observation.record.RequestedModel = publicRequestedModel
	observation.record.Stream = streaming
	model, err := h.snapshot.ResolveModel(requestedModel, internalInvocation)
	if err != nil {
		fail(
			http.StatusNotFound,
			ledger.OutcomeRejected,
			"model_not_found",
			"requested model was not found",
		)
		return
	}
	guardrails := model.Guardrails
	piiPolicy := config.EffectivePIIPolicy{Mode: config.PIIModeDisabled}
	if model.Privacy != nil {
		piiPolicy = model.Privacy.PII.Effective()
	}
	if internalInvocation {
		guardrails = config.GuardrailPolicy{}
		piiPolicy = config.EffectivePIIPolicy{Mode: config.PIIModeDisabled}
	}
	overallContext, cancelOverall := context.WithTimeout(
		request.Context(),
		model.Limits.Effective().OverallTimeout,
	)
	defer cancelOverall()
	request = request.WithContext(overallContext)
	overallDeadline, _ := overallContext.Deadline()
	var piiSession privacy.Session
	piiMediaInspectionRequired := false
	if piiPolicy.Mode != config.PIIModeDisabled {
		if piiPolicy.TraceContent == config.PIITraceDisabled {
			observation.disableSavedTrace()
		}
		piiSession, err = newPIISession(
			overallContext, h.privacy, piiPolicy, callerIdentity,
		)
		if err == nil && streaming && !piiSession.SupportsStreaming() {
			err = errors.New("configured PII detector does not support bounded streaming")
		}
		if err == nil {
			if piiPolicy.MediaText == config.PIIMediaTextRequired {
				piiMediaInspectionRequired, err = h.operation.piiMediaInput(envelope, streaming)
			}
		}
		if err == nil {
			raw, envelope, err = h.operation.substituteAndValidatePIIRequest(
				overallContext,
				raw,
				pathModel,
				streaming,
				piiSession,
			)
		}
		if err != nil {
			recordPrivacyFailure(h.privacy)
			if piiPolicy.TraceContent == config.PIITraceMasked {
				observation.disableSavedTrace()
			}
			observation.setSavedTraceMetadata("sparkroute.pii.state", "failed")
			if piiPolicy.FailureMode == config.PIIFailClosed {
				fail(
					http.StatusBadGateway,
					ledger.OutcomeUpstreamError,
					"pii_transform_failed",
					"PII substitution could not safely transform the request",
				)
				return
			}
			piiSession = nil
		} else {
			recordPrivacyInspection(h.privacy)
			observation.setSavedTraceMetadata("sparkroute.pii.state", "substituted")
			observation.setSavedTraceMetadata(
				"sparkroute.pii.mapping_count",
				strconv.Itoa(piiSession.MappingCount()),
			)
			if piiPolicy.TraceContent == config.PIITraceMasked {
				observation.captureSavedTraceRequest(raw)
			}
		}
	}
	projectionOperation, projectionSupported := h.projectionOperation()
	if piiMediaInspectionRequired && (projectionPolicy == nil || !projectionSupported) {
		recordPrivacyFailure(h.privacy)
		if piiPolicy.FailureMode == config.PIIFailClosed {
			fail(
				http.StatusBadGateway,
				ledger.OutcomeUpstreamError,
				"pii_media_inspection_required",
				"required multimedia text inspection is unavailable",
			)
			return
		}
		if piiPolicy.TraceContent == config.PIITraceMasked {
			observation.disableSavedTrace()
		}
		piiSession = nil
		piiMediaInspectionRequired = false
	}
	if projectionSupported && projectionPolicy != nil && !internalInvocation {
		projectionInput, projectionInputErr := rewriteModel(envelope, requestedModel)
		var projected []byte
		var projectionTrace *modelrouter.MMProjectionTrace
		var projectionErr error
		if projectionInputErr != nil {
			projectionErr = projectionInputErr
			projectionTrace = failedProjectionTrace(
				projectionPolicy,
				"could not prepare MM projection request",
			)
		} else if h.mmProjection == nil {
			projectionErr = errors.New("MM projection service is not configured")
			projectionTrace = failedProjectionTrace(
				projectionPolicy,
				projectionErr.Error(),
			)
		} else {
			projected, projectionTrace, projectionErr = h.mmProjection.Project(
				overallContext,
				projectionInput,
				*projectionPolicy,
				projectionOperation,
				requestID,
			)
		}
		if projectionErr == nil && int64(len(projected)) > h.maxRequestBytes {
			projectionErr = errors.New("MM projection transformed request exceeds the gateway request size limit")
			projectionTrace.State = "failed"
			projectionTrace.Error = projectionErr.Error()
		}
		if projectionErr == nil && piiMediaInspectionRequired &&
			(projectionTrace.State != "applied" ||
				projectionTrace.TextInspectionState != "complete" ||
				projectionTrace.MediaCount < 1 ||
				projectionTrace.InspectedMedia != projectionTrace.MediaCount) {
			projectionErr = errors.New("MM projection did not attest complete multimedia text inspection")
			projectionTrace.State = "failed"
			projectionTrace.Error = projectionErr.Error()
		}
		if projectionErr == nil {
			projected, projectionErr = validatePreGuardrailReplacement(
				h.operation,
				projectionInput,
				projected,
				requestedModel,
				streaming,
			)
			if projectionErr != nil {
				projectionErr = errors.New("MM projection returned an invalid transformed request")
				projectionTrace.State = "failed"
				projectionTrace.Error = projectionErr.Error()
			}
		}
		clientError := false
		projectionStatus := http.StatusServiceUnavailable
		var typedProjectionError *mmprojection.Error
		if errors.As(projectionErr, &typedProjectionError) &&
			typedProjectionError.ClientRequestError() {
			clientError = true
			projectionStatus = typedProjectionError.Status
		}
		if projectionErr != nil {
			if piiMediaInspectionRequired {
				recordPrivacyFailure(h.privacy)
			}
			if clientError || projectionFailureMode(projectionPolicy) == "fail_closed" ||
				(piiMediaInspectionRequired && piiPolicy.FailureMode == config.PIIFailClosed) {
				applyMMProjectionObservation(w.Header(), observation, projectionTrace)
				outcome := ledger.OutcomeUpstreamError
				if clientError {
					outcome = ledger.OutcomeRejected
				}
				fail(
					projectionStatus,
					outcome,
					"mm_projection_failed",
					projectionErr.Error(),
				)
				return
			}
			if piiMediaInspectionRequired {
				if piiPolicy.TraceContent == config.PIITraceMasked {
					observation.disableSavedTrace()
				}
				piiSession = nil
			}
			projectionTrace.State = "fallback"
		} else {
			raw = projected
			envelope, requestedModel, streaming, err = h.operation.decodeRequest(
				raw,
				pathModel,
			)
			if err != nil {
				projectionTrace.State = "failed"
				projectionTrace.Error = "MM projection returned an invalid transformed request"
				applyMMProjectionObservation(w.Header(), observation, projectionTrace)
				fail(
					http.StatusServiceUnavailable,
					ledger.OutcomeUpstreamError,
					"mm_projection_failed",
					projectionTrace.Error,
				)
				return
			}
		}
		applyMMProjectionObservation(w.Header(), observation, projectionTrace)
	}
	raw, err = h.operation.guardrailRequestBody(
		raw,
		envelope,
		requestedModel,
	)
	if err != nil {
		fail(
			http.StatusInternalServerError,
			ledger.OutcomeConfigError,
			"request_rewrite_error",
			"could not prepare request for guardrail processing",
		)
		return
	}
	if piiSession != nil {
		raw, envelope, err = h.operation.substituteAndValidatePIIRequest(
			overallContext,
			raw,
			pathModel,
			streaming,
			piiSession,
		)
		if err != nil {
			recordPrivacyFailure(h.privacy)
			if piiPolicy.TraceContent == config.PIITraceMasked {
				observation.disableSavedTrace()
			}
			if piiPolicy.FailureMode == config.PIIFailClosed {
				fail(
					http.StatusBadGateway,
					ledger.OutcomeUpstreamError,
					"pii_transform_failed",
					"PII substitution could not safely transform the projected request",
				)
				return
			}
			piiSession = nil
		} else if piiPolicy.TraceContent == config.PIITraceMasked {
			observation.captureSavedTraceRequest(raw)
			observation.setSavedTraceMetadata(
				"sparkroute.pii.mapping_count",
				strconv.Itoa(piiSession.MappingCount()),
			)
		}
	}
	if streaming && len(guardrails.Post) != 0 && guardrails.Stream == nil {
		fail(
			http.StatusBadRequest,
			ledger.OutcomeRejected,
			"unsupported_feature",
			"streaming post-response guardrails require a guardrails.stream policy",
		)
		return
	}

	if len(guardrails.Pre) != 0 {
		guardrailResult := h.applyPreGuardrails(
			request,
			guardrails.Pre,
			raw,
			requestedModel,
			streaming,
		)
		switch {
		case guardrailResult.blocked:
			fail(
				http.StatusBadRequest,
				ledger.OutcomeRejected,
				"guardrail_blocked",
				"request was rejected by a configured guardrail",
			)
			return
		case guardrailResult.failed:
			fail(
				http.StatusBadGateway,
				ledger.OutcomeUpstreamError,
				"guardrail_failed",
				"a required request guardrail could not produce a valid verdict",
			)
			return
		}
		raw = guardrailResult.body
		envelope, requestedModel, streaming, err = h.operation.decodeRequest(
			raw,
			pathModel,
		)
		if err != nil {
			fail(
				http.StatusBadGateway,
				ledger.OutcomeConfigError,
				"guardrail_invalid_replacement",
				"a request guardrail produced an invalid replacement",
			)
			return
		}
		if piiSession != nil {
			raw, envelope, err = h.operation.substituteAndValidatePIIRequest(
				overallContext,
				raw,
				pathModel,
				streaming,
				piiSession,
			)
			if err != nil {
				recordPrivacyFailure(h.privacy)
				if piiPolicy.TraceContent == config.PIITraceMasked {
					observation.disableSavedTrace()
				}
				if piiPolicy.FailureMode == config.PIIFailClosed {
					fail(
						http.StatusBadGateway,
						ledger.OutcomeUpstreamError,
						"pii_transform_failed",
						"PII substitution could not safely transform the guarded request",
					)
					return
				}
				piiSession = nil
			} else if piiPolicy.TraceContent == config.PIITraceMasked {
				observation.captureSavedTraceRequest(raw)
				observation.setSavedTraceMetadata(
					"sparkroute.pii.mapping_count",
					strconv.Itoa(piiSession.MappingCount()),
				)
			}
		}
	}
	requiredCapabilities, err := h.operation.detectCapabilities(envelope, streaming)
	if err != nil {
		fail(http.StatusBadRequest, ledger.OutcomeRejected, requestValidationCode(err), err.Error())
		return
	}
	if h.operation == openAIOperationResponses {
		filtered := make([]config.Capability, 0, len(requiredCapabilities))
		for _, capability := range requiredCapabilities {
			if capability != config.CapabilityResponses {
				filtered = append(filtered, capability)
			}
		}
		requiredCapabilities = filtered
	}
	var bedrockChatBody []byte
	var bedrockChatError error
	var anthropicChatBody []byte
	var anthropicChatError error
	var geminiChatBody []byte
	var geminiChatError error
	var responsesChatBody []byte
	var responsesChatError error
	var geminiEmbeddingBody []byte
	var geminiEmbeddingDimensions int64
	var geminiEmbeddingInputCount int
	var geminiEmbeddingError error
	var crossDialectError error
	hasNativeProtocolTarget := func(protocol config.Protocol) (bool, bool) {
		found, targetErr := h.snapshot.HasTargetNativeProtocol(
			requestedModel,
			protocol,
			internalInvocation,
		)
		if targetErr != nil {
			if errors.Is(targetErr, routing.ErrModelNotFound) {
				return false, true
			}
			fail(
				http.StatusInternalServerError,
				ledger.OutcomeConfigError,
				"routing_error",
				"could not inspect virtual-model targets",
			)
			return false, false
		}
		return found, true
	}
	if h.operation == openAIOperationChatCompletions {
		hasOpenAITarget, ok := hasNativeProtocolTarget(config.ProtocolOpenAI)
		if !ok {
			return
		}
		hasBedrockTarget, ok := hasNativeProtocolTarget(config.ProtocolBedrock)
		if !ok {
			return
		}
		hasAnthropicTarget, ok := hasNativeProtocolTarget(config.ProtocolAnthropic)
		if !ok {
			return
		}
		hasGeminiTarget, ok := hasNativeProtocolTarget(config.ProtocolGemini)
		if !ok {
			return
		}
		crossDialectFailures := make([]string, 0, 3)
		if hasBedrockTarget {
			bedrockChatBody, bedrockChatError =
				translateChatCompletionsRequestToBedrock(envelope)
			if bedrockChatError != nil {
				crossDialectFailures = append(
					crossDialectFailures,
					fmt.Sprintf("Bedrock Converse: %v", bedrockChatError),
				)
			}
		}
		if hasAnthropicTarget {
			anthropicChatBody, anthropicChatError =
				translateChatCompletionsRequestToAnthropic(envelope)
			if anthropicChatError != nil {
				crossDialectFailures = append(
					crossDialectFailures,
					fmt.Sprintf("Anthropic Messages: %v", anthropicChatError),
				)
			}
		}
		if hasGeminiTarget {
			geminiChatBody, geminiChatError =
				translateChatCompletionsRequestToGemini(envelope)
			if geminiChatError != nil {
				crossDialectFailures = append(
					crossDialectFailures,
					fmt.Sprintf("Gemini GenerateContent: %v", geminiChatError),
				)
			}
		}
		switch len(crossDialectFailures) {
		case 1:
			for _, candidate := range []error{
				bedrockChatError,
				anthropicChatError,
				geminiChatError,
			} {
				if candidate != nil {
					crossDialectError = candidate
					break
				}
			}
		case 2, 3:
			crossDialectError = unsupportedFeature(
				"request cannot be represented by a configured cross-dialect target: %s",
				strings.Join(crossDialectFailures, "; "),
			)
		}
		hasCompatibleCrossDialectTarget :=
			hasBedrockTarget && bedrockChatError == nil ||
				hasAnthropicTarget && anthropicChatError == nil ||
				hasGeminiTarget && geminiChatError == nil
		hasCrossDialectTarget := hasBedrockTarget ||
			hasAnthropicTarget ||
			hasGeminiTarget
		if !hasCrossDialectTarget {
			crossDialectError = nil
		}
		if !hasOpenAITarget &&
			hasCrossDialectTarget &&
			!hasCompatibleCrossDialectTarget {
			configuredFailures := make([]string, 0, 3)
			for _, failure := range []struct {
				configured bool
				name       string
				err        error
			}{
				{hasBedrockTarget, "Bedrock Converse", bedrockChatError},
				{hasAnthropicTarget, "Anthropic Messages", anthropicChatError},
				{hasGeminiTarget, "Gemini GenerateContent", geminiChatError},
			} {
				if failure.configured {
					configuredFailures = append(
						configuredFailures,
						fmt.Sprintf(
							"%s: %v",
							failure.name,
							failure.err,
						),
					)
					crossDialectError = failure.err
				}
			}
			if len(configuredFailures) > 1 {
				crossDialectError = unsupportedFeature(
					"request cannot be represented by any configured cross-dialect target: %s",
					strings.Join(configuredFailures, "; "),
				)
			}
			fail(
				http.StatusBadRequest,
				ledger.OutcomeRejected,
				requestValidationCode(crossDialectError),
				crossDialectError.Error(),
			)
			return
		}
	}
	if h.operation == openAIOperationResponses {
		responsesChatBody, responsesChatError =
			translateResponsesRequestToChat(raw)
		crossDialectError = responsesChatError
	}
	if h.operation == openAIOperationEmbeddings {
		hasOpenAITarget, ok := hasNativeProtocolTarget(config.ProtocolOpenAI)
		if !ok {
			return
		}
		hasGeminiTarget, ok := hasNativeProtocolTarget(config.ProtocolGemini)
		if !ok {
			return
		}
		if hasGeminiTarget {
			geminiEmbeddingBody,
				geminiEmbeddingDimensions,
				geminiEmbeddingInputCount,
				geminiEmbeddingError =
				translateOpenAIEmbeddingsRequestToGemini(envelope)
			crossDialectError = geminiEmbeddingError
		}
		if !hasOpenAITarget &&
			hasGeminiTarget &&
			geminiEmbeddingError != nil {
			fail(
				http.StatusBadRequest,
				ledger.OutcomeRejected,
				requestValidationCode(geminiEmbeddingError),
				geminiEmbeddingError.Error(),
			)
			return
		}
	}
	protocolResolver := func(
		provider config.Provider,
		deployment config.Deployment,
	) (routing.ProtocolRoute, bool) {
		ingress := config.Protocol(h.operation.protocol())
		if h.operation == openAIOperationResponses &&
			deployment.SupportsNativeProtocol(provider, config.ProtocolOpenAI) {
			if deployment.DeclaresCapability(config.CapabilityResponses) {
				return routing.ProtocolRoute{
					Protocol: config.ProtocolOpenAI,
					Native:   true,
					RequiredCapabilities: []config.Capability{
						config.CapabilityResponses,
					},
				}, true
			}
			if responsesChatError == nil {
				return routing.ProtocolRoute{Protocol: config.ProtocolOpenAI}, true
			}
			return routing.ProtocolRoute{}, false
		}
		if deployment.SupportsNativeProtocol(provider, ingress) {
			return routing.ProtocolRoute{Protocol: ingress, Native: true}, true
		}
		for _, protocol := range deployment.EffectiveNativeProtocols(provider) {
			switch {
			case h.operation == openAIOperationChatCompletions &&
				protocol == config.ProtocolBedrock && bedrockChatError == nil:
				return routing.ProtocolRoute{Protocol: protocol}, true
			case h.operation == openAIOperationChatCompletions &&
				protocol == config.ProtocolAnthropic && anthropicChatError == nil:
				return routing.ProtocolRoute{Protocol: protocol}, true
			case h.operation == openAIOperationChatCompletions &&
				protocol == config.ProtocolGemini && geminiChatError == nil:
				return routing.ProtocolRoute{Protocol: protocol}, true
			case h.operation == openAIOperationEmbeddings &&
				protocol == config.ProtocolGemini && geminiEmbeddingError == nil:
				return routing.ProtocolRoute{Protocol: protocol}, true
			}
		}
		return routing.ProtocolRoute{}, false
	}
	for _, capability := range protocolHeaderCapabilities {
		requiredCapabilities = addRequiredCapability(
			requiredCapabilities,
			capability,
		)
	}
	fileIDs, err := collectProviderFileIDs(h.operation, envelope)
	if err != nil {
		fail(
			http.StatusBadRequest,
			ledger.OutcomeRejected,
			"invalid_request_body",
			err.Error(),
		)
		return
	}
	if len(fileIDs) != 0 {
		requiredCapabilities = addRequiredCapability(
			requiredCapabilities,
			config.CapabilityFiles,
		)
		requiredCapabilities = addRequiredCapability(
			requiredCapabilities,
			config.CapabilityFileInput,
		)
	}
	var itemIDs []string
	if h.operation.isResponsesOperation() {
		itemIDs, err = collectResponsesItemAffinityIDs(envelope["input"])
		if err != nil {
			fail(
				http.StatusBadRequest,
				ledger.OutcomeRejected,
				"invalid_request_body",
				err.Error(),
			)
			return
		}
		if len(itemIDs) != 0 {
			requiredCapabilities = addRequiredCapability(
				requiredCapabilities,
				config.CapabilityStoredCompletion,
			)
		}
	}
	observation.telemetry.SetRequiredCapabilities(
		capabilityStrings(requiredCapabilities),
	)
	responseState := responsesRequestState{
		ownerScope:          responsesCallerScope(callerIdentity),
		fileIDs:             fileIDs,
		itemIDs:             itemIDs,
		embeddingDimensions: geminiEmbeddingDimensions,
		embeddingInputCount: geminiEmbeddingInputCount,
	}
	var pinnedDeployment string
	var pinnedProvider string
	var pinnedUpstreamModel string
	if h.operation.isResponsesOperation() {
		classified := classifyResponsesState(envelope)
		if h.operation == openAIOperationResponsesCompact {
			classified = classifyResponsesCompactState(envelope)
		}
		classified.ownerScope = responseState.ownerScope
		classified.fileIDs = responseState.fileIDs
		classified.itemIDs = responseState.itemIDs
		responseState = classified
		if responseState.previousResponseID != "" {
			affinity, found, _, resolveErr := resolveResponsesAffinity(
				overallContext,
				h.responsesState,
				responseState.ownerScope,
				responseState.previousResponseID,
			)
			if resolveErr != nil {
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
					http.StatusConflict,
					ledger.OutcomeRejected,
					"state_affinity_not_found",
					"previous_response_id is unknown or expired at this gateway",
				)
				return
			}
			if affinity.VirtualModel != model.Name {
				fail(
					http.StatusConflict,
					ledger.OutcomeRejected,
					"state_affinity_model_mismatch",
					"previous_response_id belongs to a different virtual model",
				)
				return
			}
			pinnedDeployment = affinity.Deployment
			pinnedProvider = affinity.Provider
			pinnedUpstreamModel = affinity.UpstreamModel
		} else if responseState.conversationID != "" {
			resourceStore, supportsResources := h.responsesState.(responsesstate.ResourceStore)
			if !supportsResources {
				fail(
					http.StatusServiceUnavailable,
					ledger.OutcomeConfigError,
					"state_affinity_unavailable",
					"provider-owned conversation state storage is not configured",
				)
				return
			}
			affinity, found, resolveErr := resourceStore.ResolveResource(
				overallContext,
				responsesstate.ResourceKey{
					Scope:      responseState.ownerScope,
					Kind:       responsesstate.ResourceConversation,
					ResourceID: responseState.conversationID,
				},
			)
			if resolveErr != nil {
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
					http.StatusConflict,
					ledger.OutcomeRejected,
					"state_affinity_not_found",
					"conversation is unknown or deleted at this gateway",
				)
				return
			}
			if affinity.VirtualModel != model.Name {
				fail(
					http.StatusConflict,
					ledger.OutcomeRejected,
					"state_affinity_model_mismatch",
					"conversation belongs to a different virtual model",
				)
				return
			}
			pinnedDeployment = affinity.Deployment
			pinnedProvider = affinity.Provider
			pinnedUpstreamModel = affinity.UpstreamModel
		}
	}
	if len(responseState.itemIDs) != 0 {
		resourceStore, supportsResources := h.responsesState.(responsesstate.ResourceStore)
		if !supportsResources {
			fail(
				http.StatusServiceUnavailable,
				ledger.OutcomeConfigError,
				"state_affinity_unavailable",
				"provider-owned item routing state storage is not configured",
			)
			return
		}
		affinity, affinityResult, resolveErr := resolveItemAffinities(
			overallContext,
			resourceStore,
			responseState.ownerScope,
			model.Name,
			responseState.itemIDs,
		)
		if resolveErr != nil {
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
		if pinnedDeployment != "" &&
			(pinnedDeployment != affinity.Deployment ||
				pinnedProvider != affinity.Provider ||
				pinnedUpstreamModel != affinity.UpstreamModel) {
			fail(
				http.StatusConflict,
				ledger.OutcomeRejected,
				"state_affinity_conflict",
				"provider-owned item and other state belong to different targets",
			)
			return
		}
		pinnedDeployment = affinity.Deployment
		pinnedProvider = affinity.Provider
		pinnedUpstreamModel = affinity.UpstreamModel
	}
	if len(responseState.fileIDs) != 0 {
		resourceStore, supportsResources := h.responsesState.(responsesstate.ResourceStore)
		if !supportsResources {
			fail(
				http.StatusServiceUnavailable,
				ledger.OutcomeConfigError,
				"state_affinity_unavailable",
				"provider file routing state storage is not configured",
			)
			return
		}
		affinity, affinityResult, resolveErr := resolveFileAffinities(
			overallContext,
			resourceStore,
			responseState.ownerScope,
			model.Name,
			responseState.fileIDs,
		)
		if resolveErr != nil {
			fail(
				http.StatusServiceUnavailable,
				ledger.OutcomeConfigError,
				"state_affinity_unavailable",
				"provider file routing state could not be resolved",
			)
			return
		}
		switch affinityResult {
		case fileAffinityNotFound:
			fail(
				http.StatusConflict,
				ledger.OutcomeRejected,
				"state_affinity_not_found",
				"file_id is unknown or deleted at this gateway",
			)
			return
		case fileAffinityModelMismatch:
			fail(
				http.StatusConflict,
				ledger.OutcomeRejected,
				"state_affinity_model_mismatch",
				"file_id belongs to a different virtual model",
			)
			return
		case fileAffinityTargetMismatch:
			fail(
				http.StatusConflict,
				ledger.OutcomeRejected,
				"state_affinity_conflict",
				"file_id values belong to different provider targets",
			)
			return
		}
		if pinnedDeployment != "" &&
			(pinnedDeployment != affinity.Deployment ||
				pinnedProvider != affinity.Provider ||
				pinnedUpstreamModel != affinity.UpstreamModel) {
			fail(
				http.StatusConflict,
				ledger.OutcomeRejected,
				"state_affinity_conflict",
				"file_id and other provider-owned state belong to different targets",
			)
			return
		}
		pinnedDeployment = affinity.Deployment
		pinnedProvider = affinity.Provider
		pinnedUpstreamModel = affinity.UpstreamModel
	}
	eligibility := routing.Eligibility(h.targets)
	if h.lifecycle != nil {
		eligibility = combinedEligibility{targets: h.targets, lifecycle: h.lifecycle}
	}
	selectionKey := trustedSelectionKey(
		model.Selection,
		callerIdentity,
		h.configRevision,
		model.Name,
	)
	plan, err := h.snapshot.BuildPlanFor(requestedModel, routing.PlanOptions{
		RequiredCapabilities: requiredCapabilities,
		ProtocolResolver:     protocolResolver,
		PinnedDeployment:     pinnedDeployment,
		PinnedProvider:       pinnedProvider,
		SingleAttempt: responseState.singleAttempt() ||
			h.operation.requiresSingleAttempt(requiredCapabilities),
		Eligibility:      eligibility,
		Picker:           h.picker,
		SelectionKey:     selectionKey,
		ProviderPriority: providerPriority,
		AllowInternal:    internalInvocation,
	})
	if err != nil {
		if errors.Is(err, routing.ErrModelNotFound) {
			fail(
				http.StatusNotFound,
				ledger.OutcomeRejected,
				"model_not_found",
				"requested model was not found",
			)
			return
		}
		if errors.Is(err, routing.ErrUnsupportedCapabilities) {
			var capabilityError *routing.UnsupportedCapabilitiesError
			message := "no configured target supports the request's required capabilities"
			if errors.As(err, &capabilityError) {
				observation.telemetry.SetRequiredCapabilities(
					capabilityStrings(capabilityError.Required),
				)
				message = fmt.Sprintf(
					"no configured target supports required capabilities: %s",
					strings.Join(capabilityStrings(capabilityError.Required), ", "),
				)
			}
			fail(
				http.StatusBadRequest,
				ledger.OutcomeRejected,
				"unsupported_feature",
				message,
			)
			return
		}
		if errors.Is(err, routing.ErrUnsupportedSemantics) &&
			crossDialectError != nil {
			fail(
				http.StatusBadRequest,
				ledger.OutcomeRejected,
				requestValidationCode(crossDialectError),
				crossDialectError.Error(),
			)
			return
		}
		if pinnedDeployment != "" {
			fail(
				http.StatusServiceUnavailable,
				ledger.OutcomeExhausted,
				"state_affinity_target_unavailable",
				"the provider-owned response state target is unavailable",
			)
			return
		}
		fail(
			http.StatusServiceUnavailable,
			ledger.OutcomeExhausted,
			"model_unavailable",
			"requested model has no available target",
		)
		return
	}
	if selectorRequest {
		// Provider request rewriting and public response presentation retain the
		// selector requested by the caller; VirtualModel records the concrete
		// logical model selected by the policy engine.
		plan.RequestedModel = publicRequestedModel
		for index := range plan.Candidates {
			plan.Candidates[index].RequestedModel = publicRequestedModel
			plan.Candidates[index].ResponseModel = config.ResponseModelRequested
		}
	}
	if pinnedUpstreamModel != "" &&
		plan.Candidates[0].Deployment.Model != pinnedUpstreamModel {
		fail(
			http.StatusServiceUnavailable,
			ledger.OutcomeConfigError,
			"state_affinity_target_changed",
			"the provider-owned response state target changed upstream model",
		)
		return
	}
	promptAffinity := h.preparePromptCacheAffinity(
		overallContext,
		model,
		callerIdentity,
		envelope,
		plan,
		pinnedDeployment != "" || internalInvocation,
	)
	promptAffinityApplied := false
	if promptAffinity.match != nil {
		promptAffinityApplied = plan.PreferDeployment(promptAffinity.match.Route.Deployment)
	}
	selectionMode := model.Selection.Mode
	if selectionMode == "" {
		selectionMode = config.SelectionWeightedRandom
	}
	routingSelection := telemetry.RoutingSelection{
		Mode: string(selectionMode), WeightedHashApplied: selectionKey != "",
		StateAffinityPinned:   stateAffinityKind != "",
		StateAffinityKind:     stateAffinityKind,
		PromptAffinityEnabled: model.Selection.PromptCacheAffinity.Enabled,
		PromptAffinityMatched: promptAffinity.match != nil,
		PromptAffinityApplied: promptAffinityApplied,
	}
	if promptAffinity.match != nil {
		routingSelection.MatchedPrefixBytes = promptAffinity.match.Prefix.Bytes
		routingSelection.MatchedPrefixSegments = promptAffinity.match.Prefix.Segments
	}
	observation.telemetry.SetRoutingSelection(routingSelection)
	observation.resolve(plan, streaming)
	budgetRequest := h.retryBudget.BeginRequest(
		plan.VirtualModel,
		plan.Retry.Budget,
	)
	defer budgetRequest.End()

	attempts := 0
	finalStatus := http.StatusBadGateway
	finalCode := "upstream_attempts_exhausted"
	finalMessage := "all eligible upstream attempts failed before a response was committed"
	var activeRetry routing.RetryBudgetLease
	var activeRetryReason string
	var activeRetryDelay time.Duration
	releaseActiveRetry := func() {
		if activeRetry != nil {
			activeRetry.Release()
			activeRetry = nil
		}
		activeRetryReason = ""
		activeRetryDelay = 0
	}
	defer releaseActiveRetry()
	scheduleRetry := func(
		canRetry bool,
		retryAfter string,
		reason string,
	) (bool, retryStopReason, error) {
		releaseActiveRetry()
		if !canRetry {
			return false, retryPolicyDisabled, nil
		}
		reservation, stop, err := reserveRetry(
			overallContext,
			budgetRequest,
			plan.Retry,
			attempts,
			retryAfter,
			reason,
			h.retryDelayPicker,
		)
		if err != nil || reservation == nil {
			return false, stop, err
		}
		activeRetry = reservation.lease
		activeRetryReason = reservation.reason
		activeRetryDelay = reservation.delay
		return true, retryAllowed, nil
	}
	applyRetryStop := func(stop retryStopReason) {
		switch stop {
		case retryBudgetDenied:
			finalStatus = http.StatusServiceUnavailable
			finalCode = "retry_budget_exhausted"
			finalMessage = "the shared retry budget has no capacity"
		case retryDeadline:
			finalStatus = http.StatusGatewayTimeout
			finalCode = "request_timeout"
			finalMessage = "the request deadline does not permit another attempt"
		}
	}
	waitForRetry := func() error {
		h.telemetry.RecordRetryDelay(
			overallContext,
			plan.VirtualModel,
			activeRetryReason,
			activeRetryDelay,
		)
		return h.retrySleeper.Sleep(overallContext, activeRetryDelay)
	}
	failRetryWait := func(waitErr error) {
		releaseActiveRetry()
		observation.record.AttemptCount = attempts
		h.setAttemptCount(w.Header(), attempts)
		switch {
		case callerContext.Err() != nil:
			observation.fail(0, ledger.OutcomeClientCancelled, "client_cancelled")
		case errors.Is(waitErr, context.DeadlineExceeded) ||
			errors.Is(overallContext.Err(), context.DeadlineExceeded):
			fail(
				http.StatusGatewayTimeout,
				ledger.OutcomeExhausted,
				"request_timeout",
				"the request deadline expired before another attempt",
			)
		default:
			fail(
				http.StatusInternalServerError,
				ledger.OutcomeConfigError,
				retryFailureClass(waitErr),
				"could not schedule another upstream attempt",
			)
		}
	}

	for index, selection := range plan.Candidates {
		if attempts >= plan.MaxAttempts {
			break
		}
		if !time.Now().Before(overallDeadline) {
			finalStatus = http.StatusGatewayTimeout
			finalCode = "request_timeout"
			finalMessage = "the request deadline expired before an upstream attempt"
			break
		}
		dynamicEndpoint := h.lifecycle != nil &&
			h.lifecycle.IsDynamic(selection.Deployment.Name)
		var lease *routing.TargetLease
		if !dynamicEndpoint {
			lease, _ = h.targets.Acquire(selection.Deployment.Name)
			if lease == nil {
				continue
			}
		}
		attempts++
		attemptStarted := time.Now()
		canRetry := attempts < plan.MaxAttempts && index+1 < len(plan.Candidates)
		attemptContext, telemetryAttempt := observation.startAttempt(
			selection,
			attempts,
			lease,
			plan.RequiredCapabilities,
			activeRetryReason,
			activeRetryDelay,
		)
		attemptTimeout := plan.Limits.PerTryTimeout
		if deadline, exists := overallContext.Deadline(); exists {
			remaining := time.Until(deadline)
			if remaining < attemptTimeout {
				attemptTimeout = remaining
			}
		}
		if attemptTimeout <= 0 {
			recordAttempt(
				h.ledger,
				telemetryAttempt,
				h.targets,
				lease,
				routing.TargetNeutral,
				requestID,
				attempts,
				attemptStarted,
				nil,
				selection,
				0,
				ledger.OutcomeTransportError,
				"overall_timeout",
				false,
				"",
				missingUsage(),
			)
			releaseActiveRetry()
			finalStatus = http.StatusGatewayTimeout
			finalCode = "request_timeout"
			finalMessage = "the request deadline expired before an upstream attempt"
			break
		}
		attemptExecutionContext, cancelAttempt := context.WithTimeout(
			attemptContext,
			attemptTimeout,
		)
		var runtimeLease *lifecycle.AdmissionLease
		if dynamicEndpoint {
			runtimeLease, err = h.lifecycle.Acquire(
				attemptExecutionContext,
				lifecycle.AdmissionRequest{
					Deployment: selection.Deployment.Name,
					BodyBytes:  int64(len(raw)),
					Features: lifecycle.RequestFeatures{
						VirtualModel: plan.VirtualModel,
						Protocol:     string(selectionUpstreamProtocol(selection)),
						Streaming:    streaming,
					},
				},
			)
			if err != nil {
				callerCancelled := callerContext.Err() != nil
				status, outcome, failureClass, message := lifecycleAdmissionFailure(
					err,
					callerCancelled,
				)
				cancelAttempt()
				if callerCancelled {
					recordAttempt(
						h.ledger, telemetryAttempt, h.targets, nil,
						routing.TargetNeutral, requestID, attempts, attemptStarted,
						nil, selection, 0, outcome, failureClass, false, "", missingUsage(),
					)
					releaseActiveRetry()
					observation.record.AttemptCount = attempts
					observation.fail(0, ledger.OutcomeClientCancelled, "client_cancelled")
					return
				}
				canRetryActivation := canRetry
				if h.lifecycle.IsActivatable(selection.Deployment.Name) &&
					index+1 < len(plan.Candidates) &&
					h.lifecycle.IsActivatable(plan.Candidates[index+1].Deployment.Name) {
					canRetryActivation = false
				}
				willRetry, stop, retryErr := scheduleRetry(
					canRetryActivation,
					"",
					failureClass,
				)
				applyRetryStop(stop)
				if !willRetry && stop == retryPolicyDisabled {
					finalStatus, finalCode, finalMessage = status, failureClass, message
					if admissionErr := new(lifecycle.AdmissionError); errors.As(err, &admissionErr) && admissionErr.RetryAfter > 0 {
						w.Header().Set(
							"Retry-After",
							strconv.Itoa(max(1, int(admissionErr.RetryAfter.Round(time.Second)/time.Second))),
						)
					}
				}
				recordAttempt(
					h.ledger, telemetryAttempt, h.targets, nil,
					routing.TargetNeutral, requestID, attempts, attemptStarted,
					nil, selection, 0, outcome, failureClass, willRetry, "", missingUsage(),
				)
				if retryErr != nil {
					releaseActiveRetry()
					observation.record.AttemptCount = attempts
					h.setAttemptCount(w.Header(), attempts)
					fail(
						http.StatusInternalServerError,
						ledger.OutcomeConfigError,
						"retry_scheduling_error",
						"could not schedule another upstream attempt",
					)
					return
				}
				if !willRetry {
					break
				}
				if waitErr := waitForRetry(); waitErr != nil {
					failRetryWait(waitErr)
					return
				}
				continue
			}
			selection.Provider.BaseURL = runtimeLease.Endpoint.BaseURL
			lease, _ = h.targets.Acquire(selection.Deployment.Name)
			if lease == nil {
				cancelAttempt()
				failureClass := "target_admission_unavailable"
				willRetry, stop, retryErr := scheduleRetry(canRetry, "", failureClass)
				applyRetryStop(stop)
				if !willRetry && stop == retryPolicyDisabled {
					finalStatus = http.StatusServiceUnavailable
					finalCode = failureClass
					finalMessage = "the activated target has no request capacity"
				}
				recordAttempt(
					h.ledger, telemetryAttempt, h.targets, nil,
					routing.TargetNeutral, requestID, attempts, attemptStarted,
					nil, selection, 0, ledger.OutcomeExhausted, failureClass,
					willRetry, "", missingUsage(),
					runtimeAttemptLease{coordinator: h.lifecycle, lease: runtimeLease},
				)
				if retryErr != nil {
					releaseActiveRetry()
					observation.record.AttemptCount = attempts
					h.setAttemptCount(w.Header(), attempts)
					fail(http.StatusInternalServerError, ledger.OutcomeConfigError,
						"retry_scheduling_error", "could not schedule another upstream attempt")
					return
				}
				if !willRetry {
					break
				}
				if waitErr := waitForRetry(); waitErr != nil {
					failRetryWait(waitErr)
					return
				}
				continue
			}
		}
		runtimeAttempt := runtimeAttemptLease{coordinator: h.lifecycle, lease: runtimeLease}
		upstreamProtocol := selectionUpstreamProtocol(selection)
		var upstreamBody []byte
		if h.operation == openAIOperationChatCompletions &&
			upstreamProtocol == config.ProtocolBedrock {
			upstreamBody = append([]byte(nil), bedrockChatBody...)
		} else if h.operation == openAIOperationChatCompletions &&
			upstreamProtocol == config.ProtocolAnthropic {
			upstreamBody, err = rewriteTranslatedAnthropicModel(
				anthropicChatBody,
				selection.Deployment.Model,
			)
		} else if h.operation == openAIOperationChatCompletions &&
			upstreamProtocol == config.ProtocolGemini {
			upstreamBody = append([]byte(nil), geminiChatBody...)
		} else if h.operation == openAIOperationEmbeddings &&
			upstreamProtocol == config.ProtocolGemini {
			if geminiEmbeddingInputCount > 1 {
				upstreamBody, err =
					rewriteGeminiBatchEmbeddingModel(
						geminiEmbeddingBody,
						selection.Deployment.Model,
					)
			} else {
				upstreamBody = append(
					[]byte(nil),
					geminiEmbeddingBody...,
				)
			}
		} else if h.operation == openAIOperationResponses &&
			upstreamProtocol == config.ProtocolOpenAI &&
			!selection.NativeProtocol {
			upstreamBody, err = rewriteTranslatedOpenAIModel(
				responsesChatBody,
				selection.Deployment.Model,
			)
		} else {
			upstreamBody, err = h.operation.rewriteRequest(
				envelope,
				selection.Deployment.Model,
			)
		}
		if err != nil {
			cancelAttempt()
			recordAttempt(
				h.ledger,
				telemetryAttempt,
				h.targets,
				lease,
				routing.TargetNeutral,
				requestID,
				attempts,
				attemptStarted,
				nil,
				selection,
				0,
				ledger.OutcomeConfigError,
				"request_rewrite_error",
				false,
				"",
				missingUsage(),
				runtimeAttempt,
			)
			releaseActiveRetry()
			observation.record.AttemptCount = attempts
			h.setAttemptCount(w.Header(), attempts)
			fail(
				http.StatusInternalServerError,
				ledger.OutcomeConfigError,
				"request_rewrite_error",
				"could not prepare upstream request",
			)
			return
		}
		upstreamBody, err = applyExtraBodyDefaults(
			upstreamBody,
			selection.Provider.ExtraBody,
			selection.Deployment.ExtraBody,
		)
		if err != nil {
			cancelAttempt()
			recordAttempt(
				h.ledger, telemetryAttempt, h.targets, lease,
				routing.TargetNeutral, requestID, attempts, attemptStarted,
				nil, selection, 0, ledger.OutcomeConfigError,
				"request_defaults_error", false, "", missingUsage(), runtimeAttempt,
			)
			releaseActiveRetry()
			observation.record.AttemptCount = attempts
			h.setAttemptCount(w.Header(), attempts)
			fail(
				http.StatusInternalServerError,
				ledger.OutcomeConfigError,
				"request_defaults_error",
				"could not apply configured upstream request defaults",
			)
			return
		}
		upstreamRequest, err := h.buildUpstreamRequest(
			request.WithContext(attemptExecutionContext),
			requestID,
			selection,
			upstreamBody,
			streaming,
		)
		if err != nil {
			callerCancelled := callerContext.Err() != nil
			overallTimedOut := !time.Now().Before(overallDeadline)
			attemptTimedOut := errors.Is(
				attemptExecutionContext.Err(),
				context.DeadlineExceeded,
			)
			cancelAttempt()
			if callerCancelled {
				recordAttempt(
					h.ledger,
					telemetryAttempt,
					h.targets,
					lease,
					routing.TargetNeutral,
					requestID,
					attempts,
					attemptStarted,
					nil,
					selection,
					0,
					ledger.OutcomeClientCancelled,
					"client_cancelled",
					false,
					"",
					missingUsage(),
					runtimeAttempt,
				)
				releaseActiveRetry()
				observation.record.AttemptCount = attempts
				observation.fail(0, ledger.OutcomeClientCancelled, "client_cancelled")
				return
			}
			failureClass := "upstream_configuration_error"
			attemptOutcome := ledger.OutcomeConfigError
			canRetryAttempt := canRetry
			if overallTimedOut {
				failureClass = "overall_timeout"
				attemptOutcome = ledger.OutcomeTransportError
				canRetryAttempt = false
				finalStatus = http.StatusGatewayTimeout
				finalCode = "request_timeout"
				finalMessage = "the request deadline expired during upstream preparation"
			} else if attemptTimedOut {
				failureClass = "per_try_timeout"
				attemptOutcome = ledger.OutcomeTransportError
				finalStatus = http.StatusGatewayTimeout
				finalCode = "upstream_timeout"
				finalMessage = "an upstream attempt exceeded its deadline"
			}
			willRetry, stop, retryErr := scheduleRetry(
				canRetryAttempt,
				"",
				failureClass,
			)
			applyRetryStop(stop)
			recordAttempt(
				h.ledger,
				telemetryAttempt,
				h.targets,
				lease,
				routing.TargetFailure,
				requestID,
				attempts,
				attemptStarted,
				nil,
				selection,
				0,
				attemptOutcome,
				failureClass,
				willRetry,
				"",
				missingUsage(),
				runtimeAttempt,
			)
			if retryErr != nil {
				releaseActiveRetry()
				observation.record.AttemptCount = attempts
				h.setAttemptCount(w.Header(), attempts)
				fail(
					http.StatusInternalServerError,
					ledger.OutcomeConfigError,
					"retry_scheduling_error",
					"could not schedule another upstream attempt",
				)
				return
			}
			if !willRetry {
				break
			}
			if waitErr := waitForRetry(); waitErr != nil {
				failRetryWait(waitErr)
				return
			}
			continue
		}
		response, err := h.client.Do(upstreamRequest)
		if err != nil {
			callerCancelled := callerContext.Err() != nil
			overallTimedOut := !time.Now().Before(overallDeadline)
			attemptTimedOut := errors.Is(
				attemptExecutionContext.Err(),
				context.DeadlineExceeded,
			)
			cancelAttempt()
			if callerCancelled {
				recordAttempt(
					h.ledger,
					telemetryAttempt,
					h.targets,
					lease,
					routing.TargetNeutral,
					requestID,
					attempts,
					attemptStarted,
					nil,
					selection,
					0,
					ledger.OutcomeClientCancelled,
					"client_cancelled",
					false,
					"",
					missingUsage(),
					runtimeAttempt,
				)
				releaseActiveRetry()
				observation.record.AttemptCount = attempts
				observation.fail(0, ledger.OutcomeClientCancelled, "client_cancelled")
				return
			}
			failureClass := "upstream_transport_error"
			canRetryAttempt := canRetry
			if overallTimedOut {
				failureClass = "overall_timeout"
				canRetryAttempt = false
				finalStatus = http.StatusGatewayTimeout
				finalCode = "request_timeout"
				finalMessage = "the request deadline expired during an upstream attempt"
			} else if attemptTimedOut {
				failureClass = "per_try_timeout"
				finalStatus = http.StatusGatewayTimeout
				finalCode = "upstream_timeout"
				finalMessage = "an upstream attempt exceeded its deadline"
			}
			willRetry, stop, retryErr := scheduleRetry(
				canRetryAttempt,
				"",
				failureClass,
			)
			applyRetryStop(stop)
			recordAttempt(
				h.ledger,
				telemetryAttempt,
				h.targets,
				lease,
				routing.TargetFailure,
				requestID,
				attempts,
				attemptStarted,
				nil,
				selection,
				0,
				ledger.OutcomeTransportError,
				failureClass,
				willRetry,
				"",
				missingUsage(),
				runtimeAttempt,
			)
			if retryErr != nil {
				releaseActiveRetry()
				observation.record.AttemptCount = attempts
				h.setAttemptCount(w.Header(), attempts)
				fail(
					http.StatusInternalServerError,
					ledger.OutcomeConfigError,
					"retry_scheduling_error",
					"could not schedule another upstream attempt",
				)
				return
			}
			if !willRetry {
				break
			}
			if waitErr := waitForRetry(); waitErr != nil {
				failRetryWait(waitErr)
				return
			}
			continue
		}
		firstByte := time.Now()
		if upstreamOperationForSelection(
			h.operation,
			selection,
			streaming,
		).isRetryableStatus(response.StatusCode) &&
			canRetry {
			failureClass := fmt.Sprintf(
				"upstream_http_%d",
				response.StatusCode,
			)
			willRetry, _, retryErr := scheduleRetry(
				true,
				response.Header.Get("Retry-After"),
				failureClass,
			)
			if retryErr == nil && willRetry {
				_ = response.Body.Close()
				cancelAttempt()
				recordAttempt(
					h.ledger,
					telemetryAttempt,
					h.targets,
					lease,
					routing.TargetFailure,
					requestID,
					attempts,
					attemptStarted,
					&firstByte,
					selection,
					response.StatusCode,
					ledger.OutcomeUpstreamError,
					failureClass,
					true,
					upstreamRequestID(response.Header),
					missingUsage(),
					runtimeAttempt,
				)
				if waitErr := waitForRetry(); waitErr != nil {
					failRetryWait(waitErr)
					return
				}
				continue
			}
		}
		observation.selectFinal(selection, attempts, firstByte)
		responseWriter := w
		var bufferedResponse *bufferedResponseWriter
		if !streaming && (len(guardrails.Post) != 0 || piiSession != nil) {
			bufferedResponse = newBufferedResponseWriter()
			responseWriter = bufferedResponse
		}
		primaryResult := h.proxyResponse(
			responseWriter,
			request,
			response,
			requestID,
			attempts,
			selection,
			streaming,
			plan.Limits.StreamIdleTimeout,
			responseState,
			guardrails,
			raw,
			streamPIIContext{
				session: piiSession, policy: piiPolicy,
				trace: traceWriter, observation: observation,
			},
		)
		cancelAttempt()
		attemptResult := primaryResult
		if primaryResult.postGuardrailTerminated {
			attemptResult.outcome = ledger.OutcomeSuccess
			attemptResult.failureClass = ""
		}
		recordAttempt(
			h.ledger,
			telemetryAttempt,
			h.targets,
			lease,
			targetResultForProxy(
				h.operation,
				selection,
				response.StatusCode,
				attemptResult,
			),
			requestID,
			attempts,
			attemptStarted,
			&firstByte,
			selection,
			response.StatusCode,
			attemptResult.outcome,
			attemptResult.failureClass,
			false,
			upstreamRequestID(response.Header),
			primaryResult.usage,
			runtimeAttempt,
		)
		releaseActiveRetry()
		result := primaryResult
		if bufferedResponse != nil {
			if primaryResult.outcome == ledger.OutcomeSuccess &&
				primaryResult.gatewayStatus >= 200 &&
				primaryResult.gatewayStatus < 300 {
				privacySafeBody := bufferedResponse.body.Bytes()
				privacyFailed := false
				if piiSession != nil {
					privacySafeBody, err = h.operation.redactPIIResponse(
						request.Context(),
						privacySafeBody,
						piiSession,
					)
					if err != nil {
						recordPrivacyFailure(h.privacy)
						observation.disableSavedTrace()
						if piiPolicy.FailureMode == config.PIIFailClosed {
							h.setAttemptCount(w.Header(), attempts)
							h.operation.writeError(
								w,
								http.StatusBadGateway,
								"pii_transform_failed",
								"PII substitution could not safely inspect the response",
								requestID,
							)
							result.gatewayStatus = http.StatusBadGateway
							result.outcome = ledger.OutcomeUpstreamError
							result.failureClass = "pii_transform_failed"
							privacyFailed = true
						} else {
							privacySafeBody = bufferedResponse.body.Bytes()
						}
					}
				}
				if privacyFailed {
					observation.completeResponse(result)
					return
				}
				postResult := h.applyPostGuardrails(
					request,
					guardrails.Post,
					raw,
					privacySafeBody,
				)
				switch {
				case postResult.blocked:
					h.setAttemptCount(w.Header(), attempts)
					h.operation.writeError(
						w,
						http.StatusBadRequest,
						"guardrail_blocked",
						"response was rejected by a configured guardrail",
						requestID,
					)
					result.gatewayStatus = http.StatusBadRequest
					result.outcome = ledger.OutcomeRejected
					result.failureClass = "guardrail_blocked"
				case postResult.failed:
					h.setAttemptCount(w.Header(), attempts)
					h.operation.writeError(
						w,
						http.StatusBadGateway,
						"guardrail_failed",
						"a required response guardrail could not produce a valid verdict",
						requestID,
					)
					result.gatewayStatus = http.StatusBadGateway
					result.outcome = ledger.OutcomeUpstreamError
					result.failureClass = "guardrail_failed"
				default:
					callerBody := postResult.body
					if piiSession != nil {
						var traceBody []byte
						traceBody, err = h.operation.redactPIIResponse(
							request.Context(),
							postResult.body,
							piiSession,
						)
						if err != nil {
							recordPrivacyFailure(h.privacy)
							observation.disableSavedTrace()
							if piiPolicy.FailureMode == config.PIIFailClosed {
								h.setAttemptCount(w.Header(), attempts)
								h.operation.writeError(
									w,
									http.StatusBadGateway,
									"pii_transform_failed",
									"PII substitution could not safely transform the response",
									requestID,
								)
								result.gatewayStatus = http.StatusBadGateway
								result.outcome = ledger.OutcomeUpstreamError
								result.failureClass = "pii_transform_failed"
								privacyFailed = true
							} else {
								traceBody = postResult.body
							}
						}
						callerBody = traceBody
						if !privacyFailed && piiPolicy.Response == config.PIIResponseRestore {
							callerBody, err = h.operation.restorePIIResponse(
								request.Context(),
								traceBody,
								piiSession,
							)
							if err != nil {
								recordPrivacyFailure(h.privacy)
								observation.disableSavedTrace()
								if piiPolicy.FailureMode == config.PIIFailClosed {
									h.setAttemptCount(w.Header(), attempts)
									h.operation.writeError(
										w,
										http.StatusBadGateway,
										"pii_restore_failed",
										"PII substitution could not safely restore the response",
										requestID,
									)
									result.gatewayStatus = http.StatusBadGateway
									result.outcome = ledger.OutcomeUpstreamError
									result.failureClass = "pii_restore_failed"
									privacyFailed = true
								} else {
									callerBody = traceBody
								}
							}
						}
						if !privacyFailed && piiPolicy.TraceContent == config.PIITraceMasked {
							traceWriter.overrideCapture(traceBody)
						}
					}
					if privacyFailed {
						break
					}
					if err := writeBufferedResponse(
						w,
						bufferedResponse,
						callerBody,
						h.operation,
					); err != nil {
						result.outcome, result.failureClass = streamFailure(
							request.Context().Err(),
							false,
						)
					}
				}
			} else {
				callerBody := bufferedResponse.body.Bytes()
				if piiSession != nil {
					if piiPolicy.TraceContent == config.PIITraceMasked {
						maskedError, maskErr := redactAllPIIJSONStrings(
							request.Context(), callerBody, piiSession,
						)
						if maskErr != nil {
							observation.disableSavedTrace()
						} else {
							traceWriter.overrideCapture(maskedError)
						}
					}
					if piiPolicy.Response == config.PIIResponseRestore {
						restoredError, restoreErr := restoreAllPIIJSONStrings(
							request.Context(), callerBody, piiSession,
						)
						if restoreErr == nil {
							callerBody = restoredError
						}
					}
				}
				if err := writeBufferedResponse(
					w,
					bufferedResponse,
					callerBody,
					h.operation,
				); err != nil {
					result.outcome, result.failureClass = streamFailure(
						request.Context().Err(),
						false,
					)
				}
			}
		}
		observation.completeResponse(result)
		if result.outcome == ledger.OutcomeSuccess &&
			result.gatewayStatus >= 200 && result.gatewayStatus < 300 {
			promptAffinity.record(selection, time.Now())
		}
		return
	}
	observation.record.AttemptCount = attempts
	h.setAttemptCount(w.Header(), attempts)
	if attempts == 0 {
		if finalStatus == http.StatusGatewayTimeout {
			fail(
				finalStatus,
				ledger.OutcomeExhausted,
				finalCode,
				finalMessage,
			)
			return
		}
		fail(
			http.StatusServiceUnavailable,
			ledger.OutcomeExhausted,
			"model_unavailable",
			"requested model has no target capacity available",
		)
		return
	}
	fail(
		finalStatus,
		ledger.OutcomeExhausted,
		finalCode,
		finalMessage,
	)
}

func selectionUpstreamProtocol(selection routing.Selection) config.Protocol {
	if selection.UpstreamProtocol != "" {
		return selection.UpstreamProtocol
	}
	return config.ProtocolForProviderType(selection.Provider.Type)
}

func nativeProtocolResolver(protocol config.Protocol) routing.ProtocolResolver {
	return func(
		provider config.Provider,
		deployment config.Deployment,
	) (routing.ProtocolRoute, bool) {
		if !deployment.SupportsNativeProtocol(provider, protocol) {
			return routing.ProtocolRoute{}, false
		}
		return routing.ProtocolRoute{Protocol: protocol, Native: true}, true
	}
}

func targetResultForProxy(
	operation openAIOperation,
	selection routing.Selection,
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
	if upstreamOperationForSelection(
		operation,
		selection,
		false,
	).isRetryableStatus(status) ||
		status >= 200 && status < 300 {
		return routing.TargetFailure
	}
	return routing.TargetNeutral
}

type proxyResult struct {
	gatewayStatus           int
	outcome                 ledger.Outcome
	failureClass            string
	usage                   ledger.TokenUsage
	postGuardrailTerminated bool
}

func (h *chatCompletionsHandler) proxyResponse(
	w http.ResponseWriter,
	request *http.Request,
	response *http.Response,
	requestID string,
	attempts int,
	selection routing.Selection,
	streaming bool,
	streamIdleTimeout time.Duration,
	responseState responsesRequestState,
	guardrails config.GuardrailPolicy,
	requestBody []byte,
	privacyContext streamPIIContext,
) proxyResult {
	upstreamProtocol := selectionUpstreamProtocol(selection)
	translatedBedrock := h.operation ==
		openAIOperationChatCompletions &&
		upstreamProtocol == config.ProtocolBedrock
	translatedAnthropic := h.operation ==
		openAIOperationChatCompletions &&
		upstreamProtocol == config.ProtocolAnthropic
	translatedGemini := h.operation ==
		openAIOperationChatCompletions &&
		upstreamProtocol == config.ProtocolGemini
	translatedGeminiEmbedding := h.operation ==
		openAIOperationEmbeddings &&
		upstreamProtocol == config.ProtocolGemini
	translatedResponses := h.operation == openAIOperationResponses &&
		upstreamProtocol == config.ProtocolOpenAI &&
		!selection.NativeProtocol
	translatedChat := translatedBedrock ||
		translatedAnthropic ||
		translatedGemini
	translatedOpenAI := translatedChat ||
		translatedGeminiEmbedding ||
		translatedResponses
	upstreamOperation := upstreamOperationForSelection(
		h.operation,
		selection,
		streaming,
	)
	contentType := response.Header.Get("Content-Type")
	sseStream := isSSEStream(contentType)
	awsEventStream := isAWSEventStream(contentType)
	eventStream := sseStream || awsEventStream
	var idleBody *streamIdleReadCloser
	if response.StatusCode >= 200 && response.StatusCode < 300 && eventStream {
		idleBody = newStreamIdleReadCloser(response.Body, streamIdleTimeout)
		response.Body = idleBody
	}
	defer func() { _ = response.Body.Close() }()
	modelName, rewriteModel := presentedModelName(selection)
	result := proxyResult{
		gatewayStatus: response.StatusCode,
		outcome:       ledger.OutcomeUpstreamError,
		failureClass:  fmt.Sprintf("upstream_http_%d", response.StatusCode),
		usage:         missingUsage(),
	}
	success := response.StatusCode >= 200 && response.StatusCode < 300
	if success {
		result.outcome = ledger.OutcomeSuccess
		result.failureClass = ""
	}
	if success && upstreamOperation.isBedrock() &&
		(streaming && !awsEventStream ||
			!streaming && (eventStream ||
				contentType != "" &&
					!isJSONContentType(contentType))) {
		h.setAttemptCount(w.Header(), attempts)
		h.operation.writeError(
			w,
			http.StatusBadGateway,
			"upstream_response_error",
			"upstream returned an invalid "+
				h.operation.responseName()+" content type",
			requestID,
		)
		result.gatewayStatus = http.StatusBadGateway
		result.outcome = ledger.OutcomeUpstreamError
		result.failureClass = "upstream_invalid_content_type"
		return result
	}
	if success && (translatedAnthropic || translatedGemini ||
		translatedGeminiEmbedding) &&
		(streaming && !sseStream ||
			!streaming && (eventStream ||
				contentType != "" &&
					!isJSONContentType(contentType))) {
		responseName := "Anthropic Messages"
		if translatedGemini {
			responseName = "Gemini GenerateContent"
		} else if translatedGeminiEmbedding {
			responseName = "Gemini EmbedContent"
			if responseState.embeddingInputCount > 1 {
				responseName = "Gemini BatchEmbedContents"
			}
		}
		h.setAttemptCount(w.Header(), attempts)
		h.operation.writeError(
			w,
			http.StatusBadGateway,
			"upstream_response_error",
			"upstream returned an invalid "+responseName+" content type",
			requestID,
		)
		result.gatewayStatus = http.StatusBadGateway
		result.outcome = ledger.OutcomeUpstreamError
		result.failureClass = "upstream_invalid_content_type"
		return result
	}
	if !success && (translatedChat || translatedGeminiEmbedding) {
		translatedDestination := http.ResponseWriter(w)
		var privacyBuffer *bufferedResponseWriter
		if streaming && privacyContext.session != nil {
			privacyBuffer = newBufferedResponseWriter()
			translatedDestination = privacyBuffer
		}
		h.operation.copyResponseHeaders(translatedDestination.Header(), response.Header)
		configureTranslatedOpenAIHeaders(translatedDestination.Header(), false)
		h.operation.setRequestIDHeaders(
			translatedDestination.Header(),
			h.requestIDHeader,
			requestID,
		)
		h.setAttemptCount(translatedDestination.Header(), attempts)
		if translatedBedrock {
			translateBedrockErrorToOpenAI(translatedDestination, response)
		} else if translatedAnthropic {
			translateAnthropicErrorToOpenAI(translatedDestination, response)
		} else {
			translateGeminiErrorToOpenAI(translatedDestination, response)
		}
		if privacyBuffer != nil {
			if err := writePIIBufferedResponse(
				w, privacyBuffer, h.operation, privacyContext, request.Context(),
			); err != nil {
				recordPrivacyFailure(h.privacy)
				h.operation.writeError(
					w, http.StatusBadGateway, "pii_transform_failed",
					"PII substitution could not safely transform the upstream error",
					requestID,
				)
				result.gatewayStatus = http.StatusBadGateway
				result.failureClass = "pii_transform_failed"
			}
		}
		return result
	}

	if success && !eventStream {
		body, err := readBoundedResponse(response.Body, h.maxResponseBytes)
		if err != nil {
			h.setAttemptCount(w.Header(), attempts)
			h.operation.writeError(
				w,
				http.StatusBadGateway,
				"upstream_response_error",
				"upstream response was invalid or exceeded the configured limit",
				requestID,
			)
			result.gatewayStatus = http.StatusBadGateway
			result.outcome = ledger.OutcomeUpstreamError
			result.failureClass = "upstream_response_limit"
			return result
		}
		if translatedResponses {
			body, err = translateChatResponseToResponses(body)
			if err == nil {
				if usage, found := h.operation.extractUsage(body); found {
					result.usage = usage
				}
				if rewriteModel {
					body, err = h.operation.rewriteJSONResponseModel(
						body,
						modelName,
					)
				}
			}
		} else if translatedBedrock {
			body, result.usage, err =
				translateBedrockResponseToChatCompletions(
					body,
					requestID,
					translatedOpenAIResponseModel(selection),
					translatedChatCreated(),
				)
		} else if translatedAnthropic {
			body, result.usage, err =
				translateAnthropicResponseToChatCompletions(
					body,
					requestID,
					translatedOpenAIResponseModel(selection),
					translatedChatCreated(),
				)
		} else if translatedGemini {
			body, result.usage, err =
				translateGeminiResponseToChatCompletions(
					body,
					requestID,
					translatedOpenAIResponseModel(selection),
					translatedChatCreated(),
				)
		} else if translatedGeminiEmbedding {
			body, result.usage, err =
				translateGeminiEmbeddingResponseToOpenAI(
					body,
					translatedOpenAIResponseModel(selection),
					responseState.embeddingDimensions,
					responseState.embeddingInputCount,
				)
		} else {
			if usage, found := h.operation.extractUsage(body); found {
				result.usage = usage
			}
			if rewriteModel {
				body, err = h.operation.rewriteJSONResponseModel(
					body,
					modelName,
				)
			}
		}
		if err == nil {
			err = h.operation.validateSuccessResponse(body)
		}
		if err != nil {
			h.setAttemptCount(w.Header(), attempts)
			h.operation.writeError(
				w,
				http.StatusBadGateway,
				"upstream_response_error",
				"upstream returned an invalid "+h.operation.responseName()+" response",
				requestID,
			)
			result.gatewayStatus = http.StatusBadGateway
			result.outcome = ledger.OutcomeUpstreamError
			result.failureClass = "upstream_invalid_response"
			return result
		}
		if h.operation == openAIOperationResponses &&
			!translatedResponses &&
			responseState.bindsResponse() {
			responseID, found, stateErr := responsesIDFromPayload(body)
			if stateErr != nil || !found {
				h.setAttemptCount(w.Header(), attempts)
				h.operation.writeError(
					w,
					http.StatusBadGateway,
					"upstream_response_error",
					"upstream returned a stored response without a valid response ID",
					requestID,
				)
				result.gatewayStatus = http.StatusBadGateway
				result.outcome = ledger.OutcomeUpstreamError
				result.failureClass = "responses_state_invalid"
				return result
			}
			if stateErr = h.bindResponsesAffinity(
				request.Context(),
				responseState.ownerScope,
				responseID,
				selection,
				responseState.conversationID != "",
			); stateErr != nil {
				status := http.StatusServiceUnavailable
				code := "state_affinity_unavailable"
				message := "provider-owned response state could not be persisted"
				failureClass := "responses_state_bind_failed"
				if errors.Is(stateErr, responsesstate.ErrConflict) {
					status = http.StatusBadGateway
					code = "upstream_response_error"
					message = "upstream returned a response ID with conflicting affinity"
					failureClass = "responses_state_conflict"
				}
				h.setAttemptCount(w.Header(), attempts)
				h.operation.writeError(
					w,
					status,
					code,
					message,
					requestID,
				)
				result.gatewayStatus = status
				result.outcome = ledger.OutcomeUpstreamError
				result.failureClass = failureClass
				return result
			}
		}
		if h.operation.isResponsesOperation() && !translatedResponses {
			failureClass, stateErr := h.bindResponseProviderItems(
				request.Context(),
				body,
				responseState,
				selection,
			)
			if stateErr != nil {
				status := http.StatusServiceUnavailable
				code := "state_affinity_unavailable"
				message := "provider-owned item routing state could not be persisted"
				switch failureClass {
				case "responses_item_state_invalid":
					status = http.StatusBadGateway
					code = "upstream_response_error"
					message = "upstream returned invalid provider-owned item state"
				case "responses_item_state_conflict":
					status = http.StatusBadGateway
					code = "upstream_response_error"
					message = "upstream returned an item ID with conflicting affinity"
				}
				h.setAttemptCount(w.Header(), attempts)
				h.operation.writeError(
					w,
					status,
					code,
					message,
					requestID,
				)
				result.gatewayStatus = status
				result.outcome = ledger.OutcomeUpstreamError
				result.failureClass = failureClass
				return result
			}
		}
		h.operation.copyResponseHeaders(w.Header(), response.Header)
		if translatedOpenAI {
			configureTranslatedOpenAIHeaders(w.Header(), false)
		} else if rewriteModel {
			removeRepresentationValidators(w.Header())
		}
		h.operation.setRequestIDHeaders(
			w.Header(),
			h.requestIDHeader,
			requestID,
		)
		h.setAttemptCount(w.Header(), attempts)
		w.Header().Del("Content-Length")
		w.Header().Set("Content-Length", strconv.Itoa(len(body)))
		w.WriteHeader(response.StatusCode)
		_, err = w.Write(body)
		if err != nil {
			result.outcome, result.failureClass = streamFailure(request.Context().Err(), false)
		}
		return result
	}
	if !success && streaming && privacyContext.session != nil && !eventStream {
		body, err := readBoundedResponse(response.Body, h.maxResponseBytes)
		if err != nil {
			h.setAttemptCount(w.Header(), attempts)
			h.operation.writeError(
				w, http.StatusBadGateway, "upstream_response_error",
				"upstream response was invalid or exceeded the configured limit",
				requestID,
			)
			result.gatewayStatus = http.StatusBadGateway
			result.failureClass = "upstream_response_limit"
			return result
		}
		privacyBuffer := newBufferedResponseWriter()
		h.operation.copyResponseHeaders(privacyBuffer.Header(), response.Header)
		h.operation.setRequestIDHeaders(
			privacyBuffer.Header(), h.requestIDHeader, requestID,
		)
		h.setAttemptCount(privacyBuffer.Header(), attempts)
		privacyBuffer.WriteHeader(response.StatusCode)
		_, _ = privacyBuffer.Write(body)
		if err := writePIIBufferedResponse(
			w, privacyBuffer, h.operation, privacyContext, request.Context(),
		); err != nil {
			recordPrivacyFailure(h.privacy)
			h.operation.writeError(
				w, http.StatusBadGateway, "pii_transform_failed",
				"PII substitution could not safely transform the upstream error",
				requestID,
			)
			result.gatewayStatus = http.StatusBadGateway
			result.failureClass = "pii_transform_failed"
		}
		return result
	}

	h.operation.copyResponseHeaders(w.Header(), response.Header)
	if translatedOpenAI && eventStream && success {
		configureTranslatedOpenAIHeaders(w.Header(), true)
	} else if rewriteModel && eventStream && success {
		removeRepresentationValidators(w.Header())
	}
	h.operation.setRequestIDHeaders(
		w.Header(),
		h.requestIDHeader,
		requestID,
	)
	h.setAttemptCount(w.Header(), attempts)
	if eventStream && success {
		w.Header().Del("Content-Length")
	}
	w.WriteHeader(response.StatusCode)
	if eventStream && !success && privacyContext.session != nil {
		format := piiStreamSSE
		if awsEventStream {
			format = piiStreamAWS
		}
		pipeline := newStreamResponsePipeline(
			w, h, request, config.GuardrailPolicy{}, requestBody,
			false, h.operation, format, privacyContext,
		)
		var err error
		if awsEventStream {
			err = proxyAWSEventMessages(pipeline.destination, response.Body, nil)
		} else {
			err = proxySSEEvents(pipeline.destination, response.Body, nil, nil)
		}
		err = pipeline.Finish(err)
		if err != nil {
			var privacyError *streamPIIError
			if errors.As(err, &privacyError) && request.Context().Err() == nil {
				recordPrivacyFailure(h.privacy)
				result.failureClass = "pii_transform_failed"
			} else {
				result.outcome, result.failureClass = streamFailure(
					request.Context().Err(),
					idleBody != nil && idleBody.TimedOut(),
				)
			}
		}
		return result
	}
	if eventStream && success {
		if translatedResponses {
			return h.proxyChatResponsesStream(
				w,
				request,
				response,
				result,
				selection,
				guardrails,
				requestBody,
				idleBody,
				privacyContext,
			)
		}
		if translatedBedrock {
			return h.proxyBedrockChatStream(
				w,
				request,
				response,
				result,
				requestID,
				selection,
				guardrails,
				requestBody,
				idleBody,
				privacyContext,
			)
		}
		if translatedAnthropic {
			return h.proxyAnthropicChatStream(
				w,
				request,
				response,
				result,
				requestID,
				selection,
				guardrails,
				requestBody,
				idleBody,
				privacyContext,
			)
		}
		if translatedGemini {
			return h.proxyGeminiChatStream(
				w,
				request,
				response,
				result,
				requestID,
				selection,
				guardrails,
				requestBody,
				idleBody,
				privacyContext,
			)
		}
		if h.operation.isBedrock() {
			return h.proxyBedrockEventStream(
				w,
				request,
				response,
				result,
				streaming,
				guardrails,
				requestBody,
				idleBody,
				privacyContext,
			)
		}
		transformEvent := h.operation.sseEventTransformer(modelName, rewriteModel)
		sawTerminal := !h.operation.streamRequiresTerminal()
		stateBound := !responseState.bindsResponse()
		stateFailure := ""
		pipeline := newStreamResponsePipeline(
			w, h, request, guardrails, requestBody,
			streaming, h.operation, piiStreamSSE, privacyContext,
		)
		err := proxySSEEvents(
			pipeline.destination,
			response.Body,
			transformEvent,
			func(payload []byte) error {
				if h.operation == openAIOperationResponses && !stateBound {
					responseID, found, stateErr := responsesIDFromPayload(payload)
					if stateErr != nil {
						stateFailure = "responses_state_invalid"
						return stateErr
					}
					if found {
						if stateErr := h.bindResponsesAffinity(
							request.Context(),
							responseState.ownerScope,
							responseID,
							selection,
							responseState.conversationID != "",
						); stateErr != nil {
							if errors.Is(stateErr, responsesstate.ErrConflict) {
								stateFailure = "responses_state_conflict"
							} else {
								stateFailure = "responses_state_bind_failed"
							}
							return stateErr
						}
						stateBound = true
					}
				}
				if h.operation == openAIOperationResponses {
					failureClass, stateErr := h.bindResponseProviderItems(
						request.Context(),
						payload,
						responseState,
						selection,
					)
					if stateErr != nil {
						stateFailure = failureClass
						return stateErr
					}
				}
				if usage, found := h.operation.extractUsage(payload); found {
					result.usage = h.operation.mergeUsage(
						result.usage,
						usage,
					)
				}
				terminal, outcome, failureClass :=
					h.operation.inspectStreamEvent(payload)
				if terminal {
					sawTerminal = true
					result.outcome = outcome
					result.failureClass = failureClass
				}
				return nil
			},
		)
		err = pipeline.Finish(err)
		if err != nil {
			var privacyError *streamPIIError
			var guardrailError *streamPostGuardrailError
			if errors.As(err, &privacyError) && request.Context().Err() == nil {
				recordPrivacyFailure(h.privacy)
				result.outcome = ledger.OutcomeUpstreamError
				result.failureClass = "pii_transform_failed"
			} else if errors.As(err, &guardrailError) &&
				request.Context().Err() == nil {
				result.postGuardrailTerminated = true
				if guardrailError.blocked {
					result.outcome = ledger.OutcomeRejected
					result.failureClass = "guardrail_blocked"
				} else {
					result.outcome = ledger.OutcomeUpstreamError
					result.failureClass = "guardrail_failed"
				}
			} else if stateFailure != "" {
				result.outcome = ledger.OutcomeUpstreamError
				result.failureClass = stateFailure
			} else {
				result.outcome, result.failureClass = streamFailure(
					request.Context().Err(),
					idleBody != nil && idleBody.TimedOut(),
				)
			}
		} else if !stateBound {
			result.outcome = ledger.OutcomeUpstreamError
			result.failureClass = "responses_state_invalid"
		} else if !sawTerminal {
			result.outcome = ledger.OutcomeStreamError
			result.failureClass =
				h.operation.streamIncompleteFailureClass()
		}
		return result
	}
	if streaming || eventStream {
		_, err := io.CopyBuffer(flushingWriter{writer: w}, response.Body, make([]byte, 32<<10))
		if err != nil {
			result.outcome, result.failureClass = streamFailure(
				request.Context().Err(),
				idleBody != nil && idleBody.TimedOut(),
			)
		}
		return result
	}
	_, err := io.CopyBuffer(w, response.Body, make([]byte, 32<<10))
	if err != nil {
		result.outcome, result.failureClass = streamFailure(request.Context().Err(), false)
	}
	return result
}

func (h *chatCompletionsHandler) proxyChatResponsesStream(
	w http.ResponseWriter,
	request *http.Request,
	response *http.Response,
	result proxyResult,
	selection routing.Selection,
	guardrails config.GuardrailPolicy,
	requestBody []byte,
	idleBody *streamIdleReadCloser,
	privacyContext streamPIIContext,
) proxyResult {
	pipeline := newStreamResponsePipeline(
		w, h, request, guardrails, requestBody,
		true, openAIOperationResponses, piiStreamSSE, privacyContext,
	)
	model := translatedOpenAIResponseModel(selection)
	sawTerminal := false
	err := proxyChatStreamToResponses(
		pipeline.destination,
		response.Body,
		model,
		func(payload []byte) error {
			if usage, found := openAIOperationResponses.extractUsage(payload); found {
				result.usage = openAIOperationResponses.mergeUsage(result.usage, usage)
			}
			terminal, outcome, failureClass :=
				openAIOperationResponses.inspectStreamEvent(payload)
			if terminal {
				sawTerminal = true
				result.outcome = outcome
				result.failureClass = failureClass
			}
			return nil
		},
	)
	err = pipeline.Finish(err)
	if err == nil && !sawTerminal {
		result.outcome = ledger.OutcomeStreamError
		result.failureClass = openAIOperationResponses.streamIncompleteFailureClass()
		return result
	}
	if err == nil {
		return result
	}
	var privacyError *streamPIIError
	if errors.As(err, &privacyError) && request.Context().Err() == nil {
		recordPrivacyFailure(h.privacy)
		result.outcome = ledger.OutcomeUpstreamError
		result.failureClass = "pii_transform_failed"
		return result
	}
	var guardrailError *streamPostGuardrailError
	if errors.As(err, &guardrailError) && request.Context().Err() == nil {
		result.postGuardrailTerminated = true
		if guardrailError.blocked {
			result.outcome = ledger.OutcomeRejected
			result.failureClass = "guardrail_blocked"
		} else {
			result.outcome = ledger.OutcomeUpstreamError
			result.failureClass = "guardrail_failed"
		}
		return result
	}
	result.outcome, result.failureClass = streamFailure(
		request.Context().Err(),
		idleBody != nil && idleBody.TimedOut(),
	)
	if request.Context().Err() == nil &&
		(idleBody == nil || !idleBody.TimedOut()) {
		result.failureClass = "chat_to_responses_stream_error"
	}
	return result
}

func (h *chatCompletionsHandler) proxyBedrockChatStream(
	w http.ResponseWriter,
	request *http.Request,
	response *http.Response,
	result proxyResult,
	requestID string,
	selection routing.Selection,
	guardrails config.GuardrailPolicy,
	requestBody []byte,
	idleBody *streamIdleReadCloser,
	privacyContext streamPIIContext,
) proxyResult {
	pipeline := newStreamResponsePipeline(
		w, h, request, guardrails, requestBody,
		true, openAIOperationChatCompletions, piiStreamSSE, privacyContext,
	)
	translated, err := proxyBedrockChatCompletionsStream(
		pipeline.destination,
		response.Body,
		requestID,
		translatedOpenAIResponseModel(selection),
		translatedChatIncludesUsage(requestBody),
		result,
	)
	result = translated
	err = pipeline.Finish(err)
	if err == nil {
		return result
	}
	var privacyError *streamPIIError
	if errors.As(err, &privacyError) && request.Context().Err() == nil {
		recordPrivacyFailure(h.privacy)
		result.outcome = ledger.OutcomeUpstreamError
		result.failureClass = "pii_transform_failed"
		return result
	}
	var guardrailError *streamPostGuardrailError
	if errors.As(err, &guardrailError) &&
		request.Context().Err() == nil {
		result.postGuardrailTerminated = true
		if guardrailError.blocked {
			result.outcome = ledger.OutcomeRejected
			result.failureClass = "guardrail_blocked"
		} else {
			result.outcome = ledger.OutcomeUpstreamError
			result.failureClass = "guardrail_failed"
		}
		return result
	}
	result.outcome, result.failureClass = streamFailure(
		request.Context().Err(),
		idleBody != nil && idleBody.TimedOut(),
	)
	if request.Context().Err() == nil &&
		(idleBody == nil || !idleBody.TimedOut()) {
		result.failureClass = chatBedrockStreamFailureClass
	}
	return result
}

func (h *chatCompletionsHandler) proxyAnthropicChatStream(
	w http.ResponseWriter,
	request *http.Request,
	response *http.Response,
	result proxyResult,
	requestID string,
	selection routing.Selection,
	guardrails config.GuardrailPolicy,
	requestBody []byte,
	idleBody *streamIdleReadCloser,
	privacyContext streamPIIContext,
) proxyResult {
	pipeline := newStreamResponsePipeline(
		w, h, request, guardrails, requestBody,
		true, openAIOperationChatCompletions, piiStreamSSE, privacyContext,
	)
	translated, err := proxyAnthropicChatCompletionsStream(
		pipeline.destination,
		response.Body,
		requestID,
		translatedOpenAIResponseModel(selection),
		translatedChatIncludesUsage(requestBody),
		result,
	)
	result = translated
	err = pipeline.Finish(err)
	if err == nil {
		return result
	}
	var privacyError *streamPIIError
	if errors.As(err, &privacyError) && request.Context().Err() == nil {
		recordPrivacyFailure(h.privacy)
		result.outcome = ledger.OutcomeUpstreamError
		result.failureClass = "pii_transform_failed"
		return result
	}
	var guardrailError *streamPostGuardrailError
	if errors.As(err, &guardrailError) &&
		request.Context().Err() == nil {
		result.postGuardrailTerminated = true
		if guardrailError.blocked {
			result.outcome = ledger.OutcomeRejected
			result.failureClass = "guardrail_blocked"
		} else {
			result.outcome = ledger.OutcomeUpstreamError
			result.failureClass = "guardrail_failed"
		}
		return result
	}
	result.outcome, result.failureClass = streamFailure(
		request.Context().Err(),
		idleBody != nil && idleBody.TimedOut(),
	)
	if request.Context().Err() == nil &&
		(idleBody == nil || !idleBody.TimedOut()) {
		result.failureClass = chatAnthropicStreamFailureClass
	}
	return result
}

func (h *chatCompletionsHandler) proxyGeminiChatStream(
	w http.ResponseWriter,
	request *http.Request,
	response *http.Response,
	result proxyResult,
	requestID string,
	selection routing.Selection,
	guardrails config.GuardrailPolicy,
	requestBody []byte,
	idleBody *streamIdleReadCloser,
	privacyContext streamPIIContext,
) proxyResult {
	pipeline := newStreamResponsePipeline(
		w, h, request, guardrails, requestBody,
		true, openAIOperationChatCompletions, piiStreamSSE, privacyContext,
	)
	translated, err := proxyGeminiChatCompletionsStream(
		pipeline.destination,
		response.Body,
		requestID,
		translatedOpenAIResponseModel(selection),
		translatedChatIncludesUsage(requestBody),
		result,
	)
	result = translated
	err = pipeline.Finish(err)
	if err == nil {
		return result
	}
	var privacyError *streamPIIError
	if errors.As(err, &privacyError) && request.Context().Err() == nil {
		recordPrivacyFailure(h.privacy)
		result.outcome = ledger.OutcomeUpstreamError
		result.failureClass = "pii_transform_failed"
		return result
	}
	var guardrailError *streamPostGuardrailError
	if errors.As(err, &guardrailError) &&
		request.Context().Err() == nil {
		result.postGuardrailTerminated = true
		if guardrailError.blocked {
			result.outcome = ledger.OutcomeRejected
			result.failureClass = "guardrail_blocked"
		} else {
			result.outcome = ledger.OutcomeUpstreamError
			result.failureClass = "guardrail_failed"
		}
		return result
	}
	result.outcome, result.failureClass = streamFailure(
		request.Context().Err(),
		idleBody != nil && idleBody.TimedOut(),
	)
	if request.Context().Err() == nil &&
		(idleBody == nil || !idleBody.TimedOut()) {
		result.failureClass = chatGeminiStreamFailureClass
	}
	return result
}

func (h *chatCompletionsHandler) proxyBedrockEventStream(
	w http.ResponseWriter,
	request *http.Request,
	response *http.Response,
	result proxyResult,
	streaming bool,
	guardrails config.GuardrailPolicy,
	requestBody []byte,
	idleBody *streamIdleReadCloser,
	privacyContext streamPIIContext,
) proxyResult {
	sawTerminal := false
	pipeline := newStreamResponsePipeline(
		w, h, request, guardrails, requestBody,
		streaming, h.operation, piiStreamAWS, privacyContext,
	)
	err := proxyAWSEventMessages(
		pipeline.destination,
		response.Body,
		func(message awsEventMessage) error {
			if usage, found := h.operation.extractUsage(
				message.payload,
			); found {
				result.usage = h.operation.mergeUsage(
					result.usage,
					usage,
				)
			}
			terminal, outcome, failureClass :=
				inspectBedrockStreamMessage(message)
			if terminal {
				sawTerminal = true
				// Never let a malformed later success terminal erase an
				// exception that was already exposed in the stream.
				if result.outcome == ledger.OutcomeSuccess ||
					outcome != ledger.OutcomeSuccess {
					result.outcome = outcome
					result.failureClass = failureClass
				}
			}
			return nil
		},
	)
	err = pipeline.Finish(err)
	if err != nil {
		var privacyError *streamPIIError
		var guardrailError *streamPostGuardrailError
		if errors.As(err, &privacyError) && request.Context().Err() == nil {
			recordPrivacyFailure(h.privacy)
			result.outcome = ledger.OutcomeUpstreamError
			result.failureClass = "pii_transform_failed"
		} else if errors.As(err, &guardrailError) &&
			request.Context().Err() == nil {
			result.postGuardrailTerminated = true
			if guardrailError.blocked {
				result.outcome = ledger.OutcomeRejected
				result.failureClass = "guardrail_blocked"
			} else {
				result.outcome = ledger.OutcomeUpstreamError
				result.failureClass = "guardrail_failed"
			}
		} else {
			result.outcome, result.failureClass = streamFailure(
				request.Context().Err(),
				idleBody != nil && idleBody.TimedOut(),
			)
			if request.Context().Err() == nil &&
				(idleBody == nil || !idleBody.TimedOut()) {
				result.failureClass =
					"bedrock_stream_invalid_message"
			}
		}
	} else if !sawTerminal {
		result.outcome = ledger.OutcomeStreamError
		result.failureClass =
			h.operation.streamIncompleteFailureClass()
	}
	return result
}

func streamFailure(
	contextError error,
	idleTimedOut bool,
) (ledger.Outcome, string) {
	if idleTimedOut {
		return ledger.OutcomeStreamError, "stream_idle_timeout"
	}
	if contextError != nil {
		return ledger.OutcomeClientCancelled, "client_cancelled"
	}
	return ledger.OutcomeStreamError, "response_stream_error"
}

func (h *chatCompletionsHandler) setAttemptCount(header http.Header, attempts int) {
	header.Set(h.attemptHeader, strconv.Itoa(attempts))
}

func lifecycleAdmissionFailure(
	err error,
	callerCancelled bool,
) (int, ledger.Outcome, string, string) {
	switch {
	case callerCancelled || errors.Is(err, context.Canceled):
		return 0, ledger.OutcomeClientCancelled, "client_cancelled", "request was cancelled"
	case errors.Is(err, context.DeadlineExceeded):
		return http.StatusGatewayTimeout, ledger.OutcomeTransportError,
			"activation_timeout", "the model did not become ready before the request deadline"
	case errors.Is(err, lifecycle.ErrQueueFull):
		return http.StatusServiceUnavailable, ledger.OutcomeExhausted,
			"cold_start_queue_full", "the model cold-start queue has no capacity"
	case errors.Is(err, lifecycle.ErrQueueBytes):
		return http.StatusServiceUnavailable, ledger.OutcomeExhausted,
			"cold_start_queue_bytes_exceeded", "the model cold-start byte queue has no capacity"
	case errors.Is(err, lifecycle.ErrColdStartRejected):
		return http.StatusServiceUnavailable, ledger.OutcomeExhausted,
			"cold_start_rejected", "the model is cold and waiting is disabled"
	case errors.Is(err, lifecycle.ErrEndpointNotReady):
		return http.StatusServiceUnavailable, ledger.OutcomeExhausted,
			"endpoint_not_ready", "the deployment has no ready endpoint"
	case errors.Is(err, lifecycle.ErrInvalidEndpoint):
		return http.StatusServiceUnavailable, ledger.OutcomeConfigError,
			"invalid_runtime_endpoint", "the runtime controller returned an unusable endpoint"
	default:
		return http.StatusServiceUnavailable, ledger.OutcomeUpstreamError,
			"activation_failed", "the model could not be activated"
	}
}

func (h *chatCompletionsHandler) buildUpstreamRequest(
	downstream *http.Request,
	requestID string,
	selection routing.Selection,
	body []byte,
	streaming bool,
) (*http.Request, error) {
	upstreamProtocol := selectionUpstreamProtocol(selection)
	upstreamOperation := upstreamOperationForSelection(
		h.operation,
		selection,
		streaming,
	)
	if h.operation == openAIOperationEmbeddings &&
		upstreamProtocol == config.ProtocolGemini &&
		isGeminiBatchEmbeddingRequest(body) {
		upstreamOperation = geminiOperationBatchEmbed
	}
	if !selection.Deployment.SupportsNativeProtocol(
		selection.Provider,
		upstreamProtocol,
	) {
		return nil, fmt.Errorf(
			"deployment %q does not natively support upstream protocol %q",
			selection.Deployment.Name,
			upstreamProtocol,
		)
	}
	baseURL := selection.Provider.BaseURL
	if upstreamOperation.isBedrock() && baseURL == "" {
		baseURL = bedrockRuntimeBaseURL(selection.Provider.Region)
	}
	endpoint, err := upstreamOperation.upstreamURL(
		baseURL,
		selection.Deployment.Model,
	)
	if err != nil {
		return nil, err
	}
	upstreamRequest, err := http.NewRequestWithContext(
		downstream.Context(),
		http.MethodPost,
		endpoint,
		bytes.NewReader(body),
	)
	if err != nil {
		return nil, err
	}
	resolvedHeaders, err := upstreamheaders.Resolve(
		downstream.Context(),
		selection.Provider.DefaultHeaders,
		selection.Deployment.UpstreamHeaders,
		upstreamheaders.PurposeInference,
		h.credentials,
	)
	if err != nil {
		return nil, err
	}
	upstreamRequest.Header = resolvedHeaders
	upstreamRequest.Header.Set("Content-Type", "application/json")
	if upstreamOperation.isBedrock() && streaming {
		upstreamRequest.Header.Set(
			"Accept",
			"application/vnd.amazon.eventstream",
		)
	} else if streaming {
		upstreamRequest.Header.Set("Accept", "text/event-stream")
	} else {
		upstreamRequest.Header.Set("Accept", "application/json")
	}
	if h.operation == openAIOperationChatCompletions &&
		upstreamOperation == anthropicOperationMessages {
		upstreamRequest.Header.Set(
			"Anthropic-Version",
			anthropicAPIVersion,
		)
		upstreamRequest.Header.Del("Anthropic-Beta")
		upstreamRequest.Header.Del("Anthropic-User-Profile-Id")
	} else {
		if err := upstreamOperation.applyProtocolHeaders(
			upstreamRequest.Header,
			downstream.Header,
		); err != nil {
			return nil, err
		}
	}
	upstreamRequest.Header.Set("User-Agent", version.UserAgent())
	upstreamRequest.Header.Set("X-Request-Id", requestID)
	h.telemetry.Inject(upstreamRequest.Context(), upstreamRequest.Header)
	if selection.Provider.Auth.Type == config.AuthAWSSigV4 {
		ref := selection.Deployment.Credential
		if ref == "" {
			ref = selection.Provider.Auth.Credential
		}
		if h.credentials == nil {
			return nil, fmt.Errorf(
				"AWS workload credential source is required",
			)
		}
		material, err := h.credentials.Resolve(
			downstream.Context(),
			ref,
		)
		if err != nil {
			return nil, fmt.Errorf(
				"resolve AWS workload credential: %w",
				err,
			)
		}
		if err := signAWSRequest(
			upstreamRequest,
			material.Value,
			selection.Provider.Region,
			bedrockAWSService,
			body,
			time.Now(),
		); err != nil {
			return nil, fmt.Errorf("sign Bedrock request: %w", err)
		}
	} else if err := upstreamheaders.ApplyAuthentication(
		downstream.Context(),
		upstreamRequest.Header,
		selection.Provider.Auth,
		selection.Deployment.Credential,
		h.credentials,
	); err != nil {
		return nil, err
	}
	return upstreamRequest, nil
}

func readBoundedBody(w http.ResponseWriter, request *http.Request, limit int64) ([]byte, error) {
	body := http.MaxBytesReader(w, request.Body, limit)
	defer func() { _ = body.Close() }()
	return io.ReadAll(body)
}

func decodeChatRequest(raw []byte) (map[string]json.RawMessage, string, bool, error) {
	if len(bytes.TrimSpace(raw)) == 0 {
		return nil, "", false, fmt.Errorf("request body is required")
	}
	var envelope map[string]json.RawMessage
	if err := json.Unmarshal(raw, &envelope); err != nil {
		return nil, "", false, fmt.Errorf("request body must be valid JSON")
	}
	rawModel, exists := envelope["model"]
	if !exists {
		return nil, "", false, fmt.Errorf("model is required")
	}
	var model string
	if err := json.Unmarshal(rawModel, &model); err != nil || strings.TrimSpace(model) == "" {
		return nil, "", false, fmt.Errorf("model must be a non-empty string")
	}
	if len(model) > maxRequestedModelBytes {
		return nil, "", false, fmt.Errorf("model must not exceed %d bytes", maxRequestedModelBytes)
	}
	var streaming bool
	if rawStream, exists := envelope["stream"]; exists {
		if err := json.Unmarshal(rawStream, &streaming); err != nil {
			return nil, "", false, fmt.Errorf("stream must be a boolean")
		}
	}
	return envelope, model, streaming, nil
}

func rewriteModel(envelope map[string]json.RawMessage, upstreamModel string) ([]byte, error) {
	envelope = cloneJSONEnvelope(envelope)
	rawModel, err := json.Marshal(upstreamModel)
	if err != nil {
		return nil, err
	}
	envelope["model"] = rawModel
	return json.Marshal(envelope)
}

func cloneJSONEnvelope(
	envelope map[string]json.RawMessage,
) map[string]json.RawMessage {
	cloned := make(map[string]json.RawMessage, len(envelope))
	for name, value := range envelope {
		cloned[name] = append(json.RawMessage(nil), value...)
	}
	return cloned
}

func chatCompletionsURL(baseURL string) (string, error) {
	parsed, err := url.Parse(baseURL)
	if err != nil {
		return "", err
	}
	parsed.Path = strings.TrimRight(parsed.Path, "/") + "/chat/completions"
	parsed.RawPath = ""
	parsed.RawQuery = ""
	parsed.Fragment = ""
	return parsed.String(), nil
}

func isJSONContentType(value string) bool {
	if value == "" {
		return true
	}
	mediaType, _, err := mime.ParseMediaType(value)
	return err == nil && mediaType == "application/json"
}

func isSSEStream(value string) bool {
	mediaType, _, err := mime.ParseMediaType(value)
	return err == nil && mediaType == "text/event-stream"
}

func isAWSEventStream(value string) bool {
	mediaType, _, err := mime.ParseMediaType(value)
	return err == nil &&
		mediaType == "application/vnd.amazon.eventstream"
}

func isRetryableStatus(status int) bool {
	switch status {
	case http.StatusRequestTimeout,
		http.StatusUnauthorized,
		http.StatusForbidden,
		http.StatusNotFound,
		http.StatusTooManyRequests,
		http.StatusInternalServerError,
		http.StatusBadGateway,
		http.StatusServiceUnavailable,
		http.StatusGatewayTimeout,
		529:
		return true
	default:
		return false
	}
}

func (o openAIOperation) isRetryableStatus(status int) bool {
	return isRetryableStatus(status) ||
		o.isBedrock() && status == http.StatusFailedDependency
}

func copyResponseHeaders(target, source http.Header) {
	connectionHeaders := make(map[string]struct{})
	for _, value := range source.Values("Connection") {
		for token := range strings.SplitSeq(value, ",") {
			connectionHeaders[strings.ToLower(strings.TrimSpace(token))] = struct{}{}
		}
	}
	for name, values := range source {
		lowerName := strings.ToLower(name)
		if _, excluded := hopByHopHeaders[lowerName]; excluded {
			continue
		}
		if _, excluded := gatewayOwnedResponseHeaders[lowerName]; excluded {
			continue
		}
		if _, excluded := connectionHeaders[lowerName]; excluded {
			continue
		}
		for _, value := range values {
			target.Add(name, value)
		}
	}
}

func removeRepresentationValidators(header http.Header) {
	for _, name := range []string{
		"Content-Digest",
		"Content-MD5",
		"Digest",
		"ETag",
		"Repr-Digest",
	} {
		header.Del(name)
	}
}

func newRequestID() (string, error) {
	var value [16]byte
	if _, err := rand.Read(value[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(value[:]), nil
}

type flushingWriter struct {
	writer http.ResponseWriter
}

func (w flushingWriter) Write(value []byte) (int, error) {
	written, err := w.writer.Write(value)
	if flusher, ok := w.writer.(http.Flusher); ok {
		flusher.Flush()
	}
	return written, err
}
