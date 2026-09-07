// SPDX-License-Identifier: AGPL-3.0-only
// Copyright 2026 Scitrera LLC
// Copyright 2026 Fox Engine Ltd.

package modelrouter

import (
	"fmt"
	"math"
	"sort"
	"strings"
	"time"
)

const maxDiscoveredMetadataSources = 64

// DiscoveredModelMetadata is a field-preserving metadata contribution for one
// logical model. Pointers distinguish a discovered zero price from a field the
// source did not observe.
type DiscoveredModelMetadata struct {
	SizeB       *float64 `json:"size_b,omitempty"`
	InputPrice  *float64 `json:"input_price,omitempty"`
	OutputPrice *float64 `json:"output_price,omitempty"`
	Context     *int     `json:"context,omitempty"`
	Tags        []string `json:"tags,omitempty"`
}

// DiscoveredMetadataSnapshot atomically replaces all metadata from Source.
type DiscoveredMetadataSnapshot struct {
	Source     string                             `json:"source"`
	ObservedAt time.Time                          `json:"observed_at"`
	Models     map[string]DiscoveredModelMetadata `json:"models"`
}

// DiscoveredMetadataState is the safe administrative projection. Effective is
// the conservative reduction of all source snapshots before authored policy
// precedence is applied.
type DiscoveredMetadataState struct {
	Generation uint64                             `json:"generation"`
	Sources    []DiscoveredMetadataSnapshot       `json:"sources"`
	Effective  map[string]DiscoveredModelMetadata `json:"effective"`
}

// ReplaceDiscoveredMetadata publishes one validated complete source snapshot.
// Metadata refreshes preserve policy revision, adaptive observations, and
// round-robin counters.
func (r *RouterManager) ReplaceDiscoveredMetadata(snapshot DiscoveredMetadataSnapshot) error {
	normalized, err := normalizeDiscoveredMetadataSnapshot(snapshot)
	if err != nil {
		return err
	}
	r.publishMu.Lock()
	defer r.publishMu.Unlock()
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, exists := r.metadata[normalized.Source]; !exists &&
		len(r.metadata) >= maxDiscoveredMetadataSources {
		return fmt.Errorf("discovered metadata has more than %d sources", maxDiscoveredMetadataSources)
	}
	r.metadata[normalized.Source] = normalized
	r.metadataGeneration++
	r.runtime.Store(compileRoutingPolicy(mergeDiscoveredMetadata(r.policy, r.metadata)))
	return nil
}

// RemoveDiscoveredMetadata withdraws one source without changing durable policy.
func (r *RouterManager) RemoveDiscoveredMetadata(source string) error {
	source = strings.TrimSpace(source)
	if source == "" || len(source) > 128 || hasControlCharacter(source) {
		return fmt.Errorf("discovered metadata source is invalid")
	}
	r.publishMu.Lock()
	defer r.publishMu.Unlock()
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, exists := r.metadata[source]; !exists {
		return nil
	}
	delete(r.metadata, source)
	r.metadataGeneration++
	r.runtime.Store(compileRoutingPolicy(mergeDiscoveredMetadata(r.policy, r.metadata)))
	return nil
}

// ValidateDiscoveredMetadataSnapshot checks the public discovery contract
// without publishing it.
func ValidateDiscoveredMetadataSnapshot(snapshot DiscoveredMetadataSnapshot) error {
	_, err := normalizeDiscoveredMetadataSnapshot(snapshot)
	return err
}

func (r *RouterManager) DiscoveredMetadata() DiscoveredMetadataState {
	r.mu.RLock()
	defer r.mu.RUnlock()
	sources := make([]DiscoveredMetadataSnapshot, 0, len(r.metadata))
	for _, snapshot := range r.metadata {
		sources = append(sources, cloneDiscoveredMetadataSnapshot(snapshot))
	}
	sort.Slice(sources, func(i, j int) bool { return sources[i].Source < sources[j].Source })
	return DiscoveredMetadataState{
		Generation: r.metadataGeneration,
		Sources:    sources,
		Effective:  aggregateDiscoveredMetadata(sources),
	}
}

func normalizeDiscoveredMetadataSnapshot(snapshot DiscoveredMetadataSnapshot) (DiscoveredMetadataSnapshot, error) {
	snapshot.Source = strings.TrimSpace(snapshot.Source)
	if snapshot.Source == "" || len(snapshot.Source) > 128 || hasControlCharacter(snapshot.Source) {
		return DiscoveredMetadataSnapshot{}, fmt.Errorf("discovered metadata source is invalid")
	}
	if len(snapshot.Models) > maxRoutingModels {
		return DiscoveredMetadataSnapshot{}, fmt.Errorf("discovered metadata source %q has more than %d models", snapshot.Source, maxRoutingModels)
	}
	if snapshot.ObservedAt.IsZero() {
		snapshot.ObservedAt = time.Now().UTC()
	} else {
		snapshot.ObservedAt = snapshot.ObservedAt.UTC()
	}
	normalized := DiscoveredMetadataSnapshot{
		Source: snapshot.Source, ObservedAt: snapshot.ObservedAt,
		Models: make(map[string]DiscoveredModelMetadata, len(snapshot.Models)),
	}
	for model, metadata := range snapshot.Models {
		model = strings.TrimSpace(model)
		if model == "" || len(model) > 256 || hasControlCharacter(model) {
			return DiscoveredMetadataSnapshot{}, fmt.Errorf("discovered metadata has invalid model name %q", model)
		}
		if err := validateDiscoveredModelMetadata(model, metadata); err != nil {
			return DiscoveredMetadataSnapshot{}, err
		}
		metadata.Tags = normalizeDiscoveredTags(metadata.Tags)
		normalized.Models[model] = cloneDiscoveredModelMetadata(metadata)
	}
	return normalized, nil
}

func validateDiscoveredModelMetadata(model string, metadata DiscoveredModelMetadata) error {
	for field, value := range map[string]*float64{
		"size_b": metadata.SizeB, "input_price": metadata.InputPrice,
		"output_price": metadata.OutputPrice,
	} {
		if value == nil {
			continue
		}
		minimum := 0.0
		if field == "size_b" {
			minimum = math.SmallestNonzeroFloat64
		}
		if *value < minimum || math.IsNaN(*value) || math.IsInf(*value, 0) {
			return fmt.Errorf("discovered model %q has invalid %s", model, field)
		}
	}
	if metadata.Context != nil && *metadata.Context <= 0 {
		return fmt.Errorf("discovered model %q has invalid context", model)
	}
	if len(metadata.Tags) > maxRoutingTagsPerModel {
		return fmt.Errorf("discovered model %q has more than %d tags", model, maxRoutingTagsPerModel)
	}
	for _, tag := range metadata.Tags {
		if strings.TrimSpace(tag) == "" || len(tag) > 128 || hasControlCharacter(tag) {
			return fmt.Errorf("discovered model %q has invalid tag %q", model, tag)
		}
	}
	return nil
}

func mergeDiscoveredMetadata(policy RoutingPolicy, sources map[string]DiscoveredMetadataSnapshot) RoutingPolicy {
	effective := clonePolicy(policy)
	ordered := make([]DiscoveredMetadataSnapshot, 0, len(sources))
	for _, snapshot := range sources {
		ordered = append(ordered, snapshot)
	}
	sort.Slice(ordered, func(i, j int) bool { return ordered[i].Source < ordered[j].Source })
	for name, discovered := range aggregateDiscoveredMetadata(ordered) {
		metadata, configured := effective.Models[name]
		if metadata.DiscoveryDisabled {
			continue
		}
		if !configured {
			// Discovery enriches candidates that are already available from the
			// execution snapshot; it does not independently admit model names.
			metadata.Enabled = true
		}
		if metadata.SizeB == 0 && discovered.SizeB != nil {
			metadata.SizeB = *discovered.SizeB
		}
		if metadata.InputPrice == 0 && discovered.InputPrice != nil {
			metadata.InputPrice = *discovered.InputPrice
		}
		if metadata.OutputPrice == 0 && discovered.OutputPrice != nil {
			metadata.OutputPrice = *discovered.OutputPrice
		}
		if metadata.Context == 0 && discovered.Context != nil {
			metadata.Context = *discovered.Context
		}
		if len(metadata.Tags) == 0 && len(discovered.Tags) > 0 {
			metadata.Tags = append([]string(nil), discovered.Tags...)
		}
		effective.Models[name] = metadata
	}
	return effective
}

func aggregateDiscoveredMetadata(sources []DiscoveredMetadataSnapshot) map[string]DiscoveredModelMetadata {
	result := make(map[string]DiscoveredModelMetadata)
	for _, source := range sources {
		models := make([]string, 0, len(source.Models))
		for model := range source.Models {
			models = append(models, model)
		}
		sort.Strings(models)
		for _, model := range models {
			current := result[model]
			current = reduceDiscoveredModelMetadata(current, source.Models[model])
			result[model] = current
		}
	}
	return result
}

func reduceDiscoveredModelMetadata(current, next DiscoveredModelMetadata) DiscoveredModelMetadata {
	// Context uses the smallest advertised value because it is an admission
	// boundary. Size and price use maxima so selectors do not understate a
	// logical model whose deployments report different values.
	if next.Context != nil && (current.Context == nil || *next.Context < *current.Context) {
		current.Context = cloneInt(next.Context)
	}
	if next.SizeB != nil && (current.SizeB == nil || *next.SizeB > *current.SizeB) {
		current.SizeB = cloneFloat(next.SizeB)
	}
	if next.InputPrice != nil && (current.InputPrice == nil || *next.InputPrice > *current.InputPrice) {
		current.InputPrice = cloneFloat(next.InputPrice)
	}
	if next.OutputPrice != nil && (current.OutputPrice == nil || *next.OutputPrice > *current.OutputPrice) {
		current.OutputPrice = cloneFloat(next.OutputPrice)
	}
	current.Tags = normalizeDiscoveredTags(append(current.Tags, next.Tags...))
	return current
}

func normalizeDiscoveredTags(tags []string) []string {
	if len(tags) == 0 {
		return nil
	}
	normalized := make([]string, 0, len(tags))
	for _, tag := range tags {
		normalized = append(normalized, strings.TrimSpace(tag))
	}
	sort.Strings(normalized)
	return compactStrings(normalized)
}

func compactStrings(values []string) []string {
	if len(values) == 0 {
		return values
	}
	result := values[:1]
	for _, value := range values[1:] {
		if value != result[len(result)-1] {
			result = append(result, value)
		}
	}
	return result
}

func cloneDiscoveredMetadataSnapshot(snapshot DiscoveredMetadataSnapshot) DiscoveredMetadataSnapshot {
	result := snapshot
	result.Models = make(map[string]DiscoveredModelMetadata, len(snapshot.Models))
	for model, metadata := range snapshot.Models {
		result.Models[model] = cloneDiscoveredModelMetadata(metadata)
	}
	return result
}

func cloneDiscoveredModelMetadata(metadata DiscoveredModelMetadata) DiscoveredModelMetadata {
	metadata.SizeB = cloneFloat(metadata.SizeB)
	metadata.InputPrice = cloneFloat(metadata.InputPrice)
	metadata.OutputPrice = cloneFloat(metadata.OutputPrice)
	metadata.Context = cloneInt(metadata.Context)
	metadata.Tags = append([]string(nil), metadata.Tags...)
	return metadata
}

func cloneFloat(value *float64) *float64 {
	if value == nil {
		return nil
	}
	cloned := *value
	return &cloned
}

func cloneInt(value *int) *int {
	if value == nil {
		return nil
	}
	cloned := *value
	return &cloned
}

var _ DiscoveredMetadataPublisher = (*RouterManager)(nil)
var _ DiscoveredMetadataInspector = (*RouterManager)(nil)
