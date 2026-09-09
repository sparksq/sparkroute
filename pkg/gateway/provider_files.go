// SPDX-FileCopyrightText: 2026 Scitrera LLC
// SPDX-FileCopyrightText: 2026 Fox Engine Ltd.
// SPDX-License-Identifier: AGPL-3.0-only

package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"sort"

	"github.com/sparksq/sparkroute/pkg/config"
	"github.com/sparksq/sparkroute/pkg/responsesstate"
)

type fileAffinityResult int

const (
	fileAffinityResolved fileAffinityResult = iota
	fileAffinityNotFound
	fileAffinityModelMismatch
	fileAffinityTargetMismatch
)

func collectProviderFileIDs(
	operation openAIOperation,
	envelope map[string]json.RawMessage,
) ([]string, error) {
	var roots []json.RawMessage
	switch operation {
	case openAIOperationChatCompletions:
		var messages []map[string]json.RawMessage
		if rawActive(envelope["messages"]) {
			if err := json.Unmarshal(envelope["messages"], &messages); err != nil {
				return nil, fmt.Errorf("messages must be an array of objects")
			}
		}
		roots = make([]json.RawMessage, 0, len(messages))
		for _, message := range messages {
			if rawActive(message["content"]) {
				roots = append(roots, message["content"])
			}
		}
	case openAIOperationResponses, openAIOperationResponsesCompact:
		for _, name := range []string{"input", "instructions"} {
			if rawActive(envelope[name]) {
				roots = append(roots, envelope[name])
			}
		}
	default:
		return nil, nil
	}

	seen := make(map[string]struct{})
	fileIDs := make([]string, 0)
	for _, root := range roots {
		if err := collectProviderFileIDsFromJSON(root, seen, &fileIDs); err != nil {
			return nil, err
		}
	}
	return fileIDs, nil
}

func collectProviderFileIDsFromJSON(
	raw json.RawMessage,
	seen map[string]struct{},
	fileIDs *[]string,
) error {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		return nil
	}
	switch trimmed[0] {
	case '[':
		var values []json.RawMessage
		if err := json.Unmarshal(trimmed, &values); err != nil {
			return fmt.Errorf("file input container must be valid JSON")
		}
		for _, value := range values {
			if err := collectProviderFileIDsFromJSON(value, seen, fileIDs); err != nil {
				return err
			}
		}
	case '{':
		var object map[string]json.RawMessage
		if err := json.Unmarshal(trimmed, &object); err != nil {
			return fmt.Errorf("file input container must be a JSON object")
		}
		if rawFileID := object["file_id"]; rawNonNull(rawFileID) {
			fileID, err := decodeProviderFileID(rawFileID)
			if err != nil {
				return err
			}
			if _, exists := seen[fileID]; !exists {
				seen[fileID] = struct{}{}
				*fileIDs = append(*fileIDs, fileID)
			}
		}
		for name, value := range object {
			if name == "file_id" {
				continue
			}
			switch name {
			case "content", "file", "items", "output":
				if err := collectProviderFileIDsFromJSON(value, seen, fileIDs); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

func decodeProviderFileID(raw json.RawMessage) (string, error) {
	var fileID string
	if err := json.Unmarshal(raw, &fileID); err != nil {
		return "", fmt.Errorf("file_id must be a string")
	}
	if err := responsesstate.ValidateResourceID(fileID); err != nil {
		return "", fmt.Errorf("file_id: %w", err)
	}
	return fileID, nil
}

func addRequiredCapability(
	required []config.Capability,
	capability config.Capability,
) []config.Capability {
	for _, existing := range required {
		if existing == capability {
			return required
		}
	}
	required = append(required, capability)
	sort.Slice(required, func(i, j int) bool {
		return required[i] < required[j]
	})
	return required
}

func resolveFileAffinities(
	ctx context.Context,
	store responsesstate.ResourceStore,
	ownerScope string,
	virtualModel string,
	fileIDs []string,
) (responsesstate.ResourceAffinity, fileAffinityResult, error) {
	var resolved responsesstate.ResourceAffinity
	for index, fileID := range fileIDs {
		affinity, found, err := store.ResolveResource(
			ctx,
			responsesstate.ResourceKey{
				Scope:      ownerScope,
				Kind:       responsesstate.ResourceFile,
				ResourceID: fileID,
			},
		)
		if err != nil {
			return responsesstate.ResourceAffinity{}, fileAffinityResolved, err
		}
		if !found || affinity.Deleted() {
			return responsesstate.ResourceAffinity{}, fileAffinityNotFound, nil
		}
		if affinity.VirtualModel != virtualModel {
			return responsesstate.ResourceAffinity{}, fileAffinityModelMismatch, nil
		}
		if index == 0 {
			resolved = affinity
			continue
		}
		if affinity.Provider != resolved.Provider ||
			affinity.Deployment != resolved.Deployment ||
			affinity.UpstreamModel != resolved.UpstreamModel {
			return responsesstate.ResourceAffinity{}, fileAffinityTargetMismatch, nil
		}
	}
	return resolved, fileAffinityResolved, nil
}
