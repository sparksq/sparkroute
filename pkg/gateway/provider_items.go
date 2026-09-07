package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/sparksq/sparkroute/pkg/responsesstate"
	"github.com/sparksq/sparkroute/pkg/routing"
)

const maxProviderItemAffinityIDs = 4096

type itemAffinityResult int

const (
	itemAffinityResolved itemAffinityResult = iota
	itemAffinityNotFound
	itemAffinityModelMismatch
	itemAffinityTargetMismatch
)

func collectResponsesItemAffinityIDs(
	raw json.RawMessage,
) ([]string, error) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 ||
		bytes.Equal(trimmed, []byte("null")) ||
		trimmed[0] == '"' {
		return nil, nil
	}
	if trimmed[0] != '[' {
		return nil, nil
	}
	var items []json.RawMessage
	if err := json.Unmarshal(trimmed, &items); err != nil {
		return nil, fmt.Errorf("input must be an array of input items")
	}
	seen := make(map[string]struct{})
	itemIDs := make([]string, 0)
	for index, rawItem := range items {
		var item map[string]json.RawMessage
		if err := json.Unmarshal(rawItem, &item); err != nil || item == nil {
			return nil, fmt.Errorf("input item %d must be an object", index)
		}
		itemType, err := responsesInputItemType(item)
		if err != nil {
			return nil, fmt.Errorf("input item %d: %w", index, err)
		}
		field, requiresAffinity := responsesItemAffinityField(itemType, item)
		if !requiresAffinity {
			continue
		}
		rawID := item[field]
		if !rawNonNull(rawID) {
			return nil, fmt.Errorf(
				"input item %d: %s is required for provider-owned %s state",
				index,
				field,
				itemType,
			)
		}
		itemID, err := decodeProviderItemID(field, rawID)
		if err != nil {
			return nil, fmt.Errorf("input item %d: %w", index, err)
		}
		if _, exists := seen[itemID]; exists {
			continue
		}
		seen[itemID] = struct{}{}
		itemIDs = append(itemIDs, itemID)
	}
	return itemIDs, nil
}

func responsesInputItemType(
	item map[string]json.RawMessage,
) (string, error) {
	var itemType string
	if rawType := item["type"]; rawActive(rawType) {
		if err := json.Unmarshal(rawType, &itemType); err != nil {
			return "", fmt.Errorf("type must be a string")
		}
	}
	if itemType == "" && rawNonNull(item["id"]) && len(item) == 1 {
		return "item_reference", nil
	}
	return itemType, nil
}

func responsesItemAffinityField(
	itemType string,
	item map[string]json.RawMessage,
) (string, bool) {
	switch itemType {
	case "item_reference":
		return "id", true
	case "computer_call_output", "function_call_output", "custom_tool_call_output",
		"shell_call_output", "apply_patch_call_output", "program_output":
		if rawNonNull(item["id"]) {
			return "id", true
		}
		return "call_id", true
	case "local_shell_call_output":
		return "id", true
	case "mcp_approval_response":
		if rawNonNull(item["id"]) {
			return "id", true
		}
		return "approval_request_id", true
	case "message", "reasoning", "function_call", "custom_tool_call":
		if rawNonNull(item["id"]) {
			return "id", true
		}
		return "", false
	case "compaction":
		return "id", true
	case "web_search_call", "file_search_call", "computer_call",
		"code_interpreter_call", "image_generation_call", "mcp_call",
		"mcp_list_tools", "mcp_approval_request", "tool_search_call",
		"tool_search_output", "shell_call", "local_shell_call",
		"apply_patch_call", "program":
		if rawNonNull(item["id"]) {
			return "id", true
		}
		if rawNonNull(item["call_id"]) {
			return "call_id", true
		}
		return "id", true
	default:
		return "", false
	}
}

func responsesProviderToolItem(itemType string) bool {
	switch itemType {
	case "web_search_call", "file_search_call", "computer_call",
		"computer_call_output", "code_interpreter_call", "image_generation_call",
		"mcp_call", "mcp_list_tools", "mcp_approval_request",
		"mcp_approval_response", "tool_search_call", "tool_search_output",
		"shell_call", "shell_call_output", "local_shell_call",
		"local_shell_call_output", "apply_patch_call",
		"apply_patch_call_output", "program", "program_output":
		return true
	default:
		return false
	}
}

func decodeProviderItemID(
	field string,
	raw json.RawMessage,
) (string, error) {
	var itemID string
	if err := json.Unmarshal(raw, &itemID); err != nil {
		return "", fmt.Errorf("%s must be a string", field)
	}
	if err := responsesstate.ValidateResourceID(itemID); err != nil {
		return "", fmt.Errorf("%s: %w", field, err)
	}
	return itemID, nil
}

func resolveItemAffinities(
	ctx context.Context,
	store responsesstate.ResourceStore,
	ownerScope string,
	virtualModel string,
	itemIDs []string,
) (responsesstate.ResourceAffinity, itemAffinityResult, error) {
	var resolved responsesstate.ResourceAffinity
	for index, itemID := range itemIDs {
		affinity, found, err := store.ResolveResource(
			ctx,
			responsesstate.ResourceKey{
				Scope:      ownerScope,
				Kind:       responsesstate.ResourceItem,
				ResourceID: itemID,
			},
		)
		if err != nil {
			return responsesstate.ResourceAffinity{}, itemAffinityResolved, err
		}
		if !found || affinity.Deleted() {
			return responsesstate.ResourceAffinity{}, itemAffinityNotFound, nil
		}
		if affinity.VirtualModel != virtualModel {
			return responsesstate.ResourceAffinity{}, itemAffinityModelMismatch, nil
		}
		if index == 0 {
			resolved = affinity
			continue
		}
		if affinity.Provider != resolved.Provider ||
			affinity.Deployment != resolved.Deployment ||
			affinity.UpstreamModel != resolved.UpstreamModel {
			return responsesstate.ResourceAffinity{}, itemAffinityTargetMismatch, nil
		}
	}
	return resolved, itemAffinityResolved, nil
}

func bindProviderItemAffinities(
	ctx context.Context,
	store responsesstate.ResourceStore,
	ownerScope string,
	selection routing.Selection,
	itemIDs []string,
	expiresAt time.Time,
) error {
	if len(itemIDs) == 0 {
		return nil
	}
	boundAt := time.Now().UTC()
	if !expiresAt.IsZero() && !expiresAt.After(boundAt) {
		return fmt.Errorf("provider item affinity expired before it could be bound")
	}
	for _, itemID := range itemIDs {
		if err := store.BindResource(
			ctx,
			responsesstate.ResourceAffinity{
				ResourceKey: responsesstate.ResourceKey{
					Scope:      ownerScope,
					Kind:       responsesstate.ResourceItem,
					ResourceID: itemID,
				},
				VirtualModel:  selection.VirtualModel,
				Provider:      selection.Provider.Name,
				Deployment:    selection.Deployment.Name,
				UpstreamModel: selection.Deployment.Model,
				BoundAt:       boundAt,
				ExpiresAt:     expiresAt,
			},
		); err != nil {
			return err
		}
	}
	return nil
}

func (h *chatCompletionsHandler) bindResponseProviderItems(
	ctx context.Context,
	payload []byte,
	state responsesRequestState,
	selection routing.Selection,
) (string, error) {
	if state.ownerScope == "" {
		return "", nil
	}
	itemIDs, err := responseProviderItemIDs(payload)
	if err != nil {
		return "responses_item_state_invalid", err
	}
	if len(itemIDs) == 0 {
		return "", nil
	}
	resourceStore, ok := h.responsesState.(responsesstate.ResourceStore)
	if !ok {
		return "responses_item_state_bind_failed",
			fmt.Errorf("provider item affinity storage is not configured")
	}
	if err := bindProviderItemAffinities(
		ctx,
		resourceStore,
		state.ownerScope,
		selection,
		itemIDs,
		state.returnedItemExpiry(),
	); err != nil {
		if errors.Is(err, responsesstate.ErrConflict) {
			return "responses_item_state_conflict", err
		}
		return "responses_item_state_bind_failed", err
	}
	return "", nil
}

func responseProviderItemIDs(payload []byte) ([]string, error) {
	if bytes.Equal(bytes.TrimSpace(payload), []byte("[DONE]")) {
		return nil, nil
	}
	var envelope map[string]json.RawMessage
	if err := json.Unmarshal(payload, &envelope); err != nil || envelope == nil {
		if err == nil {
			err = fmt.Errorf("payload must be a JSON object")
		}
		return nil, err
	}
	seen := make(map[string]struct{})
	itemIDs := make([]string, 0)
	if rawResponse := envelope["response"]; rawNonNull(rawResponse) {
		var response map[string]json.RawMessage
		if err := json.Unmarshal(rawResponse, &response); err != nil || response == nil {
			return nil, fmt.Errorf("response must be a JSON object")
		}
		if err := appendProviderItemArray(
			response["output"],
			seen,
			&itemIDs,
		); err != nil {
			return nil, fmt.Errorf("response.output: %w", err)
		}
	}
	if err := appendProviderItemArray(
		envelope["output"],
		seen,
		&itemIDs,
	); err != nil {
		return nil, fmt.Errorf("output: %w", err)
	}
	var objectType string
	_ = json.Unmarshal(envelope["object"], &objectType)
	if objectType == "list" {
		if err := appendProviderItemArray(
			envelope["data"],
			seen,
			&itemIDs,
		); err != nil {
			return nil, fmt.Errorf("data: %w", err)
		}
	}
	if rawItem := envelope["item"]; rawNonNull(rawItem) {
		if err := appendProviderItem(rawItem, seen, &itemIDs); err != nil {
			return nil, fmt.Errorf("item: %w", err)
		}
	}
	return itemIDs, nil
}

func conversationProviderItemIDs(
	payload []byte,
	includeRoot bool,
) ([]string, error) {
	var envelope map[string]json.RawMessage
	if err := json.Unmarshal(payload, &envelope); err != nil || envelope == nil {
		if err == nil {
			err = fmt.Errorf("payload must be a JSON object")
		}
		return nil, err
	}
	seen := make(map[string]struct{})
	itemIDs := make([]string, 0)
	if err := appendProviderItemArray(
		envelope["data"],
		seen,
		&itemIDs,
	); err != nil {
		return nil, fmt.Errorf("data: %w", err)
	}
	if includeRoot {
		raw, err := json.Marshal(envelope)
		if err != nil {
			return nil, err
		}
		if err := appendProviderItem(raw, seen, &itemIDs); err != nil {
			return nil, err
		}
	}
	return itemIDs, nil
}

func appendProviderItemArray(
	raw json.RawMessage,
	seen map[string]struct{},
	itemIDs *[]string,
) error {
	if !rawNonNull(raw) {
		return nil
	}
	var items []json.RawMessage
	if err := json.Unmarshal(raw, &items); err != nil {
		return fmt.Errorf("must be an array of item objects")
	}
	for _, item := range items {
		if err := appendProviderItem(item, seen, itemIDs); err != nil {
			return err
		}
	}
	return nil
}

func appendProviderItem(
	raw json.RawMessage,
	seen map[string]struct{},
	itemIDs *[]string,
) error {
	var item map[string]json.RawMessage
	if err := json.Unmarshal(raw, &item); err != nil || item == nil {
		return fmt.Errorf("must contain item objects")
	}
	for _, field := range []string{"id", "call_id"} {
		if !rawNonNull(item[field]) {
			continue
		}
		itemID, err := decodeProviderItemID(field, item[field])
		if err != nil {
			return err
		}
		if _, exists := seen[itemID]; exists {
			continue
		}
		if len(*itemIDs) >= maxProviderItemAffinityIDs {
			return fmt.Errorf(
				"contains more than %d provider item identifiers",
				maxProviderItemAffinityIDs,
			)
		}
		seen[itemID] = struct{}{}
		*itemIDs = append(*itemIDs, itemID)
	}
	return nil
}
