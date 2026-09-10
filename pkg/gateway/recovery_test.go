// SPDX-FileCopyrightText: 2026 Scitrera LLC
// SPDX-FileCopyrightText: 2026 Fox Engine Ltd.
// SPDX-License-Identifier: AGPL-3.0-only

package gateway

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sparksq/sparkroute/pkg/config"
	"github.com/sparksq/sparkroute/pkg/endpointregistry"
	"github.com/sparksq/sparkroute/pkg/lifecycle"
	"github.com/sparksq/sparkroute/pkg/routing"
	"github.com/sparksq/sparkroute/pkg/sparkrun"
)

func TestRestartableFailureClassification(t *testing.T) {
	for _, test := range []struct {
		status  int
		class   string
		restart bool
	}{
		{500, "upstream_http_500", true}, {502, "upstream_http_502", true},
		{503, "upstream_http_503", false}, {429, "upstream_http_429", false},
		{401, "upstream_http_401", false}, {400, "upstream_http_400", false},
		{0, "upstream_transport_error", true}, {0, "per_try_timeout", true},
		{200, "stream_idle_timeout", true}, {200, "response_stream_error", true},
		{0, "upstream_configuration_error", false}, {0, "client_cancelled", false},
		{0, "overall_timeout", false}, {200, "pii_transform_failed", false},
	} {
		if got := restartableFailure(test.status, test.class); got != test.restart {
			t.Errorf("%d/%s = %v", test.status, test.class, got)
		}
	}
}

type recoveryBridge struct {
	mu       sync.Mutex
	endpoint sparkrun.Endpoint
	stops    int
}

func (b *recoveryBridge) Capabilities(context.Context) (sparkrun.Capabilities, error) {
	return sparkrun.Capabilities{}, nil
}
func (b *recoveryBridge) EnsureReady(context.Context, sparkrun.Binding, time.Duration) (sparkrun.EnsureResult, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	ep := b.endpoint
	return sparkrun.EnsureResult{State: "ready", Endpoint: &ep}, nil
}
func (b *recoveryBridge) Discover(context.Context, *sparkrun.Binding) (sparkrun.DiscoverResult, error) {
	return sparkrun.DiscoverResult{}, nil
}
func (b *recoveryBridge) Stop(_ context.Context, _ sparkrun.Binding, id string) (sparkrun.StopResult, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if id != b.endpoint.ClusterID {
		return sparkrun.StopResult{}, fmt.Errorf("wrong job")
	}
	b.stops++
	b.endpoint.ClusterID, b.endpoint.JobID = "new-job", "new-job"
	return sparkrun.StopResult{State: "offline", ClusterIDs: []string{id}}, nil
}

func TestCircuitFailuresRecoverOwnedWorkloadWithoutWaitingForIdle(t *testing.T) {
	for _, coldStart := range []config.ColdStartPolicy{config.ColdStartWait, config.ColdStartReject} {
		t.Run(string(coldStart), func(t *testing.T) {
			bridge := &recoveryBridge{}
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				bridge.mu.Lock()
				healthy := bridge.stops > 0
				bridge.mu.Unlock()
				w.Header().Set("Content-Type", "application/json")
				if !healthy {
					w.WriteHeader(500)
					_, _ = w.Write([]byte(`{"error":{"message":"engine failed"}}`))
					return
				}
				_, _ = w.Write([]byte(`{"id":"recovered","object":"chat.completion","model":"upstream","choices":[],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`))
			}))
			defer upstream.Close()
			u, _ := url.Parse(upstream.URL)
			host, portText, _ := net.SplitHostPort(u.Host)
			port, _ := strconv.Atoi(portText)
			bridge.endpoint = sparkrun.Endpoint{State: "ready", Owned: true, ClusterName: "lab", ClusterID: "old-job", JobID: "old-job", Host: host, Port: port, Protocol: "openai", RecipeRevision: "recipe-revision", ServedModels: []string{"upstream"}}
			document := dynamicGatewayDocument("")
			source := &document.Deployments[0].EndpointSource
			source.ColdStart = coldStart
			source.Controller, source.RecipeRevision, source.ClusterCandidates = "sparkrun", "recipe-revision", []string{"lab"}
			source.IdleTTL, source.IdleAction = config.Duration(time.Hour), "sleep"
			source.Recovery = config.RecoveryPolicy{Action: "restart", FailedProbes: 1, UnhealthyFor: config.Duration(time.Millisecond), DrainTimeout: config.Duration(time.Second)}
			document.Deployments[0].Circuit = config.CircuitPolicy{ConsecutiveFailures: 1, BaseEjectionTime: config.Duration(time.Millisecond), MaxEjectionTime: config.Duration(time.Millisecond)}
			targets, err := lifecycle.TargetsFromDocument(document)
			if err != nil {
				t.Fatal(err)
			}
			registry := endpointregistry.NewMemory()
			controller, err := sparkrun.New(sparkrun.Options{Bridge: bridge, Registry: registry, Targets: targets})
			if err != nil {
				t.Fatal(err)
			}
			defer controller.Close()
			coordinator, err := lifecycle.NewAdmissionCoordinator(targets, lifecycle.AdmissionOptions{Registry: registry, Inspector: registry, Controllers: map[string]lifecycle.Controller{"sparkrun": controller}, Authorizer: controller, PollInterval: 10 * time.Millisecond})
			if err != nil {
				t.Fatal(err)
			}
			manager, err := routing.NewTargetManager(document.Deployments, routing.TargetManagerOptions{})
			if err != nil {
				t.Fatal(err)
			}
			handler, err := NewDataHandler(document, DataOptions{Lifecycle: coordinator, TargetManager: manager})
			if err != nil {
				t.Fatal(err)
			}
			if coldStart == config.ColdStartReject {
				if _, err := coordinator.Start(context.Background(), "deployment"); err != nil {
					t.Fatal(err)
				}
			}
			request := func() *httptest.ResponseRecorder {
				r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"virtual","messages":[{"role":"user","content":"hello"}]}`))
				r.Header.Set("Content-Type", "application/json")
				response := httptest.NewRecorder()
				handler.ServeHTTP(response, r)
				return response
			}
			if response := request(); response.Code == 200 {
				t.Fatal("broken backend returned success")
			}
			time.Sleep(3 * time.Millisecond)
			if response := request(); response.Code == 200 {
				t.Fatal("half-open failure returned success")
			}
			deadline := time.Now().Add(2 * time.Second)
			for time.Now().Before(deadline) {
				status, _ := controller.Status(context.Background(), targets[0].Binding)
				if status.Recovery != nil && status.Recovery.Attempts == 1 && status.Recovery.Phase == "healthy" {
					if status.State != endpointregistry.StateReady || status.JobID != "new-job" {
						t.Fatal("prewarmed replacement status still reports old job", status)
					}
					break
				}
				time.Sleep(time.Millisecond)
			}
			time.Sleep(3 * time.Millisecond) // Allow the existing circuit ejection to elapse.
			response := request()
			if response.Code != 200 {
				t.Fatalf("recovered request = %d %s", response.Code, response.Body.String())
			}
			bridge.mu.Lock()
			stops := bridge.stops
			bridge.mu.Unlock()
			if stops != 1 {
				t.Fatalf("stops = %d", stops)
			}
			status, _ := manager.Status("deployment")
			if status.CircuitState != routing.CircuitClosed {
				t.Fatalf("circuit = %s", status.CircuitState)
			}
			live, _ := controller.Status(context.Background(), targets[0].Binding)
			if live.Endpoint == nil || live.Endpoint.JobID != "new-job" || live.Endpoint.FencingToken <= 0 {
				t.Fatalf("replacement was not registered through fenced admission: %+v", live)
			}

		})
	}
}
