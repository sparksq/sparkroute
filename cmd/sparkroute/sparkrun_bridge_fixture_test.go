// SPDX-License-Identifier: AGPL-3.0-only
// Copyright 2026 Scitrera LLC
// Copyright 2026 Fox Engine Ltd.

package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/sparksq/sparkroute/pkg/config"
)

const acceptanceBridgeStateEnvironment = "SPARKROUTE_ACCEPTANCE_BRIDGE_STATE"

type acceptanceBridgeState struct {
	CatalogRevision         string                                         `json:"catalog_revision,omitempty"`
	ActiveRecipeRevision    string                                         `json:"active_recipe_revision,omitempty"`
	LaunchCalls             int                                            `json:"launch_calls"`
	Profiles                map[string]config.RequestProfileImplementation `json:"profiles,omitempty"`
	Host                    string                                         `json:"host"`
	Port                    int                                            `json:"port"`
	WarmVisible             bool                                           `json:"warm_visible"`
	ColdActive              bool                                           `json:"cold_active"`
	EnsureDelayMilliseconds int                                            `json:"ensure_delay_ms"`
	BadRecipeRevision       bool                                           `json:"bad_recipe_revision"`
	ProcessCalls            int                                            `json:"process_calls"`
	CapabilitiesCalls       int                                            `json:"capabilities_calls"`
	DiscoverCalls           int                                            `json:"discover_calls"`
	EnsureCalls             int                                            `json:"ensure_calls"`
	StopCalls               int                                            `json:"stop_calls"`
	LastRecipe              string                                         `json:"last_recipe,omitempty"`
	LastRecipeRevision      string                                         `json:"last_recipe_revision,omitempty"`
}

type acceptanceBridgeRequest struct {
	Wait           *bool                    `json:"wait,omitempty"`
	Arguments      map[string]any           `json:"arguments,omitempty"`
	SchemaVersion  int                      `json:"schema_version"`
	RequestID      string                   `json:"request_id"`
	Operation      string                   `json:"operation"`
	Binding        *acceptanceBridgeBinding `json:"binding,omitempty"`
	WorkloadID     string                   `json:"workload_id,omitempty"`
	TimeoutSeconds float64                  `json:"timeout_seconds,omitempty"`
}

type acceptanceBridgeBinding struct {
	Recipe            string            `json:"recipe"`
	RecipeRevision    string            `json:"recipe_revision,omitempty"`
	ClusterCandidates []string          `json:"cluster_candidates,omitempty"`
	Overrides         map[string]string `json:"overrides,omitempty"`
}

type acceptanceBridgeResponse struct {
	SchemaVersion int                    `json:"schema_version"`
	RequestID     string                 `json:"request_id"`
	OK            bool                   `json:"ok"`
	Result        any                    `json:"result,omitempty"`
	Error         *acceptanceBridgeError `json:"error,omitempty"`
}

type acceptanceBridgeError struct {
	Code      string `json:"code"`
	Message   string `json:"message"`
	Retryable bool   `json:"retryable"`
}

func TestMain(m *testing.M) {
	if len(os.Args) > 1 && os.Args[1] == "gateway-bridge" &&
		os.Getenv(acceptanceBridgeStateEnvironment) != "" {
		os.Exit(runAcceptanceBridge())
	}
	os.Exit(m.Run())
}

func runAcceptanceBridge() int {
	var request acceptanceBridgeRequest
	decoder := json.NewDecoder(io.LimitReader(os.Stdin, 1<<20))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&request); err != nil {
		return 2
	}
	statePath := os.Getenv(acceptanceBridgeStateEnvironment)
	state, err := updateAcceptanceBridgeState(statePath, func(state *acceptanceBridgeState) error {
		state.ProcessCalls++
		switch request.Operation {
		case "workloads", "catalog_resolve", "catalog_clusters":
		case "capabilities":
			state.CapabilitiesCalls++
		case "discover":
			state.DiscoverCalls++
		case "ensure_ready":
			state.EnsureCalls++
			if request.Binding != nil {
				state.LastRecipe = request.Binding.Recipe
				state.LastRecipeRevision = request.Binding.RecipeRevision
				if state.CatalogRevision != "" {
					state.ActiveRecipeRevision = request.Binding.RecipeRevision
				}
			}
		case "stop":
			state.StopCalls++
			state.ColdActive = false
		default:
			return fmt.Errorf("unsupported operation %q", request.Operation)
		}
		return nil
	})
	if err != nil {
		return 3
	}

	response := acceptanceBridgeResponse{
		SchemaVersion: request.SchemaVersion,
		RequestID:     request.RequestID,
		OK:            true,
	}
	switch request.Operation {
	case "catalog_resolve":
		revision := state.CatalogRevision
		if revision == "" {
			revision = acceptanceColdRecipeRevision(state)
		}
		response.Result = map[string]any{"reference": "cold-recipe", "recipe_revision": revision, "model": "cold-upstream", "runtime": "acceptance-runtime", "native_protocols": []string{"openai"}, "profile_source_kind": "recipe", "trusted": true, "sparkroute": map[string]any{"request_profiles": state.Profiles}}
	case "catalog_clusters":
		response.Result = map[string]any{"clusters": []any{map[string]any{"name": "local-cluster", "host_count": 2, "hosts": []string{"one", "two"}}}}
	case "workloads":
		jobs := []any{}
		if state.ColdActive {
			jobs = append(jobs, map[string]any{"job_id": "cold-job", "cluster_name": "local-cluster", "recipe_revision": acceptanceColdRecipeRevision(state), "lifecycle_state": "running"})
		}
		response.Result = map[string]any{"plugins": []any{}, "workloads": jobs, "complete": true}
	case "capabilities":
		response.Result = map[string]any{
			"protocol_version": request.SchemaVersion,
			"operations":       []string{"discover", "ensure_ready", "stop"},
			"sparkrun_version": "acceptance-fixture",
		}
	case "discover":
		endpoints := make([]any, 0, 2)
		if state.WarmVisible {
			endpoints = append(endpoints, acceptanceEndpoint(
				state,
				"warm-upstream",
				"warm-cluster",
				"warm-job",
				"warm-recipe",
				"warmrev00001",
				7,
				8192,
			))
		}
		if state.ColdActive {
			endpoints = append(endpoints, acceptanceEndpoint(
				state,
				"cold-upstream",
				"cold-cluster",
				"cold-job",
				"cold-recipe",
				acceptanceColdRecipeRevision(state),
				32,
				32768,
			))
		}
		response.Result = map[string]any{"endpoints": endpoints}
	case "ensure_ready":
		if request.Binding == nil || request.Binding.Recipe != "cold-recipe" {
			response.OK = false
			response.Error = &acceptanceBridgeError{
				Code: "binding_not_found", Message: "fixture binding was not found",
			}
			break
		}
		if state.EnsureDelayMilliseconds > 0 {
			time.Sleep(time.Duration(state.EnsureDelayMilliseconds) * time.Millisecond)
		}
		state, err = updateAcceptanceBridgeState(statePath, func(current *acceptanceBridgeState) error {
			if !current.ColdActive {
				current.LaunchCalls++
			}
			current.ColdActive = true
			return nil
		})
		if err != nil {
			return 4
		}
		response.Result = map[string]any{
			"state": "ready",
			"endpoint": acceptanceEndpoint(
				state,
				"cold-upstream",
				"cold-cluster",
				"cold-job",
				"cold-recipe",
				acceptanceColdRecipeRevision(state),
				32,
				32768,
			),
			"adopted": false,
		}
	case "stop":
		response.Result = map[string]any{
			"state":        "offline",
			"workload_ids": []string{"cold-cluster"},
		}
	default:
		response.OK = false
		response.Error = &acceptanceBridgeError{
			Code: "unsupported_operation", Message: "fixture operation is unsupported",
		}
	}
	if request.Operation == "ensure_ready" && response.OK {
		response.Result = map[string]any{"operation_id": "fixture", "state": "succeeded", "phase": "complete", "updated_at": float64(time.Now().Unix()), "result": response.Result}
	}
	if err := json.NewEncoder(os.Stdout).Encode(response); err != nil {
		return 5
	}
	return 0
}

func acceptanceEndpoint(
	state acceptanceBridgeState,
	model string,
	clusterID string,
	jobID string,
	recipe string,
	recipeRevision string,
	sizeB float64,
	contextSize int,
) map[string]any {
	return map[string]any{
		"state":           "ready",
		"workload_id":     clusterID,
		"cluster_name":    "local-cluster",
		"owned":           true,
		"job_id":          jobID,
		"host":            state.Host,
		"port":            state.Port,
		"protocol":        "openai",
		"served_models":   []string{model},
		"recipe":          recipe,
		"recipe_revision": recipeRevision,
		"runtime":         "acceptance-runtime",
		"model_metadata": map[string]any{
			model: map[string]any{
				"size_b": sizeB, "context": contextSize,
				"tags": []string{"acceptance", "sparkrun"},
			},
		},
	}
}

func acceptanceColdRecipeRevision(state acceptanceBridgeState) string {
	if state.BadRecipeRevision {
		return "badrev000000"
	}
	if state.ActiveRecipeRevision != "" {
		return state.ActiveRecipeRevision
	}
	return "coldrev00001"
}

func updateAcceptanceBridgeState(
	path string,
	update func(*acceptanceBridgeState) error,
) (acceptanceBridgeState, error) {
	lockPath := path + ".lock"
	deadline := time.Now().Add(10 * time.Second)
	for {
		err := os.Mkdir(lockPath, 0o700)
		if err == nil {
			break
		}
		if !errors.Is(err, os.ErrExist) {
			return acceptanceBridgeState{}, err
		}
		if time.Now().After(deadline) {
			return acceptanceBridgeState{}, fmt.Errorf("timed out acquiring fixture lock")
		}
		time.Sleep(2 * time.Millisecond)
	}
	defer func() { _ = os.Remove(lockPath) }()

	var state acceptanceBridgeState
	raw, err := os.ReadFile(path)
	if err == nil {
		if err := json.Unmarshal(raw, &state); err != nil {
			return acceptanceBridgeState{}, err
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return acceptanceBridgeState{}, err
	}
	if update != nil {
		if err := update(&state); err != nil {
			return acceptanceBridgeState{}, err
		}
	}
	raw, err = json.Marshal(state)
	if err != nil {
		return acceptanceBridgeState{}, err
	}
	temporary := filepath.Join(
		filepath.Dir(path),
		"."+filepath.Base(path)+"."+strconv.Itoa(os.Getpid())+".tmp",
	)
	if err := os.WriteFile(temporary, raw, 0o600); err != nil {
		return acceptanceBridgeState{}, err
	}
	if err := os.Rename(temporary, path); err != nil {
		return acceptanceBridgeState{}, err
	}
	return state, nil
}

// mustAcceptanceBridgeState reads without the fixture lock. Writers publish by
// atomic rename, so a reader always sees a whole document; and a bridge killed
// mid-update (activation timeouts do that) can leave the lock directory behind,
// which would otherwise stall every later read for the full lock deadline.
func mustAcceptanceBridgeState(t *testing.T, path string) acceptanceBridgeState {
	t.Helper()
	var state acceptanceBridgeState
	raw, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return state
	}
	if err != nil {
		t.Fatalf("read acceptance bridge state: %v", err)
	}
	if err := json.Unmarshal(raw, &state); err != nil {
		t.Fatalf("read acceptance bridge state: %v", err)
	}
	return state
}

func TestAcceptanceBridgeStateReadIgnoresAbandonedLock(t *testing.T) {
	path := filepath.Join(t.TempDir(), "bridge.json")
	if _, err := updateAcceptanceBridgeState(path, func(state *acceptanceBridgeState) error {
		state.ColdActive = true
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	// A bridge killed while holding the lock leaves its directory behind.
	if err := os.Mkdir(path+".lock", 0o700); err != nil {
		t.Fatal(err)
	}
	started := time.Now()
	if !mustAcceptanceBridgeState(t, path).ColdActive {
		t.Fatal("state not read")
	}
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("read waited %s on an abandoned lock", elapsed)
	}
}
