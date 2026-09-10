// SPDX-FileCopyrightText: 2026 Scitrera LLC
// SPDX-FileCopyrightText: 2026 Fox Engine Ltd.
// SPDX-License-Identifier: AGPL-3.0-only

package gateway

import (
	"context"
	"net/http"
	"time"

	"github.com/sparksq/sparkroute/pkg/config"
	"github.com/sparksq/sparkroute/pkg/identity"
	"github.com/sparksq/sparkroute/pkg/ledger"
	"github.com/sparksq/sparkroute/pkg/lifecycle"
	"github.com/sparksq/sparkroute/pkg/modelrouter"
	"github.com/sparksq/sparkroute/pkg/routing"
	"github.com/sparksq/sparkroute/pkg/savedtrace"
	"github.com/sparksq/sparkroute/pkg/telemetry"
)

type requestObservation struct {
	recorder                 ledger.Recorder
	record                   ledger.RequestRecord
	telemetry                *telemetry.Request
	savedTraceRecorder       savedtrace.Recorder
	savedTraceResponse       *savedTraceResponseWriter
	savedTraceRequest        savedtrace.Payload
	savedTraceMetadata       map[string]string
	savedTraceConversationID string
	maxSavedTraceBytes       int64
}

func newRequestObservation(
	recorder ledger.Recorder,
	requestID string,
	configRevision string,
	operation string,
	caller identity.Identity,
	telemetryRequest *telemetry.Request,
) *requestObservation {
	return &requestObservation{
		recorder:  recorder,
		telemetry: telemetryRequest,
		record: ledger.RequestRecord{
			RequestID:        requestID,
			StartedAt:        time.Now(),
			PrincipalID:      caller.Principal.ID,
			PrincipalType:    caller.Principal.Type,
			PrincipalSubject: caller.Principal.Subject,
			PrincipalRoles: append(
				[]string(nil),
				caller.Principal.Roles...,
			),
			TenantID:       caller.Principal.Tenant,
			Attribution:    cloneAttribution(caller.Attribution),
			Protocol:       "openai",
			Operation:      operation,
			ConfigRevision: configRevision,
			Outcome:        ledger.OutcomeRejected,
			Usage: ledger.TokenUsage{
				Completeness: ledger.UsageMissing,
			},
		},
	}
}

func cloneAttribution(source identity.Attribution) map[string]string {
	if source == nil {
		return nil
	}
	result := make(map[string]string, len(source))
	for key, value := range source {
		result[key] = value
	}
	return result
}

func (o *requestObservation) finish() {
	o.record.CompletedAt = time.Now()
	o.record.Latency = o.record.CompletedAt.Sub(o.record.StartedAt)
	o.recorder.Record(ledger.NewRequestRecord(o.record))
	o.telemetry.End(o.record)
	o.finishSavedTrace()
}

func (o *requestObservation) enableSavedTrace(
	recorder savedtrace.Recorder,
	response *savedTraceResponseWriter,
	maxBodyBytes int64,
	requestContentType string,
) {
	if recorder == nil || response == nil {
		return
	}
	o.savedTraceRecorder = recorder
	o.savedTraceResponse = response
	o.maxSavedTraceBytes = maxBodyBytes
	o.savedTraceRequest.ContentType = savedTraceContentType(requestContentType)
	o.savedTraceMetadata = cloneAttribution(o.record.Attribution)
}

func (o *requestObservation) captureSavedTraceRequest(body []byte) {
	if o.savedTraceRecorder == nil {
		return
	}
	o.savedTraceRequest.Body, o.savedTraceRequest.Truncated = boundedTraceBody(
		body,
		o.maxSavedTraceBytes,
	)
}

func (o *requestObservation) disableSavedTrace() {
	if o == nil {
		return
	}
	o.savedTraceRecorder = nil
}

func (o *requestObservation) setSavedTraceMetadata(key, value string) {
	if o == nil || o.savedTraceRecorder == nil || key == "" || value == "" {
		return
	}
	if o.savedTraceMetadata == nil {
		o.savedTraceMetadata = make(map[string]string)
	}
	o.savedTraceMetadata[key] = value
}

func (o *requestObservation) setMMProjection(trace *modelrouter.MMProjectionTrace) {
	if o == nil || trace == nil {
		return
	}
	o.telemetry.SetMMProjection(
		trace.State,
		trace.ProviderID,
		trace.FailureMode,
		trace.HTTPStatus,
		trace.DurationMS,
		trace.MediaCount,
		trace.CacheHit,
	)
	if o.savedTraceRecorder == nil {
		return
	}
	if o.savedTraceMetadata == nil {
		o.savedTraceMetadata = make(map[string]string)
	}
	o.savedTraceMetadata["sparkroute.mm_projection.state"] = trace.State
	o.savedTraceMetadata["sparkroute.mm_projection.provider"] = trace.ProviderID
	o.savedTraceMetadata["sparkroute.mm_projection.failure_mode"] = trace.FailureMode
	if trace.AnalyzerModel != "" {
		o.savedTraceMetadata["sparkroute.mm_projection.analyzer_model"] = trace.AnalyzerModel
	}
}

func (o *requestObservation) finishSavedTrace() {
	if o.savedTraceRecorder == nil || o.savedTraceResponse == nil {
		return
	}
	response := o.savedTraceResponse.payload()
	conversationID, responseID, parentResponseID := savedTraceCorrelation(
		o.savedTraceRequest, response,
	)
	if o.savedTraceConversationID != "" {
		conversationID = o.savedTraceConversationID
	}
	if conversationID == "" {
		conversationID = o.savedTraceMetadata[identity.AttributeThreadID]
	}
	o.savedTraceRecorder.Record(savedtrace.Record{
		Version:                savedtrace.SchemaVersion,
		RequestID:              o.record.RequestID,
		StartedAt:              o.record.StartedAt,
		CompletedAt:            o.record.CompletedAt,
		TenantID:               o.record.TenantID,
		PrincipalID:            o.record.PrincipalID,
		PrincipalType:          o.record.PrincipalType,
		PrincipalSubject:       o.record.PrincipalSubject,
		ConversationID:         conversationID,
		ResponseID:             responseID,
		ParentResponseID:       parentResponseID,
		SessionID:              o.savedTraceMetadata[identity.AttributeSession],
		Metadata:               o.savedTraceMetadata,
		Protocol:               o.record.Protocol,
		Operation:              o.record.Operation,
		Stream:                 o.record.Stream,
		RequestedModel:         o.record.RequestedModel,
		VirtualModel:           o.record.VirtualModel,
		ResponsePresentedModel: o.record.ResponsePresentedModel,
		ConfigRevision:         o.record.ConfigRevision,
		FinalProvider:          o.record.FinalProvider,
		FinalDeployment:        o.record.FinalDeployment,
		FinalUpstreamModel:     o.record.FinalUpstreamModel,
		AttemptCount:           o.record.AttemptCount,
		HTTPStatus:             o.record.HTTPStatus,
		Outcome:                string(o.record.Outcome),
		FailureClass:           o.record.FailureClass,
		CaptureOutcome:         savedTraceCaptureOutcome(o.record.Stream, o.record.Outcome, o.savedTraceRequest, response),
		Request:                o.savedTraceRequest,
		Response:               response,
	})
}

func (o *requestObservation) setSavedTraceConversationID(value string) {
	o.savedTraceConversationID = value
}

func (o *requestObservation) fail(status int, outcome ledger.Outcome, failureClass string) {
	o.record.HTTPStatus = status
	o.record.Outcome = outcome
	o.record.FailureClass = failureClass
}

func (o *requestObservation) resolve(plan routing.Plan, streaming bool) {
	o.record.RequestedModel = plan.RequestedModel
	o.record.VirtualModel = plan.VirtualModel
	o.record.Stream = streaming
	o.telemetry.SetStream(streaming)
	o.telemetry.SetRequiredCapabilities(capabilityStrings(plan.RequiredCapabilities))
}

func (o *requestObservation) selectFinal(
	selection routing.Selection,
	attempts int,
	firstByte time.Time,
) {
	o.record.FinalProvider = selection.Provider.Name
	o.record.FinalDeployment = selection.Deployment.Name
	o.record.FinalUpstreamModel = selection.Deployment.Model
	o.record.AttemptCount = attempts
	if name, rewritten := presentedModelName(selection); rewritten {
		o.record.ResponsePresentedModel = name
	} else {
		o.record.ResponsePresentedModel = selection.Deployment.Model
	}
	duration := firstByte.Sub(o.record.StartedAt)
	o.record.TimeToFirstByte = &duration
}

func (o *requestObservation) completeResponse(result proxyResult) {
	o.record.HTTPStatus = result.gatewayStatus
	o.record.Outcome = result.outcome
	o.record.FailureClass = result.failureClass
	o.record.Usage = result.usage
}

func (o *requestObservation) startAttempt(
	selection routing.Selection,
	attempt int,
	lease *routing.TargetLease,
	required []config.Capability,
	retryReason string,
	retryDelay time.Duration,
) (context.Context, *telemetry.Attempt) {
	circuitState := routing.CircuitClosed
	halfOpenProbe := false
	activeAtAdmission := 0
	if lease != nil {
		circuitState = lease.CircuitState
		halfOpenProbe = lease.HalfOpenProbe
		activeAtAdmission = lease.ActiveRequests
	}
	return o.telemetry.StartAttempt(telemetry.AttemptStart{
		VirtualModel:         o.record.VirtualModel,
		Provider:             selection.Provider.Name,
		Deployment:           selection.Deployment.Name,
		UpstreamModel:        selection.Deployment.Model,
		UpstreamProtocol:     string(selectionUpstreamProtocol(selection)),
		NativeProtocol:       selection.NativeProtocol,
		RequiredCapabilities: capabilityStrings(required),
		CircuitState:         string(circuitState),
		HalfOpenProbe:        halfOpenProbe,
		ActiveAtAdmission:    activeAtAdmission,
		RetryReason:          retryReason,
		RetryDelay:           retryDelay,
		Attempt:              attempt,
		PoolPriority:         selection.PoolPriority,
	})
}

func recordAttempt(
	recorder ledger.Recorder,
	telemetryAttempt *telemetry.Attempt,
	targets *routing.TargetManager,
	lease *routing.TargetLease,
	targetResult routing.TargetResult,
	requestID string,
	attempt int,
	started time.Time,
	firstByte *time.Time,
	selection routing.Selection,
	status int,
	outcome ledger.Outcome,
	failureClass string,
	retried bool,
	upstreamRequestID string,
	usage ledger.TokenUsage,
	runtime ...runtimeAttemptLease,
) {
	completed := time.Now()
	record := ledger.AttemptRecord{
		RequestID:         requestID,
		Attempt:           attempt,
		StartedAt:         started,
		FirstByteAt:       firstByte,
		CompletedAt:       completed,
		Provider:          selection.Provider.Name,
		Deployment:        selection.Deployment.Name,
		UpstreamModel:     selection.Deployment.Model,
		PoolPriority:      selection.PoolPriority,
		HTTPStatus:        status,
		UpstreamRequestID: upstreamRequestID,
		Outcome:           outcome,
		FailureClass:      failureClass,
		Retried:           retried,
		Latency:           completed.Sub(started),
		Usage:             usage,
	}
	recorder.Record(ledger.NewAttemptRecord(record))
	telemetryAttempt.End(record)
	targets.CompleteWithFailureClass(lease, targetResult, failureClass)
	requestOutcome := lifecycle.RequestOutcome{Success: outcome == ledger.OutcomeSuccess, Error: failureClass}
	if lease != nil {
		requestOutcome.HealthObserved = lease.HealthApplied
		requestOutcome.CircuitOpened = lease.CircuitOpened
		requestOutcome.HalfOpenProbe = lease.HalfOpenProbe
		requestOutcome.BackendFailure = targetResult == routing.TargetFailure && restartableFailure(status, failureClass)
	}
	for _, item := range runtime {
		if item.coordinator == nil || item.lease == nil {
			continue
		}
		releaseCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		_ = item.coordinator.Release(releaseCtx, item.lease, requestOutcome)
		cancel()
	}
}

type runtimeAttemptLease struct {
	coordinator *lifecycle.AdmissionCoordinator
	lease       *lifecycle.AdmissionLease
}

func missingUsage() ledger.TokenUsage {
	return ledger.TokenUsage{Completeness: ledger.UsageMissing}
}

func upstreamRequestID(header http.Header) string {
	for _, name := range []string{
		"X-Request-Id",
		"Request-Id",
		"X-Upstream-Request-Id",
		"X-Amzn-Requestid",
	} {
		if value := header.Get(name); value != "" {
			if len(value) > 512 {
				value = value[:512]
			}
			return value
		}
	}
	return ""
}

// An open circuit can also represent invalid credentials or overload. Those
// conditions must not turn an otherwise healthy process into a restart loop.
func restartableFailure(status int, failureClass string) bool {
	if status == 429 || status == 503 || status >= 400 && status < 500 {
		return false
	}
	if status >= 500 && status <= 599 {
		return true
	}
	switch failureClass {
	case "upstream_transport_error", "upstream_transport", "per_try_timeout", "stream_idle_timeout", "stream_read_error", "upstream_read_error", "response_stream_error":
		return true
	}
	return false
}
