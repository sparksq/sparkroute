// SPDX-License-Identifier: AGPL-3.0-only
// Copyright 2026 Scitrera LLC and Fox Engine Ltd.

package config

import "testing"

func TestSubscriptionConfiguration(t *testing.T) {
	d := validDocument()
	d.Providers[0] = Provider{Name: d.Providers[0].Name, Type: "openai_subscription", SubscriptionProfile: "personal-codex"}
	d.Deployments[0].Credential = ""
	d.Deployments[0].Capabilities = append(d.Deployments[0].Capabilities, CapabilityResponses)
	if err := d.Validate(); err != nil {
		t.Fatal(err)
	}
	for name, mutate := range map[string]func(*Document){
		"redirect credentials":  func(d *Document) { d.Providers[0].BaseURL = "https://other.example" },
		"profile path":          func(d *Document) { d.Providers[0].SubscriptionProfile = "../account" },
		"missing profile":       func(d *Document) { d.Providers[0].SubscriptionProfile = "" },
		"auth override":         func(d *Document) { d.Providers[0].Auth = ProviderAuth{Type: AuthBearer, Credential: "env://TOKEN"} },
		"deployment credential": func(d *Document) { d.Deployments[0].Credential = "env://TOKEN" },
		"wrong protocol":        func(d *Document) { d.Deployments[0].NativeProtocols = []Protocol{ProtocolAnthropic} },
		"missing responses":     func(d *Document) { d.Deployments[0].Capabilities = nil },
	} {
		t.Run(name, func(t *testing.T) {
			copy := d
			copy.Providers = append([]Provider(nil), d.Providers...)
			copy.Deployments = append([]Deployment(nil), d.Deployments...)
			mutate(&copy)
			if err := copy.Validate(); err == nil {
				t.Fatal("accepted unsafe subscription configuration")
			}
		})
	}
}
