// SPDX-FileCopyrightText: 2026 Scitrera LLC
// SPDX-FileCopyrightText: 2026 Fox Engine Ltd.
// SPDX-License-Identifier: AGPL-3.0-only

// Package ledger defines storage-neutral request, attempt, and runtime-event persistence.
package ledger

import (
	"context"
	"encoding/json"
	"fmt"
	"time"
)

const (
	MaxRawUsageBytes      = 32 << 10
	MaxAttributionBytes   = 16 << 10
	MaxRuntimeReasonBytes = 64
)

type Kind string

const (
	KindRequest      Kind = "request"
	KindAttempt      Kind = "attempt"
	KindRuntimeEvent Kind = "runtime_event"
)

type Outcome string

const (
	OutcomeSuccess         Outcome = "success"
	OutcomeRejected        Outcome = "rejected"
	OutcomeUpstreamError   Outcome = "upstream_error"
	OutcomeTransportError  Outcome = "transport_error"
	OutcomeConfigError     Outcome = "configuration_error"
	OutcomeClientCancelled Outcome = "client_cancelled"
	OutcomeStreamError     Outcome = "stream_error"
	OutcomeExhausted       Outcome = "attempts_exhausted"
)

type UsageCompleteness string

const (
	UsageMissing  UsageCompleteness = "missing"
	UsagePartial  UsageCompleteness = "partial"
	UsageComplete UsageCompleteness = "complete"
)

// TokenUsage uses pointers so a provider-reported zero remains distinct from a
// category the provider did not report.
type TokenUsage struct {
	InputTokens              *int64            `json:"input_tokens,omitempty"`
	OutputTokens             *int64            `json:"output_tokens,omitempty"`
	TotalTokens              *int64            `json:"total_tokens,omitempty"`
	CachedInputTokens        *int64            `json:"cached_input_tokens,omitempty"`
	CacheCreationTokens      *int64            `json:"cache_creation_tokens,omitempty"`
	ReasoningTokens          *int64            `json:"reasoning_tokens,omitempty"`
	ToolUsePromptTokens      *int64            `json:"tool_use_prompt_tokens,omitempty"`
	AcceptedPredictionTokens *int64            `json:"accepted_prediction_tokens,omitempty"`
	RejectedPredictionTokens *int64            `json:"rejected_prediction_tokens,omitempty"`
	ProviderComponents       map[string]int64  `json:"provider_components,omitempty"`
	Raw                      json.RawMessage   `json:"raw,omitempty"`
	Completeness             UsageCompleteness `json:"completeness"`
	NormalizationVersion     string            `json:"normalization_version,omitempty"`
}

type RequestRecord struct {
	RequestID              string            `json:"request_id"`
	StartedAt              time.Time         `json:"started_at"`
	CompletedAt            time.Time         `json:"completed_at"`
	PrincipalID            string            `json:"principal_id,omitempty"`
	PrincipalType          string            `json:"principal_type,omitempty"`
	PrincipalSubject       string            `json:"principal_subject,omitempty"`
	PrincipalRoles         []string          `json:"principal_roles,omitempty"`
	TenantID               string            `json:"tenant_id,omitempty"`
	Attribution            map[string]string `json:"attribution,omitempty"`
	Protocol               string            `json:"protocol"`
	Operation              string            `json:"operation"`
	Stream                 bool              `json:"stream"`
	RequestedModel         string            `json:"requested_model,omitempty"`
	VirtualModel           string            `json:"virtual_model,omitempty"`
	ResponsePresentedModel string            `json:"response_presented_model,omitempty"`
	ConfigRevision         string            `json:"config_revision,omitempty"`
	FinalProvider          string            `json:"final_provider,omitempty"`
	FinalDeployment        string            `json:"final_deployment,omitempty"`
	FinalUpstreamModel     string            `json:"final_upstream_model,omitempty"`
	AttemptCount           int               `json:"attempt_count"`
	HTTPStatus             int               `json:"http_status,omitempty"`
	Outcome                Outcome           `json:"outcome"`
	FailureClass           string            `json:"failure_class,omitempty"`
	Latency                time.Duration     `json:"latency"`
	TimeToFirstByte        *time.Duration    `json:"time_to_first_byte,omitempty"`
	Usage                  TokenUsage        `json:"usage"`
}

type AttemptRecord struct {
	RequestID         string        `json:"request_id"`
	Attempt           int           `json:"attempt"`
	StartedAt         time.Time     `json:"started_at"`
	FirstByteAt       *time.Time    `json:"first_byte_at,omitempty"`
	CompletedAt       time.Time     `json:"completed_at"`
	Provider          string        `json:"provider"`
	Deployment        string        `json:"deployment"`
	UpstreamModel     string        `json:"upstream_model"`
	PoolPriority      int           `json:"pool_priority"`
	HTTPStatus        int           `json:"http_status,omitempty"`
	UpstreamRequestID string        `json:"upstream_request_id,omitempty"`
	Outcome           Outcome       `json:"outcome"`
	FailureClass      string        `json:"failure_class,omitempty"`
	Retried           bool          `json:"retried"`
	Latency           time.Duration `json:"latency"`
	Usage             TokenUsage    `json:"usage"`
}

type RuntimeOutcome string

const (
	RuntimeOutcomeSuccess   RuntimeOutcome = "success"
	RuntimeOutcomeFailure   RuntimeOutcome = "failure"
	RuntimeOutcomeTimeout   RuntimeOutcome = "timeout"
	RuntimeOutcomeCancelled RuntimeOutcome = "cancelled"
	RuntimeOutcomeRejected  RuntimeOutcome = "rejected"
)

// RuntimeEventRecord is content-free lifecycle history. Reason is a bounded,
// low-cardinality classification rather than a raw controller error.
type RuntimeEventRecord struct {
	EventID          string         `json:"event_id"`
	OccurredAt       time.Time      `json:"occurred_at"`
	Controller       string         `json:"controller"`
	BindingRevision  string         `json:"binding_revision,omitempty"`
	VirtualModel     string         `json:"virtual_model,omitempty"`
	Deployment       string         `json:"deployment,omitempty"`
	EndpointInstance string         `json:"endpoint_instance,omitempty"`
	ClusterID        string         `json:"cluster_id,omitempty"`
	JobID            string         `json:"job_id,omitempty"`
	RecipeRevision   string         `json:"recipe_revision,omitempty"`
	PriorState       string         `json:"prior_state,omitempty"`
	NewState         string         `json:"new_state"`
	Reason           string         `json:"reason,omitempty"`
	Latency          *time.Duration `json:"latency,omitempty"`
	QueueDepth       int            `json:"queue_depth"`
	WaiterCount      int            `json:"waiter_count"`
	Outcome          RuntimeOutcome `json:"outcome"`
}

// Record is a typed union. Exactly one payload matching Kind must be present.
type Record struct {
	Kind         Kind                `json:"kind"`
	Request      *RequestRecord      `json:"request,omitempty"`
	Attempt      *AttemptRecord      `json:"attempt,omitempty"`
	RuntimeEvent *RuntimeEventRecord `json:"runtime_event,omitempty"`
}

func NewRequestRecord(record RequestRecord) Record {
	return Record{Kind: KindRequest, Request: &record}
}

func NewAttemptRecord(record AttemptRecord) Record {
	return Record{Kind: KindAttempt, Attempt: &record}
}

func NewRuntimeEventRecord(record RuntimeEventRecord) Record {
	return Record{Kind: KindRuntimeEvent, RuntimeEvent: &record}
}

func (r Record) Validate() error {
	switch r.Kind {
	case KindRequest:
		if r.Request == nil || r.Attempt != nil || r.RuntimeEvent != nil {
			return fmt.Errorf("request record must contain only a request payload")
		}
		if r.Request.RequestID == "" {
			return fmt.Errorf("request_id is required")
		}
		if err := validateRecordTimes(r.Request.StartedAt, r.Request.CompletedAt); err != nil {
			return err
		}
		if err := validateOutcome(r.Request.Outcome); err != nil {
			return err
		}
		for name, value := range map[string]string{
			"request_id":               r.Request.RequestID,
			"principal_id":             r.Request.PrincipalID,
			"principal_type":           r.Request.PrincipalType,
			"principal_subject":        r.Request.PrincipalSubject,
			"tenant_id":                r.Request.TenantID,
			"protocol":                 r.Request.Protocol,
			"operation":                r.Request.Operation,
			"requested_model":          r.Request.RequestedModel,
			"virtual_model":            r.Request.VirtualModel,
			"response_presented_model": r.Request.ResponsePresentedModel,
			"config_revision":          r.Request.ConfigRevision,
			"final_provider":           r.Request.FinalProvider,
			"final_deployment":         r.Request.FinalDeployment,
			"final_upstream_model":     r.Request.FinalUpstreamModel,
			"failure_class":            r.Request.FailureClass,
		} {
			if err := validateBoundedString(name, value, 1024); err != nil {
				return err
			}
		}
		if len(r.Request.PrincipalRoles) > 64 {
			return fmt.Errorf("too many principal roles")
		}
		for _, role := range r.Request.PrincipalRoles {
			if err := validateBoundedString("principal role", role, 256); err != nil {
				return err
			}
		}
		if err := validateAttribution(r.Request.Attribution); err != nil {
			return err
		}
		return validateUsage(r.Request.Usage)
	case KindAttempt:
		if r.Attempt == nil || r.Request != nil || r.RuntimeEvent != nil {
			return fmt.Errorf("attempt record must contain only an attempt payload")
		}
		if r.Attempt.RequestID == "" || r.Attempt.Attempt <= 0 {
			return fmt.Errorf("request_id and positive attempt are required")
		}
		if err := validateRecordTimes(r.Attempt.StartedAt, r.Attempt.CompletedAt); err != nil {
			return err
		}
		if err := validateOutcome(r.Attempt.Outcome); err != nil {
			return err
		}
		for name, value := range map[string]string{
			"request_id":          r.Attempt.RequestID,
			"provider":            r.Attempt.Provider,
			"deployment":          r.Attempt.Deployment,
			"upstream_model":      r.Attempt.UpstreamModel,
			"upstream_request_id": r.Attempt.UpstreamRequestID,
			"failure_class":       r.Attempt.FailureClass,
		} {
			if err := validateBoundedString(name, value, 1024); err != nil {
				return err
			}
		}
		return validateUsage(r.Attempt.Usage)
	case KindRuntimeEvent:
		if r.RuntimeEvent == nil || r.Request != nil || r.Attempt != nil {
			return fmt.Errorf("runtime event record must contain only a runtime event payload")
		}
		return validateRuntimeEvent(*r.RuntimeEvent)
	default:
		return fmt.Errorf("unsupported record kind %q", r.Kind)
	}
}

func validateRuntimeEvent(record RuntimeEventRecord) error {
	if record.EventID == "" || record.Controller == "" || record.NewState == "" {
		return fmt.Errorf("event_id, controller, and new_state are required")
	}
	if record.OccurredAt.IsZero() {
		return fmt.Errorf("runtime event occurred_at is required")
	}
	for name, value := range map[string]string{
		"event_id":          record.EventID,
		"controller":        record.Controller,
		"binding_revision":  record.BindingRevision,
		"virtual_model":     record.VirtualModel,
		"deployment":        record.Deployment,
		"endpoint_instance": record.EndpointInstance,
		"cluster_id":        record.ClusterID,
		"job_id":            record.JobID,
		"recipe_revision":   record.RecipeRevision,
	} {
		if err := validateBoundedString(name, value, 1024); err != nil {
			return err
		}
	}
	if record.PriorState != "" && !validRuntimeState(record.PriorState) {
		return fmt.Errorf("invalid prior runtime state %q", record.PriorState)
	}
	if !validRuntimeState(record.NewState) {
		return fmt.Errorf("invalid new runtime state %q", record.NewState)
	}
	if !validRuntimeOutcome(record.Outcome) {
		return fmt.Errorf("invalid runtime outcome %q", record.Outcome)
	}
	if record.Reason != sanitizeRuntimeReason(record.Reason) {
		return fmt.Errorf("runtime reason must be a sanitized low-cardinality identifier")
	}
	if record.Latency != nil && *record.Latency < 0 {
		return fmt.Errorf("runtime latency must not be negative")
	}
	if record.QueueDepth < 0 || record.WaiterCount < 0 {
		return fmt.Errorf("runtime queue counters must not be negative")
	}
	return nil
}

func validRuntimeState(state string) bool {
	switch state {
	case "offline", "activating", "ready", "draining", "deactivating", "failed", "unknown":
		return true
	default:
		return false
	}
}

func validRuntimeOutcome(outcome RuntimeOutcome) bool {
	switch outcome {
	case RuntimeOutcomeSuccess, RuntimeOutcomeFailure, RuntimeOutcomeTimeout,
		RuntimeOutcomeCancelled, RuntimeOutcomeRejected:
		return true
	default:
		return false
	}
}

func sanitizeRuntimeReason(reason string) string {
	if len(reason) > MaxRuntimeReasonBytes {
		return reason[:MaxRuntimeReasonBytes]
	}
	for _, current := range reason {
		if (current < 'a' || current > 'z') && (current < '0' || current > '9') &&
			current != '-' && current != '_' {
			return ""
		}
	}
	return reason
}

func validateAttribution(attribution map[string]string) error {
	if len(attribution) > 64 {
		return fmt.Errorf("too many attribution fields")
	}
	total := 0
	for key, value := range attribution {
		if key == "" {
			return fmt.Errorf("attribution key is empty")
		}
		if err := validateBoundedString("attribution key", key, 128); err != nil {
			return err
		}
		if err := validateBoundedString("attribution value", value, 1024); err != nil {
			return err
		}
		total += len(key) + len(value)
		if total > MaxAttributionBytes {
			return fmt.Errorf("attribution exceeds %d bytes", MaxAttributionBytes)
		}
	}
	return nil
}

func validateUsage(usage TokenUsage) error {
	switch usage.Completeness {
	case UsageMissing, UsagePartial, UsageComplete:
	default:
		return fmt.Errorf("invalid usage completeness %q", usage.Completeness)
	}
	if len(usage.Raw) > MaxRawUsageBytes {
		return fmt.Errorf("raw usage exceeds %d bytes", MaxRawUsageBytes)
	}
	if len(usage.Raw) > 0 && !json.Valid(usage.Raw) {
		return fmt.Errorf("raw usage is not valid JSON")
	}
	for name, value := range map[string]*int64{
		"input_tokens":               usage.InputTokens,
		"output_tokens":              usage.OutputTokens,
		"total_tokens":               usage.TotalTokens,
		"cached_input_tokens":        usage.CachedInputTokens,
		"cache_creation_tokens":      usage.CacheCreationTokens,
		"reasoning_tokens":           usage.ReasoningTokens,
		"tool_use_prompt_tokens":     usage.ToolUsePromptTokens,
		"accepted_prediction_tokens": usage.AcceptedPredictionTokens,
		"rejected_prediction_tokens": usage.RejectedPredictionTokens,
	} {
		if value != nil && *value < 0 {
			return fmt.Errorf("%s must not be negative", name)
		}
	}
	if len(usage.ProviderComponents) > 64 {
		return fmt.Errorf("too many provider usage components")
	}
	for name, value := range usage.ProviderComponents {
		if err := validateBoundedString("provider component", name, 128); err != nil {
			return err
		}
		if value < 0 {
			return fmt.Errorf("provider component %q must not be negative", name)
		}
	}
	if err := validateBoundedString(
		"normalization_version",
		usage.NormalizationVersion,
		128,
	); err != nil {
		return err
	}
	return nil
}

func validateRecordTimes(started, completed time.Time) error {
	if started.IsZero() || completed.IsZero() {
		return fmt.Errorf("record timestamps are required")
	}
	if completed.Before(started) {
		return fmt.Errorf("record completion precedes start")
	}
	return nil
}

func validateOutcome(outcome Outcome) error {
	switch outcome {
	case OutcomeSuccess,
		OutcomeRejected,
		OutcomeUpstreamError,
		OutcomeTransportError,
		OutcomeConfigError,
		OutcomeClientCancelled,
		OutcomeStreamError,
		OutcomeExhausted:
		return nil
	default:
		return fmt.Errorf("invalid outcome %q", outcome)
	}
}

func validateBoundedString(name, value string, limit int) error {
	if len(value) > limit {
		return fmt.Errorf("%s exceeds %d bytes", name, limit)
	}
	return nil
}

func (r Record) EstimatedBytes() int {
	const fixedOverhead = 256
	size := fixedOverhead
	if r.Request != nil {
		size += len(r.Request.RequestID) +
			len(r.Request.PrincipalID) +
			len(r.Request.PrincipalType) +
			len(r.Request.PrincipalSubject) +
			len(r.Request.TenantID) +
			len(r.Request.RequestedModel) +
			len(r.Request.VirtualModel) +
			len(r.Request.FinalProvider) +
			len(r.Request.FinalDeployment) +
			len(r.Request.FinalUpstreamModel) +
			len(r.Request.Usage.Raw)
		for _, role := range r.Request.PrincipalRoles {
			size += len(role)
		}
		for key, value := range r.Request.Attribution {
			size += len(key) + len(value)
		}
	}
	if r.Attempt != nil {
		size += len(r.Attempt.RequestID) +
			len(r.Attempt.Provider) +
			len(r.Attempt.Deployment) +
			len(r.Attempt.UpstreamModel) +
			len(r.Attempt.Usage.Raw)
	}
	if r.RuntimeEvent != nil {
		size += len(r.RuntimeEvent.EventID) +
			len(r.RuntimeEvent.Controller) +
			len(r.RuntimeEvent.BindingRevision) +
			len(r.RuntimeEvent.VirtualModel) +
			len(r.RuntimeEvent.Deployment) +
			len(r.RuntimeEvent.EndpointInstance) +
			len(r.RuntimeEvent.ClusterID) +
			len(r.RuntimeEvent.JobID) +
			len(r.RuntimeEvent.RecipeRevision) +
			len(r.RuntimeEvent.Reason)
	}
	return size
}

type Store interface {
	AppendBatch(ctx context.Context, records []Record) error
	Health(ctx context.Context) error
	Close() error
}

// Recorder is the non-blocking request-path contract.
type Recorder interface {
	Record(Record)
}

type DiscardRecorder struct{}

func (DiscardRecorder) Record(Record) {}

type DiscardStore struct{}

func (DiscardStore) AppendBatch(context.Context, []Record) error { return nil }
func (DiscardStore) Health(context.Context) error                { return nil }
func (DiscardStore) Close() error                                { return nil }

var (
	_ Recorder = DiscardRecorder{}
	_ Store    = DiscardStore{}
)
