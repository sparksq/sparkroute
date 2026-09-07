package gateway

import (
	"bytes"
	"context"
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
	upstreamheaders "github.com/sparksq/sparkroute/pkg/headers"
	"github.com/sparksq/sparkroute/pkg/identity"
	"github.com/sparksq/sparkroute/pkg/ledger"
	"github.com/sparksq/sparkroute/pkg/responsesstate"
	"github.com/sparksq/sparkroute/pkg/routing"
	"github.com/sparksq/sparkroute/pkg/telemetry"
	"github.com/sparksq/sparkroute/pkg/version"
)

const defaultFileModel = "default"

type filesHandler struct {
	core *chatCompletionsHandler
}

type fileRoute struct {
	operation      string
	method         string
	allowed        string
	fileID         string
	suffix         string
	listResources  bool
	createResource bool
	deleteResource bool
	content        bool
}

func newFilesHandler(
	snapshot *routing.Snapshot,
	targets *routing.TargetManager,
	retryBudget routing.RetryBudget,
	options DataOptions,
	instrumentation *telemetry.Instrumentation,
) http.Handler {
	return &filesHandler{core: newOpenAIHandler(
		openAIOperationResponses,
		snapshot,
		targets,
		retryBudget,
		options,
		instrumentation,
	)}
}

func (h *filesHandler) ServeHTTP(w http.ResponseWriter, request *http.Request) {
	callerContext := request.Context()
	callerIdentity, _ := identity.FromContext(callerContext)
	route, routeStatus, routeCode, routeMessage := matchFileRoute(request)
	operation := route.operation
	if operation == "" {
		operation = "files_resource"
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
	observation := newRequestObservation(
		h.core.ledger,
		requestID,
		h.core.configRevision,
		operation,
		callerIdentity,
		telemetryRequest,
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
	if route.createResource {
		if !isMultipartFormContentType(request.Header.Get("Content-Type")) {
			fail(
				http.StatusUnsupportedMediaType,
				ledger.OutcomeRejected,
				"unsupported_content_type",
				"Content-Type must be multipart/form-data with a boundary",
			)
			return
		}
		if request.ContentLength > h.core.maxRequestBytes {
			fail(
				http.StatusRequestEntityTooLarge,
				ledger.OutcomeRejected,
				"request_too_large",
				"request body is too large",
			)
			return
		}
		request.Body = http.MaxBytesReader(w, request.Body, h.core.maxRequestBytes)
	}
	resourceStore, supportsResources := h.core.responsesState.(responsesstate.ResourceStore)
	if !supportsResources {
		fail(
			http.StatusServiceUnavailable,
			ledger.OutcomeConfigError,
			"state_affinity_unavailable",
			"provider file routing state storage is not configured",
		)
		return
	}

	ownerScope := responsesCallerScope(callerIdentity)
	if route.listResources {
		fileStore, ok := resourceStore.(responsesstate.FileStore)
		if !ok {
			fail(
				http.StatusServiceUnavailable,
				ledger.OutcomeConfigError,
				"file_index_unavailable",
				"provider file index storage is not configured",
			)
			return
		}
		query, queryErr := parseFileListQuery(request.URL.Query(), ownerScope)
		if queryErr != nil {
			code := "invalid_query"
			message := queryErr.Error()
			if errors.Is(queryErr, responsesstate.ErrInvalidFileCursor) {
				code = "invalid_after"
				message = "after must be a valid file ID"
			}
			fail(
				http.StatusBadRequest,
				ledger.OutcomeRejected,
				code,
				message,
			)
			return
		}
		page, listErr := fileStore.ListFiles(request.Context(), query)
		if listErr != nil {
			if errors.Is(listErr, responsesstate.ErrInvalidFileCursor) {
				fail(
					http.StatusBadRequest,
					ledger.OutcomeRejected,
					"invalid_after",
					"after must identify a file in the caller's filtered list",
				)
				return
			}
			fail(
				http.StatusServiceUnavailable,
				ledger.OutcomeConfigError,
				"file_index_unavailable",
				"provider file index could not be read",
			)
			return
		}
		privacyContext, privacyErr := h.privacyContext(
			request.Context(), defaultFileModel, callerIdentity,
		)
		if privacyErr != nil && privacyContext.policy.FailureMode == config.PIIFailClosed {
			fail(
				http.StatusBadGateway,
				ledger.OutcomeUpstreamError,
				"pii_file_inspection_failed",
				"PII inspection could not safely transform file metadata",
			)
			return
		}
		if privacyErr == nil && privacyContext.enabled() && privacyContext.policy.Files.Metadata {
			if privacyErr = transformFileRecords(request.Context(), page.Records, privacyContext); privacyErr != nil {
				recordPrivacyFailure(h.core.privacy)
				if privacyContext.policy.FailureMode == config.PIIFailClosed {
					fail(
						http.StatusBadGateway,
						ledger.OutcomeUpstreamError,
						"pii_file_inspection_failed",
						"PII inspection could not safely transform file metadata",
					)
					return
				}
			}
		}
		response := fileListEnvelope{
			Object:  "list",
			Data:    page.Records,
			HasMore: page.HasMore,
		}
		if len(page.Records) > 0 {
			response.FirstID = page.Records[0].ID
			response.LastID = page.Records[len(page.Records)-1].ID
		}
		writeJSON(w, http.StatusOK, response)
		observation.completeResponse(proxyResult{
			gatewayStatus: http.StatusOK,
			outcome:       ledger.OutcomeSuccess,
			usage:         missingUsage(),
		})
		return
	}
	if route.createResource {
		if _, ok := resourceStore.(responsesstate.FileStore); !ok {
			fail(
				http.StatusServiceUnavailable,
				ledger.OutcomeConfigError,
				"file_index_unavailable",
				"provider file index storage is not configured",
			)
			return
		}
	}
	requestedModel := defaultFileModel
	requiredCapabilities := []config.Capability{
		config.CapabilityFileInput,
		config.CapabilityFiles,
	}
	planOptions := routing.PlanOptions{
		RequiredCapabilities: requiredCapabilities,
		ProtocolResolver:     nativeProtocolResolver(config.ProtocolOpenAI),
		SingleAttempt:        true,
		Eligibility:          h.core.targets,
		Picker:               h.core.picker,
	}
	var affinity responsesstate.ResourceAffinity
	if route.fileID != "" {
		var found bool
		affinity, found, err = resourceStore.ResolveResource(
			request.Context(),
			responsesstate.ResourceKey{
				Scope:      ownerScope,
				Kind:       responsesstate.ResourceFile,
				ResourceID: route.fileID,
			},
		)
		if err != nil {
			fail(
				http.StatusServiceUnavailable,
				ledger.OutcomeConfigError,
				"state_affinity_unavailable",
				"provider file routing state could not be resolved",
			)
			return
		}
		if !found || affinity.Deleted() {
			fail(
				http.StatusNotFound,
				ledger.OutcomeRejected,
				"state_affinity_not_found",
				"file is unknown or deleted at this gateway",
			)
			return
		}
		requestedModel = affinity.VirtualModel
		planOptions.PinnedDeployment = affinity.Deployment
		planOptions.PinnedProvider = affinity.Provider
	}
	observation.record.RequestedModel = requestedModel
	observation.telemetry.SetRequiredCapabilities(
		capabilityStrings(requiredCapabilities),
	)
	plan, err := h.core.snapshot.BuildPlanFor(requestedModel, planOptions)
	if err != nil {
		switch {
		case errors.Is(err, routing.ErrModelNotFound) && route.createResource:
			fail(
				http.StatusServiceUnavailable,
				ledger.OutcomeConfigError,
				"default_model_not_found",
				"file upload requires an externally addressable default virtual model",
			)
		case errors.Is(err, routing.ErrModelNotFound):
			fail(
				http.StatusServiceUnavailable,
				ledger.OutcomeConfigError,
				"state_affinity_model_unavailable",
				"the file's virtual model is no longer configured",
			)
		case errors.Is(err, routing.ErrUnsupportedCapabilities) && route.fileID != "":
			fail(
				http.StatusServiceUnavailable,
				ledger.OutcomeConfigError,
				"state_affinity_capability_unavailable",
				"the file's target no longer supports file resources and inputs",
			)
		case errors.Is(err, routing.ErrUnsupportedCapabilities):
			fail(
				http.StatusBadRequest,
				ledger.OutcomeRejected,
				"unsupported_feature",
				"no configured default target supports file resources and inputs",
			)
		default:
			code := "default_model_unavailable"
			message := "the default virtual model has no available file target"
			if route.fileID != "" {
				code = "state_affinity_target_unavailable"
				message = "the provider file target is unavailable"
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
	if route.fileID != "" &&
		plan.Candidates[0].Deployment.Model != affinity.UpstreamModel {
		fail(
			http.StatusServiceUnavailable,
			ledger.OutcomeConfigError,
			"state_affinity_target_changed",
			"the provider file target changed upstream model",
		)
		return
	}
	privacyContext, privacyErr := h.privacyContext(
		request.Context(), requestedModel, callerIdentity,
	)
	if privacyErr != nil && privacyContext.policy.FailureMode == config.PIIFailClosed {
		fail(
			http.StatusBadGateway,
			ledger.OutcomeUpstreamError,
			"pii_file_inspection_failed",
			"PII inspection could not safely prepare the file request",
		)
		return
	}
	if privacyErr != nil {
		privacyContext = filePIIContext{}
	}
	var preparedUpload []byte
	preparedContentType := ""
	if route.createResource && privacyContext.enabled() {
		original, readErr := io.ReadAll(request.Body)
		closeErr := request.Body.Close()
		if readErr == nil {
			readErr = closeErr
		}
		if readErr != nil {
			var maximumError *http.MaxBytesError
			if errors.As(readErr, &maximumError) {
				fail(
					http.StatusRequestEntityTooLarge,
					ledger.OutcomeRejected,
					"request_too_large",
					"request body is too large",
				)
			} else {
				fail(
					http.StatusBadRequest,
					ledger.OutcomeRejected,
					"invalid_request_body",
					"file upload body could not be read",
				)
			}
			return
		}
		preparedUpload, preparedContentType, privacyErr = preparePIIFileUpload(
			request.Context(), original, request.Header.Get("Content-Type"),
			privacyContext, h.core.maxRequestBytes,
		)
		if privacyErr != nil {
			recordPrivacyFailure(h.core.privacy)
			if privacyContext.policy.FailureMode == config.PIIFailClosed {
				fail(
					http.StatusBadRequest,
					ledger.OutcomeRejected,
					"pii_file_inspection_failed",
					"PII inspection could not safely transform the file upload",
				)
				return
			}
			preparedUpload = original
			preparedContentType = request.Header.Get("Content-Type")
		}
	}
	observation.resolve(plan, false)
	selection := plan.Candidates[0]
	lease, _ := h.core.targets.Acquire(selection.Deployment.Name)
	if lease == nil {
		code := "default_model_unavailable"
		message := "the default file target has no capacity available"
		if route.fileID != "" {
			code = "state_affinity_target_unavailable"
			message = "the provider file target has no capacity available"
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
		preparedUpload,
		preparedContentType,
	)
	if err != nil {
		h.recordFileFailure(
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
		h.recordFileFailure(
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
	var result proxyResult
	if route.content {
		result = h.proxyContentResponse(
			w,
			request,
			response,
			requestID,
			attempt,
			privacyContext,
		)
	} else {
		result = h.proxyJSONResponse(
			w,
			request,
			response,
			requestID,
			attempt,
			selection,
			route,
			ownerScope,
			resourceStore,
			privacyContext,
		)
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
}

func matchFileRoute(
	request *http.Request,
) (fileRoute, int, string, string) {
	if _, err := url.ParseQuery(request.URL.RawQuery); err != nil {
		return fileRoute{}, http.StatusBadRequest, "invalid_query", "query parameters are invalid"
	}
	if request.URL.Path == "/v1/files" {
		switch request.Method {
		case http.MethodPost:
			return fileRoute{
				operation:      "files_upload",
				method:         http.MethodPost,
				allowed:        http.MethodGet + ", " + http.MethodPost,
				createResource: true,
			}, 0, "", ""
		case http.MethodGet:
			return fileRoute{
				operation:     "files_list",
				method:        http.MethodGet,
				allowed:       http.MethodGet + ", " + http.MethodPost,
				listResources: true,
			}, 0, "", ""
		default:
			return fileRoute{
				allowed: http.MethodGet + ", " + http.MethodPost,
			}, http.StatusMethodNotAllowed, "method_not_allowed", "method not allowed"
		}
	}
	const prefix = "/v1/files/"
	if !strings.HasPrefix(request.URL.Path, prefix) {
		return fileRoute{}, http.StatusNotFound, "route_not_found", "route not found"
	}
	parts := strings.Split(strings.TrimPrefix(request.URL.Path, prefix), "/")
	if len(parts) < 1 || len(parts) > 2 || parts[0] == "" {
		return fileRoute{}, http.StatusNotFound, "route_not_found", "route not found"
	}
	fileID := parts[0]
	if fileID == "." ||
		fileID == ".." ||
		strings.Contains(fileID, "\\") ||
		responsesstate.ValidateResourceID(fileID) != nil {
		return fileRoute{}, http.StatusBadRequest, "invalid_file_id", "file ID is invalid"
	}
	if len(parts) == 1 {
		switch request.Method {
		case http.MethodGet:
			return fileRoute{
				operation: "files_retrieve",
				method:    http.MethodGet,
				allowed:   http.MethodGet + ", " + http.MethodDelete,
				fileID:    fileID,
			}, 0, "", ""
		case http.MethodDelete:
			return fileRoute{
				operation:      "files_delete",
				method:         http.MethodDelete,
				allowed:        http.MethodGet + ", " + http.MethodDelete,
				fileID:         fileID,
				deleteResource: true,
			}, 0, "", ""
		default:
			return fileRoute{
				allowed: http.MethodGet + ", " + http.MethodDelete,
			}, http.StatusMethodNotAllowed, "method_not_allowed", "method not allowed"
		}
	}
	if parts[1] != "content" {
		return fileRoute{}, http.StatusNotFound, "route_not_found", "route not found"
	}
	route := fileRoute{
		operation: "files_content",
		method:    http.MethodGet,
		allowed:   http.MethodGet,
		fileID:    fileID,
		suffix:    "/content",
		content:   true,
	}
	if request.Method != route.method {
		return route, http.StatusMethodNotAllowed, "method_not_allowed", "method not allowed"
	}
	return route, 0, "", ""
}

func isMultipartFormContentType(value string) bool {
	mediaType, parameters, err := mime.ParseMediaType(value)
	return err == nil &&
		mediaType == "multipart/form-data" &&
		parameters["boundary"] != ""
}

func (h *filesHandler) buildUpstreamRequest(
	downstream *http.Request,
	requestID string,
	selection routing.Selection,
	route fileRoute,
	preparedUpload []byte,
	preparedContentType string,
) (*http.Request, error) {
	if selectionUpstreamProtocol(selection) != config.ProtocolOpenAI ||
		!selection.Deployment.SupportsNativeProtocol(
			selection.Provider,
			config.ProtocolOpenAI,
		) {
		return nil, fmt.Errorf(
			"deployment %q does not natively support OpenAI Files",
			selection.Deployment.Name,
		)
	}
	endpoint, err := fileURL(
		selection.Provider.BaseURL,
		route.fileID,
		route.suffix,
		downstream.URL.RawQuery,
	)
	if err != nil {
		return nil, err
	}
	var body io.Reader
	if route.createResource {
		if preparedUpload != nil {
			body = bytes.NewReader(preparedUpload)
		} else {
			body = downstream.Body
		}
	}
	upstreamRequest, err := http.NewRequestWithContext(
		downstream.Context(),
		route.method,
		endpoint,
		body,
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
	if route.content {
		upstreamRequest.Header.Set("Accept", "*/*")
	} else {
		upstreamRequest.Header.Set("Accept", "application/json")
	}
	if route.createResource {
		contentType := downstream.Header.Get("Content-Type")
		contentLength := downstream.ContentLength
		if preparedUpload != nil {
			contentType = preparedContentType
			contentLength = int64(len(preparedUpload))
		}
		upstreamRequest.Header.Set(
			"Content-Type",
			contentType,
		)
		upstreamRequest.ContentLength = contentLength
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

func fileURL(
	baseURL string,
	fileID string,
	suffix string,
	rawQuery string,
) (string, error) {
	parsed, err := url.Parse(baseURL)
	if err != nil {
		return "", err
	}
	parsed.Path = strings.TrimRight(parsed.Path, "/") + "/files"
	if fileID != "" {
		parsed.Path += "/" + fileID + suffix
	}
	parsed.RawPath = ""
	parsed.RawQuery = rawQuery
	parsed.Fragment = ""
	return parsed.String(), nil
}

func (h *filesHandler) proxyJSONResponse(
	w http.ResponseWriter,
	request *http.Request,
	response *http.Response,
	requestID string,
	attempt int,
	selection routing.Selection,
	route fileRoute,
	ownerScope string,
	resourceStore responsesstate.ResourceStore,
	privacyContext filePIIContext,
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
				"upstream returned an invalid Files response",
			)
			result.gatewayStatus = http.StatusBadGateway
			result.outcome = ledger.OutcomeUpstreamError
			result.failureClass = "upstream_invalid_response"
			return result
		}
		if route.createResource {
			boundAt := time.Now().UTC()
			fileRecord, stateErr := fileRecordFromPayload(body, ownerScope, boundAt)
			if stateErr != nil {
				h.core.setAttemptCount(w.Header(), attempt)
				writeOpenAIError(
					w,
					http.StatusBadGateway,
					"upstream_response_error",
					"upstream returned a file without a valid ID",
				)
				result.gatewayStatus = http.StatusBadGateway
				result.outcome = ledger.OutcomeUpstreamError
				result.failureClass = "file_state_invalid"
				return result
			}
			fileStore, ok := resourceStore.(responsesstate.FileStore)
			if !ok {
				stateErr = fmt.Errorf("file index storage is unavailable")
			} else {
				stateErr = fileStore.BindFile(
					request.Context(),
					responsesstate.ResourceAffinity{
						ResourceKey: responsesstate.ResourceKey{
							Scope:      ownerScope,
							Kind:       responsesstate.ResourceFile,
							ResourceID: fileRecord.ID,
						},
						VirtualModel:  selection.VirtualModel,
						Provider:      selection.Provider.Name,
						Deployment:    selection.Deployment.Name,
						UpstreamModel: selection.Deployment.Model,
						BoundAt:       boundAt,
					},
					fileRecord,
				)
			}
			if stateErr != nil {
				status := http.StatusServiceUnavailable
				code := "state_affinity_unavailable"
				message := "provider file routing state could not be persisted"
				result.failureClass = "file_state_bind_failed"
				if errors.Is(stateErr, responsesstate.ErrConflict) {
					status = http.StatusBadGateway
					code = "upstream_response_error"
					message = "upstream returned a file ID with conflicting affinity"
					result.failureClass = "file_state_conflict"
				}
				h.core.setAttemptCount(w.Header(), attempt)
				writeOpenAIError(w, status, code, message)
				result.gatewayStatus = status
				result.outcome = ledger.OutcomeUpstreamError
				return result
			}
		}
		if route.deleteResource {
			deletedAt := time.Now().UTC()
			stateErr := resourceStore.TombstoneResource(
				request.Context(),
				responsesstate.ResourceKey{
					Scope:      ownerScope,
					Kind:       responsesstate.ResourceFile,
					ResourceID: route.fileID,
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
					"deleted file routing state could not be retained",
				)
				result.gatewayStatus = http.StatusServiceUnavailable
				result.outcome = ledger.OutcomeUpstreamError
				result.failureClass = "file_state_tombstone_failed"
				return result
			}
		}
		if privacyContext.enabled() && privacyContext.policy.Files.Metadata {
			transformed, transformErr := transformFileMetadataResponse(
				request.Context(), body, privacyContext,
			)
			if transformErr != nil {
				recordPrivacyFailure(h.core.privacy)
				if privacyContext.policy.FailureMode == config.PIIFailClosed {
					h.core.setAttemptCount(w.Header(), attempt)
					writeOpenAIError(
						w,
						http.StatusBadGateway,
						"pii_file_inspection_failed",
						"PII inspection could not safely transform file metadata",
					)
					result.gatewayStatus = http.StatusBadGateway
					result.outcome = ledger.OutcomeUpstreamError
					result.failureClass = "pii_file_inspection_failed"
					return result
				}
			} else {
				body = transformed
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

func (h *filesHandler) proxyContentResponse(
	w http.ResponseWriter,
	request *http.Request,
	response *http.Response,
	requestID string,
	attempt int,
	privacyContext filePIIContext,
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
	if success && privacyContext.enabled() &&
		privacyContext.policy.Files.Text != config.PIIFileTextDisabled {
		typedText := inspectableTextUpload(response.Header.Get("Content-Type"), "")
		required := privacyContext.policy.Files.Text == config.PIIFileTextRequired
		if !typedText && required {
			recordPrivacyFailure(h.core.privacy)
			h.core.setAttemptCount(w.Header(), attempt)
			writeOpenAIError(
				w,
				http.StatusBadGateway,
				"pii_file_inspection_failed",
				"provider content is not explicitly typed as inspectable text",
			)
			result.gatewayStatus = http.StatusBadGateway
			result.outcome = ledger.OutcomeUpstreamError
			result.failureClass = "pii_file_inspection_failed"
			return result
		}
		if typedText {
			body, readErr := readBoundedResponse(response.Body, h.core.maxResponseBytes)
			if readErr != nil {
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
			var transformed []byte
			var transformErr error
			if int64(len(body)) > privacyContext.policy.Files.MaxTextBytes {
				transformErr = fmt.Errorf("text file exceeds the configured PII inspection limit")
			} else {
				transformed, transformErr = transformFileTextResponse(
					request.Context(), body, privacyContext,
				)
			}
			if transformErr != nil {
				recordPrivacyFailure(h.core.privacy)
				if privacyContext.policy.FailureMode == config.PIIFailClosed {
					h.core.setAttemptCount(w.Header(), attempt)
					writeOpenAIError(
						w,
						http.StatusBadGateway,
						"pii_file_inspection_failed",
						"PII inspection could not safely transform provider file content",
					)
					result.gatewayStatus = http.StatusBadGateway
					result.outcome = ledger.OutcomeUpstreamError
					result.failureClass = "pii_file_inspection_failed"
					return result
				}
			}
			if transformErr == nil {
				body = transformed
			}
			copyResponseHeaders(w.Header(), response.Header)
			w.Header().Set(h.core.requestIDHeader, requestID)
			h.core.setAttemptCount(w.Header(), attempt)
			w.Header().Set("Content-Length", strconv.Itoa(len(body)))
			w.WriteHeader(response.StatusCode)
			if _, err := w.Write(body); err != nil {
				result.outcome, result.failureClass = streamFailure(request.Context().Err(), false)
			}
			return result
		}
	}
	copyResponseHeaders(w.Header(), response.Header)
	w.Header().Set(h.core.requestIDHeader, requestID)
	h.core.setAttemptCount(w.Header(), attempt)
	w.WriteHeader(response.StatusCode)
	if _, err := io.Copy(w, response.Body); err != nil {
		result.outcome, result.failureClass = streamFailure(request.Context().Err(), false)
	}
	return result
}

func fileRecordFromPayload(
	payload []byte,
	scope string,
	boundAt time.Time,
) (responsesstate.FileRecord, error) {
	var envelope struct {
		ID        string `json:"id"`
		Object    string `json:"object"`
		Bytes     *int64 `json:"bytes"`
		CreatedAt *int64 `json:"created_at"`
		ExpiresAt *int64 `json:"expires_at"`
		Filename  string `json:"filename"`
		Purpose   string `json:"purpose"`
	}
	if err := json.Unmarshal(payload, &envelope); err != nil {
		return responsesstate.FileRecord{}, fmt.Errorf("file response must be an object")
	}
	if envelope.Object != "" && envelope.Object != "file" {
		return responsesstate.FileRecord{}, fmt.Errorf("file response object is invalid")
	}
	record := responsesstate.FileRecord{
		Scope: scope, ID: envelope.ID, Object: "file",
		CreatedAt: boundAt.Unix(), Filename: envelope.Filename, Purpose: envelope.Purpose,
	}
	if envelope.Bytes != nil {
		record.Bytes = *envelope.Bytes
	}
	if envelope.CreatedAt != nil {
		record.CreatedAt = *envelope.CreatedAt
	}
	if envelope.ExpiresAt != nil {
		record.ExpiresAt = *envelope.ExpiresAt
	}
	if err := record.Validate(); err != nil {
		return responsesstate.FileRecord{}, err
	}
	return record, nil
}

type fileListEnvelope struct {
	Object  string                      `json:"object"`
	Data    []responsesstate.FileRecord `json:"data"`
	FirstID string                      `json:"first_id,omitempty"`
	LastID  string                      `json:"last_id,omitempty"`
	HasMore bool                        `json:"has_more"`
}

func parseFileListQuery(
	values url.Values,
	scope string,
) (responsesstate.FileQuery, error) {
	for name, items := range values {
		switch name {
		case "purpose", "limit", "order", "after":
		default:
			return responsesstate.FileQuery{}, fmt.Errorf("unsupported query parameter %q", name)
		}
		if len(items) != 1 {
			return responsesstate.FileQuery{}, fmt.Errorf("query parameter %q must appear once", name)
		}
	}
	query := responsesstate.FileQuery{
		Scope:   scope,
		Purpose: values.Get("purpose"),
		Order:   responsesstate.FileOrder(values.Get("order")),
		After:   values.Get("after"),
	}
	if rawLimit := values.Get("limit"); rawLimit != "" {
		limit, err := strconv.Atoi(rawLimit)
		if err != nil {
			return responsesstate.FileQuery{}, fmt.Errorf("limit must be an integer")
		}
		query.Limit = limit
	}
	query, err := responsesstate.NormalizeFileQuery(query)
	if err != nil {
		return responsesstate.FileQuery{}, err
	}
	return query, nil
}

func (h *filesHandler) recordFileFailure(
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
	message := "the provider file request failed"
	var maxBytesError *http.MaxBytesError
	if errors.As(failure, &maxBytesError) {
		outcome = ledger.OutcomeRejected
		failureClass = "request_too_large"
		targetResult = routing.TargetNeutral
		status = http.StatusRequestEntityTooLarge
		code = "request_too_large"
		message = "request body is too large"
	} else if callerContext.Err() != nil {
		outcome = ledger.OutcomeClientCancelled
		failureClass = "client_cancelled"
		targetResult = routing.TargetNeutral
		status = 0
	} else if errors.Is(attemptContext.Err(), context.DeadlineExceeded) {
		failureClass = "upstream_timeout"
		status = http.StatusGatewayTimeout
		code = "upstream_timeout"
		message = "the upstream request exceeded its deadline"
	} else {
		var urlError *url.Error
		if !errors.As(failure, &urlError) {
			outcome = ledger.OutcomeConfigError
			failureClass = "upstream_configuration_error"
			targetResult = routing.TargetNeutral
			status = http.StatusInternalServerError
			code = "upstream_configuration_error"
			message = "could not prepare the provider file request"
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
	observation.fail(status, outcome, failureClass)
	writeOpenAIError(w, status, code, message)
}
