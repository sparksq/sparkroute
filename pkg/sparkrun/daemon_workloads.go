// SPDX-FileCopyrightText: 2026 Scitrera LLC
// SPDX-FileCopyrightText: 2026 Fox Engine Ltd.
// SPDX-License-Identifier: AGPL-3.0-only

package sparkrun

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
)

// Host workload DTOs are independent of the released stdio bridge format.
// WorkloadID identifies deployed work; daemon job IDs identify invocations.
type daemonWorkload struct {
	WorkloadID     string   `json:"workload_id"`
	ClusterName    string   `json:"cluster_name"`
	RecipeRevision string   `json:"recipe_revision"`
	Owner          string   `json:"owner"`
	Owned          bool     `json:"owned"`
	Provider       string   `json:"provider"`
	Actions        []string `json:"actions"`
	State          string   `json:"state"`
	Observation    string   `json:"observation"`
}

func (w daemonWorkload) project() WorkloadInfo {
	plugins := []string{}
	if w.Provider != "" {
		plugins = append(plugins, w.Provider)
	}
	return WorkloadInfo{JobID: w.WorkloadID, ClusterName: w.ClusterName, RecipeRevision: w.RecipeRevision,
		Owned: w.Owned, PluginsInUse: plugins, LifecycleActions: w.Actions, LifecycleState: w.State, State: w.State}
}
func daemonBindingArguments(binding Binding, timeout float64) map[string]any {
	args := map[string]any{"recipe": binding.Recipe}
	if binding.RecipeRevision != "" {
		args["recipe_revision"] = binding.RecipeRevision
	}
	if len(binding.ClusterCandidates) > 0 {
		args["cluster_candidates"] = binding.ClusterCandidates
	}
	if binding.Overrides != nil {
		args["overrides"] = binding.Overrides
	}
	if timeout > 0 {
		args["timeout"] = timeout
	}
	return args
}
func decodeDaemonReady(raw json.RawMessage, binding Binding) (EnsureResult, error) {
	var value struct {
		State      string         `json:"state"`
		WorkloadID string         `json:"workload_id"`
		Adopted    bool           `json:"adopted"`
		ObservedAt float64        `json:"observed_at"`
		Workload   daemonWorkload `json:"workload"`
		Endpoint   struct {
			WorkloadID      string   `json:"workload_id"`
			ClusterName     string   `json:"cluster_name"`
			RecipeRevision  string   `json:"recipe_revision"`
			Owner           string   `json:"owner"`
			Model           string   `json:"model"`
			ServedModelName string   `json:"served_model_name"`
			ActualModels    []string `json:"actual_models"`
			Runtime         string   `json:"runtime"`
			Host            string   `json:"host"`
			Port            int      `json:"port"`
			Healthy         bool     `json:"healthy"`
			NativeProtocols []string `json:"native_protocols"`
			Capabilities    []string `json:"capabilities"`
			MaxModelLen     *int     `json:"max_model_len"`
		} `json:"endpoint"`
	}
	if err := decodeStrict(raw, &value); err != nil {
		return EnsureResult{}, err
	}
	ep, w := value.Endpoint, value.Workload
	if value.State != "ready" || value.WorkloadID == "" || ep.WorkloadID != value.WorkloadID || w.WorkloadID != value.WorkloadID ||
		value.ObservedAt <= 0 || !ep.Healthy || len(ep.ActualModels) == 0 || ep.RecipeRevision == "" ||
		ep.RecipeRevision != w.RecipeRevision || ep.Owner != w.Owner || w.Owned != (w.Owner == "sparkroute") ||
		(binding.RecipeRevision != "" && ep.RecipeRevision != binding.RecipeRevision) ||
		(len(binding.ClusterCandidates) > 0 && !slices.Contains(binding.ClusterCandidates, ep.ClusterName)) {
		return EnsureResult{}, fmt.Errorf("daemon readiness result has inconsistent workload identity, ownership or observation")
	}
	if !slices.Contains(ep.NativeProtocols, "openai") {
		return EnsureResult{}, fmt.Errorf("daemon endpoint does not support OpenAI protocol")
	}
	info := w.project()
	metadata := map[string]ModelMetadata{}
	for _, model := range ep.ActualModels {
		metadata[model] = ModelMetadata{Context: ep.MaxModelLen}
	}
	endpoint := &Endpoint{State: "ready", WorkloadID: ep.WorkloadID, JobID: ep.WorkloadID, ClusterName: ep.ClusterName,
		Recipe: binding.Recipe, RecipeRevision: ep.RecipeRevision, Host: ep.Host, Port: ep.Port, Protocol: "openai",
		ServedModels: ep.ActualModels, Runtime: ep.Runtime, Owned: w.Owned, PluginsInUse: info.PluginsInUse,
		LifecycleActions: w.Actions, ObservedAt: value.ObservedAt, ModelMetadata: metadata}
	return EnsureResult{State: value.State, Adopted: value.Adopted, Endpoint: endpoint}, nil
}

func (c *Client) adaptDaemonWorkloads(ctx context.Context, operation string, raw json.RawMessage) (json.RawMessage, error) {
	switch operation {
	case "sleep", "wake", "workload_status":
		var value daemonWorkload
		if err := decodeStrict(raw, &value); err != nil {
			return nil, err
		}
		return json.Marshal(value.project())
	case "workloads":
		var value struct {
			Workloads []daemonWorkload `json:"workloads"`
			Complete  bool             `json:"complete"`
			Providers []struct {
				Name    string `json:"name"`
				Version int    `json:"version"`
			} `json:"providers"`
		}
		if err := decodeStrict(raw, &value); err != nil {
			return nil, err
		}
		var plugins struct {
			Plugins []PluginAvailability `json:"plugins"`
		}
		if err := c.invokeDaemon(ctx, bridgeRequest{Operation: "catalog_plugins"}, &plugins); err != nil {
			return nil, err
		}
		result := WorkloadReport{Complete: value.Complete, Plugins: plugins.Plugins, Workloads: []WorkloadInfo{}}
		for _, row := range value.Workloads {
			result.Workloads = append(result.Workloads, row.project())
		}
		return json.Marshal(result)
	case "catalog_plugins":
		var value struct {
			Plugins []struct {
				Name      string `json:"name"`
				PluginID  string `json:"plugin_id"`
				Enabled   bool   `json:"enabled"`
				Loaded    bool   `json:"loaded"`
				Version   string `json:"version"`
				Lifecycle bool   `json:"lifecycle"`
			} `json:"plugins"`
		}
		if err := decodeStrict(raw, &value); err != nil {
			return nil, err
		}
		plugins := []PluginAvailability{}
		for _, row := range value.Plugins {
			name := row.PluginID
			if name == "" {
				name = row.Name
			}
			plugins = append(plugins, PluginAvailability{Name: name, Installed: true, Enabled: row.Enabled && row.Loaded, Lifecycle: row.Lifecycle})
		}
		return json.Marshal(map[string]any{"plugins": plugins})
	case "resolve":
		var value struct {
			Name           string `json:"name"`
			RecipeRevision string `json:"recipe_revision"`
			Model          string `json:"model"`
			Runtime        string `json:"runtime"`
		}
		if err := json.Unmarshal(raw, &value); err != nil {
			return nil, err
		}
		return json.Marshal(map[string]string{"recipe": value.Name, "recipe_revision": value.RecipeRevision, "model": value.Model, "runtime": value.Runtime})
	}
	return raw, nil
}
