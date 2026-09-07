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
)

const acceptanceBridgeStateEnvironment = "SPARKROUTE_ACCEPTANCE_BRIDGE_STATE"

type acceptanceBridgeState struct {
	Host                    string `json:"host"`
	Port                    int    `json:"port"`
	WarmVisible             bool   `json:"warm_visible"`
	ColdActive              bool   `json:"cold_active"`
	EnsureDelayMilliseconds int    `json:"ensure_delay_ms"`
	BadRecipeRevision       bool   `json:"bad_recipe_revision"`
	ProcessCalls            int    `json:"process_calls"`
	CapabilitiesCalls       int    `json:"capabilities_calls"`
	DiscoverCalls           int    `json:"discover_calls"`
	EnsureCalls             int    `json:"ensure_calls"`
	StopCalls               int    `json:"stop_calls"`
	LastRecipe              string `json:"last_recipe,omitempty"`
	LastRecipeRevision      string `json:"last_recipe_revision,omitempty"`
}

type acceptanceBridgeRequest struct {
	SchemaVersion  int                      `json:"schema_version"`
	RequestID      string                   `json:"request_id"`
	Operation      string                   `json:"operation"`
	Binding        *acceptanceBridgeBinding `json:"binding,omitempty"`
	ClusterID      string                   `json:"cluster_id,omitempty"`
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
		case "capabilities":
			state.CapabilitiesCalls++
		case "discover":
			state.DiscoverCalls++
		case "ensure_ready":
			state.EnsureCalls++
			if request.Binding != nil {
				state.LastRecipe = request.Binding.Recipe
				state.LastRecipeRevision = request.Binding.RecipeRevision
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
		SchemaVersion: 1,
		RequestID:     request.RequestID,
		OK:            true,
	}
	switch request.Operation {
	case "capabilities":
		response.Result = map[string]any{
			"protocol_version": 1,
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
			"state":       "offline",
			"cluster_ids": []string{"cold-cluster"},
		}
	default:
		response.OK = false
		response.Error = &acceptanceBridgeError{
			Code: "unsupported_operation", Message: "fixture operation is unsupported",
		}
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
		"cluster_id":      clusterID,
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
	defer os.Remove(lockPath)

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

func mustAcceptanceBridgeState(t *testing.T, path string) acceptanceBridgeState {
	t.Helper()
	state, err := updateAcceptanceBridgeState(path, nil)
	if err != nil {
		t.Fatalf("read acceptance bridge state: %v", err)
	}
	return state
}
