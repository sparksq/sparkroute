package pii

import (
	"context"
	"strings"
	"testing"

	"github.com/sparksq/sparkroute/pkg/config"
	privacycontract "github.com/sparksq/sparkroute/pkg/privacy"
)

func TestProviderAdaptsBuiltinEngineToPrivacyContract(t *testing.T) {
	t.Parallel()
	document := providerTestDocument(config.PIIScopeRequest)
	provider, err := NewProvider(document, NewBuiltinDetector(), nil)
	if err != nil {
		t.Fatalf("NewProvider() error = %v", err)
	}
	session, err := provider.NewSession(
		context.Background(),
		[]privacycontract.Entity{privacycontract.EntityEmail},
		privacycontract.Scope{},
	)
	if err != nil {
		t.Fatalf("NewSession() error = %v", err)
	}
	masked, err := session.Substitute(context.Background(), "drew@example.com")
	if err != nil || strings.Contains(masked, "drew@example.com") {
		t.Fatalf("Substitute() = %q, %v", masked, err)
	}
	if restored := session.Restore(masked); restored != "drew@example.com" {
		t.Fatalf("Restore() = %q", restored)
	}
	if status := provider.PrivacyStatus(context.Background()); !status.Enabled || status.ConfiguredModels != 1 {
		t.Fatalf("PrivacyStatus() = %#v", status)
	}
}

func TestDormantProviderCanEnableRequestPolicyButNotConversationWithoutStore(t *testing.T) {
	t.Parallel()
	provider, err := NewProvider(config.Document{}, NewBuiltinDetector(), nil)
	if err != nil {
		t.Fatalf("NewProvider() error = %v", err)
	}
	if _, err := provider.ForDocument(providerTestDocument(config.PIIScopeRequest)); err != nil {
		t.Fatalf("ForDocument(request) error = %v", err)
	}
	if _, err := provider.ForDocument(providerTestDocument(config.PIIScopeConversation)); err == nil || !strings.Contains(err.Error(), "encrypted mapping directory") {
		t.Fatalf("ForDocument(conversation) error = %v", err)
	}
}

func providerTestDocument(scope config.PIIScope) config.Document {
	return config.Document{VirtualModels: []config.VirtualModel{{
		Name: "private",
		Privacy: &config.PrivacyPolicy{PII: &config.PIIPolicy{
			Entities: []config.PIIEntity{config.PIIEntityEmail}, Scope: scope,
		}},
	}}}
}
