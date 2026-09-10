// SPDX-FileCopyrightText: 2026 Scitrera LLC
// SPDX-FileCopyrightText: 2026 Fox Engine Ltd.
// SPDX-License-Identifier: AGPL-3.0-only

package sparkrun

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"time"
	"unicode"

	"github.com/sparksq/sparkroute/pkg/endpointregistry"
	"github.com/sparksq/sparkroute/pkg/lifecycle"
)

type PluginAvailability struct {
	Name      string `json:"name"`
	Installed bool   `json:"installed"`
	Enabled   bool   `json:"enabled"`
	Lifecycle bool   `json:"lifecycle"`
}
type WorkloadInfo struct {
	JobID            string   `json:"job_id"`
	ClusterName      string   `json:"cluster_name"`
	RecipeRevision   string   `json:"recipe_revision"`
	Owned            bool     `json:"owned"`
	PluginsInUse     []string `json:"plugins_in_use"`
	LifecycleActions []string `json:"lifecycle_actions"`
	LifecycleState   string   `json:"lifecycle_state"`
	State            string   `json:"state,omitempty"`
}
type WorkloadReport struct {
	Plugins   []PluginAvailability `json:"plugins"`
	Workloads []WorkloadInfo       `json:"workloads"`
}
type WorkloadBridge interface {
	InspectWorkloads(context.Context) (WorkloadReport, error)
	Lifecycle(context.Context, Binding, string, string, time.Duration) (WorkloadInfo, error)
}

// WorkloadControl joins explicit Start to normal admission; the controller
// handles receipt-backed Stop and the optional plugin lifecycle actions.
type WorkloadControl struct {
	Controller *Controller
	Admission  *lifecycle.AdmissionCoordinator
}

func (w WorkloadControl) WorkloadAction(ctx context.Context, deployment, jobID, action string) (WorkloadInfo, error) {
	if action != "start" {
		return w.Controller.WorkloadAction(ctx, deployment, jobID, action)
	}
	target, ok := w.Controller.targets[deployment]
	if !ok || target.Source != lifecycle.EndpointActivatable {
		return WorkloadInfo{}, fmt.Errorf("choose a configured recipe deployment")
	}
	endpoint, err := w.Admission.Start(ctx, deployment)
	if err != nil {
		return WorkloadInfo{}, err
	}
	w.Controller.mu.Lock()
	info, exists := w.Controller.workloadInfo[endpoint.JobID]
	w.Controller.mu.Unlock()
	if !exists {
		info = WorkloadInfo{JobID: endpoint.JobID, ClusterName: endpoint.Metadata["cluster_name"], RecipeRevision: endpoint.RecipeRevision,
			Owned: endpoint.Metadata["owned"] == "true", LifecycleState: "running"}
	}
	info.State = "ready"
	return info, nil
}

func (c *Client) InspectWorkloads(ctx context.Context) (WorkloadReport, error) {
	var result WorkloadReport
	err := c.invoke(ctx, bridgeRequest{Operation: "workloads"}, &result)
	if err == nil {
		for _, info := range result.Workloads {
			if err = validateWorkloadInfo(info); err != nil {
				break
			}
		}
		if len(result.Workloads) > 256 || len(result.Plugins) > 64 {
			err = fmt.Errorf("workload report exceeds limits")
		}
	}
	return result, err
}
func (c *Client) Lifecycle(ctx context.Context, binding Binding, job, action string, timeout time.Duration) (WorkloadInfo, error) {
	if action == "status" {
		action = "workload_status"
	}
	if action != "workload_status" && action != "sleep" && action != "wake" {
		return WorkloadInfo{}, fmt.Errorf("unsupported lifecycle action")
	}
	var result WorkloadInfo
	err := c.invoke(ctx, bridgeRequest{Operation: action, Binding: &binding, ClusterID: job, TimeoutSeconds: timeout.Seconds()}, &result)
	if err == nil {
		err = validateWorkloadInfo(result)
	}
	if err == nil && (result.JobID != job || result.RecipeRevision != binding.RecipeRevision || len(binding.ClusterCandidates) > 0 && !slices.Contains(binding.ClusterCandidates, result.ClusterName)) {
		err = fmt.Errorf("lifecycle response does not match the requested workload")
	}
	return result, err
}
func (c *Controller) inspectWorkloads(ctx context.Context) error {
	bridge, ok := c.bridge.(WorkloadBridge)
	if !ok {
		return nil
	}
	report, err := bridge.InspectWorkloads(ctx)
	if err != nil {
		return err
	}
	if len(report.Workloads) > 256 {
		return fmt.Errorf("too many workload receipts")
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.workloadInfo = make(map[string]WorkloadInfo, len(report.Workloads))
	for _, info := range report.Workloads {
		c.workloadInfo[info.JobID] = info
		for _, target := range c.targets {
			if target.Source != lifecycle.EndpointActivatable || target.Binding.RecipeRevision != info.RecipeRevision || !slices.Contains(target.Binding.ClusterCandidates, info.ClusterName) {
				continue
			}
			key := lifecycle.ActivationKey(target.Binding)
			state := c.states[key]
			if state == nil {
				state = &bindingState{binding: cloneBinding(target.Binding), state: endpointregistry.StateOffline, updatedAt: c.now().UTC()}
				c.states[key] = state
			}
			if state.jobID == "" || state.jobID == info.JobID {
				state.jobID = info.JobID
				if info.LifecycleState != "running" {
					state.phase = lifecycle.SanitizeReason(info.LifecycleState)
				}
			}
		}
	}
	return nil
}

// WorkloadAction requires an exact configured target and receipt, and holds the
// same physical-workload gate as admission and idle shutdown across generations.
func (c *Controller) WorkloadAction(ctx context.Context, deployment, jobID, action string) (WorkloadInfo, error) {
	target, ok := c.targets[deployment]
	if !ok || target.Source != lifecycle.EndpointActivatable || jobID == "" {
		return WorkloadInfo{}, fmt.Errorf("choose a configured recipe workload")
	}
	bridge, ok := c.bridge.(WorkloadBridge)
	if !ok {
		return WorkloadInfo{}, fmt.Errorf("workload controls are unavailable")
	}
	unlock, err := c.workloads.lock(ctx, target.Binding)
	if err != nil {
		return WorkloadInfo{}, err
	}
	defer unlock()
	report, err := bridge.InspectWorkloads(ctx)
	if err != nil {
		return WorkloadInfo{}, err
	}
	index := slices.IndexFunc(report.Workloads, func(info WorkloadInfo) bool {
		return info.JobID == jobID && info.RecipeRevision == target.Binding.RecipeRevision && slices.Contains(target.Binding.ClusterCandidates, info.ClusterName)
	})
	if index < 0 || (action != "stop" && !slices.Contains(report.Workloads[index].LifecycleActions, action)) {
		return WorkloadInfo{}, fmt.Errorf("this job does not support the requested lifecycle action")
	}
	if action != "status" {
		if !report.Workloads[index].Owned {
			return WorkloadInfo{}, fmt.Errorf("externally owned workloads cannot be changed")
		}
		if err := c.workloads.canStop(jobID); err != nil {
			return WorkloadInfo{}, err
		}
		if action == "stop" {
			if err := c.stopLocked(ctx, lifecycle.ActivationKey(target.Binding), target.Binding, jobID); err != nil {
				return WorkloadInfo{}, err
			}
			c.workloads.resetRecovery(target.Binding.RecipeRevision, report.Workloads[index].ClusterName)
			info := report.Workloads[index]
			info.State, info.LifecycleState, info.LifecycleActions = "offline", "offline", nil
			c.mu.Lock()
			if c.workloadInfo == nil {
				c.workloadInfo = map[string]WorkloadInfo{}
			}
			c.workloadInfo[jobID] = info
			c.mu.Unlock()
			return info, nil
		}
		c.workloads.suspend(jobID)
		defer c.workloads.finishAction(jobID)
	}
	result, err := bridge.Lifecycle(ctx, bridgeBinding(target.Binding), jobID, action, target.Binding.ActivationTimeout)
	if err != nil {
		return WorkloadInfo{}, err
	}
	c.mu.Lock()
	if c.workloadInfo == nil {
		c.workloadInfo = map[string]WorkloadInfo{}
	}
	c.workloadInfo[jobID] = result
	c.mu.Unlock()
	if result.LifecycleState != "running" {
		c.workloads.stopped(jobID)
		for _, id := range c.unapproveTarget(deployment) {
			if err := c.registry.Remove(ctx, id); err != nil {
				return WorkloadInfo{}, err
			}
		}
		if err := c.publishMetadata(); err != nil {
			return WorkloadInfo{}, err
		}
	} else {
		c.workloads.finishAction(jobID)
		c.workloads.running(jobID)
	}
	return result, nil
}

func validateWorkloadInfo(info WorkloadInfo) error {
	for _, value := range []string{info.JobID, info.ClusterName, info.RecipeRevision} {
		if len(value) > 1024 || strings.IndexFunc(value, unicode.IsControl) >= 0 {
			return fmt.Errorf("invalid workload identity")
		}
	}
	if info.JobID == "" || len(info.PluginsInUse) > 64 || len(info.LifecycleActions) > 3 {
		return fmt.Errorf("invalid workload receipt")
	}
	for _, value := range info.PluginsInUse {
		if value == "" || len(value) > 64 || strings.IndexFunc(value, unicode.IsControl) >= 0 {
			return fmt.Errorf("invalid plugin name")
		}
	}
	for _, action := range info.LifecycleActions {
		if action != "status" && action != "sleep" && action != "wake" {
			return fmt.Errorf("invalid lifecycle action")
		}
	}
	if info.LifecycleState != "" && (len(info.LifecycleState) > 64 || lifecycle.SanitizeReason(info.LifecycleState) != info.LifecycleState) {
		return fmt.Errorf("invalid workload lifecycle state")
	}
	return nil
}
