// SPDX-License-Identifier: AGPL-3.0-only
// Copyright 2026 Scitrera LLC
// Copyright 2026 Fox Engine Ltd.

package mmprojection

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/sparksq/sparkroute/pkg/credentials"
	"github.com/sparksq/sparkroute/pkg/modelrouter"
)

type testCredentials map[credentials.Ref]string

func (s testCredentials) Resolve(
	_ context.Context,
	ref credentials.Ref,
) (credentials.Material, error) {
	value, exists := s[ref]
	if !exists {
		return credentials.Material{}, fmt.Errorf("missing test credential")
	}
	return credentials.Material{Value: []byte(value)}, nil
}

func TestClientProjectsBoundedAuthenticatedRequest(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		if request.URL.Path != "/v1/mmprojection/chat/completions" ||
			request.Header.Get("Authorization") != "Bearer bridge-token" ||
			request.Header.Get(HopHeader) != "1" ||
			request.Header.Get(AnalyzerModelHeader) != "analyzer-a" ||
			request.Header.Get(TraceIDHeader) != "request-a" {
			t.Errorf("projection request = %s %#v", request.URL.Path, request.Header)
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("X-MM-Bridge-Request-ID", "bridge-a")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"version": 1, "applied": true, "media_count": 1,
			"cache_hit": true, "analyzer_model": "analyzer-a",
			"text_inspection": map[string]any{
				"version": 1, "state": "complete", "media_count": 1,
			},
			"body": map[string]any{
				"model": "logical-a",
				"messages": []any{map[string]any{
					"role": "user", "content": "projected evidence",
				}},
			},
		})
	}))
	defer server.Close()

	client, err := New(Options{
		URL: server.URL + "/v1", TokenReference: "env://BRIDGE_TOKEN",
		Credentials:    testCredentials{"env://BRIDGE_TOKEN": "bridge-token"},
		DefaultTimeout: time.Second,
	})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	body, trace, err := client.Project(
		context.Background(),
		[]byte(`{"model":"logical-a","messages":[]}`),
		modelrouter.MMProjectionPolicy{
			AnalyzerModel: "analyzer-a", FailureMode: "fail_closed",
		},
		OperationChat,
		"request-a",
	)
	if err != nil {
		t.Fatalf("Project() error = %v", err)
	}
	if !json.Valid(body) || trace.State != "applied" ||
		trace.ProviderID != ProviderID || trace.RequestID != "bridge-a" ||
		trace.MediaCount != 1 || !trace.CacheHit ||
		trace.TextInspectionState != "complete" || trace.InspectedMedia != 1 ||
		trace.AnalyzerModel != "analyzer-a" {
		t.Fatalf("body=%s trace=%#v", body, trace)
	}
	authorized, err := client.Authenticate(
		context.Background(), "Bearer bridge-token",
	)
	if err != nil || !authorized {
		t.Fatalf("Authenticate() = %v, %v", authorized, err)
	}
	if authorized, err = client.Authenticate(
		context.Background(), "Bearer wrong-token",
	); err != nil || authorized {
		t.Fatalf("wrong Authenticate() = %v, %v", authorized, err)
	}
}

func TestClientClassifiesInvalidMediaWithoutOpeningCircuit(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnsupportedMediaType)
		_, _ = w.Write([]byte(`{"error":{"message":"unsupported inline media type"}}`))
	}))
	defer server.Close()
	client, err := New(Options{
		URL: server.URL, TokenReference: "env://TOKEN",
		Credentials: testCredentials{"env://TOKEN": "secret"},
	})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	for index := 0; index < failureThreshold+1; index++ {
		_, trace, projectErr := client.Project(
			context.Background(), []byte(`{"model":"m"}`),
			modelrouter.MMProjectionPolicy{}, OperationResponses, "",
		)
		var typed *Error
		if !errors.As(projectErr, &typed) || !typed.ClientRequestError() ||
			trace.HTTPStatus != http.StatusUnsupportedMediaType {
			t.Fatalf("Project() trace=%#v error=%v", trace, projectErr)
		}
	}
}

func TestClientProbePublishesOnlyBoundedRedactedStatus(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		if request.Header.Get("Authorization") != "Bearer bridge-token" {
			t.Errorf("Authorization = %q", request.Header.Get("Authorization"))
		}
		w.Header().Set("Content-Type", "application/json")
		switch request.URL.Path {
		case "/v1/models":
			_, _ = w.Write([]byte(`{"data":[{"id":"vision-analyzer"},{"id":"audio-analyzer"}]}`))
		case "/v1/mmprojection/capabilities":
			_, _ = w.Write([]byte(`{"name":"pilco-mmbridge","projection_api":1,"endpoints":["/v1/chat/completions","/v1/responses"]}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()
	client, err := New(Options{
		URL: server.URL + "/v1", TokenReference: "env://PRIVATE_REFERENCE",
		Credentials:          testCredentials{"env://PRIVATE_REFERENCE": "bridge-token"},
		DefaultAnalyzerModel: "vision-analyzer",
	})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	status, err := client.Probe(context.Background())
	if err != nil || status.State != "healthy" || status.ProjectionAPI != 1 ||
		status.Models != 2 || status.Provider != ProviderID ||
		status.AnalyzerModel != "vision-analyzer" || status.LastSuccessAt == nil {
		t.Fatalf("Probe() = %#v, %v", status, err)
	}
	raw, _ := json.Marshal(status)
	if strings.Contains(string(raw), server.URL) ||
		strings.Contains(string(raw), "PRIVATE_REFERENCE") ||
		strings.Contains(string(raw), "bridge-token") ||
		strings.Contains(string(raw), "audio-analyzer") {
		t.Fatalf("status leaked private bridge data: %s", raw)
	}
}

func TestClientProbeFailureIsSanitizedAndCanRecoverOpenCircuit(t *testing.T) {
	t.Parallel()

	failing := true
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		if failing {
			w.WriteHeader(http.StatusBadGateway)
			_, _ = w.Write([]byte(`{"error":{"message":"private upstream failure detail"}}`))
			return
		}
		switch request.URL.Path {
		case "/models":
			_, _ = w.Write([]byte(`{"data":[{"id":"analyzer"}]}`))
		case "/mmprojection/capabilities":
			_, _ = w.Write([]byte(`{"name":"pilco-mmbridge","projection_api":1,"endpoints":["/v1/chat/completions","/v1/responses"]}`))
		default:
			_, _ = w.Write([]byte(`{"version":1,"applied":false,"media_count":0,"body":{"model":"m"}}`))
		}
	}))
	defer server.Close()
	client, err := New(Options{
		URL: server.URL, TokenReference: "env://TOKEN",
		Credentials: testCredentials{"env://TOKEN": "secret"},
	})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	for index := 0; index < failureThreshold; index++ {
		_, _, _ = client.Project(
			context.Background(), []byte(`{"model":"m"}`),
			modelrouter.MMProjectionPolicy{}, OperationResponses, "",
		)
	}
	if status := client.Status(); !status.CircuitOpen || status.LastError != "http_error" {
		t.Fatalf("open status = %#v", status)
	}
	if status, probeErr := client.Probe(context.Background()); probeErr == nil ||
		status.LastError != "http_error" || strings.Contains(status.LastError, "private") {
		t.Fatalf("failed Probe() = %#v, %v", status, probeErr)
	}
	failing = false
	status, err := client.Probe(context.Background())
	if err != nil || status.State != "healthy" || status.CircuitOpen ||
		status.ConsecutiveFailures != 0 {
		t.Fatalf("recovery Probe() = %#v, %v", status, err)
	}
}
