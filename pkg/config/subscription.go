// SPDX-License-Identifier: AGPL-3.0-only
// Copyright 2026 Scitrera LLC and Fox Engine Ltd.

package config

import "fmt"

// ValidateSubscriptionProfile checks a stable, non-secret credential profile name.
func ValidateSubscriptionProfile(profile string) error {
	if len(profile) < 1 || len(profile) > 64 {
		return fmt.Errorf("subscription profile must contain 1–64 letters, digits, underscores, or hyphens")
	}
	for _, c := range profile {
		if c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '_' || c == '-' {
			continue
		}
		return fmt.Errorf("subscription profile must contain only letters, digits, underscores, or hyphens")
	}
	return nil
}

func validateSubscriptionProvider(provider Provider) error {
	if provider.Type != "openai_subscription" {
		if provider.SubscriptionProfile != "" {
			return fmt.Errorf("subscription_profile requires an openai_subscription provider")
		}
		return nil
	}
	if err := ValidateSubscriptionProfile(provider.SubscriptionProfile); err != nil {
		return err
	}
	if provider.BaseURL != "" || provider.Auth != (ProviderAuth{}) || provider.Region != "" {
		return fmt.Errorf("subscription providers use the fixed Codex endpoint and managed sign-in; omit base_url, auth, and region")
	}
	return nil
}
