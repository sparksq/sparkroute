// SPDX-FileCopyrightText: 2026 Scitrera LLC
// SPDX-FileCopyrightText: 2026 Fox Engine Ltd.
// SPDX-License-Identifier: AGPL-3.0-only

package config

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestRecoveryPolicyValidationAndRoundTrip(t *testing.T) {
	for _, policy := range []RecoveryPolicy{{}, {Action: "restart"}, {Action: "stop"}} {
		if err := policy.Validate(); err != nil {
			t.Fatal(err)
		}
	}
	for _, policy := range []RecoveryPolicy{
		{MaxRestarts: 3}, {Action: "sleep"}, {Action: "restart", FailedProbes: -1},
		{Action: "restart", MaxRestarts: 101}, {Action: "restart", UnhealthyFor: -1},
		{Action: "restart", DrainTimeout: Duration(25 * time.Hour)}, {Action: "restart", Backoff: Duration(time.Hour)},
	} {
		if policy.Validate() == nil {
			t.Fatalf("accepted %+v", policy)
		}
	}
	p := RecoveryPolicy{Action: "restart", FailedProbes: 4, UnhealthyFor: Duration(3 * time.Minute)}
	source := EndpointSource{Type: EndpointSourceActivatable, Controller: "sparkrun", Recipe: "recipe", Revision: "binding", RecipeRevision: "recipe-revision", Recovery: p}
	if err := source.Validate(); err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(source)
	if err != nil {
		t.Fatal(err)
	}
	var restored EndpointSource
	if err = json.Unmarshal(raw, &restored); err != nil || restored.Recovery != p {
		t.Fatalf("round trip %s: %+v %v", raw, restored, err)
	}
	for _, kind := range []EndpointSourceType{EndpointSourceStatic, EndpointSourceDiscovered} {
		source.Type = kind
		if source.Validate() == nil {
			t.Fatalf("accepted recovery on %s", kind)
		}
	}
	source.Type, source.Controller = EndpointSourceActivatable, "other-controller"
	if source.Validate() == nil {
		t.Fatal("accepted recovery for another controller")
	}
	raw, _ = json.Marshal(EndpointSource{})
	if strings.Contains(string(raw), "recovery") {
		t.Fatalf("disabled recovery changed serialized config: %s", raw)
	}
}
