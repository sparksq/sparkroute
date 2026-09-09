// SPDX-FileCopyrightText: 2026 Scitrera LLC
// SPDX-FileCopyrightText: 2026 Fox Engine Ltd.
// SPDX-License-Identifier: AGPL-3.0-only

package config_test

import (
	"strings"
	"testing"

	"github.com/sparksq/sparkroute/pkg/config"
	"github.com/sparksq/sparkroute/pkg/config/managed"
)

func TestMMProjectionSettingsValidation(t *testing.T) {
	valid := config.MMProjectionConfig{Enabled: true, URL: "http://localhost:8100/v1", TokenRef: "env://MMBRIDGE_TOKEN", AnalyzerModel: "vision", TimeoutMS: 600000}
	for name, change := range map[string]func(*config.MMProjectionConfig){
		"missing URL":        func(m *config.MMProjectionConfig) { m.URL = "" },
		"credentials in URL": func(m *config.MMProjectionConfig) { m.URL = "https://user:secret@example.test/v1" },
		"query in URL":       func(m *config.MMProjectionConfig) { m.URL = "https://example.test/v1?token=secret" },
		"fragment":           func(m *config.MMProjectionConfig) { m.URL += "#fragment" },
		"invalid scheme":     func(m *config.MMProjectionConfig) { m.URL = "file:///private/file" },
		"missing token":      func(m *config.MMProjectionConfig) { m.TokenRef = "" },
		"literal token":      func(m *config.MMProjectionConfig) { m.TokenRef = "secret" },
		"analyzer controls":  func(m *config.MMProjectionConfig) { m.AnalyzerModel = "model\x7f" },
		"analyzer length":    func(m *config.MMProjectionConfig) { m.AnalyzerModel = strings.Repeat("a", 257) },
		"negative timeout":   func(m *config.MMProjectionConfig) { m.TimeoutMS = -1 },
		"short timeout":      func(m *config.MMProjectionConfig) { m.TimeoutMS = 999 },
		"long timeout":       func(m *config.MMProjectionConfig) { m.TimeoutMS = 900001 },
	} {
		t.Run(name, func(t *testing.T) {
			candidate := valid
			change(&candidate)
			document := managed.EmptyDocument()
			document.MMProjection = &candidate
			err := document.Validate()
			if err == nil || !strings.Contains(err.Error(), "mm_projection") || strings.Contains(err.Error(), "secret") {
				t.Fatalf("expected scoped, redacted validation error, got %v", err)
			}
		})
	}
	for _, setting := range []*config.MMProjectionConfig{nil, {}, &valid, {Enabled: true, URL: valid.URL, TokenRef: valid.TokenRef}} {
		document := managed.EmptyDocument()
		document.MMProjection = setting
		if err := document.Validate(); err != nil {
			t.Fatal(err)
		}
	}
}

func TestMMProjectionOperatorOwnershipAndCredentialTracking(t *testing.T) {
	doc := managed.EmptyDocument()
	doc.MMProjection = &config.MMProjectionConfig{Enabled: true, URL: "https://example.test/v1", TokenRef: "env://MMBRIDGE_TOKEN"}
	merged, err := managed.Merge(map[managed.Owner]config.Document{managed.OwnerOperator: doc})
	if err != nil {
		t.Fatal(err)
	}
	doc.MMProjection.Enabled = false
	if !merged.MMProjection.Enabled {
		t.Fatal("merged connection aliases authored settings")
	}
	if len(merged.CredentialReferences()) != 1 || len(doc.CredentialReferences()) != 0 {
		t.Fatal("only enabled connections must track credentials")
	}
	if _, err := managed.Merge(map[managed.Owner]config.Document{managed.OwnerSparkrun: doc}); err == nil {
		t.Fatal("generated owner controls MMBridge connection")
	}
}
