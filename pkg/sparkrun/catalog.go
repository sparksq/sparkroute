// SPDX-FileCopyrightText: 2026 Scitrera LLC
// SPDX-FileCopyrightText: 2026 Fox Engine Ltd.
// SPDX-License-Identifier: AGPL-3.0-only

package sparkrun

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"reflect"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/sparksq/sparkroute/pkg/config"
	"github.com/sparksq/sparkroute/pkg/config/managed"
	"github.com/sparksq/sparkroute/pkg/modelrouter"
)

// Catalog is available before any lifecycle target exists. Discovery uses only
// controller-local caches; mutation is restricted by the admin handler.
type Catalog interface {
	Catalog(context.Context, string, map[string]any, any) error
}

func (c *Client) Catalog(ctx context.Context, operation string, arguments map[string]any, result any) error {
	return c.invoke(ctx, bridgeRequest{Operation: operation, Arguments: arguments}, result)
}

type Operation struct {
	WorkloadID  string          `json:"workload_id,omitempty"`
	ClusterName string          `json:"cluster_name,omitempty"`
	ID          string          `json:"operation_id"`
	State       string          `json:"state"`
	Phase       string          `json:"phase"`
	UpdatedAt   float64         `json:"updated_at"`
	Result      json.RawMessage `json:"result,omitempty"`
	Error       *BridgeError    `json:"error,omitempty"`
}

// RecipeSettings supply deployment capabilities and profile implementations.
// Profile sources follow recipe updates independently of operator model patches.
type RecipeSettings struct {
	Capabilities    []config.Capability                            `json:"capabilities,omitempty"`
	RequestProfiles map[string]config.RequestProfileImplementation `json:"request_profiles,omitempty"`
}

type RecipeDetails struct {
	ProfileSourceKind string                               `json:"profile_source_kind,omitempty"`
	ModelMetadata     *modelrouter.DiscoveredModelMetadata `json:"model_metadata,omitempty"`
	Sparkroute        RecipeSettings                       `json:"sparkroute,omitempty"`
	Metadata          map[string]any                       `json:"metadata"`
	HFModel           string                               `json:"hf_model"`
	AvailablePlugins  []string                             `json:"available_plugins"`
	Reference         string                               `json:"reference"`
	Name              string                               `json:"name"`
	SourcePath        string                               `json:"source_path"`
	Registry          *string                              `json:"registry"`
	Model             string                               `json:"model"`
	Runtime           string                               `json:"runtime"`
	Description       string                               `json:"description"`
	MinNodes          int                                  `json:"min_nodes"`
	Defaults          map[string]any                       `json:"defaults"`
	Revision          string                               `json:"recipe_revision"`
	NativeAPIOptions  []string                             `json:"native_api_options"`
	NativeProtocols   []config.Protocol                    `json:"native_protocols"`
	Capabilities      []config.Capability                  `json:"capabilities"`
	RequiredPlugins   []string                             `json:"required_plugins"`
	Trusted           bool                                 `json:"trusted"`
	Issues            []map[string]any                     `json:"issues"`
}

type Cluster struct {
	Hosts       []string `json:"hosts,omitempty"`
	Name        string   `json:"name"`
	Description string   `json:"description"`
	HostCount   int      `json:"host_count"`
	Default     bool     `json:"default"`
}

type RecipeDraft struct {
	Recovery           config.RecoveryPolicy `json:"recovery,omitzero"`
	NativeAPIs         []string              `json:"native_apis"`
	IdleAction         string                `json:"idle_action"`
	Deployment         string                `json:"deployment,omitempty"`
	Reference          string                `json:"reference"`
	RecipeRevision     string                `json:"recipe_revision"`
	Name               string                `json:"name"`
	Aliases            []string              `json:"aliases"`
	Cluster            string                `json:"cluster"`
	FallbackClusters   []string              `json:"fallback_clusters"`
	Overrides          map[string]string     `json:"overrides"`
	ActivationTimeout  config.Duration       `json:"activation_timeout"`
	IdleTTL            config.Duration       `json:"idle_ttl"`
	MaxQueuedWaiters   int                   `json:"max_queued_waiters"`
	MaxQueuedBodyBytes int64                 `json:"max_queued_body_bytes"`
}

func resolveDetails(ctx context.Context, catalog Catalog, reference string, overrides map[string]string) (RecipeDetails, error) {
	var details RecipeDetails
	if overrides == nil {
		overrides = map[string]string{}
	}
	err := catalog.Catalog(ctx, "catalog_resolve", map[string]any{"reference": reference, "overrides": overrides}, &details)
	if err != nil {
		return details, err
	}
	if err := config.ValidateCapabilitySet(details.Sparkroute.Capabilities); err != nil {
		return details, fmt.Errorf("sparkroute.capabilities: %w", err)
	}
	for _, capability := range details.Sparkroute.Capabilities {
		if !slices.Contains(details.Capabilities, capability) {
			details.Capabilities = append(details.Capabilities, capability)
		}
	}
	if err := config.ValidateProfileImplementations(details.Sparkroute.RequestProfiles); err != nil {
		return details, fmt.Errorf("sparkroute.request_profiles: %w", err)
	}
	for _, issue := range details.Issues {
		if issue["severity"] == "error" {
			return details, fmt.Errorf("recipe requires attention: %v", issue["message"])
		}
	}
	if details.Reference == "" || details.Revision == "" || details.Model == "" {
		return details, fmt.Errorf("recipe preview is incomplete")
	}
	return details, nil
}

// PrepareRecipeDraft returns a complete operator draft; it never saves or launches.
// Reusing a workload also reuses its lifecycle policy, including generated entries.
func PrepareRecipeDraft(ctx context.Context, catalog Catalog, operator, generated config.Document, input RecipeDraft) (config.Document, string, bool, error) {

	details, err := resolveDetails(ctx, catalog, input.Reference, input.Overrides)
	if err != nil {
		return operator, "", false, err
	}
	if input.NativeAPIs != nil {
		if len(input.NativeAPIs) == 0 || len(input.NativeAPIs) > 8 {
			return operator, "", false, fmt.Errorf("choose at least one native API")
		}
		details.NativeProtocols = nil
		details.Capabilities = slices.DeleteFunc(details.Capabilities, func(c config.Capability) bool { return c == config.CapabilityResponses })
		for _, api := range input.NativeAPIs {
			if !slices.Contains(details.NativeAPIOptions, api) {
				return operator, "", false, fmt.Errorf("native API %q is unavailable for this runtime family", api)
			}
			protocol := config.ProtocolOpenAI
			if api == "messages" {
				protocol = config.ProtocolAnthropic
			}
			if !slices.Contains(details.NativeProtocols, protocol) {
				details.NativeProtocols = append(details.NativeProtocols, protocol)
			}
			if api == "responses" {
				details.Capabilities = append(details.Capabilities, config.CapabilityResponses)
			}
		}
		if slices.Contains(input.NativeAPIs, "responses") && !slices.Contains(input.NativeAPIs, "chat_completions") {
			return operator, "", false, fmt.Errorf("responses requires chat completions for this runtime")
		}
	}
	if input.RecipeRevision == "" || input.RecipeRevision != details.Revision {
		return operator, "", false, &RecipeRevisionError{Deployment: input.Deployment, Recipe: details.Name, Clusters: append([]string{input.Cluster}, input.FallbackClusters...), SavedRevision: input.RecipeRevision, CurrentRevision: details.Revision}
	}
	var clusters struct {
		Clusters []Cluster `json:"clusters"`
	}
	if err := catalog.Catalog(ctx, "catalog_clusters", nil, &clusters); err != nil {
		return operator, "", false, err
	}
	candidates := append([]string{input.Cluster}, input.FallbackClusters...)
	seenClusters := map[string]bool{}
	if len(candidates) > 64 {
		return operator, "", false, fmt.Errorf("choose at most 64 clusters")
	}
	for _, candidate := range candidates {
		if seenClusters[candidate] || !slices.ContainsFunc(clusters.Clusters, func(c Cluster) bool { return c.Name == candidate && c.HostCount > 0 }) {
			return operator, "", false, fmt.Errorf("choose distinct available named clusters")
		}
		seenClusters[candidate] = true
	}
	if err := input.Recovery.Validate(); err != nil {
		return operator, "", false, fmt.Errorf("recovery: %w", err)
	}
	if input.ActivationTimeout == 0 {
		input.ActivationTimeout = config.Duration(30 * time.Minute)
	}
	if input.ActivationTimeout.Value() <= 0 || input.ActivationTimeout.Value() > time.Hour || input.IdleTTL.Value() < 0 {
		return operator, "", false, fmt.Errorf("cold-start wait must be between zero and 60 minutes; idle timeout cannot be negative")
	}
	if input.Deployment != "" {
		index := slices.IndexFunc(operator.Deployments, func(d config.Deployment) bool { return d.Name == input.Deployment })
		if index < 0 {
			return operator, "", false, fmt.Errorf("choose an operator-managed sparkrun deployment to edit")
		}
		deployment := operator.Deployments[index]
		if deployment.EndpointSource.Controller != "sparkrun" || deployment.EndpointSource.Type != config.EndpointSourceActivatable {
			return operator, "", false, fmt.Errorf("only recipe-backed sparkrun deployments can be edited with recipe settings")
		}
		source := deployment.EndpointSource
		source.Recipe, source.RecipeRevision = details.Reference, details.Revision
		source.ClusterCandidates, source.Overrides = candidates, input.Overrides
		source.IdleAction = input.IdleAction
		source.Recovery = input.Recovery
		source.ActivationTimeout, source.IdleTTL = input.ActivationTimeout, input.IdleTTL
		source.MaxQueuedWaiters, source.MaxQueuedBodyBytes = input.MaxQueuedWaiters, input.MaxQueuedBodyBytes
		source.Revision = ""
		raw, _ := json.Marshal(source)
		revision := sha256.Sum256(raw)
		source.Revision = hex.EncodeToString(revision[:12])
		// Keep stable routing references and operator policy while changing the recipe binding.
		deployment.EndpointSource, deployment.Model = source, details.Model
		deployment.RequestProfiles = config.CloneProfileValue(details.Sparkroute.RequestProfiles)
		deployment.ProfileSource = recipeProfileSource(details)
		deployment.NativeProtocols = details.NativeProtocols
		deployment.Capabilities = slices.DeleteFunc(slices.Clone(deployment.Capabilities), func(c config.Capability) bool { return c == config.CapabilityResponses })
		if slices.Contains(details.Capabilities, config.CapabilityResponses) {
			deployment.Capabilities = append(deployment.Capabilities, config.CapabilityResponses)
		}
		previousTitle := "sparkrun:" + strings.Join(operator.Deployments[index].EndpointSource.ClusterCandidates, ",") + ":" + operator.Deployments[index].Model
		if deployment.Title == previousTitle {
			deployment.Title = "sparkrun:" + strings.Join(candidates, ",") + ":" + details.Model
		}
		operator.Deployments = slices.Clone(operator.Deployments)
		operator.Deployments[index] = deployment
		if err := ValidateWorkloadSharing(config.Document{Deployments: append(slices.Clone(operator.Deployments), generated.Deployments...)}); err != nil {
			return operator, "", false, err
		}
		return operator, deployment.Name, false, nil
	}
	allDeployments := append(slices.Clone(operator.Deployments), generated.Deployments...)
	deploymentName, reused := "", false
	for _, deployment := range allDeployments {
		source := deployment.EndpointSource
		if source.Type == config.EndpointSourceActivatable && source.Controller == "sparkrun" && source.RecipeRevision == details.Revision && slices.Equal(source.ClusterCandidates, candidates) {
			if config.ProfileDigest(deployment.RequestProfiles) != config.ProfileDigest(details.Sparkroute.RequestProfiles) && (deployment.ProfileSource == nil || deployment.ProfileSource.Recipe != details.Reference) {
				return operator, "", false, fmt.Errorf("shared deployment has different request profile implementations; update its recipe source first")
			}
			deploymentName, reused = deployment.Name, true
			break
		}
	}
	if !reused {
		digest := sha256.Sum256([]byte(details.Revision + "\x00" + strings.Join(candidates, "\x00")))
		deploymentName = "sparkrun:" + hex.EncodeToString(digest[:12])
		providerName := managed.SharedSparkrunProvider
		provider := config.Provider{Name: providerName, Type: "sparkrun"}
		found := false
		for _, existing := range append(slices.Clone(operator.Providers), generated.Providers...) {
			if existing.Name == providerName {
				if !reflect.DeepEqual(existing, provider) {
					return operator, "", false, fmt.Errorf("the shared sparkrun provider has conflicting settings")
				}
				found = true
			}
		}
		if !found {
			// Keep an empty-install draft complete. The managed store moves this
			// reserved provider into the sparkrun set atomically on Save.
			operator.Providers = append(slices.Clone(operator.Providers), provider)
		}

		source := config.EndpointSource{Type: config.EndpointSourceActivatable, Controller: "sparkrun", Recipe: details.Reference,
			RecipeRevision: details.Revision, ClusterCandidates: candidates, Overrides: input.Overrides,
			ActivationTimeout: input.ActivationTimeout, IdleTTL: input.IdleTTL, IdleAction: input.IdleAction, ColdStart: config.ColdStartWait, Recovery: input.Recovery,
			MaxQueuedWaiters: input.MaxQueuedWaiters, MaxQueuedBodyBytes: input.MaxQueuedBodyBytes}
		raw, _ := json.Marshal(source)
		revision := sha256.Sum256(raw)
		source.Revision = hex.EncodeToString(revision[:12])
		operator.Deployments = append(operator.Deployments, config.Deployment{Name: deploymentName,
			Title: "sparkrun:" + strings.Join(candidates, ",") + ":" + details.Model, Provider: providerName, Model: details.Model,
			NativeProtocols: details.NativeProtocols, Capabilities: details.Capabilities, EndpointSource: source, ModelMetadata: details.ModelMetadata,
			RequestProfiles: config.CloneProfileValue(details.Sparkroute.RequestProfiles), ProfileSource: recipeProfileSource(details)})
	}
	required := []config.Capability(nil)
	if slices.Contains(details.Capabilities, config.Capability("single_vector_embedding")) {
		required = []config.Capability{"single_vector_embedding"}
	}
	base := config.VirtualModel{Name: input.Name, Aliases: slices.Clone(input.Aliases), RequiredCapabilities: required,
		Pools: []config.RoutingPool{{Priority: 0, Targets: []config.WeightedTarget{{Deployment: deploymentName, Weight: 1}}}}}
	additions := []config.VirtualModel{base}
	selectors := make([]string, 0, len(details.Sparkroute.RequestProfiles))
	for selector := range details.Sparkroute.RequestProfiles {
		selectors = append(selectors, selector)
	}
	sort.Strings(selectors)
	for _, selector := range selectors {
		profile := config.VirtualModel{Name: base.Name + ":" + selector, Profile: &config.RequestProfileBinding{Parent: base.Name, Selector: selector}}
		additions = append(additions, profile)
	}
	allModels := append(slices.Clone(operator.VirtualModels), generated.VirtualModels...)
	allModels = append(allModels, additions...)
	resolved, err := (config.Document{VirtualModels: allModels}).ResolveRequestProfiles()
	if err != nil {
		return operator, "", false, err
	}
	occupied := map[string]bool{}
	for _, model := range resolved.VirtualModels {
		for _, name := range append([]string{model.Name}, model.Aliases...) {
			if occupied[name] {
				return operator, "", false, fmt.Errorf("recipe model or request profile conflicts with existing model or alias %q", name)
			}
			occupied[name] = true
		}
	}
	operator.VirtualModels = append(slices.Clone(operator.VirtualModels), additions...)
	return operator, deploymentName, reused, nil
}

// ChangedRecipeBindings selects new or changed launch/lifecycle bindings by
// deployment identity. Display metadata and routing edits do not adopt a new
// recipe. Compare against the merged document at the caller's expected revision.
func ChangedRecipeBindings(baseline, candidate config.Document) config.Document {
	previous := make(map[string]config.Deployment, len(baseline.Deployments))
	for _, deployment := range baseline.Deployments {
		previous[deployment.Name] = deployment
	}
	changed := config.Document{}
	for _, deployment := range candidate.Deployments {
		source := deployment.EndpointSource
		if source.Type != config.EndpointSourceActivatable || source.Controller != "sparkrun" {
			continue
		}
		old, exists := previous[deployment.Name]
		// JSON normalizes omitted maps/slices and is deterministic for overrides.
		before, _ := json.Marshal(old.EndpointSource)
		after, _ := json.Marshal(source)
		if !exists || old.Model != deployment.Model || old.Provider != deployment.Provider || string(before) != string(after) {
			changed.Deployments = append(changed.Deployments, deployment)
		}
	}
	return changed
}

// RecipeRevisionError identifies an explicit recipe edit that needs review.
// It is distinct from an unavailable catalog and never advances a saved pin.
type RecipeRevisionError struct {
	Deployment      string
	Recipe          string
	Clusters        []string
	SavedRevision   string
	CurrentRevision string
}

func (e *RecipeRevisionError) Error() string {
	name := e.Recipe
	if name == "" {
		name = e.Deployment
	}
	if len(e.Clusters) > 0 {
		name += " on " + strings.Join(e.Clusters, ", ")
	}
	return fmt.Sprintf("Recipe %s changed since it was reviewed. Review the recipe update in Model Deployments before applying this binding change.", name)
}

// ValidateRecipeBindings re-resolves exact selections on both Validate and Save.
// Revision pins include launch overrides, so editing an override requires a new preview.
func ValidateRecipeBindings(ctx context.Context, catalog Catalog, document config.Document) error {
	if err := ValidateWorkloadSharing(document); err != nil {
		return err
	}
	var clusters struct {
		Clusters []Cluster `json:"clusters"`
	}
	loadedClusters := false
	for _, deployment := range document.Deployments {
		source := deployment.EndpointSource
		if source.Type != config.EndpointSourceActivatable || source.Controller != "sparkrun" {
			continue
		}
		if catalog == nil {
			return fmt.Errorf("sparkrun catalog is unavailable")
		}
		details, err := resolveDetails(ctx, catalog, source.Recipe, source.Overrides)
		if err != nil {
			return err
		}
		if source.IdleAction == "sleep" {
			var available struct {
				Plugins []PluginAvailability `json:"plugins"`
			}
			if err := catalog.Catalog(ctx, "catalog_plugins", nil, &available); err != nil {
				return err
			}
			if !slices.ContainsFunc(available.Plugins, func(p PluginAvailability) bool { return p.Name == "coldsnap" && p.Lifecycle }) {
				return fmt.Errorf("idle sleep requires the enabled ColdSnap plugin with its workload lifecycle API")
			}
			if !slices.Contains(details.RequiredPlugins, "sparkrun.plugins.coldsnap") && !slices.Contains(details.RequiredPlugins, "coldsnap") {
				return fmt.Errorf("idle sleep requires a ColdSnap recipe")
			}
		}
		if details.Revision != source.RecipeRevision || details.Model != deployment.Model {
			return &RecipeRevisionError{Deployment: deployment.Name, Recipe: details.Name, Clusters: source.ClusterCandidates, SavedRevision: source.RecipeRevision, CurrentRevision: details.Revision}
		}
		if !loadedClusters {
			if err := catalog.Catalog(ctx, "catalog_clusters", nil, &clusters); err != nil {
				return err
			}
			loadedClusters = true
		}
		for _, name := range source.ClusterCandidates {
			if !slices.ContainsFunc(clusters.Clusters, func(c Cluster) bool { return c.Name == name && c.HostCount > 0 }) {
				return fmt.Errorf("configured sparkrun cluster is unavailable")
			}
		}
	}
	return nil
}

func RetainRecipeImports(ctx context.Context, catalog Catalog, document config.Document) error {
	for _, deployment := range document.Deployments {
		source := deployment.EndpointSource
		if source.Type != config.EndpointSourceActivatable || source.Controller != "sparkrun" {
			continue
		}
		var result struct {
			Retained bool `json:"retained"`
		}
		if err := catalog.Catalog(ctx, "catalog_retain", map[string]any{"reference": source.Recipe}, &result); err != nil {
			return err
		}
	}
	return nil
}

// ValidateWorkloadSharing is structural and also runs for generated-set syncs.
// Unscoped bindings overlap every named cluster and cannot silently compete
// with a deployment created by the operator UI.
func ValidateWorkloadSharing(document config.Document) error {
	byRevision := map[string][]config.Deployment{}
	for _, deployment := range document.Deployments {
		source := deployment.EndpointSource
		if source.Type != config.EndpointSourceActivatable || source.Controller != "sparkrun" {
			continue
		}
		for _, previous := range byRevision[source.RecipeRevision] {
			names := previous.EndpointSource.ClusterCandidates
			overlap := len(names) == 0 || len(source.ClusterCandidates) == 0
			for _, name := range source.ClusterCandidates {
				overlap = overlap || slices.Contains(names, name)
			}
			if overlap {
				return fmt.Errorf("%s and %s control the same sparkrun workload; share one deployment through virtual models", previous.Name, deployment.Name)
			}
		}
		byRevision[source.RecipeRevision] = append(byRevision[source.RecipeRevision], deployment)
	}
	return nil
}

func recipeProfileSource(details RecipeDetails) *config.RequestProfileSource {
	return &config.RequestProfileSource{Recipe: details.Reference, RecipeRevision: details.Revision, Kind: details.ProfileSourceKind, Revision: config.ProfileDigest(details.Sparkroute.RequestProfiles)}
}
