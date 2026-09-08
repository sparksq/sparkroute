package sparkrun

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"reflect"
	"slices"
	"time"

	"github.com/sparksq/sparkroute/pkg/config"
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
	ClusterID   string          `json:"cluster_id,omitempty"`
	ClusterName string          `json:"cluster_name,omitempty"`
	ID          string          `json:"operation_id"`
	State       string          `json:"state"`
	Phase       string          `json:"phase"`
	UpdatedAt   float64         `json:"updated_at"`
	Result      json.RawMessage `json:"result,omitempty"`
	Error       *BridgeError    `json:"error,omitempty"`
}

type RecipeDetails struct {
	AvailablePlugins []string            `json:"available_plugins"`
	Reference        string              `json:"reference"`
	Name             string              `json:"name"`
	SourcePath       string              `json:"source_path"`
	Registry         *string             `json:"registry"`
	Model            string              `json:"model"`
	Runtime          string              `json:"runtime"`
	Description      string              `json:"description"`
	MinNodes         int                 `json:"min_nodes"`
	Defaults         map[string]any      `json:"defaults"`
	Revision         string              `json:"recipe_revision"`
	NativeProtocols  []config.Protocol   `json:"native_protocols"`
	Capabilities     []config.Capability `json:"capabilities"`
	RequiredPlugins  []string            `json:"required_plugins"`
	Trusted          bool                `json:"trusted"`
	Issues           []map[string]any    `json:"issues"`
}

type Cluster struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	HostCount   int    `json:"host_count"`
	Default     bool   `json:"default"`
}

type RecipeDraft struct {
	Reference          string            `json:"reference"`
	RecipeRevision     string            `json:"recipe_revision"`
	Name               string            `json:"name"`
	Aliases            []string          `json:"aliases"`
	Cluster            string            `json:"cluster"`
	Overrides          map[string]string `json:"overrides"`
	ActivationTimeout  config.Duration   `json:"activation_timeout"`
	IdleTTL            config.Duration   `json:"idle_ttl"`
	MaxQueuedWaiters   int               `json:"max_queued_waiters"`
	MaxQueuedBodyBytes int64             `json:"max_queued_body_bytes"`
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
	if input.RecipeRevision == "" || input.RecipeRevision != details.Revision {
		return operator, "", false, fmt.Errorf("recipe changed; refresh its preview before adding it")
	}
	var clusters struct {
		Clusters []Cluster `json:"clusters"`
	}
	if err := catalog.Catalog(ctx, "catalog_clusters", nil, &clusters); err != nil {
		return operator, "", false, err
	}
	if !slices.ContainsFunc(clusters.Clusters, func(c Cluster) bool { return c.Name == input.Cluster && c.HostCount > 0 }) {
		return operator, "", false, fmt.Errorf("choose an available named cluster")
	}
	if input.ActivationTimeout == 0 {
		input.ActivationTimeout = config.Duration(15 * time.Minute)
	}
	if input.ActivationTimeout.Value() <= 0 || input.ActivationTimeout.Value() > time.Hour || input.IdleTTL.Value() < 0 {
		return operator, "", false, fmt.Errorf("cold-start wait must be between zero and 60 minutes; idle timeout cannot be negative")
	}
	allDeployments := append(slices.Clone(operator.Deployments), generated.Deployments...)
	deploymentName, reused := "", false
	for _, deployment := range allDeployments {
		source := deployment.EndpointSource
		if source.Type == config.EndpointSourceActivatable && source.Controller == "sparkrun" && source.RecipeRevision == details.Revision && slices.Equal(source.ClusterCandidates, []string{input.Cluster}) {
			deploymentName, reused = deployment.Name, true
			break
		}
	}
	if !reused {
		digest := sha256.Sum256([]byte(details.Revision + "\x00" + input.Cluster))
		deploymentName = "sparkrun:" + hex.EncodeToString(digest[:12])
		providerName := "sparkrun:operator"
		provider := config.Provider{Name: providerName, Type: "openai"}
		found := false
		for _, existing := range append(slices.Clone(operator.Providers), generated.Providers...) {
			if existing.Name == providerName {
				if !reflect.DeepEqual(existing, provider) {
					return operator, "", false, fmt.Errorf("the reserved SparkRun provider has conflicting settings")
				}
				found = true
			}
		}
		if !found {
			operator.Providers = append(operator.Providers, provider)
		}
		source := config.EndpointSource{Type: config.EndpointSourceActivatable, Controller: "sparkrun", Recipe: details.Reference,
			RecipeRevision: details.Revision, ClusterCandidates: []string{input.Cluster}, Overrides: input.Overrides,
			ActivationTimeout: input.ActivationTimeout, IdleTTL: input.IdleTTL, ColdStart: config.ColdStartWait,
			MaxQueuedWaiters: input.MaxQueuedWaiters, MaxQueuedBodyBytes: input.MaxQueuedBodyBytes}
		raw, _ := json.Marshal(source)
		revision := sha256.Sum256(raw)
		source.Revision = hex.EncodeToString(revision[:12])
		operator.Deployments = append(operator.Deployments, config.Deployment{Name: deploymentName,
			Title: "sparkrun:" + input.Cluster + ":" + details.Model, Provider: providerName, Model: details.Model,
			NativeProtocols: details.NativeProtocols, Capabilities: details.Capabilities, EndpointSource: source})
	}
	required := []config.Capability(nil)
	if slices.Contains(details.Capabilities, config.Capability("single_vector_embedding")) {
		required = []config.Capability{"single_vector_embedding"}
	}
	operator.VirtualModels = append(operator.VirtualModels, config.VirtualModel{Name: input.Name, Aliases: input.Aliases, RequiredCapabilities: required,
		Pools: []config.RoutingPool{{Priority: 0, Targets: []config.WeightedTarget{{Deployment: deploymentName, Weight: 1}}}}})
	return operator, deploymentName, reused, nil
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
			return fmt.Errorf("SparkRun catalog is unavailable")
		}
		details, err := resolveDetails(ctx, catalog, source.Recipe, source.Overrides)
		if err != nil {
			return err
		}
		if details.Revision != source.RecipeRevision || details.Model != deployment.Model {
			return fmt.Errorf("recipe for %s changed; refresh the recipe preview", deployment.Name)
		}
		if !loadedClusters {
			if err := catalog.Catalog(ctx, "catalog_clusters", nil, &clusters); err != nil {
				return err
			}
			loadedClusters = true
		}
		for _, name := range source.ClusterCandidates {
			if !slices.ContainsFunc(clusters.Clusters, func(c Cluster) bool { return c.Name == name && c.HostCount > 0 }) {
				return fmt.Errorf("configured SparkRun cluster is unavailable")
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
				return fmt.Errorf("%s and %s control the same SparkRun workload; share one deployment through virtual models", previous.Name, deployment.Name)
			}
		}
		byRevision[source.RecipeRevision] = append(byRevision[source.RecipeRevision], deployment)
	}
	return nil
}
