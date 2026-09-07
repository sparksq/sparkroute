// SPDX-License-Identifier: AGPL-3.0-only
// Copyright 2026 Scitrera LLC
// Copyright 2026 Fox Engine Ltd.

// Package mmprojection implements the authenticated, bounded multimedia
// request-transform contract used between SparkRoute and MMBridge.
package mmprojection

import (
	"bytes"
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/sparksq/sparkroute/pkg/credentials"
	"github.com/sparksq/sparkroute/pkg/modelrouter"
)

const (
	ProviderID = "pilco-mmbridge"

	HopHeader           = "X-SparkRun-MMProjection-Hop"
	AnalyzerModelHeader = "X-SparkRun-MMProjection-Analyzer-Model"
	TraceIDHeader       = "X-SparkRun-Trace-ID"

	AnalyzerChatPath      = "/internal/mmprojection-analyzer/v1/chat/completions"
	AnalyzerResponsesPath = "/internal/mmprojection-analyzer/v1/responses"

	defaultResponseBytes = int64(40 << 20)
	defaultProbeBytes    = int64(2 << 20)
	defaultProbeTimeout  = 10 * time.Second
	failureThreshold     = 3
	circuitDuration      = 30 * time.Second
)

type Operation string

const (
	OperationChat      Operation = "chat_completions"
	OperationResponses Operation = "responses"
)

func (o Operation) servicePath() (string, bool) {
	switch o {
	case OperationChat:
		return "/mmprojection/chat/completions", true
	case OperationResponses:
		return "/mmprojection/responses", true
	default:
		return "", false
	}
}

type Options struct {
	URL                  string
	TokenReference       credentials.Ref
	Credentials          credentials.Source
	DefaultAnalyzerModel string
	DefaultTimeout       time.Duration
	MaxResponseBytes     int64
	HTTPClient           *http.Client
}

type Client struct {
	baseURL              string
	tokenReference       credentials.Ref
	credentials          credentials.Source
	defaultAnalyzerModel string
	defaultTimeout       time.Duration
	maxResponseBytes     int64
	client               *http.Client

	mu             sync.RWMutex
	failures       int
	openUntil      time.Time
	state          string
	lastAttempt    time.Time
	lastSuccess    time.Time
	lastHTTPStatus int
	lastLatencyMS  int64
	lastError      string
	projectionAPI  int
	models         int
}

// Status is the bounded, content-free projection runtime projection exposed to
// administrators. It intentionally omits the bridge URL, credential reference,
// discovered model names, response bodies, and raw transport errors.
type Status struct {
	Configured          bool       `json:"configured"`
	State               string     `json:"state"`
	Provider            string     `json:"provider,omitempty"`
	AnalyzerModel       string     `json:"analyzer_model,omitempty"`
	ProjectionAPI       int        `json:"projection_api,omitempty"`
	Models              int        `json:"models"`
	ConsecutiveFailures int        `json:"consecutive_failures"`
	CircuitOpen         bool       `json:"circuit_open"`
	CircuitOpenUntil    *time.Time `json:"circuit_open_until,omitempty"`
	LastAttemptAt       *time.Time `json:"last_attempt_at,omitempty"`
	LastSuccessAt       *time.Time `json:"last_success_at,omitempty"`
	LastHTTPStatus      int        `json:"last_http_status,omitempty"`
	LastLatencyMS       int64      `json:"last_latency_ms,omitempty"`
	LastError           string     `json:"last_error,omitempty"`
}

// StatusProber is the narrow admin-control-plane contract implemented by a
// projection client.
type StatusProber interface {
	Status() Status
	Probe(context.Context) (Status, error)
}

// DisabledStatus provides an explicit stable value when projection execution
// is not configured in this process.
func DisabledStatus() Status {
	return Status{State: "disabled", Models: 0}
}

type responseEnvelope struct {
	Version        int                     `json:"version"`
	Applied        bool                    `json:"applied"`
	MediaCount     int                     `json:"media_count"`
	CacheHit       bool                    `json:"cache_hit"`
	AnalyzerModel  string                  `json:"analyzer_model"`
	Body           json.RawMessage         `json:"body"`
	TextInspection *textInspectionEnvelope `json:"text_inspection,omitempty"`
}

type textInspectionEnvelope struct {
	Version    int    `json:"version"`
	State      string `json:"state"`
	MediaCount int    `json:"media_count"`
}

// Error distinguishes invalid caller media from bridge/runtime failures. The
// gateway always rejects client errors even when a policy otherwise permits
// operational fail-open behavior.
type Error struct {
	Status  int
	Message string
}

func (e *Error) Error() string { return e.Message }

func (e *Error) ClientRequestError() bool {
	switch e.Status {
	case http.StatusBadRequest, http.StatusRequestEntityTooLarge,
		http.StatusUnsupportedMediaType, http.StatusUnprocessableEntity:
		return true
	default:
		return false
	}
}

func New(options Options) (*Client, error) {
	baseURL, err := normalizeURL(options.URL)
	if err != nil {
		return nil, err
	}
	if options.TokenReference == "" {
		return nil, errors.New("MM projection token credential reference is required")
	}
	if err := options.TokenReference.Validate(); err != nil {
		return nil, fmt.Errorf("MM projection token credential reference: %w", err)
	}
	if options.Credentials == nil {
		return nil, errors.New("MM projection credential source is required")
	}
	if err := validateModel(options.DefaultAnalyzerModel); err != nil {
		return nil, fmt.Errorf("default MM projection analyzer model: %w", err)
	}
	defaultTimeout := options.DefaultTimeout
	if defaultTimeout <= 0 {
		defaultTimeout = 10 * time.Minute
	}
	maxResponseBytes := options.MaxResponseBytes
	if maxResponseBytes <= 0 {
		maxResponseBytes = defaultResponseBytes
	}
	client := options.HTTPClient
	if client == nil {
		client = &http.Client{Transport: &http.Transport{
			DialContext: (&net.Dialer{
				Timeout: 5 * time.Second, KeepAlive: 30 * time.Second,
			}).DialContext,
			TLSHandshakeTimeout:   5 * time.Second,
			IdleConnTimeout:       90 * time.Second,
			MaxIdleConnsPerHost:   8,
			ExpectContinueTimeout: time.Second,
		}}
	}
	clientCopy := *client
	clientCopy.CheckRedirect = func(_ *http.Request, _ []*http.Request) error {
		return http.ErrUseLastResponse
	}
	return &Client{
		baseURL: baseURL, tokenReference: options.TokenReference,
		credentials:          options.Credentials,
		defaultAnalyzerModel: strings.TrimSpace(options.DefaultAnalyzerModel),
		defaultTimeout:       defaultTimeout, maxResponseBytes: maxResponseBytes,
		client: &clientCopy, state: "unknown",
	}, nil
}

func normalizeURL(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", errors.New("MM projection URL is required")
	}
	parsed, err := url.Parse(raw)
	if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") ||
		parsed.Host == "" || parsed.User != nil || parsed.RawQuery != "" ||
		parsed.Fragment != "" {
		return "", errors.New("MM projection URL must be an http(s) base URL without credentials, query, or fragment")
	}
	parsed.Path = strings.TrimRight(parsed.Path, "/")
	return parsed.String(), nil
}

func validateModel(model string) error {
	model = strings.TrimSpace(model)
	if len(model) > 256 {
		return errors.New("must not exceed 256 bytes")
	}
	for _, char := range model {
		if char < 32 || char == 127 {
			return errors.New("contains control characters")
		}
	}
	return nil
}

func (c *Client) DefaultAnalyzerModel() string {
	if c == nil {
		return ""
	}
	return c.defaultAnalyzerModel
}

// Status returns a redacted snapshot without resolving credentials or making a
// network request.
func (c *Client) Status() Status {
	if c == nil {
		return DisabledStatus()
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	now := time.Now()
	state := c.state
	open := now.Before(c.openUntil)
	if open {
		state = "circuit_open"
	}
	result := Status{
		Configured: true, State: state, Provider: ProviderID,
		AnalyzerModel: c.defaultAnalyzerModel, ProjectionAPI: c.projectionAPI,
		Models: c.models, ConsecutiveFailures: c.failures,
		CircuitOpen: open, LastHTTPStatus: c.lastHTTPStatus,
		LastLatencyMS: c.lastLatencyMS, LastError: c.lastError,
	}
	if open {
		result.CircuitOpenUntil = optionalTime(c.openUntil)
	}
	result.LastAttemptAt = optionalTime(c.lastAttempt)
	result.LastSuccessAt = optionalTime(c.lastSuccess)
	return result
}

// Probe verifies the authenticated MMBridge discovery and capability contract.
// An explicit operator probe bypasses the local circuit so it can recover a
// client immediately after the bridge returns.
func (c *Client) Probe(parent context.Context) (Status, error) {
	if c == nil {
		return DisabledStatus(), errors.New("MM projection is not configured")
	}
	ctx, cancel := context.WithTimeout(parent, defaultProbeTimeout)
	defer cancel()
	started := c.beginAttempt()
	token, err := c.resolveToken(ctx)
	if err != nil {
		c.recordOperationalFailure(parent, "credential_unavailable", 0, elapsedMS(started))
		return c.Status(), errors.New("MM projection probe failed")
	}
	modelsPayload, status, err := c.probeGET(ctx, token, "/models", defaultProbeBytes)
	if err != nil {
		c.recordOperationalFailure(parent, probeFailureReason(err), status, elapsedMS(started))
		return c.Status(), errors.New("MM projection probe failed")
	}
	var models struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if json.Unmarshal(modelsPayload, &models) != nil || len(models.Data) == 0 || len(models.Data) > 10000 {
		c.recordOperationalFailure(parent, "invalid_contract", status, elapsedMS(started))
		return c.Status(), errors.New("MM projection probe failed")
	}
	validModels := 0
	for _, model := range models.Data {
		if err := validateModel(model.ID); err == nil && strings.TrimSpace(model.ID) != "" {
			validModels++
		}
	}
	if validModels == 0 {
		c.recordOperationalFailure(parent, "invalid_contract", status, elapsedMS(started))
		return c.Status(), errors.New("MM projection probe failed")
	}
	capabilityPayload, status, err := c.probeGET(ctx, token, "/mmprojection/capabilities", 1<<20)
	if err != nil {
		c.recordOperationalFailure(parent, probeFailureReason(err), status, elapsedMS(started))
		return c.Status(), errors.New("MM projection probe failed")
	}
	var capability struct {
		Name          string   `json:"name"`
		ProjectionAPI int      `json:"projection_api"`
		Endpoints     []string `json:"endpoints"`
	}
	if json.Unmarshal(capabilityPayload, &capability) != nil ||
		capability.Name != ProviderID || capability.ProjectionAPI != 1 ||
		!containsAll(capability.Endpoints, "/v1/chat/completions", "/v1/responses") {
		c.recordOperationalFailure(parent, "invalid_contract", status, elapsedMS(started))
		return c.Status(), errors.New("MM projection probe failed")
	}
	c.recordProbeSuccess(status, elapsedMS(started), capability.ProjectionAPI, validModels)
	return c.Status(), nil
}

func (c *Client) Project(
	parent context.Context,
	body []byte,
	policy modelrouter.MMProjectionPolicy,
	operation Operation,
	traceID string,
) ([]byte, *modelrouter.MMProjectionTrace, error) {
	mode := strings.TrimSpace(policy.FailureMode)
	if mode == "" {
		mode = "fallback"
	}
	trace := &modelrouter.MMProjectionTrace{
		ProviderID: ProviderID, State: "failed", FailureMode: mode,
	}
	path, supported := operation.servicePath()
	if !supported {
		err := errors.New("MM projection does not support this operation")
		trace.Error = compactError(err.Error())
		return nil, trace, err
	}
	if err := c.circuitError(); err != nil {
		c.recordCircuitBlocked()
		trace.Error = err.Error()
		return nil, trace, err
	}
	timeout := c.defaultTimeout
	if policy.TimeoutMS > 0 {
		timeout = time.Duration(policy.TimeoutMS) * time.Millisecond
	}
	ctx, cancel := context.WithTimeout(parent, timeout)
	defer cancel()
	started := c.beginAttempt()
	token, err := c.resolveToken(ctx)
	if err != nil {
		trace.DurationMS = time.Since(started).Milliseconds()
		trace.Error = "MM projection credential is unavailable"
		c.recordOperationalFailure(parent, "credential_unavailable", 0, trace.DurationMS)
		return nil, trace, errors.New(trace.Error)
	}
	req, err := http.NewRequestWithContext(
		ctx, http.MethodPost, c.baseURL+path, bytes.NewReader(body),
	)
	if err != nil {
		trace.Error = compactError(err.Error())
		return nil, trace, err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Accept-Encoding", "identity")
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set(HopHeader, "1")
	if traceID = strings.TrimSpace(traceID); traceID != "" {
		req.Header.Set(TraceIDHeader, truncate(traceID, 128))
	}
	analyzerModel := strings.TrimSpace(policy.AnalyzerModel)
	if analyzerModel != "" {
		req.Header.Set(AnalyzerModelHeader, analyzerModel)
	}
	response, err := c.client.Do(req)
	trace.DurationMS = time.Since(started).Milliseconds()
	if err != nil {
		err = transportError(err)
		trace.Error = err.Error()
		c.recordOperationalFailure(parent, "transport_unavailable", 0, trace.DurationMS)
		return nil, trace, err
	}
	defer response.Body.Close()
	trace.HTTPStatus = response.StatusCode
	trace.RequestID = truncate(strings.TrimSpace(response.Header.Get("X-MM-Bridge-Request-ID")), 256)
	payload, readErr := io.ReadAll(io.LimitReader(response.Body, c.maxResponseBytes+1))
	if readErr != nil {
		trace.Error = compactError(readErr.Error())
		c.recordOperationalFailure(parent, "response_read_failed", response.StatusCode, trace.DurationMS)
		return nil, trace, readErr
	}
	if int64(len(payload)) > c.maxResponseBytes {
		err = errors.New("MM projection response exceeds size limit")
		trace.Error = err.Error()
		c.recordOperationalFailure(parent, "response_too_large", response.StatusCode, trace.DurationMS)
		return nil, trace, err
	}
	if response.StatusCode/100 != 2 {
		message := fmt.Sprintf("MM projection service returned HTTP %d", response.StatusCode)
		typed := &Error{Status: response.StatusCode, Message: message}
		if typed.ClientRequestError() {
			typed.Message = compactUpstreamError(payload, message)
			trace.Error = typed.Message
			c.recordNeutral(response.StatusCode, trace.DurationMS)
			return nil, trace, typed
		}
		trace.Error = message
		c.recordOperationalFailure(parent, "http_error", response.StatusCode, trace.DurationMS)
		return nil, trace, typed
	}
	var result responseEnvelope
	if err := json.Unmarshal(payload, &result); err != nil {
		err = fmt.Errorf("invalid MM projection response: %w", err)
		trace.Error = compactError(err.Error())
		c.recordOperationalFailure(parent, "invalid_contract", response.StatusCode, trace.DurationMS)
		return nil, trace, err
	}
	if result.Version != 1 || result.MediaCount < 0 || !isJSONObject(result.Body) ||
		validateModel(result.AnalyzerModel) != nil {
		err = errors.New("invalid MM projection response contract")
		trace.Error = err.Error()
		c.recordOperationalFailure(parent, "invalid_contract", response.StatusCode, trace.DurationMS)
		return nil, trace, err
	}
	if result.TextInspection != nil {
		inspection := result.TextInspection
		if inspection.Version != 1 || inspection.MediaCount < 0 ||
			(inspection.State != "complete" && inspection.State != "partial") ||
			inspection.MediaCount > result.MediaCount {
			err = errors.New("invalid MM projection text inspection contract")
			trace.Error = err.Error()
			c.recordOperationalFailure(parent, "invalid_contract", response.StatusCode, trace.DurationMS)
			return nil, trace, err
		}
		trace.TextInspectionState = inspection.State
		trace.InspectedMedia = inspection.MediaCount
	}
	trace.MediaCount = result.MediaCount
	trace.CacheHit = result.CacheHit
	trace.AnalyzerModel = truncate(strings.TrimSpace(result.AnalyzerModel), 256)
	if result.Applied {
		trace.State = "applied"
	} else {
		trace.State = "not_needed"
	}
	c.recordSuccess(response.StatusCode, trace.DurationMS)
	return append([]byte(nil), result.Body...), trace, nil
}

// Authenticate validates one protected analyzer callback using the same
// rotating credential as bridge calls. It never returns the credential.
func (c *Client) Authenticate(ctx context.Context, authorization string) (bool, error) {
	if c == nil {
		return false, nil
	}
	token, err := c.resolveToken(ctx)
	if err != nil {
		return false, err
	}
	want := "Bearer " + token
	got := strings.TrimSpace(authorization)
	if len(got) != len(want) {
		return false, nil
	}
	return subtle.ConstantTimeCompare([]byte(got), []byte(want)) == 1, nil
}

func (c *Client) resolveToken(ctx context.Context) (string, error) {
	material, err := c.credentials.Resolve(ctx, c.tokenReference)
	if err != nil {
		return "", err
	}
	token := strings.TrimSpace(string(material.Value))
	if token == "" {
		return "", errors.New("resolved MM projection credential is empty")
	}
	return token, nil
}

func (c *Client) circuitError() error {
	c.mu.RLock()
	until := c.openUntil
	c.mu.RUnlock()
	if time.Now().Before(until) {
		return errors.New("MM projection circuit is temporarily open")
	}
	return nil
}

func (c *Client) beginAttempt() time.Time {
	started := time.Now()
	c.mu.Lock()
	c.lastAttempt = started.UTC()
	c.mu.Unlock()
	return started
}

func (c *Client) recordFailure() {
	c.mu.Lock()
	c.failures++
	c.state = "unavailable"
	if c.failures >= failureThreshold {
		c.openUntil = time.Now().Add(circuitDuration)
	}
	c.mu.Unlock()
}

func (c *Client) recordOperationalFailure(
	parent context.Context,
	reason string,
	httpStatus int,
	latencyMS int64,
) {
	if parent.Err() != nil {
		return
	}
	c.recordFailure()
	c.mu.Lock()
	c.lastError = reason
	c.lastHTTPStatus = httpStatus
	c.lastLatencyMS = latencyMS
	c.mu.Unlock()
}

func (c *Client) recordSuccess(httpStatus int, latencyMS int64) {
	c.mu.Lock()
	c.failures = 0
	c.openUntil = time.Time{}
	c.state = "healthy"
	c.lastSuccess = time.Now().UTC()
	c.lastHTTPStatus = httpStatus
	c.lastLatencyMS = latencyMS
	c.lastError = ""
	c.projectionAPI = 1
	c.mu.Unlock()
}

func (c *Client) recordProbeSuccess(httpStatus int, latencyMS int64, projectionAPI, models int) {
	c.mu.Lock()
	c.failures = 0
	c.openUntil = time.Time{}
	c.state = "healthy"
	c.lastSuccess = time.Now().UTC()
	c.lastHTTPStatus = httpStatus
	c.lastLatencyMS = latencyMS
	c.lastError = ""
	c.projectionAPI = projectionAPI
	c.models = models
	c.mu.Unlock()
}

func (c *Client) recordNeutral(httpStatus int, latencyMS int64) {
	c.mu.Lock()
	c.lastHTTPStatus = httpStatus
	c.lastLatencyMS = latencyMS
	c.mu.Unlock()
}

func (c *Client) recordCircuitBlocked() {
	c.beginAttempt()
	c.mu.Lock()
	c.lastError = "circuit_open"
	c.lastHTTPStatus = 0
	c.lastLatencyMS = 0
	c.mu.Unlock()
}

func (c *Client) probeGET(
	ctx context.Context,
	token, path string,
	maximumBytes int64,
) ([]byte, int, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+path, nil)
	if err != nil {
		return nil, 0, err
	}
	request.Header.Set("Accept", "application/json")
	request.Header.Set("Accept-Encoding", "identity")
	request.Header.Set("Authorization", "Bearer "+token)
	request.Header.Set(HopHeader, "1")
	response, err := c.client.Do(request)
	if err != nil {
		return nil, 0, transportError(err)
	}
	defer response.Body.Close()
	payload, readErr := io.ReadAll(io.LimitReader(response.Body, maximumBytes+1))
	if readErr != nil {
		return nil, response.StatusCode, readErr
	}
	if int64(len(payload)) > maximumBytes {
		return nil, response.StatusCode, errors.New("probe response too large")
	}
	if response.StatusCode/100 != 2 {
		return nil, response.StatusCode, errors.New("probe HTTP error")
	}
	return payload, response.StatusCode, nil
}

func probeFailureReason(err error) string {
	message := err.Error()
	switch {
	case strings.Contains(message, "timed out"):
		return "timeout"
	case strings.Contains(message, "too large"):
		return "response_too_large"
	case strings.Contains(message, "HTTP"):
		return "http_error"
	default:
		return "transport_unavailable"
	}
}

func containsAll(values []string, required ...string) bool {
	seen := make(map[string]bool, len(values))
	for _, value := range values {
		seen[strings.TrimSpace(value)] = true
	}
	for _, value := range required {
		if !seen[value] {
			return false
		}
	}
	return true
}

func elapsedMS(started time.Time) int64 {
	return time.Since(started).Milliseconds()
}

func optionalTime(value time.Time) *time.Time {
	if value.IsZero() {
		return nil
	}
	result := value.UTC()
	return &result
}

func transportError(err error) error {
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		return errors.New("MM projection service timed out")
	case errors.Is(err, context.Canceled):
		return errors.New("MM projection request was canceled")
	default:
		return errors.New("MM projection service is unavailable")
	}
}

func compactUpstreamError(payload []byte, fallback string) string {
	var envelope struct {
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if json.Unmarshal(payload, &envelope) == nil && strings.TrimSpace(envelope.Error.Message) != "" {
		return compactError(envelope.Error.Message)
	}
	return fallback
}

func compactError(message string) string {
	message = strings.TrimSpace(strings.ReplaceAll(message, "\n", " "))
	return truncate(message, 500)
}

func truncate(value string, maximum int) string {
	if len(value) <= maximum {
		return value
	}
	if maximum <= 3 {
		return value[:maximum]
	}
	return value[:maximum-3] + "..."
}

func isJSONObject(raw []byte) bool {
	trimmed := bytes.TrimSpace(raw)
	return len(trimmed) >= 2 && trimmed[0] == '{' &&
		trimmed[len(trimmed)-1] == '}' && json.Valid(trimmed)
}
