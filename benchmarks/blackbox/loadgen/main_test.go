package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestExecuteRequestCapturesRequestIDAndTTFB(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.Header.Get("User-Agent") != "sparkroute-blackbox/1 run-a" {
			t.Errorf("User-Agent = %q", request.Header.Get("User-Agent"))
		}
		writer.Header().Set("X-Request-Id", "request-a")
		writer.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(writer).Encode(map[string]any{"ok": true})
	}))
	defer server.Close()

	result := executeRequest(context.Background(), server.Client(), options{
		targetURL: server.URL, timeout: time.Second, body: []byte(`{"model":"benchmark"}`),
		headers: http.Header{"Content-Type": {"application/json"}},
		expect:  `"ok":true`, requestIDHeader: "X-Request-Id",
		maxResponseBytes: 1024,
	}, "run-a", "measurement", 1)
	if !result.Success || result.Status != http.StatusOK || result.RequestID != "request-a" {
		t.Fatalf("result = %#v", result)
	}
	if result.LatencyUS < 0 || result.TTFBUS < 0 || result.Bytes == 0 {
		t.Fatalf("timing/body result = %#v", result)
	}
}

func TestExecuteRequestRejectsMissingExpectedContent(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		_, _ = writer.Write([]byte(`{"ok":false}`))
	}))
	defer server.Close()
	result := executeRequest(context.Background(), server.Client(), options{
		targetURL: server.URL, timeout: time.Second, body: []byte(`{}`),
		headers: http.Header{}, expect: `"ok":true`, requestIDHeader: "X-Request-Id",
		maxResponseBytes: 1024,
	}, "run-a", "measurement", 1)
	if result.Success || result.Error != "expected_content_missing" {
		t.Fatalf("result = %#v", result)
	}
}

func TestSummarizeDistributionUsesNearestRank(t *testing.T) {
	t.Parallel()
	values := make([]int64, 100)
	for index := range values {
		values[index] = int64(100 - index)
	}
	got := summarizeDistribution(values)
	if got.Minimum != 1 || got.P50 != 50 || got.P95 != 95 || got.P99 != 99 || got.Maximum != 100 {
		t.Fatalf("distribution = %#v", got)
	}
}

func TestHeaderFlagsRejectMalformedValue(t *testing.T) {
	t.Parallel()
	var headers headerFlags
	if err := headers.Set("missing-separator"); err == nil {
		t.Fatal("Set() error = nil")
	}
	if err := headers.Set("X-Test: value"); err != nil {
		t.Fatalf("Set() error = %v", err)
	}
	if got := http.Header(headers).Get("X-Test"); got != "value" {
		t.Fatalf("X-Test = %q", got)
	}
}
