package sparkrun

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/sparksq/sparkroute/pkg/config"
	"github.com/sparksq/sparkroute/pkg/config/managed"
)

type catalogFixture struct {
	revision string
	calls    []string
}

func (f *catalogFixture) Catalog(_ context.Context, operation string, _ map[string]any, result any) error {
	f.calls = append(f.calls, operation)
	var value any
	switch operation {
	case "catalog_resolve":
		value = RecipeDetails{Reference: "catalog:123", Name: "recipe", Revision: f.revision, Model: "test/model", Runtime: "vllm", NativeProtocols: []config.Protocol{config.ProtocolOpenAI}, Trusted: true}
	case "catalog_clusters":
		value = map[string]any{"clusters": []Cluster{{Name: "lab", HostCount: 2, Default: true}}}
	default:
		panic("unexpected catalog operation: " + operation)
	}
	raw, _ := json.Marshal(value)
	return json.Unmarshal(raw, result)
}

func TestRecipeDraftFromEmptyConfigurationAndSharedDeployment(t *testing.T) {
	f := &catalogFixture{revision: "recipe-revision"}
	empty := managed.EmptyDocument()
	input := RecipeDraft{Reference: "catalog:123", RecipeRevision: f.revision, Name: "coding", Aliases: []string{"code"}, Cluster: "lab", IdleTTL: config.Duration(30 * time.Minute)}
	document, deployment, reused, err := PrepareRecipeDraft(context.Background(), f, empty, empty, input)
	if err != nil {
		t.Fatal(err)
	}
	if reused || document.Deployments[0].Title != "sparkrun:lab:test/model" {
		t.Fatal(document)
	}
	if err := document.Validate(); err != nil {
		t.Fatal(err)
	}
	if document.Deployments[0].EndpointSource.ActivationTimeout.Value() != 15*time.Minute {
		t.Fatal("default wait")
	}
	if _, ok := document.CanonicalModel("code"); !ok {
		t.Fatal("missing alias")
	}
	if err := ValidateRecipeBindings(context.Background(), f, document); err != nil {
		t.Fatal(err)
	}
	input.Name = "assistant"
	input.Aliases = nil
	input.IdleTTL = 0
	shared, secondID, reused, err := PrepareRecipeDraft(context.Background(), f, empty, document, input)
	if err != nil {
		t.Fatal(err)
	}
	if !reused || secondID != deployment || len(shared.Deployments) != 0 {
		t.Fatal("did not reuse generated deployment")
	}
	if _, err := managed.Merge(map[managed.Owner]config.Document{managed.OwnerOperator: shared, managed.OwnerSparkrun: document}); err != nil {
		t.Fatal(err)
	}
	for _, operation := range f.calls {
		if operation != "catalog_resolve" && operation != "catalog_clusters" {
			t.Fatal(operation)
		}
	}
}

func TestRecipeDraftRejectsDriftMissingClusterAndDuplicateLifecycle(t *testing.T) {
	f := &catalogFixture{revision: "current"}
	empty := managed.EmptyDocument()
	input := RecipeDraft{Reference: "catalog:123", RecipeRevision: "old", Name: "coding", Cluster: "lab"}
	if _, _, _, err := PrepareRecipeDraft(context.Background(), f, empty, empty, input); err == nil || !strings.Contains(err.Error(), "changed") {
		t.Fatal(err)
	}
	input.RecipeRevision = "current"
	input.Cluster = "removed"
	if _, _, _, err := PrepareRecipeDraft(context.Background(), f, empty, empty, input); err == nil {
		t.Fatal("accepted missing cluster")
	}
	input.Cluster = "lab"
	document, _, _, _ := PrepareRecipeDraft(context.Background(), f, empty, empty, input)
	duplicate := document.Deployments[0]
	duplicate.Name = "duplicate"
	document.Deployments = append(document.Deployments, duplicate)
	if err := ValidateRecipeBindings(context.Background(), f, document); err == nil || !strings.Contains(err.Error(), "same sparkrun workload") {
		t.Fatal(err)
	}
	document.Deployments = document.Deployments[:1]
	f.revision = "changed"
	if err := ValidateRecipeBindings(context.Background(), f, document); err == nil {
		t.Fatal("accepted recipe drift at save")
	}
}

func TestEditRecipeDeploymentPreservesRoutingIdentityAndRevisionsPolicy(t *testing.T) {
	f := &catalogFixture{revision: "original"}
	empty := managed.EmptyDocument()
	input := RecipeDraft{Reference: "catalog:123", RecipeRevision: f.revision, Name: "coding", Aliases: []string{"code"}, Cluster: "lab"}
	document, id, _, err := PrepareRecipeDraft(context.Background(), f, empty, empty, input)
	if err != nil {
		t.Fatal(err)
	}
	before, _ := json.Marshal(document.VirtualModels)
	revision := document.Deployments[0].EndpointSource.Revision
	document.Deployments[0].Title = "Custom display title"
	input.Deployment, input.Name, input.Aliases = id, "", nil
	input.IdleTTL = config.Duration(30 * time.Minute)
	changed, actual, _, err := PrepareRecipeDraft(context.Background(), f, document, empty, input)
	if err != nil {
		t.Fatal(err)
	}
	after, _ := json.Marshal(changed.VirtualModels)
	if actual != id || string(before) != string(after) || len(changed.Deployments) != 1 {
		t.Fatal("route identity changed")
	}
	target := changed.Deployments[0]
	if target.Title != "Custom display title" || target.EndpointSource.Revision == revision || target.EndpointSource.IdleTTL != input.IdleTTL {
		t.Fatal("policy edit did not preserve identity", target)
	}
	input.Deployment = "generated-or-deleted"
	if _, _, _, err := PrepareRecipeDraft(context.Background(), f, empty, document, input); err == nil {
		t.Fatal("edited a generated deployment")
	}
}
