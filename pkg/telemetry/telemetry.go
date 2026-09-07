package telemetry

import (
	"context"
	"fmt"
	"net/http"
	"sort"
	"time"

	"github.com/sparksq/sparkroute/pkg/identity"
	"github.com/sparksq/sparkroute/pkg/ledger"
	"github.com/sparksq/sparkroute/pkg/routing"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/propagation"
	semconv "go.opentelemetry.io/otel/semconv/v1.37.0"
	"go.opentelemetry.io/otel/trace"
)

const (
	InstrumentationScope = "github.com/sparksq/sparkroute"
	SemconvVersion       = "1.37.0"
)

type Options struct {
	TracerProvider trace.TracerProvider
	MeterProvider  metric.MeterProvider
	Propagator     propagation.TextMapPropagator
	Conventions    Conventions
}

type Instrumentation struct {
	tracer     trace.Tracer
	propagator propagation.TextMapPropagator
	convention Conventions

	requests              metric.Int64Counter
	requestDuration       metric.Float64Histogram
	requestFirstByte      metric.Float64Histogram
	activeRequests        metric.Int64UpDownCounter
	activeStreams         metric.Int64UpDownCounter
	targetActive          metric.Int64UpDownCounter
	admissionRejects      metric.Int64Counter
	circuitChanges        metric.Int64Counter
	circuitEjections      metric.Int64Counter
	activeRetries         metric.Int64UpDownCounter
	retryBudgetDrops      metric.Int64Counter
	retryDelay            metric.Float64Histogram
	attempts              metric.Int64Counter
	attemptDuration       metric.Float64Histogram
	retries               metric.Int64Counter
	tokens                metric.Int64Counter
	usageIncomplete       metric.Int64Counter
	modelRoutingDecisions metric.Int64Counter
	modelRoutingDuration  metric.Float64Histogram
	stageScore            metric.Float64Histogram
	stageConfidence       metric.Float64Histogram
	stageSeverity         metric.Float64Histogram
	stageSpinning         metric.Float64Histogram
	stageExploring        metric.Float64Histogram
	stageProduction       metric.Float64Histogram
}

type RequestStart struct {
	RequestID      string
	Protocol       string
	Operation      string
	ConfigRevision string
	Identity       identity.Identity
	StartedAt      time.Time
}

type AttemptStart struct {
	VirtualModel         string
	Provider             string
	Deployment           string
	UpstreamModel        string
	UpstreamProtocol     string
	NativeProtocol       bool
	RequiredCapabilities []string
	CircuitState         string
	HalfOpenProbe        bool
	ActiveAtAdmission    int
	RetryReason          string
	RetryDelay           time.Duration
	Attempt              int
	PoolPriority         int
}

type RoutingSelection struct {
	Mode                  string
	WeightedHashApplied   bool
	StateAffinityPinned   bool
	StateAffinityKind     string
	PromptAffinityEnabled bool
	PromptAffinityMatched bool
	PromptAffinityApplied bool
	MatchedPrefixBytes    int
	MatchedPrefixSegments int
}

type ModelRoutingSelection struct {
	Router               string
	Strategy             string
	SelectedVirtualModel string
	DecisionSource       string
	Tier                 string
	Duration             time.Duration
	Stage                bool
	Score                float64
	Confidence           float64
	Severity             float64
	Spinning             float64
	Exploring            float64
	ProductionIntensity  float64
}

type Request struct {
	instrumentation *Instrumentation
	context         context.Context
	span            trace.Span
	started         time.Time
	activeAttrs     []attribute.KeyValue
	stream          bool
}

type Attempt struct {
	instrumentation *Instrumentation
	context         context.Context
	span            trace.Span
	start           AttemptStart
	started         time.Time
}

func New(options Options) (*Instrumentation, error) {
	tracerProvider := options.TracerProvider
	if tracerProvider == nil {
		tracerProvider = otel.GetTracerProvider()
	}
	meterProvider := options.MeterProvider
	if meterProvider == nil {
		meterProvider = otel.GetMeterProvider()
	}
	propagator := options.Propagator
	if propagator == nil {
		propagator = propagation.TraceContext{}
	}
	convention := options.Conventions
	if convention == nil {
		convention = NoConventions{}
	}

	meter := meterProvider.Meter(InstrumentationScope)
	requests, err := meter.Int64Counter(
		"llm.gateway.requests",
		metric.WithDescription("Completed LLM gateway requests."),
		metric.WithUnit("{request}"),
	)
	if err != nil {
		return nil, fmt.Errorf("create requests counter: %w", err)
	}
	requestDuration, err := meter.Float64Histogram(
		"llm.gateway.request.duration",
		metric.WithDescription("LLM gateway request duration."),
		metric.WithUnit("s"),
	)
	if err != nil {
		return nil, fmt.Errorf("create request duration histogram: %w", err)
	}
	requestFirstByte, err := meter.Float64Histogram(
		"llm.gateway.request.time_to_first_byte",
		metric.WithDescription("Time until upstream response headers are received."),
		metric.WithUnit("s"),
	)
	if err != nil {
		return nil, fmt.Errorf("create request first-byte histogram: %w", err)
	}
	activeRequests, err := meter.Int64UpDownCounter(
		"llm.gateway.active_requests",
		metric.WithDescription("Active LLM gateway requests."),
		metric.WithUnit("{request}"),
	)
	if err != nil {
		return nil, fmt.Errorf("create active requests counter: %w", err)
	}
	activeStreams, err := meter.Int64UpDownCounter(
		"llm.gateway.active_streams",
		metric.WithDescription("Active streaming LLM gateway requests."),
		metric.WithUnit("{stream}"),
	)
	if err != nil {
		return nil, fmt.Errorf("create active streams counter: %w", err)
	}
	targetActive, err := meter.Int64UpDownCounter(
		"llm.gateway.target.active_requests",
		metric.WithDescription("Active requests admitted to each deployment."),
		metric.WithUnit("{request}"),
	)
	if err != nil {
		return nil, fmt.Errorf("create target active requests counter: %w", err)
	}
	admissionRejects, err := meter.Int64Counter(
		"llm.gateway.target.admission_rejections",
		metric.WithDescription("Attempt admissions rejected by runtime target gates."),
		metric.WithUnit("{rejection}"),
	)
	if err != nil {
		return nil, fmt.Errorf("create target admission rejection counter: %w", err)
	}
	circuitChanges, err := meter.Int64Counter(
		"llm.gateway.target.circuit_transitions",
		metric.WithDescription("Deployment circuit state transitions."),
		metric.WithUnit("{transition}"),
	)
	if err != nil {
		return nil, fmt.Errorf("create circuit transition counter: %w", err)
	}
	circuitEjections, err := meter.Int64Counter(
		"llm.gateway.target.circuit_ejections",
		metric.WithDescription("Deployment circuit ejections."),
		metric.WithUnit("{ejection}"),
	)
	if err != nil {
		return nil, fmt.Errorf("create circuit ejection counter: %w", err)
	}
	activeRetries, err := meter.Int64UpDownCounter(
		"llm.gateway.retry.active",
		metric.WithDescription("Active retries admitted by the shared budget."),
		metric.WithUnit("{retry}"),
	)
	if err != nil {
		return nil, fmt.Errorf("create active retries counter: %w", err)
	}
	retryBudgetDrops, err := meter.Int64Counter(
		"llm.gateway.retry.budget_rejections",
		metric.WithDescription("Retries rejected by the shared concurrency budget."),
		metric.WithUnit("{rejection}"),
	)
	if err != nil {
		return nil, fmt.Errorf("create retry budget rejection counter: %w", err)
	}
	retryDelay, err := meter.Float64Histogram(
		"llm.gateway.retry.delay",
		metric.WithDescription("Delay before an admitted retry attempt."),
		metric.WithUnit("s"),
	)
	if err != nil {
		return nil, fmt.Errorf("create retry delay histogram: %w", err)
	}
	attempts, err := meter.Int64Counter(
		"llm.gateway.attempts",
		metric.WithDescription("Completed upstream LLM attempts."),
		metric.WithUnit("{attempt}"),
	)
	if err != nil {
		return nil, fmt.Errorf("create attempts counter: %w", err)
	}
	attemptDuration, err := meter.Float64Histogram(
		"llm.gateway.attempt.duration",
		metric.WithDescription("Upstream LLM attempt duration."),
		metric.WithUnit("s"),
	)
	if err != nil {
		return nil, fmt.Errorf("create attempt duration histogram: %w", err)
	}
	retries, err := meter.Int64Counter(
		"llm.gateway.retries",
		metric.WithDescription("Upstream attempts for which another retry was scheduled."),
		metric.WithUnit("{retry}"),
	)
	if err != nil {
		return nil, fmt.Errorf("create retries counter: %w", err)
	}
	tokens, err := meter.Int64Counter(
		"llm.gateway.attempt.tokens",
		metric.WithDescription("Provider-reported normalized token usage for upstream attempts."),
		metric.WithUnit("{token}"),
	)
	if err != nil {
		return nil, fmt.Errorf("create token counter: %w", err)
	}
	usageIncomplete, err := meter.Int64Counter(
		"llm.gateway.attempt.usage_incomplete",
		metric.WithDescription("Upstream attempts with missing or partial token usage."),
		metric.WithUnit("{attempt}"),
	)
	if err != nil {
		return nil, fmt.Errorf("create incomplete usage counter: %w", err)
	}
	modelRoutingDecisions, err := meter.Int64Counter(
		"llm.gateway.model_router.decisions",
		metric.WithDescription("Completed logical model-router decisions."),
		metric.WithUnit("{decision}"),
	)
	if err != nil {
		return nil, fmt.Errorf("create model-router decisions counter: %w", err)
	}
	modelRoutingDuration, err := meter.Float64Histogram(
		"llm.gateway.model_router.duration",
		metric.WithDescription("Logical model-router decision duration."),
		metric.WithUnit("s"),
	)
	if err != nil {
		return nil, fmt.Errorf("create model-router duration histogram: %w", err)
	}
	newStageHistogram := func(name, description string) (metric.Float64Histogram, error) {
		return meter.Float64Histogram(name, metric.WithDescription(description), metric.WithUnit("1"))
	}
	stageScore, err := newStageHistogram("llm.gateway.stage_router.score", "Stage-router signed selection score.")
	if err != nil {
		return nil, fmt.Errorf("create stage-router score histogram: %w", err)
	}
	stageConfidence, err := newStageHistogram("llm.gateway.stage_router.confidence", "Stage-router decision confidence.")
	if err != nil {
		return nil, fmt.Errorf("create stage-router confidence histogram: %w", err)
	}
	stageSeverity, err := newStageHistogram("llm.gateway.stage_router.severity", "Stage-router recent tool-error severity.")
	if err != nil {
		return nil, fmt.Errorf("create stage-router severity histogram: %w", err)
	}
	stageSpinning, err := newStageHistogram("llm.gateway.stage_router.spinning", "Stage-router unproductive-activity signal.")
	if err != nil {
		return nil, fmt.Errorf("create stage-router spinning histogram: %w", err)
	}
	stageExploring, err := newStageHistogram("llm.gateway.stage_router.exploring", "Stage-router exploratory-activity signal.")
	if err != nil {
		return nil, fmt.Errorf("create stage-router exploring histogram: %w", err)
	}
	stageProduction, err := newStageHistogram("llm.gateway.stage_router.production_intensity", "Stage-router recent production intensity.")
	if err != nil {
		return nil, fmt.Errorf("create stage-router production histogram: %w", err)
	}

	return &Instrumentation{
		tracer:                tracerProvider.Tracer(InstrumentationScope),
		propagator:            propagator,
		convention:            convention,
		requests:              requests,
		requestDuration:       requestDuration,
		requestFirstByte:      requestFirstByte,
		activeRequests:        activeRequests,
		activeStreams:         activeStreams,
		targetActive:          targetActive,
		admissionRejects:      admissionRejects,
		circuitChanges:        circuitChanges,
		circuitEjections:      circuitEjections,
		activeRetries:         activeRetries,
		retryBudgetDrops:      retryBudgetDrops,
		retryDelay:            retryDelay,
		attempts:              attempts,
		attemptDuration:       attemptDuration,
		retries:               retries,
		tokens:                tokens,
		usageIncomplete:       usageIncomplete,
		modelRoutingDecisions: modelRoutingDecisions,
		modelRoutingDuration:  modelRoutingDuration,
		stageScore:            stageScore,
		stageConfidence:       stageConfidence,
		stageSeverity:         stageSeverity,
		stageSpinning:         stageSpinning,
		stageExploring:        stageExploring,
		stageProduction:       stageProduction,
	}, nil
}

func (i *Instrumentation) StartRequest(
	ctx context.Context,
	headers http.Header,
	start RequestStart,
) (context.Context, *Request) {
	ctx = i.propagator.Extract(ctx, propagation.HeaderCarrier(headers))
	attrs := []attribute.KeyValue{
		semconv.GenAIOperationNameChat,
		attribute.String("llm.gateway.protocol", start.Protocol),
		attribute.String("llm.gateway.operation", start.Operation),
	}
	if start.RequestID != "" {
		attrs = append(attrs, attribute.String("llm.gateway.request.id", start.RequestID))
	}
	if start.ConfigRevision != "" {
		attrs = append(attrs, attribute.String("llm.gateway.config.revision", start.ConfigRevision))
	}
	attrs = append(attrs, identityAttributes(start.Identity)...)
	attrs = append(attrs, i.convention.Request(start)...)
	spanOptions := []trace.SpanStartOption{
		trace.WithSpanKind(trace.SpanKindServer),
		trace.WithAttributes(attrs...),
	}
	startedAt := start.StartedAt
	if startedAt.IsZero() {
		startedAt = time.Now()
	} else {
		spanOptions = append(spanOptions, trace.WithTimestamp(startedAt))
	}
	ctx, span := i.tracer.Start(ctx, "llm.gateway.chat_completions", spanOptions...)
	activeAttrs := []attribute.KeyValue{
		attribute.String("llm.gateway.protocol", start.Protocol),
		attribute.String("llm.gateway.operation", start.Operation),
	}
	i.activeRequests.Add(ctx, 1, metric.WithAttributes(activeAttrs...))
	return ctx, &Request{
		instrumentation: i,
		context:         ctx,
		span:            span,
		started:         startedAt,
		activeAttrs:     activeAttrs,
	}
}

func identityAttributes(value identity.Identity) []attribute.KeyValue {
	attrs := make([]attribute.KeyValue, 0, 5+len(value.Attribution))
	if value.Principal.ID != "" {
		attrs = append(attrs, attribute.String(
			"llm.gateway.client.principal.id",
			value.Principal.ID,
		))
	}
	if value.Principal.Type != "" {
		attrs = append(attrs, attribute.String(
			"llm.gateway.client.principal.type",
			value.Principal.Type,
		))
	}
	if value.Principal.Subject != "" {
		attrs = append(attrs, attribute.String(
			"llm.gateway.client.principal.subject",
			value.Principal.Subject,
		))
	}
	if len(value.Principal.Roles) > 0 {
		attrs = append(attrs, attribute.StringSlice(
			"llm.gateway.client.principal.roles",
			append([]string(nil), value.Principal.Roles...),
		))
	}
	if value.Principal.Tenant != "" {
		attrs = append(attrs, attribute.String(
			"llm.gateway.client.tenant.id",
			value.Principal.Tenant,
		))
	}
	keys := make([]string, 0, len(value.Attribution))
	for key := range value.Attribution {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		attrs = append(attrs, attribute.String(key, value.Attribution[key]))
	}
	return attrs
}

func (r *Request) StartAttempt(start AttemptStart) (context.Context, *Attempt) {
	attrs := []attribute.KeyValue{
		semconv.GenAIOperationNameChat,
		semconv.GenAIRequestModel(start.UpstreamModel),
		attribute.String("llm.gateway.virtual_model", start.VirtualModel),
		attribute.String("llm.gateway.provider", start.Provider),
		attribute.String("llm.gateway.deployment", start.Deployment),
		attribute.String("llm.gateway.upstream.protocol", start.UpstreamProtocol),
		attribute.Bool("llm.gateway.upstream.protocol_native", start.NativeProtocol),
		attribute.Int("llm.gateway.attempt", start.Attempt),
		attribute.Int("llm.gateway.pool_priority", start.PoolPriority),
		attribute.String("llm.gateway.circuit.state", start.CircuitState),
		attribute.Bool("llm.gateway.circuit.half_open_probe", start.HalfOpenProbe),
		attribute.Int("llm.gateway.target.active_at_admission", start.ActiveAtAdmission),
	}
	if len(start.RequiredCapabilities) > 0 {
		attrs = append(attrs, attribute.StringSlice(
			"llm.gateway.required_capabilities",
			append([]string(nil), start.RequiredCapabilities...),
		))
	}
	if start.RetryReason != "" {
		attrs = append(attrs,
			attribute.String("llm.gateway.retry.reason", start.RetryReason),
			attribute.Int64(
				"llm.gateway.retry.delay_ms",
				start.RetryDelay.Milliseconds(),
			),
		)
	}
	attrs = append(attrs, r.instrumentation.convention.Attempt(start)...)
	ctx, span := r.instrumentation.tracer.Start(
		r.context,
		"llm.gateway.upstream.chat",
		trace.WithSpanKind(trace.SpanKindClient),
		trace.WithAttributes(attrs...),
	)
	return ctx, &Attempt{
		instrumentation: r.instrumentation,
		context:         ctx,
		span:            span,
		start:           start,
		started:         time.Now(),
	}
}

func (r *Request) SetRequiredCapabilities(capabilities []string) {
	if len(capabilities) == 0 {
		return
	}
	r.span.SetAttributes(attribute.StringSlice(
		"llm.gateway.required_capabilities",
		append([]string(nil), capabilities...),
	))
}

// SetRoutingSelection records only content-free routing decision metadata.
// Prefix digests, identity keys, and prompt material are intentionally absent.
func (r *Request) SetRoutingSelection(selection RoutingSelection) {
	if r == nil {
		return
	}
	attrs := []attribute.KeyValue{
		attribute.String("llm.gateway.routing.mode", selection.Mode),
		attribute.Bool("llm.gateway.routing.weighted_hash_applied", selection.WeightedHashApplied),
		attribute.Bool("llm.gateway.routing.state_affinity.pinned", selection.StateAffinityPinned),
		attribute.Bool("llm.gateway.routing.prompt_cache_affinity.enabled", selection.PromptAffinityEnabled),
		attribute.Bool("llm.gateway.routing.prompt_cache_affinity.matched", selection.PromptAffinityMatched),
		attribute.Bool("llm.gateway.routing.prompt_cache_affinity.applied", selection.PromptAffinityApplied),
	}
	if selection.StateAffinityKind != "" {
		attrs = append(attrs, attribute.String(
			"llm.gateway.routing.state_affinity.kind",
			selection.StateAffinityKind,
		))
	}
	if selection.PromptAffinityMatched {
		attrs = append(attrs,
			attribute.Int("llm.gateway.routing.prompt_cache_affinity.prefix_bytes", selection.MatchedPrefixBytes),
			attribute.Int("llm.gateway.routing.prompt_cache_affinity.prefix_segments", selection.MatchedPrefixSegments),
		)
	}
	r.span.SetAttributes(attrs...)
}

// SetModelRouting records bounded, content-free selector telemetry. Request
// text, tool names/output, identities, free-form rationale, and policy kwargs
// never become metric dimensions.
func (r *Request) SetModelRouting(selection ModelRoutingSelection) {
	if r == nil {
		return
	}
	router := boundedRoutingLabel(selection.Router, map[string]bool{
		"fox-sparkroute-native": true, "passthrough": true,
	})
	strategy := boundedRoutingLabel(selection.Strategy, map[string]bool{
		"explicit": true, "random": true, "round_robin": true,
		"weighted_round_robin": true, "smallest": true, "largest": true,
		"lowest_cost": true, "fastest": true, "balanced": true, "stage_router": true,
	})
	source := boundedRoutingLabel(selection.DecisionSource, map[string]bool{
		"": true, "override": true, "tests_passed": true, "dimensions": true,
		"fall_open": true, "tier_unavailable": true,
	})
	tier := boundedRoutingLabel(selection.Tier, map[string]bool{"": true, "capable": true, "efficient": true})
	attrs := []attribute.KeyValue{
		attribute.String("llm.gateway.model_router", router),
		attribute.String("llm.gateway.model_router.strategy", strategy),
		attribute.String("llm.gateway.model_router.decision_source", source),
		attribute.String("llm.gateway.model_router.tier", tier),
	}
	if selection.SelectedVirtualModel != "" {
		attrs = append(attrs, attribute.String("llm.gateway.model_router.selected_virtual_model", selection.SelectedVirtualModel))
	}
	r.span.SetAttributes(attrs...)
	r.instrumentation.modelRoutingDecisions.Add(r.context, 1, metric.WithAttributes(attrs...))
	r.instrumentation.modelRoutingDuration.Record(r.context, selection.Duration.Seconds(), metric.WithAttributes(
		attribute.String("llm.gateway.model_router", router),
		attribute.String("llm.gateway.model_router.strategy", strategy),
	))
	if !selection.Stage {
		return
	}
	r.instrumentation.stageScore.Record(r.context, selection.Score)
	r.instrumentation.stageConfidence.Record(r.context, selection.Confidence)
	r.instrumentation.stageSeverity.Record(r.context, selection.Severity)
	r.instrumentation.stageSpinning.Record(r.context, selection.Spinning)
	r.instrumentation.stageExploring.Record(r.context, selection.Exploring)
	r.instrumentation.stageProduction.Record(r.context, selection.ProductionIntensity)
}

func boundedRoutingLabel(value string, allowed map[string]bool) string {
	if allowed[value] {
		return value
	}
	return "other"
}

// SetMMProjection records the content-free outcome of the request-transform
// stage separately from completion-provider attempts.
func (r *Request) SetMMProjection(
	state string,
	provider string,
	failureMode string,
	httpStatus int,
	durationMS int64,
	mediaCount int,
	cacheHit bool,
) {
	if r == nil {
		return
	}
	attrs := []attribute.KeyValue{
		attribute.String("llm.gateway.mm_projection.state", state),
		attribute.String("llm.gateway.mm_projection.provider", provider),
		attribute.String("llm.gateway.mm_projection.failure_mode", failureMode),
		attribute.Int64("llm.gateway.mm_projection.duration_ms", durationMS),
		attribute.Int("llm.gateway.mm_projection.media_count", mediaCount),
		attribute.Bool("llm.gateway.mm_projection.cache_hit", cacheHit),
	}
	if httpStatus != 0 {
		attrs = append(attrs, attribute.Int("llm.gateway.mm_projection.http_status", httpStatus))
	}
	r.span.SetAttributes(attrs...)
}

// SetStream marks the request as streaming once the ingress body is decoded.
// It is safe to call more than once; only the first true transition changes the
// active-stream gauge.
func (r *Request) SetStream(stream bool) {
	if !stream || r.stream {
		return
	}
	r.stream = true
	r.instrumentation.activeStreams.Add(
		r.context,
		1,
		metric.WithAttributes(r.activeAttrs...),
	)
}

func (i *Instrumentation) Inject(ctx context.Context, headers http.Header) {
	i.propagator.Inject(ctx, propagation.HeaderCarrier(headers))
}

func (r *Request) End(record ledger.RequestRecord) {
	attrs := []attribute.KeyValue{
		attribute.String("llm.gateway.outcome", string(record.Outcome)),
		attribute.String("llm.gateway.requested_model", record.RequestedModel),
		attribute.String("llm.gateway.virtual_model", record.VirtualModel),
		attribute.Int("llm.gateway.attempt_count", record.AttemptCount),
		attribute.Bool("llm.gateway.stream", record.Stream),
		attribute.String("llm.gateway.usage.completeness", string(record.Usage.Completeness)),
	}
	if record.RequestedModel != "" {
		attrs = append(attrs, semconv.GenAIRequestModel(record.RequestedModel))
	}
	if record.FinalProvider != "" {
		attrs = append(attrs,
			attribute.String("llm.gateway.provider", record.FinalProvider),
			attribute.String("llm.gateway.deployment", record.FinalDeployment),
			attribute.String("llm.gateway.upstream_model", record.FinalUpstreamModel),
		)
	}
	if record.ResponsePresentedModel != "" {
		attrs = append(attrs, attribute.String(
			"gen_ai.response.model",
			record.ResponsePresentedModel,
		))
	}
	if record.HTTPStatus > 0 {
		attrs = append(attrs, semconv.HTTPResponseStatusCode(record.HTTPStatus))
	}
	if record.FailureClass != "" {
		attrs = append(attrs, attribute.String("error.type", record.FailureClass))
	}
	if record.TimeToFirstByte != nil {
		attrs = append(attrs, attribute.Int64(
			"llm.gateway.time_to_first_byte_ms",
			record.TimeToFirstByte.Milliseconds(),
		))
	}
	attrs = append(attrs, usageAttributes(record.Usage)...)
	r.span.SetAttributes(attrs...)
	if record.Outcome != ledger.OutcomeSuccess {
		r.span.SetStatus(codes.Error, record.FailureClass)
	}

	metricAttrs := []attribute.KeyValue{
		attribute.String("llm.gateway.protocol", record.Protocol),
		attribute.String("llm.gateway.operation", record.Operation),
		attribute.String("llm.gateway.virtual_model", record.VirtualModel),
		attribute.String("llm.gateway.outcome", string(record.Outcome)),
		attribute.Bool("llm.gateway.stream", record.Stream),
	}
	r.instrumentation.requests.Add(r.context, 1, metric.WithAttributes(metricAttrs...))
	r.instrumentation.requestDuration.Record(
		r.context,
		durationSeconds(record.Latency, r.started),
		metric.WithAttributes(metricAttrs...),
	)
	if record.TimeToFirstByte != nil {
		r.instrumentation.requestFirstByte.Record(
			r.context,
			record.TimeToFirstByte.Seconds(),
			metric.WithAttributes(metricAttrs...),
		)
	}
	r.instrumentation.activeRequests.Add(
		r.context,
		-1,
		metric.WithAttributes(r.activeAttrs...),
	)
	if r.stream {
		r.instrumentation.activeStreams.Add(
			r.context,
			-1,
			metric.WithAttributes(r.activeAttrs...),
		)
	}
	r.span.End()
}

func (a *Attempt) End(record ledger.AttemptRecord) {
	attrs := []attribute.KeyValue{
		attribute.String("llm.gateway.outcome", string(record.Outcome)),
		attribute.Bool("llm.gateway.retried", record.Retried),
		attribute.String("llm.gateway.usage.completeness", string(record.Usage.Completeness)),
	}
	if record.HTTPStatus > 0 {
		attrs = append(attrs, semconv.HTTPResponseStatusCode(record.HTTPStatus))
	}
	if record.UpstreamRequestID != "" {
		attrs = append(attrs, attribute.String(
			"llm.gateway.upstream.request_id",
			record.UpstreamRequestID,
		))
	}
	if record.FailureClass != "" {
		attrs = append(attrs, attribute.String("error.type", record.FailureClass))
	}
	attrs = append(attrs, usageAttributes(record.Usage)...)
	a.span.SetAttributes(attrs...)
	if record.Outcome != ledger.OutcomeSuccess {
		a.span.SetStatus(codes.Error, record.FailureClass)
	}

	metricAttrs := []attribute.KeyValue{
		attribute.String("llm.gateway.virtual_model", a.start.VirtualModel),
		attribute.String("llm.gateway.provider", record.Provider),
		attribute.String("llm.gateway.deployment", record.Deployment),
		attribute.String("llm.gateway.upstream.protocol", a.start.UpstreamProtocol),
		attribute.Bool("llm.gateway.upstream.protocol_native", a.start.NativeProtocol),
		attribute.String("llm.gateway.outcome", string(record.Outcome)),
	}
	a.instrumentation.attempts.Add(a.context, 1, metric.WithAttributes(metricAttrs...))
	a.instrumentation.attemptDuration.Record(
		a.context,
		durationSeconds(record.Latency, a.started),
		metric.WithAttributes(metricAttrs...),
	)
	if record.Retried {
		a.instrumentation.retries.Add(a.context, 1, metric.WithAttributes(metricAttrs...))
	}
	a.instrumentation.recordUsage(a.context, metricAttrs, record.Usage)
	a.span.End()
}

func (i *Instrumentation) recordUsage(
	ctx context.Context,
	base []attribute.KeyValue,
	usage ledger.TokenUsage,
) {
	if usage.InputTokens != nil {
		attrs := appendCopy(base, attribute.String("llm.gateway.token.type", "input"))
		i.tokens.Add(ctx, *usage.InputTokens, metric.WithAttributes(attrs...))
	}
	if usage.OutputTokens != nil {
		attrs := appendCopy(base, attribute.String("llm.gateway.token.type", "output"))
		i.tokens.Add(ctx, *usage.OutputTokens, metric.WithAttributes(attrs...))
	}
	for _, component := range []struct {
		name  string
		value *int64
	}{
		{"cached_input", usage.CachedInputTokens},
		{"cache_creation", usage.CacheCreationTokens},
		{"reasoning", usage.ReasoningTokens},
		{"tool_use_prompt", usage.ToolUsePromptTokens},
		{"accepted_prediction", usage.AcceptedPredictionTokens},
		{"rejected_prediction", usage.RejectedPredictionTokens},
	} {
		if component.value == nil {
			continue
		}
		attrs := appendCopy(base, attribute.String("llm.gateway.token.type", component.name))
		i.tokens.Add(ctx, *component.value, metric.WithAttributes(attrs...))
	}
	if usage.Completeness != ledger.UsageComplete {
		attrs := appendCopy(
			base,
			attribute.String("llm.gateway.usage.completeness", string(usage.Completeness)),
		)
		i.usageIncomplete.Add(ctx, 1, metric.WithAttributes(attrs...))
	}
}

func usageAttributes(usage ledger.TokenUsage) []attribute.KeyValue {
	attrs := make([]attribute.KeyValue, 0, 10)
	if usage.InputTokens != nil {
		attrs = append(attrs, semconv.GenAIUsageInputTokensKey.Int64(*usage.InputTokens))
	}
	if usage.OutputTokens != nil {
		attrs = append(attrs, semconv.GenAIUsageOutputTokensKey.Int64(*usage.OutputTokens))
	}
	if usage.TotalTokens != nil {
		attrs = append(attrs, attribute.Int64("llm.gateway.usage.total_tokens", *usage.TotalTokens))
	}
	if usage.CachedInputTokens != nil {
		attrs = append(attrs, attribute.Int64(
			"llm.gateway.usage.cached_input_tokens",
			*usage.CachedInputTokens,
		))
	}
	if usage.ReasoningTokens != nil {
		attrs = append(attrs, attribute.Int64(
			"llm.gateway.usage.reasoning_tokens",
			*usage.ReasoningTokens,
		))
	}
	if usage.CacheCreationTokens != nil {
		attrs = append(attrs, attribute.Int64("llm.gateway.usage.cache_creation_tokens", *usage.CacheCreationTokens))
	}
	if usage.ToolUsePromptTokens != nil {
		attrs = append(attrs, attribute.Int64("llm.gateway.usage.tool_use_prompt_tokens", *usage.ToolUsePromptTokens))
	}
	if usage.AcceptedPredictionTokens != nil {
		attrs = append(attrs, attribute.Int64("llm.gateway.usage.accepted_prediction_tokens", *usage.AcceptedPredictionTokens))
	}
	if usage.RejectedPredictionTokens != nil {
		attrs = append(attrs, attribute.Int64("llm.gateway.usage.rejected_prediction_tokens", *usage.RejectedPredictionTokens))
	}
	return attrs
}

func durationSeconds(recorded time.Duration, started time.Time) float64 {
	if recorded <= 0 {
		recorded = time.Since(started)
	}
	return recorded.Seconds()
}

func appendCopy(values []attribute.KeyValue, value attribute.KeyValue) []attribute.KeyValue {
	result := make([]attribute.KeyValue, len(values), len(values)+1)
	copy(result, values)
	return append(result, value)
}

var _ routing.TargetObserver = (*Instrumentation)(nil)
var _ routing.RetryBudgetObserver = (*Instrumentation)(nil)

func (i *Instrumentation) TargetAdmissionChanged(deployment string, delta int64) {
	i.targetActive.Add(
		context.Background(),
		delta,
		metric.WithAttributes(attribute.String("llm.gateway.deployment", deployment)),
	)
}

func (i *Instrumentation) TargetAdmissionRejected(event routing.AdmissionRejection) {
	i.admissionRejects.Add(
		context.Background(),
		1,
		metric.WithAttributes(
			attribute.String("llm.gateway.deployment", event.Deployment),
			attribute.String("llm.gateway.admission.reason", string(event.Reason)),
		),
	)
}

func (i *Instrumentation) TargetCircuitChanged(event routing.CircuitTransition) {
	attrs := []attribute.KeyValue{
		attribute.String("llm.gateway.deployment", event.Deployment),
		attribute.String("llm.gateway.circuit.from", string(event.From)),
		attribute.String("llm.gateway.circuit.to", string(event.To)),
		attribute.String("llm.gateway.circuit.reason", event.Reason),
	}
	i.circuitChanges.Add(context.Background(), 1, metric.WithAttributes(attrs...))
	if event.To == routing.CircuitOpen {
		i.circuitEjections.Add(
			context.Background(),
			1,
			metric.WithAttributes(
				attribute.String("llm.gateway.deployment", event.Deployment),
				attribute.String("llm.gateway.circuit.reason", event.Reason),
			),
		)
	}
}

func (i *Instrumentation) RetryBudgetActiveChanged(
	virtualModel string,
	delta int64,
) {
	i.activeRetries.Add(
		context.Background(),
		delta,
		metric.WithAttributes(
			attribute.String("llm.gateway.virtual_model", virtualModel),
		),
	)
}

func (i *Instrumentation) RetryBudgetRejected(
	event routing.RetryBudgetRejection,
) {
	i.retryBudgetDrops.Add(
		context.Background(),
		1,
		metric.WithAttributes(
			attribute.String("llm.gateway.virtual_model", event.VirtualModel),
		),
	)
}

func (i *Instrumentation) RecordRetryDelay(
	ctx context.Context,
	virtualModel string,
	reason string,
	delay time.Duration,
) {
	i.retryDelay.Record(
		ctx,
		delay.Seconds(),
		metric.WithAttributes(
			attribute.String("llm.gateway.virtual_model", virtualModel),
			attribute.String("llm.gateway.retry.reason", reason),
		),
	)
}
