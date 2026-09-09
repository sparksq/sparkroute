// SPDX-FileCopyrightText: 2026 Scitrera LLC
// SPDX-FileCopyrightText: 2026 Fox Engine Ltd.
// SPDX-License-Identifier: AGPL-3.0-only

package gateway

import (
	"strings"

	"github.com/sparksq/sparkroute/pkg/config"
	"github.com/sparksq/sparkroute/pkg/identity"
)

func trustedSelectionKey(
	policy config.SelectionPolicy,
	caller identity.Identity,
	configRevision string,
	virtualModel string,
) string {
	if policy.Mode != config.SelectionWeightedHash {
		return ""
	}
	var value string
	switch policy.HashKey {
	case config.SelectionHashTenant:
		value = caller.Principal.Tenant
		if value == "" {
			value = caller.Attribution[identity.AttributeTenant]
		}
	case config.SelectionHashPrincipal:
		value = caller.Principal.ID
	case config.SelectionHashUser:
		value = caller.Attribution[identity.AttributeUser]
	case config.SelectionHashWorkspace:
		value = caller.Attribution[identity.AttributeWorkspace]
	case config.SelectionHashThreadID:
		value = caller.Attribution[identity.AttributeThreadID]
	case config.SelectionHashTaskID:
		value = caller.Attribution[identity.AttributeTaskID]
	case config.SelectionHashExperiment:
		value = caller.Attribution[identity.AttributeExperiment]
	case config.SelectionHashSession:
		value = caller.Attribution[identity.AttributeSession]
	}
	if strings.TrimSpace(value) == "" {
		return ""
	}
	return "v1\x00" + configRevision + "\x00" + virtualModel + "\x00" +
		string(policy.HashKey) + "\x00" + value
}
