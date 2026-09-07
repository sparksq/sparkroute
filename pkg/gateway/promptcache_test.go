package gateway

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sparksq/sparkroute/pkg/config"
	"github.com/sparksq/sparkroute/pkg/identity"
	"github.com/sparksq/sparkroute/pkg/promptcache"
	"github.com/sparksq/sparkroute/pkg/routing"
)

type notifyingPromptCacheBackend struct {
	promptcache.Backend
	recorded chan struct{}
}

func (b notifyingPromptCacheBackend) Record(
	ctx context.Context,
	observation promptcache.Observation,
) error {
	err := b.Backend.Record(ctx, observation)
	if err == nil {
		select {
		case b.recorded <- struct{}{}:
		default:
		}
	}
	return err
}

func TestPromptCacheAffinityReusesSuccessfulDeploymentForFollowup(t *testing.T) {
	t.Parallel()

	var callsA, callsB atomic.Int64
	upstream := func(calls *atomic.Int64) *httptest.Server {
		return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			calls.Add(1)
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{
				"id":"chatcmpl-cache","object":"chat.completion","created":1,
				"model":"upstream","choices":[{"index":0,"message":{"role":"assistant","content":"a1"},"finish_reason":"stop"}]
			}`)
		}))
	}
	serverA := upstream(&callsA)
	defer serverA.Close()
	serverB := upstream(&callsB)
	defer serverB.Close()

	document := config.Document{
		Providers: []config.Provider{
			{Name: "a", Type: "openai_compatible", BaseURL: serverA.URL + "/v1"},
			{Name: "b", Type: "openai_compatible", BaseURL: serverB.URL + "/v1"},
		},
		Deployments: []config.Deployment{
			{Name: "a", Provider: "a", Model: "upstream"},
			{Name: "b", Provider: "b", Model: "upstream"},
		},
		VirtualModels: []config.VirtualModel{{
			Name: "virtual",
			Selection: config.SelectionPolicy{
				Mode: config.SelectionWeightedRandom,
				PromptCacheAffinity: config.PromptCacheAffinityPolicy{
					Enabled: true, Scope: config.PromptCacheAffinityTenant,
					MinPrefixBytes: 1, TTL: config.Duration(time.Minute),
				},
			},
			Pools: []config.RoutingPool{{Targets: []config.WeightedTarget{
				{Deployment: "a", Weight: 100},
				{Deployment: "b", Weight: 100},
			}}},
		}},
	}
	memory := promptcache.NewMemoryStore(promptcache.MemoryOptions{})
	recorded := make(chan struct{}, 2)
	directory, err := promptcache.NewDirectory(
		notifyingPromptCacheBackend{Backend: memory, recorded: recorded},
		promptcache.DirectoryOptions{},
	)
	if err != nil {
		t.Fatalf("NewDirectory() error = %v", err)
	}
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		if closeErr := directory.Close(ctx); closeErr != nil {
			t.Errorf("Close() error = %v", closeErr)
		}
	}()
	fingerprinter, err := promptcache.NewFingerprinter([]byte(strings.Repeat("k", 32)))
	if err != nil {
		t.Fatalf("NewFingerprinter() error = %v", err)
	}
	handler, err := NewDataHandler(document, DataOptions{
		RoutingPicker:  &responseSequencePicker{values: []int64{150, 0, 0, 0}},
		ConfigRevision: "revision-a", PromptCache: directory,
		PromptFingerprinter: fingerprinter,
	})
	if err != nil {
		t.Fatalf("NewDataHandler() error = %v", err)
	}

	serve := func(body string) *httptest.ResponseRecorder {
		response := httptest.NewRecorder()
		request := httptest.NewRequest(
			http.MethodPost, "/v1/chat/completions", strings.NewReader(body),
		)
		request.Header.Set("Content-Type", "application/json")
		handler.ServeHTTP(response, request)
		return response
	}
	first := serve(`{
		"model":"virtual",
		"messages":[
			{"role":"system","content":"shared system"},
			{"role":"user","content":"q1"}
		]
	}`)
	if first.Code != http.StatusOK {
		t.Fatalf("first status = %d, body = %s", first.Code, first.Body.String())
	}
	select {
	case <-recorded:
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for prompt-cache observation")
	}
	second := serve(`{
		"model":"virtual",
		"messages":[
			{"role":"system","content":"shared system"},
			{"role":"user","content":"q1"},
			{"role":"assistant","content":"a1"},
			{"role":"user","content":"q2"}
		]
	}`)
	if second.Code != http.StatusOK {
		t.Fatalf("second status = %d, body = %s", second.Code, second.Body.String())
	}
	if callsA.Load() != 0 || callsB.Load() != 2 {
		t.Fatalf("upstream calls a=%d b=%d, want affinity to b", callsA.Load(), callsB.Load())
	}
}

func TestPromptCacheScopeUsesImplicitCallerWithoutAuthentication(t *testing.T) {
	t.Parallel()

	got, ok := promptCacheScope(config.PromptCacheAffinityCaller, identity.Identity{})
	if !ok || got != "caller\x00\x00anonymous" {
		t.Fatalf("promptCacheScope() = %q, %v", got, ok)
	}
}

func TestPromptCacheCandidatesStayInFirstPreferenceClass(t *testing.T) {
	t.Parallel()

	routes := promptCacheCandidates(routing.Plan{Candidates: []routing.Selection{
		{
			Provider:     config.Provider{Name: "provider-a"},
			Deployment:   config.Deployment{Name: "deployment-a"},
			PoolPriority: 1, PreferenceClass: 4,
		},
		{
			Provider:     config.Provider{Name: "provider-b"},
			Deployment:   config.Deployment{Name: "deployment-b"},
			PoolPriority: 1, PreferenceClass: 4,
		},
		{
			Provider:     config.Provider{Name: "provider-c"},
			Deployment:   config.Deployment{Name: "deployment-c"},
			PoolPriority: 1, PreferenceClass: 5,
		},
		{
			Provider:     config.Provider{Name: "provider-d"},
			Deployment:   config.Deployment{Name: "deployment-d"},
			PoolPriority: 2, PreferenceClass: 8,
		},
	}})
	if len(routes) != 2 ||
		routes[0].Deployment != "deployment-a" ||
		routes[1].Deployment != "deployment-b" {
		t.Fatalf("promptCacheCandidates() = %#v", routes)
	}
}
