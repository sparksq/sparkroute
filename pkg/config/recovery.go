// SPDX-FileCopyrightText: 2026 Scitrera LLC
// SPDX-FileCopyrightText: 2026 Fox Engine Ltd.
// SPDX-License-Identifier: AGPL-3.0-only

package config

import (
	"fmt"
	"time"
)

// RecoveryPolicy is opt-in remediation of a persistently failing owned job.
// Its budget is shared by physical workload replacements, not by request aliases.
type RecoveryPolicy struct {
	Action       string   `json:"action,omitempty"`
	FailedProbes int      `json:"failed_probes,omitempty"`
	UnhealthyFor Duration `json:"unhealthy_for,omitempty"`
	DrainTimeout Duration `json:"drain_timeout,omitempty"`
	Backoff      Duration `json:"backoff,omitempty"`
	MaxBackoff   Duration `json:"max_backoff,omitempty"`
	MaxRestarts  int      `json:"max_restarts,omitempty"`
}

func (p RecoveryPolicy) Effective() RecoveryPolicy {
	if p.Action == "" {
		return RecoveryPolicy{}
	}
	if p.FailedProbes == 0 {
		p.FailedProbes = 3
	}
	if p.UnhealthyFor == 0 {
		p.UnhealthyFor = Duration(2 * time.Minute)
	}
	if p.DrainTimeout == 0 {
		p.DrainTimeout = Duration(30 * time.Second)
	}
	if p.Backoff == 0 {
		p.Backoff = Duration(time.Minute)
	}
	if p.MaxBackoff == 0 {
		p.MaxBackoff = Duration(15 * time.Minute)
	}
	if p.MaxRestarts == 0 {
		p.MaxRestarts = 3
	}
	return p
}

func (p RecoveryPolicy) Validate() error {
	if p.Action == "" {
		if p != (RecoveryPolicy{}) {
			return fmt.Errorf("action is required when recovery settings are supplied")
		}
		return nil
	}
	if p.Action != "restart" && p.Action != "stop" {
		return fmt.Errorf("action must be restart or stop")
	}
	p = p.Effective()
	if p.FailedProbes < 1 || p.FailedProbes > 100 {
		return fmt.Errorf("failed_probes must be between 1 and 100")
	}
	if p.MaxRestarts < 1 || p.MaxRestarts > 100 {
		return fmt.Errorf("max_restarts must be between 1 and 100")
	}
	for _, field := range []struct {
		name  string
		value Duration
	}{
		{"unhealthy_for", p.UnhealthyFor}, {"drain_timeout", p.DrainTimeout},
		{"backoff", p.Backoff}, {"max_backoff", p.MaxBackoff},
	} {
		if field.value.Value() <= 0 || field.value.Value() > 24*time.Hour {
			return fmt.Errorf("%s must be greater than zero and at most 24h", field.name)
		}
	}
	if p.MaxBackoff < p.Backoff {
		return fmt.Errorf("max_backoff must be at least backoff")
	}
	return nil
}
