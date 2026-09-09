// SPDX-FileCopyrightText: 2026 Scitrera LLC
// SPDX-FileCopyrightText: 2026 Fox Engine Ltd.
// SPDX-License-Identifier: AGPL-3.0-only

// Command loadgen runs a closed-loop HTTP benchmark and emits both per-request
// JSONL evidence and a machine-readable summary. It is intentionally generic:
// the target may be SparkRoute, Envoy, Traefik, or another HTTP proxy.
package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/http/httptrace"
	"net/url"
	"os"
	"os/exec"
	"os/signal"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

const defaultMaxResponseBytes = int64(16 << 20)

type headerFlags http.Header

func (h *headerFlags) String() string { return fmt.Sprint(http.Header(*h)) }

func (h *headerFlags) Set(value string) error {
	name, raw, found := strings.Cut(value, ":")
	name = http.CanonicalHeaderKey(strings.TrimSpace(name))
	if !found || name == "" {
		return fmt.Errorf("header must use Name: value syntax")
	}
	if *h == nil {
		*h = headerFlags(make(http.Header))
	}
	http.Header(*h).Add(name, strings.TrimSpace(raw))
	return nil
}

type options struct {
	candidate        string
	targetURL        string
	scenario         string
	duration         time.Duration
	warmup           time.Duration
	concurrency      int
	timeout          time.Duration
	body             []byte
	headers          http.Header
	expect           string
	requestIDHeader  string
	maxResponseBytes int64
	requestsOutput   string
	summaryOutput    string
	processPID       int
	profileURL       string
	profileOutput    string
}

type requestResult struct {
	RunID       string    `json:"run_id"`
	Phase       string    `json:"phase"`
	Sequence    uint64    `json:"sequence"`
	StartedAt   time.Time `json:"started_at"`
	CompletedAt time.Time `json:"completed_at"`
	Status      int       `json:"status,omitempty"`
	Success     bool      `json:"success"`
	LatencyUS   int64     `json:"latency_us"`
	TTFBUS      int64     `json:"ttfb_us,omitempty"`
	Bytes       int64     `json:"bytes"`
	RequestID   string    `json:"request_id,omitempty"`
	Error       string    `json:"error,omitempty"`
}

type phaseSummary struct {
	DurationSeconds float64        `json:"duration_seconds"`
	Attempted       uint64         `json:"attempted"`
	Succeeded       uint64         `json:"succeeded"`
	Failed          uint64         `json:"failed"`
	MissingIDs      uint64         `json:"missing_request_ids"`
	Bytes           uint64         `json:"response_bytes"`
	RequestsPerSec  float64        `json:"requests_per_second"`
	StatusCounts    map[string]int `json:"status_counts"`
	ErrorCounts     map[string]int `json:"error_counts,omitempty"`
	Latency         distribution   `json:"latency_us"`
	TTFB            distribution   `json:"ttfb_us"`
}

type distribution struct {
	Minimum int64 `json:"minimum"`
	P50     int64 `json:"p50"`
	P95     int64 `json:"p95"`
	P99     int64 `json:"p99"`
	Maximum int64 `json:"maximum"`
}

type processSummary struct {
	PID               int     `json:"pid"`
	CPUSeconds        float64 `json:"cpu_seconds"`
	AverageCPUCores   float64 `json:"average_cpu_cores"`
	CPUUSPerSuccess   float64 `json:"cpu_us_per_success,omitempty"`
	PeakRSSBytes      int64   `json:"peak_rss_bytes"`
	ClockTicksPerSec  int64   `json:"clock_ticks_per_second"`
	SamplingErrorText string  `json:"sampling_error,omitempty"`
}

type summary struct {
	SchemaVersion int             `json:"schema_version"`
	RunID         string          `json:"run_id"`
	Candidate     string          `json:"candidate"`
	TargetURL     string          `json:"target_url"`
	Scenario      string          `json:"scenario"`
	Concurrency   int             `json:"concurrency"`
	StartedAt     time.Time       `json:"started_at"`
	CompletedAt   time.Time       `json:"completed_at"`
	Warmup        phaseSummary    `json:"warmup"`
	Measurement   phaseSummary    `json:"measurement"`
	Process       *processSummary `json:"process,omitempty"`
	ProfileOutput string          `json:"profile_output,omitempty"`
	ProfileError  string          `json:"profile_error,omitempty"`
	GoVersion     string          `json:"go_version"`
	GOOS          string          `json:"goos"`
	GOARCH        string          `json:"goarch"`
}

type phaseAccumulator struct {
	mu        sync.Mutex
	results   []requestResult
	statuses  map[string]int
	errors    map[string]int
	attempted uint64
	succeeded uint64
	missingID uint64
	bytes     uint64
}

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "loadgen:", err)
		os.Exit(1)
	}
}

func run() error {
	var headers headerFlags
	var bodyFile string
	var opts options
	flag.StringVar(&opts.candidate, "candidate", "candidate", "candidate name recorded in output")
	flag.StringVar(&opts.targetURL, "url", "", "exact inference endpoint URL")
	flag.StringVar(&opts.scenario, "scenario", "unary", "scenario label: unary or stream")
	flag.DurationVar(&opts.duration, "duration", 30*time.Second, "measurement duration")
	flag.DurationVar(&opts.warmup, "warmup", 2*time.Second, "warm-up duration")
	flag.IntVar(&opts.concurrency, "concurrency", 1, "closed-loop worker count")
	flag.DurationVar(&opts.timeout, "timeout", 2*time.Minute, "per-request timeout")
	flag.StringVar(&bodyFile, "body-file", "", "request body file; defaults to a Chat Completions request")
	flag.Var(&headers, "header", "request header in Name: value form; repeatable")
	flag.StringVar(&opts.expect, "expect", "", "response substring required for success")
	flag.StringVar(&opts.requestIDHeader, "request-id-header", "X-Request-Id", "gateway response request-ID header")
	flag.Int64Var(&opts.maxResponseBytes, "max-response-bytes", defaultMaxResponseBytes, "maximum response bytes read per request")
	flag.StringVar(&opts.requestsOutput, "requests-output", "requests.jsonl", "per-request JSONL output path")
	flag.StringVar(&opts.summaryOutput, "summary-output", "summary.json", "summary JSON output path")
	flag.IntVar(&opts.processPID, "process-pid", 0, "optional candidate host PID for Linux CPU/RSS accounting")
	flag.StringVar(&opts.profileURL, "pprof-url", "", "optional Go CPU profile URL or operations base URL")
	flag.StringVar(&opts.profileOutput, "pprof-output", "", "CPU profile output; required with -pprof-url")
	flag.Parse()

	if err := validateOptions(&opts); err != nil {
		return err
	}
	if bodyFile == "" {
		opts.body = defaultBody(opts.scenario == "stream")
	} else {
		body, err := os.ReadFile(bodyFile)
		if err != nil {
			return fmt.Errorf("read request body: %w", err)
		}
		opts.body = body
	}
	opts.headers = make(http.Header)
	for name, values := range http.Header(headers) {
		opts.headers[name] = append([]string(nil), values...)
	}
	if opts.headers.Get("Content-Type") == "" {
		opts.headers.Set("Content-Type", "application/json")
	}

	requestFile, err := createOutput(opts.requestsOutput)
	if err != nil {
		return err
	}
	defer func() { _ = requestFile.Close() }()
	encoder := json.NewEncoder(requestFile)
	var outputMu sync.Mutex

	runID, err := randomID()
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	transport := &http.Transport{
		Proxy:                 http.ProxyFromEnvironment,
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          max(128, opts.concurrency*2),
		MaxIdleConnsPerHost:   max(64, opts.concurrency),
		MaxConnsPerHost:       0,
		IdleConnTimeout:       90 * time.Second,
		ResponseHeaderTimeout: opts.timeout,
	}
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport}

	startedAt := time.Now().UTC()
	warmup, _, err := runPhase(ctx, client, opts, runID, "warmup", opts.warmup, encoder, &outputMu)
	if err != nil {
		return err
	}

	var sampler *processSampler
	if opts.processPID > 0 {
		sampler, err = startProcessSampler(opts.processPID)
		if err != nil {
			return fmt.Errorf("start process sampler: %w", err)
		}
	}
	profileDone := make(chan error, 1)
	if opts.profileURL != "" {
		go func() {
			profileDone <- captureCPUProfile(ctx, opts.profileURL, opts.profileOutput, opts.duration)
		}()
	}
	measurement, measurementElapsed, err := runPhase(
		ctx, client, opts, runID, "measurement", opts.duration, encoder, &outputMu,
	)
	if err != nil {
		return err
	}

	var process *processSummary
	if sampler != nil {
		process = sampler.stop(measurementElapsed, measurement.Succeeded)
	}
	result := summary{
		SchemaVersion: 1,
		RunID:         runID,
		Candidate:     opts.candidate,
		TargetURL:     opts.targetURL,
		Scenario:      opts.scenario,
		Concurrency:   opts.concurrency,
		StartedAt:     startedAt,
		CompletedAt:   time.Now().UTC(),
		Warmup:        warmup,
		Measurement:   measurement,
		Process:       process,
		GoVersion:     runtime.Version(),
		GOOS:          runtime.GOOS,
		GOARCH:        runtime.GOARCH,
	}
	if opts.profileURL != "" {
		result.ProfileOutput = opts.profileOutput
		if profileErr := <-profileDone; profileErr != nil {
			result.ProfileError = profileErr.Error()
		}
	}
	if err := requestFile.Sync(); err != nil {
		return fmt.Errorf("sync request evidence: %w", err)
	}
	if err := writeJSON(opts.summaryOutput, result); err != nil {
		return err
	}
	return json.NewEncoder(os.Stdout).Encode(result)
}

func validateOptions(opts *options) error {
	if strings.TrimSpace(opts.candidate) == "" {
		return fmt.Errorf("-candidate is required")
	}
	parsed, err := url.ParseRequestURI(opts.targetURL)
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		return fmt.Errorf("-url must be an absolute HTTP(S) URL")
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return fmt.Errorf("-url scheme must be http or https")
	}
	if opts.scenario != "unary" && opts.scenario != "stream" {
		return fmt.Errorf("-scenario must be unary or stream")
	}
	if opts.duration <= 0 || opts.warmup < 0 || opts.timeout <= 0 {
		return fmt.Errorf("durations and timeout must be positive; warmup may be zero")
	}
	if opts.concurrency <= 0 || opts.concurrency > 1_000_000 {
		return fmt.Errorf("-concurrency must be between 1 and 1000000")
	}
	if opts.maxResponseBytes <= 0 {
		return fmt.Errorf("-max-response-bytes must be positive")
	}
	if strings.TrimSpace(opts.requestsOutput) == "" || strings.TrimSpace(opts.summaryOutput) == "" {
		return fmt.Errorf("output paths are required")
	}
	if opts.profileURL != "" && opts.profileOutput == "" {
		return fmt.Errorf("-pprof-output is required with -pprof-url")
	}
	return nil
}

func defaultBody(stream bool) []byte {
	body := fmt.Sprintf(
		`{"model":"benchmark","messages":[{"role":"user","content":"benchmark"}],"stream":%t}`,
		stream,
	)
	return []byte(body)
}

func runPhase(
	ctx context.Context,
	client *http.Client,
	opts options,
	runID string,
	phase string,
	duration time.Duration,
	encoder *json.Encoder,
	outputMu *sync.Mutex,
) (phaseSummary, time.Duration, error) {
	if duration == 0 {
		return phaseSummary{StatusCounts: map[string]int{}}, 0, nil
	}
	accumulator := &phaseAccumulator{
		statuses: make(map[string]int),
		errors:   make(map[string]int),
	}
	started := time.Now()
	stopAt := started.Add(duration)
	var sequence atomic.Uint64
	var workers sync.WaitGroup
	workers.Add(opts.concurrency)
	for range opts.concurrency {
		go func() {
			defer workers.Done()
			for time.Now().Before(stopAt) && ctx.Err() == nil {
				result := executeRequest(ctx, client, opts, runID, phase, sequence.Add(1))
				accumulator.add(result)
				outputMu.Lock()
				encodeErr := encoder.Encode(result)
				outputMu.Unlock()
				if encodeErr != nil {
					accumulator.addOutputError(encodeErr)
					return
				}
			}
		}()
	}
	workers.Wait()
	elapsed := time.Since(started)
	if ctx.Err() != nil {
		return phaseSummary{}, elapsed, ctx.Err()
	}
	if err := accumulator.outputError(); err != nil {
		return phaseSummary{}, elapsed, err
	}
	return accumulator.summary(elapsed), elapsed, nil
}

func executeRequest(
	ctx context.Context,
	client *http.Client,
	opts options,
	runID, phase string,
	sequence uint64,
) requestResult {
	started := time.Now()
	result := requestResult{
		RunID: runID, Phase: phase, Sequence: sequence, StartedAt: started.UTC(),
	}
	requestCtx, cancel := context.WithTimeout(ctx, opts.timeout)
	defer cancel()
	var firstByte time.Time
	requestCtx = httptrace.WithClientTrace(requestCtx, &httptrace.ClientTrace{
		GotFirstResponseByte: func() { firstByte = time.Now() },
	})
	request, err := http.NewRequestWithContext(
		requestCtx, http.MethodPost, opts.targetURL, bytes.NewReader(opts.body),
	)
	if err != nil {
		return finishRequest(result, started, firstByte, 0, 0, "construct: "+err.Error())
	}
	request.Header = opts.headers.Clone()
	request.Header.Set("User-Agent", "sparkroute-blackbox/1 "+runID)
	response, err := client.Do(request)
	if err != nil {
		return finishRequest(result, started, firstByte, 0, 0, classifyError(err))
	}
	defer func() { _ = response.Body.Close() }()
	body, readErr := io.ReadAll(io.LimitReader(response.Body, opts.maxResponseBytes+1))
	if readErr != nil {
		return finishRequest(result, started, firstByte, response.StatusCode, int64(len(body)), "read: "+readErr.Error())
	}
	if int64(len(body)) > opts.maxResponseBytes {
		return finishRequest(result, started, firstByte, response.StatusCode, int64(len(body)), "response_too_large")
	}
	result.RequestID = strings.TrimSpace(response.Header.Get(opts.requestIDHeader))
	errorText := ""
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		errorText = "http_status"
	} else if opts.expect != "" && !bytes.Contains(body, []byte(opts.expect)) {
		errorText = "expected_content_missing"
	}
	return finishRequest(result, started, firstByte, response.StatusCode, int64(len(body)), errorText)
}

func finishRequest(
	result requestResult,
	started, firstByte time.Time,
	status int,
	bytesRead int64,
	errorText string,
) requestResult {
	completed := time.Now()
	result.CompletedAt = completed.UTC()
	result.Status = status
	result.Bytes = bytesRead
	result.LatencyUS = completed.Sub(started).Microseconds()
	if !firstByte.IsZero() {
		result.TTFBUS = firstByte.Sub(started).Microseconds()
	}
	result.Error = errorText
	result.Success = errorText == ""
	return result
}

func classifyError(err error) string {
	if errors.Is(err, context.DeadlineExceeded) {
		return "timeout"
	}
	if errors.Is(err, context.Canceled) {
		return "cancelled"
	}
	return "transport: " + err.Error()
}

func (a *phaseAccumulator) add(result requestResult) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.results = append(a.results, result)
	a.attempted++
	a.bytes += uint64(max(0, result.Bytes))
	if result.Status != 0 {
		a.statuses[strconv.Itoa(result.Status)]++
	}
	if result.Success {
		a.succeeded++
		if result.RequestID == "" {
			a.missingID++
		}
	} else {
		a.errors[result.Error]++
	}
}

func (a *phaseAccumulator) addOutputError(err error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.errors["output: "+err.Error()]++
}

func (a *phaseAccumulator) outputError() error {
	a.mu.Lock()
	defer a.mu.Unlock()
	for name := range a.errors {
		if strings.HasPrefix(name, "output: ") {
			return errors.New(name)
		}
	}
	return nil
}

func (a *phaseAccumulator) summary(elapsed time.Duration) phaseSummary {
	a.mu.Lock()
	defer a.mu.Unlock()
	latencies := make([]int64, 0, len(a.results))
	ttfb := make([]int64, 0, len(a.results))
	for _, result := range a.results {
		latencies = append(latencies, result.LatencyUS)
		if result.TTFBUS > 0 {
			ttfb = append(ttfb, result.TTFBUS)
		}
	}
	failed := a.attempted - a.succeeded
	return phaseSummary{
		DurationSeconds: elapsed.Seconds(),
		Attempted:       a.attempted,
		Succeeded:       a.succeeded,
		Failed:          failed,
		MissingIDs:      a.missingID,
		Bytes:           a.bytes,
		RequestsPerSec:  float64(a.succeeded) / elapsed.Seconds(),
		StatusCounts:    cloneCounts(a.statuses),
		ErrorCounts:     cloneCounts(a.errors),
		Latency:         summarizeDistribution(latencies),
		TTFB:            summarizeDistribution(ttfb),
	}
}

func summarizeDistribution(values []int64) distribution {
	if len(values) == 0 {
		return distribution{}
	}
	sort.Slice(values, func(i, j int) bool { return values[i] < values[j] })
	return distribution{
		Minimum: values[0],
		P50:     percentile(values, 0.50),
		P95:     percentile(values, 0.95),
		P99:     percentile(values, 0.99),
		Maximum: values[len(values)-1],
	}
}

func percentile(values []int64, quantile float64) int64 {
	if len(values) == 0 {
		return 0
	}
	index := int(math.Ceil(quantile*float64(len(values)))) - 1
	index = max(0, min(index, len(values)-1))
	return values[index]
}

func cloneCounts(source map[string]int) map[string]int {
	if len(source) == 0 {
		return nil
	}
	result := make(map[string]int, len(source))
	for key, value := range source {
		result[key] = value
	}
	return result
}

func randomID() (string, error) {
	raw := make([]byte, 12)
	if _, err := rand.Read(raw); err != nil {
		return "", fmt.Errorf("generate run ID: %w", err)
	}
	return hex.EncodeToString(raw), nil
}

func createOutput(path string) (*os.File, error) {
	file, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return nil, fmt.Errorf("create %s: %w", path, err)
	}
	return file, nil
}

func writeJSON(path string, value any) error {
	file, err := createOutput(path)
	if err != nil {
		return err
	}
	encoder := json.NewEncoder(file)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(value); err != nil {
		_ = file.Close()
		return fmt.Errorf("encode %s: %w", path, err)
	}
	if err := file.Sync(); err != nil {
		_ = file.Close()
		return fmt.Errorf("sync %s: %w", path, err)
	}
	if err := file.Close(); err != nil {
		return fmt.Errorf("close %s: %w", path, err)
	}
	return nil
}

type processSampler struct {
	pid        int
	ticks      int64
	startTicks uint64
	peakRSS    atomic.Int64
	stopCh     chan struct{}
	done       chan struct{}
	errMu      sync.Mutex
	errors     []string
}

func startProcessSampler(pid int) (*processSampler, error) {
	if runtime.GOOS != "linux" {
		return nil, fmt.Errorf("process sampling is supported only on Linux")
	}
	ticks, err := clockTicks()
	if err != nil {
		return nil, err
	}
	startTicks, rss, err := readProcess(pid)
	if err != nil {
		return nil, err
	}
	sampler := &processSampler{
		pid: pid, ticks: ticks, startTicks: startTicks,
		stopCh: make(chan struct{}), done: make(chan struct{}),
	}
	sampler.peakRSS.Store(rss)
	go sampler.run()
	return sampler, nil
}

func (s *processSampler) run() {
	defer close(s.done)
	ticker := time.NewTicker(50 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			_, rss, err := readProcess(s.pid)
			if err != nil {
				s.recordError(err)
				continue
			}
			for current := s.peakRSS.Load(); rss > current && !s.peakRSS.CompareAndSwap(current, rss); current = s.peakRSS.Load() {
			}
		case <-s.stopCh:
			return
		}
	}
}

func (s *processSampler) stop(elapsed time.Duration, successes uint64) *processSummary {
	close(s.stopCh)
	<-s.done
	endTicks, rss, err := readProcess(s.pid)
	if err != nil {
		s.recordError(err)
	}
	if rss > s.peakRSS.Load() {
		s.peakRSS.Store(rss)
	}
	delta := uint64(0)
	if endTicks >= s.startTicks {
		delta = endTicks - s.startTicks
	}
	cpuSeconds := float64(delta) / float64(s.ticks)
	result := &processSummary{
		PID: s.pid, CPUSeconds: cpuSeconds, PeakRSSBytes: s.peakRSS.Load(),
		ClockTicksPerSec: s.ticks,
	}
	if elapsed > 0 {
		result.AverageCPUCores = cpuSeconds / elapsed.Seconds()
	}
	if successes > 0 {
		result.CPUUSPerSuccess = cpuSeconds * 1_000_000 / float64(successes)
	}
	s.errMu.Lock()
	result.SamplingErrorText = strings.Join(s.errors, "; ")
	s.errMu.Unlock()
	return result
}

func (s *processSampler) recordError(err error) {
	s.errMu.Lock()
	defer s.errMu.Unlock()
	if len(s.errors) < 4 {
		s.errors = append(s.errors, err.Error())
	}
}

func clockTicks() (int64, error) {
	output, err := exec.Command("getconf", "CLK_TCK").Output()
	if err != nil {
		return 0, fmt.Errorf("run getconf CLK_TCK: %w", err)
	}
	value, err := strconv.ParseInt(strings.TrimSpace(string(output)), 10, 64)
	if err != nil || value <= 0 {
		return 0, fmt.Errorf("parse getconf CLK_TCK output %q", strings.TrimSpace(string(output)))
	}
	return value, nil
}

func readProcess(pid int) (uint64, int64, error) {
	stat, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return 0, 0, fmt.Errorf("read process stat: %w", err)
	}
	closing := bytes.LastIndexByte(stat, ')')
	if closing < 0 {
		return 0, 0, fmt.Errorf("parse process stat: command terminator missing")
	}
	fields := strings.Fields(string(stat[closing+1:]))
	if len(fields) <= 12 {
		return 0, 0, fmt.Errorf("parse process stat: too few fields")
	}
	userTicks, err := strconv.ParseUint(fields[11], 10, 64)
	if err != nil {
		return 0, 0, fmt.Errorf("parse process user ticks: %w", err)
	}
	systemTicks, err := strconv.ParseUint(fields[12], 10, 64)
	if err != nil {
		return 0, 0, fmt.Errorf("parse process system ticks: %w", err)
	}
	status, err := os.ReadFile(fmt.Sprintf("/proc/%d/status", pid))
	if err != nil {
		return 0, 0, fmt.Errorf("read process status: %w", err)
	}
	var rss int64
	for _, line := range strings.Split(string(status), "\n") {
		if !strings.HasPrefix(line, "VmRSS:") {
			continue
		}
		parts := strings.Fields(line)
		if len(parts) < 2 {
			break
		}
		kib, parseErr := strconv.ParseInt(parts[1], 10, 64)
		if parseErr != nil {
			return 0, 0, fmt.Errorf("parse process RSS: %w", parseErr)
		}
		rss = kib * 1024
		break
	}
	return userTicks + systemTicks, rss, nil
}

func captureCPUProfile(
	ctx context.Context,
	rawURL, output string,
	duration time.Duration,
) error {
	parsed, err := url.Parse(rawURL)
	if err != nil {
		return fmt.Errorf("parse pprof URL: %w", err)
	}
	if !strings.Contains(parsed.Path, "/debug/pprof/profile") {
		parsed.Path = strings.TrimRight(parsed.Path, "/") + "/debug/pprof/profile"
	}
	query := parsed.Query()
	query.Set("seconds", strconv.Itoa(max(1, int(math.Ceil(duration.Seconds())))))
	parsed.RawQuery = query.Encode()
	profileCtx, cancel := context.WithTimeout(ctx, duration+30*time.Second)
	defer cancel()
	request, err := http.NewRequestWithContext(profileCtx, http.MethodGet, parsed.String(), nil)
	if err != nil {
		return err
	}
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		return fmt.Errorf("capture CPU profile: %w", err)
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(response.Body, 4096))
		return fmt.Errorf("capture CPU profile: status %d: %s", response.StatusCode, strings.TrimSpace(string(body)))
	}
	file, err := createOutput(output)
	if err != nil {
		return err
	}
	if _, err := io.Copy(file, response.Body); err != nil {
		_ = file.Close()
		return fmt.Errorf("write CPU profile: %w", err)
	}
	if err := file.Close(); err != nil {
		return fmt.Errorf("close CPU profile: %w", err)
	}
	return nil
}
