package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sparksq/sparkroute/pkg/config"
	"github.com/sparksq/sparkroute/pkg/privacy"
)

func piiDocument(scope config.PIIScope) config.Document {
	return config.Document{VirtualModels: []config.VirtualModel{{Name: "private", Privacy: &config.PrivacyPolicy{PII: &config.PIIPolicy{Scope: scope, Entities: []config.PIIEntity{config.PIIEntityEmail}}}}}}
}

func TestPrivacyRuntimeRequestIsDiskFreeAndConversationSurvivesRestart(t *testing.T) {
	directory := filepath.Join(t.TempDir(), "private")
	path := filepath.Join(directory, "mappings.sqlite")
	runtime := &privacyRuntime{ctx: context.Background(), path: path}
	provider, err := runtime.ForDocument(piiDocument(config.PIIScopeRequest))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(directory); !os.IsNotExist(err) {
		t.Fatalf("request policy created storage: %v", err)
	}
	session, err := provider.NewSession(t.Context(), []privacy.Entity{privacy.EntityEmail}, privacy.Scope{})
	if err != nil {
		t.Fatal(err)
	}
	masked, err := session.Substitute(t.Context(), "test@example.com")
	if err != nil || strings.Contains(masked, "test@example.com") || session.Restore(masked) != "test@example.com" {
		t.Fatalf("round trip: %q %v", masked, err)
	}
	provider, err = runtime.ForDocument(piiDocument(config.PIIScopeConversation))
	if err != nil {
		t.Fatal(err)
	}
	scope := privacy.Scope{Tenant: "standalone", Principal: "alice", Conversation: "thread"}
	session, err = provider.NewSession(t.Context(), []privacy.Entity{privacy.EntityEmail}, scope)
	if err != nil {
		t.Fatal(err)
	}
	masked, err = session.Substitute(t.Context(), "test@example.com")
	if err != nil {
		t.Fatal(err)
	}
	if err = runtime.Close(); err != nil {
		t.Fatal(err)
	}
	restarted := &privacyRuntime{ctx: context.Background(), path: path}
	defer restarted.Close()
	provider, err = restarted.ForDocument(piiDocument(config.PIIScopeConversation))
	if err != nil {
		t.Fatal(err)
	}
	session, err = provider.NewSession(t.Context(), []privacy.Entity{privacy.EntityEmail}, scope)
	if err != nil {
		t.Fatal(err)
	}
	if session.Restore(masked) != "test@example.com" {
		t.Fatal("restart lost mapping")
	}
	scope.Principal = "bob"
	other, err := provider.NewSession(t.Context(), []privacy.Entity{privacy.EntityEmail}, scope)
	if err != nil {
		t.Fatal(err)
	}
	if other.Restore(masked) != masked {
		t.Fatal("mapping crossed principal boundary")
	}
}

func TestPrivacyRuntimeNeverReplacesMissingKeyForExistingDatabase(t *testing.T) {
	root := t.TempDir()
	_ = os.Chmod(root, 0700)
	database := filepath.Join(root, "mappings.sqlite")
	if err := os.WriteFile(database, []byte("existing database"), 0600); err != nil {
		t.Fatal(err)
	}
	key := filepath.Join(root, "keyring.json")
	if _, err := loadPIIKeyring(key, database, true); err == nil {
		t.Fatal("replaced a lost key")
	}
	if _, err := os.Stat(key); !os.IsNotExist(err) {
		t.Fatal("created replacement key")
	}
	if _, err := loadPIIKeyring(key, filepath.Join(root, "fresh.sqlite"), false); err == nil {
		t.Fatal("created explicit operator key")
	}
}

func TestPrivacyRuntimeEnablesConversationStorageForAssignedProfile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "private", "mappings.sqlite")
	runtime := &privacyRuntime{ctx: context.Background(), path: path}
	defer runtime.Close()
	ref := "conversation"
	document := config.Document{PIIProfiles: map[string]config.PIIPolicy{ref: {Scope: config.PIIScopeConversation}}, ModelPolicies: map[string]config.ModelPolicyAssignment{"generated": {PIIProfile: &ref}}}
	if _, err := runtime.ForDocument(document); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("dormant profile created storage")
	}
	document.VirtualModels = []config.VirtualModel{{Name: "generated"}}
	if _, err := runtime.ForDocument(document); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("active profile did not create conversation storage: %v", err)
	}
}
