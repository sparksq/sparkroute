// SPDX-FileCopyrightText: 2026 Scitrera LLC
// SPDX-FileCopyrightText: 2026 Fox Engine Ltd.
// SPDX-License-Identifier: AGPL-3.0-only

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
	settings RecipeSettings
	calls    []string
}

func (f *catalogFixture) Catalog(_ context.Context, operation string, _ map[string]any, result any) error {
	f.calls = append(f.calls, operation)
	var value any
	switch operation {
	case "catalog_resolve":
		value = RecipeDetails{Sparkroute: f.settings, Reference: "catalog:123", Name: "recipe", Revision: f.revision, Model: "test/model", Runtime: "vllm", NativeAPIOptions: []string{"chat_completions", "responses", "messages"}, NativeProtocols: []config.Protocol{config.ProtocolOpenAI}, Trusted: true}
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
	if len(document.Providers) != 1 || document.Providers[0].Name != "sparkrun" || document.Deployments[0].Provider != "sparkrun" {
		t.Fatal("empty-install draft did not use the shared provider", document)
	}
	if err := document.Validate(); err != nil {
		t.Fatal(err)
	}
	if document.Deployments[0].EndpointSource.ActivationTimeout.Value() != 30*time.Minute {
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

func TestRecipeDraftReusesPrepopulatedProvider(t *testing.T) {
	f := &catalogFixture{revision: "recipe-revision"}
	generated := config.Document{Providers: []config.Provider{{Name: "sparkrun", Type: "sparkrun"}}}
	input := RecipeDraft{Reference: "catalog:123", RecipeRevision: f.revision, Name: "coding", Cluster: "lab"}
	draft, _, reused, err := PrepareRecipeDraft(context.Background(), f, managed.EmptyDocument(), generated, input)
	if err != nil || reused || len(draft.Providers) != 0 || draft.Deployments[0].Provider != "sparkrun" {
		t.Fatal("did not reuse the generated provider", draft, err)
	}
	if _, err := managed.Merge(map[managed.Owner]config.Document{managed.OwnerOperator: draft, managed.OwnerSparkrun: generated}); err != nil {
		t.Fatal(err)
	}
	generated.Providers[0].BaseURL = "http://localhost:8000/v1"
	if _, _, _, err := PrepareRecipeDraft(context.Background(), f, managed.EmptyDocument(), generated, input); err == nil || !strings.Contains(err.Error(), "conflicting settings") {
		t.Fatal("accepted conflicting shared provider", err)
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
	input.Recovery = config.RecoveryPolicy{Action: "stop", FailedProbes: 4}
	changed, actual, _, err := PrepareRecipeDraft(context.Background(), f, document, empty, input)
	if err != nil {
		t.Fatal(err)
	}
	after, _ := json.Marshal(changed.VirtualModels)
	if actual != id || string(before) != string(after) || len(changed.Deployments) != 1 {
		t.Fatal("route identity changed")
	}
	target := changed.Deployments[0]
	if target.Title != "Custom display title" || target.EndpointSource.Revision == revision || target.EndpointSource.IdleTTL != input.IdleTTL || target.EndpointSource.Recovery != input.Recovery {
		t.Fatal("policy edit did not preserve identity", target)
	}
	input.Deployment = "generated-or-deleted"
	if _, _, _, err := PrepareRecipeDraft(context.Background(), f, empty, document, input); err == nil {
		t.Fatal("edited a generated deployment")
	}
}

type fallbackCatalogFixture struct{ catalogFixture }

func (f *fallbackCatalogFixture) Catalog(ctx context.Context, op string, args map[string]any, result any) error {
	if op == "catalog_clusters" {
		raw := []byte(`{"clusters":[{"name":"lab","host_count":2},{"name":"spare","host_count":2}]}`)
		return json.Unmarshal(raw, result)
	}
	return f.catalogFixture.Catalog(ctx, op, args, result)
}
func TestRecipeDraftPreservesOrderedFallbackAndRejectsDuplicates(t *testing.T) {
	f := &fallbackCatalogFixture{catalogFixture{revision: "revision"}}
	input := RecipeDraft{Reference: "recipe", RecipeRevision: "revision", Name: "coding", Cluster: "lab", FallbackClusters: []string{"spare"}}
	document, id, _, err := PrepareRecipeDraft(context.Background(), f, managed.EmptyDocument(), managed.EmptyDocument(), input)
	if err != nil {
		t.Fatal(err)
	}
	candidates := document.Deployments[0].EndpointSource.ClusterCandidates
	if len(candidates) != 2 || candidates[0] != "lab" || candidates[1] != "spare" {
		t.Fatal(candidates)
	}
	input.Deployment = id
	input.Cluster = "spare"
	input.FallbackClusters = []string{"lab"}
	edited, editedID, _, err := PrepareRecipeDraft(context.Background(), f, document, managed.EmptyDocument(), input)
	if err != nil || editedID != id || edited.Deployments[0].EndpointSource.ClusterCandidates[0] != "spare" {
		t.Fatal(edited, err)
	}
	input.FallbackClusters = []string{"spare"}
	if _, _, _, err := PrepareRecipeDraft(context.Background(), f, document, managed.EmptyDocument(), input); err == nil {
		t.Fatal("duplicate candidate accepted")
	}
}

func TestRecipeNativeAPIChoicesPersistAndEdit(t *testing.T) {
	f := &catalogFixture{revision: "r"}
	empty := managed.EmptyDocument()
	input := RecipeDraft{Reference: "catalog:123", RecipeRevision: "r", Name: "coding", Cluster: "lab", NativeAPIs: []string{"chat_completions", "responses", "messages"}}
	doc, id, _, err := PrepareRecipeDraft(context.Background(), f, empty, empty, input)
	if err != nil {
		t.Fatal(err)
	}
	if doc.Providers[0].Type != "sparkrun" || len(doc.Deployments[0].NativeProtocols) != 2 || len(doc.Deployments[0].Capabilities) != 1 {
		t.Fatal("native declarations missing", doc)
	}
	input.Deployment = id
	input.NativeAPIs = []string{"chat_completions"}
	edited, _, _, err := PrepareRecipeDraft(context.Background(), f, doc, empty, input)
	if err != nil {
		t.Fatal(err)
	}
	if len(edited.Deployments[0].Capabilities) != 0 || len(edited.Deployments[0].NativeProtocols) != 1 {
		t.Fatal("unselected API persisted")
	}
	input.NativeAPIs = []string{"generate_content"}
	if _, _, _, err := PrepareRecipeDraft(context.Background(), f, doc, empty, input); err == nil {
		t.Fatal("accepted API outside runtime family")
	}
}

func TestRecipeGatewayDefaultsCreateProfilesOnOneDeployment(t *testing.T) {
	f := &catalogFixture{revision: "same-workload", settings: RecipeSettings{
		Capabilities: []config.Capability{config.CapabilityVision},
		RequestProfiles: map[string]map[string]map[string]json.RawMessage{
			"low":   {"chat_completions": {"chat_template_kwargs": json.RawMessage(`{"enable_thinking":false}`)}, "responses": {"reasoning": json.RawMessage(`{"effort":"low"}`)}},
			"xhigh": {"chat_completions": {"reasoning_effort": json.RawMessage(`"xhigh"`)}},
		},
	}}
	empty := managed.EmptyDocument()
	input := RecipeDraft{Reference: "catalog:123", RecipeRevision: f.revision, Name: "coding", Aliases: []string{"code"}, Cluster: "lab"}
	doc, id, reused, err := PrepareRecipeDraft(context.Background(), f, empty, empty, input)
	if err != nil {
		t.Fatal(err)
	}
	if reused || len(doc.Deployments) != 1 || len(doc.VirtualModels) != 3 {
		t.Fatal(doc)
	}
	if err := doc.Validate(); err != nil {
		t.Fatal(err)
	}
	if len(doc.Deployments[0].Capabilities) != 1 || doc.Deployments[0].Capabilities[0] != config.CapabilityVision {
		t.Fatal("vision missing", doc)
	}
	for _, model := range doc.VirtualModels {
		if model.Pools[0].Targets[0].Deployment != id {
			t.Fatal("profile created another workload")
		}
	}
	low, ok := doc.CanonicalModel("code:low")
	if !ok || low.Name != "coding:low" || string(low.RequestOverrides["responses"]["reasoning"]) != `{"effort":"low"}` {
		t.Fatal(low)
	}
	// A different public name can share generated workload settings and import its profiles.
	input.Name, input.Aliases = "assistant", nil
	shared, sharedID, reused, err := PrepareRecipeDraft(context.Background(), f, empty, doc, input)
	if err != nil || !reused || sharedID != id || len(shared.Deployments) != 0 || len(shared.VirtualModels) != 3 {
		t.Fatal(err, shared)
	}
	if _, err := managed.Merge(map[managed.Owner]config.Document{managed.OwnerOperator: shared, managed.OwnerSparkrun: doc}); err != nil {
		t.Fatal(err)
	}
	// Applying lifecycle edits does not replace request parameters already edited by the operator.
	doc.VirtualModels[1].RequestOverrides = map[string]map[string]json.RawMessage{"chat_completions": {"temperature": json.RawMessage(`0.9`)}}
	input.Deployment = id
	edited, _, _, err := PrepareRecipeDraft(context.Background(), f, doc, empty, input)
	if err != nil || string(edited.VirtualModels[1].RequestOverrides["chat_completions"]["temperature"]) != "0.9" {
		t.Fatal("overwrote operator profile", err)
	}
}

func TestRecipeProfilesRejectNameCollisionsAndInvalidOverrides(t *testing.T) {
	empty := managed.EmptyDocument()
	input := RecipeDraft{Reference: "catalog:123", RecipeRevision: "r", Name: "coding", Cluster: "lab"}
	f := &catalogFixture{revision: "r", settings: RecipeSettings{RequestProfiles: map[string]map[string]map[string]json.RawMessage{
		"low": {"chat_completions": {"temperature": json.RawMessage(`0.2`)}},
	}}}
	for _, model := range []config.VirtualModel{{Name: "coding:low"}, {Name: "other", Aliases: []string{"coding:low"}}} {
		generated := empty
		generated.VirtualModels = []config.VirtualModel{model}
		if _, _, _, err := PrepareRecipeDraft(context.Background(), f, empty, generated, input); err == nil || !strings.Contains(err.Error(), "conflicts") {
			t.Fatal("accepted collision", err)
		}
	}
	f.settings.RequestProfiles["low"]["chat_completions"] = map[string]json.RawMessage{"model": json.RawMessage(`"other"`)}
	if _, _, _, err := PrepareRecipeDraft(context.Background(), f, empty, empty, input); err == nil || !strings.Contains(err.Error(), "protocol-owned") {
		t.Fatal("accepted protected field", err)
	}
	f.settings.RequestProfiles = map[string]map[string]map[string]json.RawMessage{"low:bad": {"responses": {"temperature": json.RawMessage(`0.2`)}}}
	if _, _, _, err := PrepareRecipeDraft(context.Background(), f, empty, empty, input); err == nil {
		t.Fatal("accepted invalid selector")
	}
}
