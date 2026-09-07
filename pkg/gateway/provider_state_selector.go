// SPDX-License-Identifier: AGPL-3.0-only
// Copyright 2026 Scitrera LLC
// Copyright 2026 Fox Engine Ltd.

package gateway

import (
	"context"
	"encoding/json"
	"net/http"

	"github.com/sparksq/sparkroute/pkg/ledger"
	"github.com/sparksq/sparkroute/pkg/modelrouter"
	"github.com/sparksq/sparkroute/pkg/responsesstate"
)

type providerStateSelectorFailure struct {
	status  int
	outcome ledger.Outcome
	code    string
	message string
}

// resolveProviderStateSelectorPin resolves only content-free affinity records.
// It runs before adaptive logical-model selection so a selector cannot move a
// provider-owned state chain to a different model. Ordinary execution resolves
// the records again after projection and guardrails and enforces the hard
// provider/deployment/upstream-model pin.
func resolveProviderStateSelectorPin(
	ctx context.Context,
	store responsesstate.Store,
	operation openAIOperation,
	envelope map[string]json.RawMessage,
	ownerScope string,
) (*modelrouter.ProviderStatePin, *providerStateSelectorFailure) {
	var state responsesRequestState
	if operation.isResponsesOperation() {
		state = classifyResponsesState(envelope)
		if operation == openAIOperationResponsesCompact {
			state = classifyResponsesCompactState(envelope)
		}
	}
	var err error
	state.itemIDs, err = collectResponsesItemAffinityIDs(envelope["input"])
	if err != nil && operation.isResponsesOperation() {
		return nil, selectorRequestFailure(err.Error())
	}
	if !operation.isResponsesOperation() {
		state.itemIDs = nil
	}
	state.fileIDs, err = collectProviderFileIDs(operation, envelope)
	if err != nil {
		return nil, selectorRequestFailure(err.Error())
	}

	var selected responsesstate.ResourceAffinity
	kind := ""
	accept := func(
		affinity responsesstate.ResourceAffinity,
		candidateKind string,
	) *providerStateSelectorFailure {
		if kind == "" {
			selected = affinity
			kind = candidateKind
			return nil
		}
		if selected.VirtualModel != affinity.VirtualModel {
			return &providerStateSelectorFailure{
				status:  http.StatusConflict,
				outcome: ledger.OutcomeRejected,
				code:    "state_affinity_model_mismatch",
				message: "provider-owned state references belong to different virtual models",
			}
		}
		if selected.Provider != affinity.Provider ||
			selected.Deployment != affinity.Deployment ||
			selected.UpstreamModel != affinity.UpstreamModel {
			return &providerStateSelectorFailure{
				status:  http.StatusConflict,
				outcome: ledger.OutcomeRejected,
				code:    "state_affinity_conflict",
				message: "provider-owned state references belong to different provider targets",
			}
		}
		if kind != candidateKind {
			kind = "mixed"
		}
		return nil
	}

	if state.previousResponseID != "" {
		affinity, found, _, resolveErr := resolveResponsesAffinity(
			ctx, store, ownerScope, state.previousResponseID,
		)
		if resolveErr != nil {
			return nil, selectorStoreFailure(
				"provider-owned response state could not be resolved",
			)
		}
		if !found {
			return nil, selectorMissingFailure(
				"previous_response_id is unknown or expired at this gateway",
			)
		}
		if failure := accept(responseResourceAffinity(affinity), "response"); failure != nil {
			return nil, failure
		}
	}

	resourceStore, supportsResources := store.(responsesstate.ResourceStore)
	needsResourceStore := state.conversationID != "" ||
		len(state.itemIDs) != 0 || len(state.fileIDs) != 0
	if needsResourceStore && !supportsResources {
		return nil, selectorStoreFailure(
			"provider-owned resource state storage is not configured",
		)
	}
	resolveResource := func(
		key responsesstate.ResourceKey,
		missingMessage string,
		unavailableMessage string,
		candidateKind string,
	) *providerStateSelectorFailure {
		affinity, found, resolveErr := resourceStore.ResolveResource(ctx, key)
		if resolveErr != nil {
			return selectorStoreFailure(unavailableMessage)
		}
		if !found || affinity.Deleted() {
			return selectorMissingFailure(missingMessage)
		}
		return accept(affinity, candidateKind)
	}

	if state.conversationID != "" {
		if failure := resolveResource(
			responsesstate.ResourceKey{
				Scope: ownerScope, Kind: responsesstate.ResourceConversation,
				ResourceID: state.conversationID,
			},
			"conversation is unknown or deleted at this gateway",
			"provider-owned conversation state could not be resolved",
			"conversation",
		); failure != nil {
			return nil, failure
		}
	}
	for _, itemID := range state.itemIDs {
		if failure := resolveResource(
			responsesstate.ResourceKey{
				Scope: ownerScope, Kind: responsesstate.ResourceItem,
				ResourceID: itemID,
			},
			"provider-owned item is unknown or deleted at this gateway",
			"provider-owned item routing state could not be resolved",
			"item",
		); failure != nil {
			return nil, failure
		}
	}
	for _, fileID := range state.fileIDs {
		if failure := resolveResource(
			responsesstate.ResourceKey{
				Scope: ownerScope, Kind: responsesstate.ResourceFile,
				ResourceID: fileID,
			},
			"file_id is unknown or deleted at this gateway",
			"provider file routing state could not be resolved",
			"file",
		); failure != nil {
			return nil, failure
		}
	}
	if kind == "" {
		return nil, nil
	}
	return &modelrouter.ProviderStatePin{
		VirtualModel: selected.VirtualModel,
		Kind:         kind,
	}, nil
}

func responseResourceAffinity(
	affinity responsesstate.Affinity,
) responsesstate.ResourceAffinity {
	return responsesstate.ResourceAffinity{
		ResourceKey: responsesstate.ResourceKey{
			Scope: affinity.Scope, Kind: responsesstate.ResourceResponse,
			ResourceID: affinity.ResponseID,
		},
		VirtualModel: affinity.VirtualModel, Provider: affinity.Provider,
		Deployment: affinity.Deployment, UpstreamModel: affinity.UpstreamModel,
		BoundAt: affinity.BoundAt, ExpiresAt: affinity.ExpiresAt,
	}
}

func selectorRequestFailure(message string) *providerStateSelectorFailure {
	return &providerStateSelectorFailure{
		status: http.StatusBadRequest, outcome: ledger.OutcomeRejected,
		code: "invalid_request_body", message: message,
	}
}

func selectorStoreFailure(message string) *providerStateSelectorFailure {
	return &providerStateSelectorFailure{
		status: http.StatusServiceUnavailable, outcome: ledger.OutcomeConfigError,
		code: "state_affinity_unavailable", message: message,
	}
}

func selectorMissingFailure(message string) *providerStateSelectorFailure {
	return &providerStateSelectorFailure{
		status: http.StatusConflict, outcome: ledger.OutcomeRejected,
		code: "state_affinity_not_found", message: message,
	}
}
