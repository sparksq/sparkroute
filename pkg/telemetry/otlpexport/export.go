// Package otlpexport builds OpenTelemetry SDK providers backed by OTLP/HTTP.
// It is kept separate from package telemetry so library users that supply their
// own providers do not need to initialize globals or adopt this bootstrap.
package otlpexport

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"go.opentelemetry.io/otel/attribute"
	metricexport "go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetrichttp"
	traceexport "go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/propagation"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.37.0"
	"go.opentelemetry.io/otel/trace"

	"github.com/sparksq/sparkroute/pkg/version"
)

const defaultShutdownTimeout = 5 * time.Second

type Options struct {
	ServiceName     string
	ServiceVersion  string
	TraceEndpoint   string
	MetricEndpoint  string
	TraceHeaders    map[string]string
	MetricHeaders   map[string]string
	MetricInterval  time.Duration
	ShutdownTimeout time.Duration
}

// Runtime owns the SDK providers created for a process. Nil providers mean that
// signal was not configured; callers may pass them directly to telemetry.Options,
// which will fall back to OpenTelemetry's global no-op providers.
type Runtime struct {
	TracerProvider trace.TracerProvider
	MeterProvider  metric.MeterProvider
	Propagator     propagation.TextMapPropagator

	shutdownTrace  func(context.Context) error
	shutdownMetric func(context.Context) error
	timeout        time.Duration
}

// FromEnvironment honors the standard OTLP HTTP endpoint/header variables.
// Traces use OTEL_EXPORTER_OTLP_TRACES_ENDPOINT, falling back to
// OTEL_EXPORTER_OTLP_ENDPOINT. Metrics intentionally require the signal-specific
// OTEL_EXPORTER_OTLP_METRICS_ENDPOINT: MLflow is an initial trace sink but does
// not provide the platform's metrics ingest path, so a generic MLflow endpoint
// must not silently produce failing /v1/metrics exports.
func FromEnvironment(
	ctx context.Context,
	serviceName string,
	serviceVersion string,
) (*Runtime, error) {
	if envBool("OTEL_SDK_DISABLED") {
		return disabledRuntime(), nil
	}
	if configured := strings.TrimSpace(os.Getenv("OTEL_SERVICE_NAME")); configured != "" {
		serviceName = configured
	}
	commonEndpoint := strings.TrimSpace(os.Getenv("OTEL_EXPORTER_OTLP_ENDPOINT"))
	traceEndpoint := strings.TrimSpace(os.Getenv("OTEL_EXPORTER_OTLP_TRACES_ENDPOINT"))
	if traceEndpoint == "" && commonEndpoint != "" {
		var err error
		traceEndpoint, err = signalEndpoint(commonEndpoint, "/v1/traces")
		if err != nil {
			return nil, fmt.Errorf("OTEL_EXPORTER_OTLP_ENDPOINT: %w", err)
		}
	}
	metricEndpoint := strings.TrimSpace(os.Getenv("OTEL_EXPORTER_OTLP_METRICS_ENDPOINT"))
	if traceEndpoint == "" && metricEndpoint == "" {
		return disabledRuntime(), nil
	}
	commonHeaders, err := parseHeaders(os.Getenv("OTEL_EXPORTER_OTLP_HEADERS"))
	if err != nil {
		return nil, fmt.Errorf("OTEL_EXPORTER_OTLP_HEADERS: %w", err)
	}
	traceHeaders, err := mergeHeaders(
		commonHeaders,
		os.Getenv("OTEL_EXPORTER_OTLP_TRACES_HEADERS"),
	)
	if err != nil {
		return nil, fmt.Errorf("OTEL_EXPORTER_OTLP_TRACES_HEADERS: %w", err)
	}
	metricHeaders, err := mergeHeaders(
		commonHeaders,
		os.Getenv("OTEL_EXPORTER_OTLP_METRICS_HEADERS"),
	)
	if err != nil {
		return nil, fmt.Errorf("OTEL_EXPORTER_OTLP_METRICS_HEADERS: %w", err)
	}
	return New(ctx, Options{
		ServiceName:    serviceName,
		ServiceVersion: serviceVersion,
		TraceEndpoint:  traceEndpoint,
		MetricEndpoint: metricEndpoint,
		TraceHeaders:   traceHeaders,
		MetricHeaders:  metricHeaders,
	})
}

func New(ctx context.Context, options Options) (*Runtime, error) {
	if options.TraceEndpoint == "" && options.MetricEndpoint == "" {
		return disabledRuntime(), nil
	}
	if options.ServiceName == "" {
		return nil, fmt.Errorf("service name is required when OTLP export is enabled")
	}
	res := resource.NewWithAttributes(
		semconv.SchemaURL,
		semconv.ServiceName(options.ServiceName),
		semconv.ServiceVersion(options.ServiceVersion),
		attribute.String("telemetry.distro.name", version.Name),
		attribute.String("telemetry.distro.version", version.Version),
	)
	runtime := &Runtime{
		Propagator: propagation.TraceContext{},
		timeout:    options.ShutdownTimeout,
	}
	if runtime.timeout <= 0 {
		runtime.timeout = defaultShutdownTimeout
	}

	if options.TraceEndpoint != "" {
		if err := validateEndpoint(options.TraceEndpoint); err != nil {
			return nil, fmt.Errorf("trace endpoint: %w", err)
		}
		exporterOptions := []traceexport.Option{
			traceexport.WithEndpointURL(options.TraceEndpoint),
		}
		if len(options.TraceHeaders) > 0 {
			exporterOptions = append(exporterOptions, traceexport.WithHeaders(options.TraceHeaders))
		}
		exporter, err := traceexport.New(ctx, exporterOptions...)
		if err != nil {
			return nil, fmt.Errorf("create OTLP trace exporter: %w", err)
		}
		provider := sdktrace.NewTracerProvider(
			sdktrace.WithBatcher(exporter),
			sdktrace.WithResource(res),
		)
		runtime.TracerProvider = provider
		runtime.shutdownTrace = provider.Shutdown
	}

	if options.MetricEndpoint != "" {
		if err := validateEndpoint(options.MetricEndpoint); err != nil {
			_ = runtime.Shutdown(context.Background())
			return nil, fmt.Errorf("metric endpoint: %w", err)
		}
		exporterOptions := []metricexport.Option{
			metricexport.WithEndpointURL(options.MetricEndpoint),
		}
		if len(options.MetricHeaders) > 0 {
			exporterOptions = append(exporterOptions, metricexport.WithHeaders(options.MetricHeaders))
		}
		exporter, err := metricexport.New(ctx, exporterOptions...)
		if err != nil {
			_ = runtime.Shutdown(context.Background())
			return nil, fmt.Errorf("create OTLP metric exporter: %w", err)
		}
		readerOptions := []sdkmetric.PeriodicReaderOption{}
		if options.MetricInterval > 0 {
			readerOptions = append(
				readerOptions,
				sdkmetric.WithInterval(options.MetricInterval),
			)
		}
		provider := sdkmetric.NewMeterProvider(
			sdkmetric.WithReader(sdkmetric.NewPeriodicReader(exporter, readerOptions...)),
			sdkmetric.WithResource(res),
		)
		runtime.MeterProvider = provider
		runtime.shutdownMetric = provider.Shutdown
	}
	return runtime, nil
}

func (r *Runtime) Shutdown(ctx context.Context) error {
	if r == nil {
		return nil
	}
	if _, hasDeadline := ctx.Deadline(); !hasDeadline && r.timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, r.timeout)
		defer cancel()
	}
	var errs []error
	if r.shutdownMetric != nil {
		errs = append(errs, r.shutdownMetric(ctx))
	}
	if r.shutdownTrace != nil {
		errs = append(errs, r.shutdownTrace(ctx))
	}
	r.shutdownMetric = nil
	r.shutdownTrace = nil
	return errors.Join(errs...)
}

func disabledRuntime() *Runtime {
	return &Runtime{
		Propagator: propagation.TraceContext{},
		timeout:    defaultShutdownTimeout,
	}
}

func validateEndpoint(value string) error {
	parsed, err := url.Parse(value)
	if err != nil {
		return err
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return fmt.Errorf("scheme must be http or https")
	}
	if parsed.Host == "" || parsed.User != nil || parsed.Fragment != "" {
		return fmt.Errorf("endpoint must have a host and no userinfo or fragment")
	}
	return nil
}

func signalEndpoint(base, path string) (string, error) {
	parsed, err := url.Parse(base)
	if err != nil {
		return "", err
	}
	if parsed.Path == "" || parsed.Path == "/" {
		parsed.Path = path
	} else {
		parsed.Path = strings.TrimRight(parsed.Path, "/") + path
	}
	return parsed.String(), nil
}

func parseHeaders(value string) (map[string]string, error) {
	result := map[string]string{}
	for raw := range strings.SplitSeq(value, ",") {
		raw = strings.TrimSpace(raw)
		if raw == "" {
			continue
		}
		key, encodedValue, ok := strings.Cut(raw, "=")
		if !ok || strings.TrimSpace(key) == "" {
			return nil, fmt.Errorf("header %q must be key=value", raw)
		}
		decodedKey, err := url.QueryUnescape(strings.TrimSpace(key))
		if err != nil {
			return nil, fmt.Errorf("decode header key: %w", err)
		}
		decodedValue, err := url.QueryUnescape(strings.TrimSpace(encodedValue))
		if err != nil {
			return nil, fmt.Errorf("decode header %q: %w", decodedKey, err)
		}
		if !validHeaderName(decodedKey) {
			return nil, fmt.Errorf("header name %q is invalid", decodedKey)
		}
		if strings.ContainsAny(decodedValue, "\r\n") {
			return nil, fmt.Errorf("header %q contains a line break", decodedKey)
		}
		result[http.CanonicalHeaderKey(decodedKey)] = decodedValue
	}
	return result, nil
}

func validHeaderName(value string) bool {
	if value == "" {
		return false
	}
	for index := range len(value) {
		char := value[index]
		switch {
		case char >= 'a' && char <= 'z',
			char >= 'A' && char <= 'Z',
			char >= '0' && char <= '9':
		case strings.ContainsRune("!#$%&'*+-.^_`|~", rune(char)):
		default:
			return false
		}
	}
	return true
}

func mergeHeaders(base map[string]string, override string) (map[string]string, error) {
	result := make(map[string]string, len(base))
	for key, value := range base {
		canonical := http.CanonicalHeaderKey(key)
		if !validHeaderName(canonical) || strings.ContainsAny(value, "\r\n") {
			return nil, fmt.Errorf("base header %q is invalid", key)
		}
		result[canonical] = value
	}
	specific, err := parseHeaders(override)
	if err != nil {
		return nil, err
	}
	for key, value := range specific {
		result[key] = value
	}
	return result, nil
}

func envBool(key string) bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv(key))) {
	case "1", "true", "yes", "on":
		return true
	default:
		return false
	}
}
