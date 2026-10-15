// SPDX-License-Identifier: AGPL-3.0-only
// Copyright 2026 Scitrera LLC
// Copyright 2026 Fox Engine Ltd.
package sparkrun

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"slices"

	"github.com/sparksq/sparkroute/pkg/config"
	"github.com/sparksq/sparkroute/pkg/config/managed"
	"github.com/sparksq/sparkroute/pkg/lifecycle"
)

// RefreshRecipeBindings follows trusted catalog updates without replacing live
// workloads. A receipt, including a sleeping or unready job, defers the update.
// Each write uses CAS and the same physical-workload gate as admission/stop.
// Unavailable catalogs or inventory never authorize a revision change.
func RefreshRecipeBindings(ctx context.Context, store managed.Store, catalog Catalog, workloads *Workloads, inspect func(context.Context) (WorkloadReport, error)) error {
	if workloads == nil || inspect == nil || catalog == nil {
		return fmt.Errorf("recipe updates require workload inventory and admission gates")
	}
	var failures []error
	for _, owner := range []managed.Owner{managed.OwnerOperator, managed.OwnerSparkrun} {
		set, err := store.GetSet(ctx, owner)
		if err != nil {
			return err
		}
		for _, deployment := range set.Document.Deployments {
			source := deployment.EndpointSource
			if source.Controller != ControllerName || source.Type != config.EndpointSourceActivatable {
				continue
			}
			if err := refreshRecipeBinding(ctx, store, owner, deployment.Name, catalog, workloads, inspect); err != nil {
				failures = append(failures, fmt.Errorf("deployment %s: %w", deployment.Name, err))
			}
		}
	}
	return errors.Join(failures...)
}

func refreshRecipeBinding(ctx context.Context, store managed.Store, owner managed.Owner, name string, catalog Catalog, workloads *Workloads, inspect func(context.Context) (WorkloadReport, error)) error {
	// Read the active revision first. Any intervening edit, including before
	// GetSet, invalidates the eventual write instead of overwriting operator work.
	baseline, revision, err := store.Load(ctx)
	if err != nil {
		return err
	}
	set, err := store.GetSet(ctx, owner)
	if err != nil {
		return err
	}
	document := set.Document
	index := slices.IndexFunc(document.Deployments, func(d config.Deployment) bool { return d.Name == name })
	if index < 0 {
		return nil
	}
	deployment := document.Deployments[index]
	source := deployment.EndpointSource
	if source.Controller != ControllerName || source.Type != config.EndpointSourceActivatable {
		return nil
	}
	details, err := resolveDetails(ctx, catalog, source.Recipe, source.Overrides)
	if err != nil {
		return err
	}
	if details.Revision == source.RecipeRevision {
		return nil
	}
	if !details.Trusted {
		return fmt.Errorf("recipe update requires trust approval")
	}
	// A different model or API contract needs an explicit deployment edit.
	if details.Model != deployment.Model {
		return fmt.Errorf("recipe update changes the served model; review recipe settings")
	}
	for _, protocol := range deployment.NativeProtocols {
		if !slices.Contains(details.NativeProtocols, protocol) {
			return fmt.Errorf("recipe update changes native APIs; review recipe settings")
		}
	}
	targets, err := lifecycle.TargetsFromDocument(baseline)
	targetIndex := slices.IndexFunc(targets, func(target lifecycle.Target) bool { return target.Deployment == name })
	if err != nil || targetIndex < 0 {
		return fmt.Errorf("invalid recipe binding: %v", err)
	}
	binding := targets[targetIndex].Binding
	unlock, err := workloads.lock(ctx, binding)
	if err != nil {
		return err
	}
	defer unlock()
	if workloads.bindingBusy(binding) {
		return nil
	}
	report, err := inspect(ctx)
	if err != nil {
		return fmt.Errorf("cannot verify previous workload is absent: %w", err)
	}
	if !report.Complete {
		return fmt.Errorf("workload inventory is incomplete or execution is in progress; retry on next reconciliation")
	}
	for _, job := range report.Workloads {
		// An unknown cluster can still overlap this binding. A job without
		// this exact fingerprint cannot be admitted by the saved binding.
		sameCluster := len(source.ClusterCandidates) == 0 || job.ClusterName == "" || slices.Contains(source.ClusterCandidates, job.ClusterName)
		if sameCluster && job.RecipeRevision == source.RecipeRevision {
			return nil
		}
	}
	source.RecipeRevision = details.Revision
	source.Revision = ""
	raw, err := json.Marshal(source)
	if err != nil {
		return err
	}
	digest := sha256.Sum256(raw)
	source.Revision = hex.EncodeToString(digest[:12])
	deployment.EndpointSource = source
	deployment.RequestProfiles = config.CloneProfileValue(details.Sparkroute.RequestProfiles)
	deployment.ProfileSource = recipeProfileSource(details)
	document.Deployments[index] = deployment
	candidate, _, err := store.BuildCandidate(ctx, owner, document, revision)
	if err != nil {
		return err
	}
	if err = ValidateWorkloadSharing(candidate); err != nil {
		return err
	}
	if err = ValidateRecipeBindings(ctx, catalog, config.Document{Deployments: []config.Deployment{deployment}}); err != nil {
		return err
	}
	// Recheck trust as well as the revision after potentially slow inventory I/O.
	latest, err := resolveDetails(ctx, catalog, source.Recipe, source.Overrides)
	if err != nil {
		return err
	}
	if !latest.Trusted || latest.Revision != source.RecipeRevision {
		return fmt.Errorf("recipe changed during automatic update; retry on next reconciliation")
	}
	_, err = store.ReplaceSet(ctx, owner, document, managed.ReplaceOptions{ExpectedActive: revision, Actor: "sparkroute:recipe-updates", Reason: "automatic recipe update for next cold start"})
	if err != nil {
		return err
	}
	// An older generation may have queued admission behind this gate. It must
	// not resurrect its retired binding while the new generation is installing.
	workloads.retireBinding(binding)
	return nil
}

func retiredBindingKey(binding lifecycle.Binding) string {
	return binding.Deployment + "\x00" + binding.Revision
}

func (w *Workloads) retireBinding(binding lifecycle.Binding) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.retiredBindings == nil {
		w.retiredBindings = map[string]bool{}
	}
	w.retiredBindings[retiredBindingKey(binding)] = true
}

func (w *Workloads) bindingRetired(binding lifecycle.Binding) bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.retiredBindings[retiredBindingKey(binding)]
}

func (w *Workloads) bindingBusy(binding lifecycle.Binding) bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	for _, job := range w.jobs {
		if job.binding.RecipeRevision == binding.RecipeRevision &&
			(len(binding.ClusterCandidates) == 0 || job.cluster == "" || slices.Contains(binding.ClusterCandidates, job.cluster)) &&
			(job.active > 0 || job.recovering || job.stopping) {
			return true
		}
	}
	return false
}
