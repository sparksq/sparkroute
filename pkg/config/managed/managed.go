// Package managed defines the single-process, ownership-aware configuration
// contract used by the mutable standalone profile. Managed sets are fragments; only
// their deterministic merge is a runnable gateway configuration.
package managed

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/sparksq/sparkroute/pkg/config"
	"github.com/sparksq/sparkroute/pkg/modelrouter"
)

var (
	ErrRevisionConflict     = errors.New("active managed configuration revision changed")
	ErrSetNotFound          = errors.New("managed configuration set not found")
	ErrInvalidConfiguration = errors.New("invalid managed gateway configuration")
)

type Owner string

const (
	OwnerOperator Owner = "operator"
	OwnerSparkrun Owner = "sparkrun"
)

var ownerOrder = []Owner{OwnerOperator, OwnerSparkrun}

func (o Owner) Validate() error {
	switch o {
	case OwnerOperator, OwnerSparkrun:
		return nil
	default:
		return fmt.Errorf("unsupported configuration owner %q", o)
	}
}

type SetMetadata struct {
	Owner     Owner          `json:"owner"`
	Revision  config.Version `json:"revision"`
	UpdatedAt time.Time      `json:"updated_at"`
	UpdatedBy string         `json:"updated_by"`
	Reason    string         `json:"reason,omitempty"`
}

type Set struct {
	SetMetadata
	Document config.Document `json:"document"`
}

// Current describes the one merged configuration stored by the standalone
// profile. Version is a content fingerprint used for integrity checks and
// optimistic concurrency; it does not identify a retained historical revision.
type Current struct {
	Version   config.Version `json:"revision"`
	UpdatedAt time.Time      `json:"updated_at"`
	UpdatedBy string         `json:"updated_by"`
	Reason    string         `json:"reason,omitempty"`
}

type ReplaceOptions struct {
	ExpectedActive config.Version
	Actor          string
	Reason         string
}

type ReplaceResult struct {
	Set     SetMetadata `json:"managed_set"`
	Current Current     `json:"current"`
	Changed bool        `json:"changed"`
}

type Validation struct {
	Owner             Owner          `json:"owner"`
	SetRevision       config.Version `json:"set_revision"`
	ActiveRevision    config.Version `json:"active_revision"`
	CandidateRevision config.Version `json:"candidate_revision"`
	Valid             bool           `json:"valid"`
}

type Store interface {
	config.WatchSource
	Initialize(context.Context, config.Document, string, string) (bool, error)
	ListSets(context.Context) ([]SetMetadata, error)
	GetSet(context.Context, Owner) (Set, error)
	BuildCandidate(context.Context, Owner, config.Document, config.Version) (config.Document, Validation, error)
	ValidateSet(context.Context, Owner, config.Document, config.Version) (Validation, error)
	ReplaceSet(context.Context, Owner, config.Document, ReplaceOptions) (ReplaceResult, error)
	Close() error
}

type Validator func(config.Document) error

// Merge applies operator exclusions to generated sparkrun entries, then
// concatenates the remaining entity collections without field overlays.
// One owner may additionally supply the document-wide capability defaults.
// The final document validator rejects cross-owner name/alias/default collisions
// and all references broken by a managed-set replacement.
func Merge(sets map[Owner]config.Document) (config.Document, error) {
	filtered := make(map[Owner]config.Document, len(sets))
	for owner, document := range sets {
		if err := owner.Validate(); err != nil {
			return config.Document{}, err
		}
		if document.SparkrunOverrides != nil && owner != OwnerOperator {
			return config.Document{}, fmt.Errorf("sparkrun_overrides must be managed by the operator")
		}
		filtered[owner] = document
	}
	if err := sets[OwnerOperator].SparkrunOverrides.Validate(); err != nil {
		return config.Document{}, err
	}
	if generated, exists := sets[OwnerSparkrun]; exists {
		var err error
		filtered[OwnerSparkrun], err = ApplySparkrunOverrides(sets[OwnerOperator], generated)
		if err != nil {
			return config.Document{}, err
		}
	}
	sets = filtered
	ordered := orderedOwners(sets)
	if err := validateOwnedEntities(sets, ordered); err != nil {
		return config.Document{}, fmt.Errorf("validate merged managed configuration: %w", err)
	}
	result := EmptyDocument()
	var capabilityDefaultsOwner Owner
	var modelRoutingOwner Owner
	for _, owner := range ordered {
		document := sets[owner]
		if document.SparkrunOverrides != nil {
			result.SparkrunOverrides = &config.SparkrunOverrides{
				ExcludedDeployments: append([]string(nil), document.SparkrunOverrides.ExcludedDeployments...),
			}
		}
		if len(document.PIIProfiles) > 0 || len(document.GuardrailProfiles) > 0 || len(document.ModelPolicies) > 0 {
			if owner != OwnerOperator {
				return config.Document{}, fmt.Errorf("policy profiles and model_policies must be managed by the operator")
			}
			// Clone nested policy data as well as maps: runtime and owner documents
			// must not share mutable assignments or profile slices.
			raw, err := json.Marshal(config.Document{PIIProfiles: document.PIIProfiles, GuardrailProfiles: document.GuardrailProfiles, ModelPolicies: document.ModelPolicies})
			if err != nil {
				return config.Document{}, err
			}
			var policies config.Document
			if err := json.Unmarshal(raw, &policies); err != nil {
				return config.Document{}, err
			}
			result.PIIProfiles = policies.PIIProfiles
			result.GuardrailProfiles = policies.GuardrailProfiles
			result.ModelPolicies = policies.ModelPolicies
		}
		if document.MMProjection != nil {
			if owner != OwnerOperator {
				return config.Document{}, fmt.Errorf("mm_projection must be managed by the operator")
			}
			connection := *document.MMProjection
			result.MMProjection = &connection
		}
		if document.Observability != nil {
			if owner != OwnerOperator {
				return config.Document{}, fmt.Errorf("observability must be managed by the operator")
			}
			raw, _ := json.Marshal(document.Observability)
			if err := json.Unmarshal(raw, &result.Observability); err != nil {
				return config.Document{}, err
			}
		}
		if !document.CapabilityDefaults.IsZero() {
			if !result.CapabilityDefaults.IsZero() {
				return config.Document{}, fmt.Errorf(
					"validate merged managed configuration: capability_defaults owned by %q conflicts with capability_defaults owned by %q",
					owner,
					capabilityDefaultsOwner,
				)
			}
			result.CapabilityDefaults = document.CapabilityDefaults
			capabilityDefaultsOwner = owner
		}
		if document.ModelRouting != nil {
			if result.ModelRouting != nil {
				return config.Document{}, fmt.Errorf(
					"validate merged managed configuration: model_routing owned by %q conflicts with model_routing owned by %q",
					owner,
					modelRoutingOwner,
				)
			}
			cloned := modelrouter.CloneRoutingPolicy(*document.ModelRouting)
			result.ModelRouting = &cloned
			modelRoutingOwner = owner
		}
		result.Providers = append(result.Providers, document.Providers...)
		result.Deployments = append(result.Deployments, document.Deployments...)
		result.VirtualModels = append(result.VirtualModels, document.VirtualModels...)
	}
	if err := result.Validate(); err != nil {
		return config.Document{}, fmt.Errorf("validate merged managed configuration: %w", err)
	}
	return result, nil
}

func orderedOwners(sets map[Owner]config.Document) []Owner {
	ordered := append([]Owner(nil), ownerOrder...)
	extra := make([]string, 0)
	for owner := range sets {
		if owner != OwnerOperator && owner != OwnerSparkrun {
			extra = append(extra, string(owner))
		}
	}
	sort.Strings(extra)
	for _, raw := range extra {
		ordered = append(ordered, Owner(raw))
	}
	result := make([]Owner, 0, len(sets))
	for _, owner := range ordered {
		if _, exists := sets[owner]; exists {
			result = append(result, owner)
		}
	}
	return result
}

type ownedEntity struct {
	owner Owner
	kind  string
	model string
}

func validateOwnedEntities(
	sets map[Owner]config.Document,
	ordered []Owner,
) error {
	providers := make(map[string]ownedEntity)
	deployments := make(map[string]ownedEntity)
	models := make(map[string]ownedEntity)
	for _, owner := range ordered {
		document := sets[owner]
		for _, provider := range document.Providers {
			current := ownedEntity{owner: owner, kind: "provider"}
			if previous, exists := providers[provider.Name]; exists {
				return ownedCollision(provider.Name, current, previous)
			}
			providers[provider.Name] = current
		}
		for _, deployment := range document.Deployments {
			current := ownedEntity{owner: owner, kind: "deployment"}
			if previous, exists := deployments[deployment.Name]; exists {
				return ownedCollision(deployment.Name, current, previous)
			}
			deployments[deployment.Name] = current
		}
		for _, model := range document.VirtualModels {
			current := ownedEntity{
				owner: owner, kind: "virtual model", model: model.Name,
			}
			if previous, exists := models[model.Name]; exists {
				return ownedCollision(model.Name, current, previous)
			}
			models[model.Name] = current
			for _, alias := range model.Aliases {
				current := ownedEntity{
					owner: owner, kind: "alias", model: model.Name,
				}
				if previous, exists := models[alias]; exists {
					return ownedCollision(alias, current, previous)
				}
				models[alias] = current
			}
		}
	}
	for _, owner := range ordered {
		document := sets[owner]
		for _, deployment := range document.Deployments {
			if _, exists := providers[deployment.Provider]; !exists {
				return fmt.Errorf(
					"deployment %q owned by %q references missing provider %q",
					deployment.Name,
					owner,
					deployment.Provider,
				)
			}
		}
		for _, model := range document.VirtualModels {
			for _, pool := range model.Pools {
				for _, target := range pool.Targets {
					if _, exists := deployments[target.Deployment]; !exists {
						return fmt.Errorf(
							"virtual model %q owned by %q references missing deployment %q",
							model.Name,
							owner,
							target.Deployment,
						)
					}
				}
			}
			phases := [][]config.Guardrail{model.Guardrails.Pre, model.Guardrails.Post}
			for _, guardrails := range phases {
				for _, guardrail := range guardrails {
					if _, exists := models[guardrail.Model]; !exists {
						return fmt.Errorf(
							"virtual model %q owned by %q guardrail %q references missing virtual model or alias %q",
							model.Name,
							owner,
							guardrail.Name,
							guardrail.Model,
						)
					}
				}
			}
		}
	}
	return nil
}

func ownedCollision(name string, current, previous ownedEntity) error {
	return fmt.Errorf(
		"%s owned by %q conflicts with %s owned by %q",
		ownedDescription(name, current),
		current.owner,
		ownedDescription(name, previous),
		previous.owner,
	)
}

func ownedDescription(name string, entity ownedEntity) string {
	if entity.kind == "alias" {
		return fmt.Sprintf("alias %q for virtual model %q", name, entity.model)
	}
	return fmt.Sprintf("%s %q", entity.kind, name)
}

func FragmentRevision(document config.Document) ([]byte, config.Version, error) {
	document = NormalizeFragment(document)
	raw, err := json.Marshal(document)
	if err != nil {
		return nil, "", fmt.Errorf("encode managed configuration fragment: %w", err)
	}
	digest := sha256.Sum256(raw)
	return raw, config.Version(hex.EncodeToString(digest[:])), nil
}

func NormalizeFragment(document config.Document) config.Document {
	if document.Providers == nil {
		document.Providers = []config.Provider{}
	}
	if document.Deployments == nil {
		document.Deployments = []config.Deployment{}
	}
	if document.VirtualModels == nil {
		document.VirtualModels = []config.VirtualModel{}
	}
	return document
}

func EmptyDocument() config.Document {
	return config.Document{
		Providers: []config.Provider{}, Deployments: []config.Deployment{},
		VirtualModels: []config.VirtualModel{},
	}
}
