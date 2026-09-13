// SPDX-FileCopyrightText: 2026 Scitrera LLC
// SPDX-FileCopyrightText: 2026 Fox Engine Ltd.
// SPDX-License-Identifier: AGPL-3.0-only

package postgres

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/sparksq/sparkroute/pkg/clientcredentials"
	"github.com/sparksq/sparkroute/pkg/identity"
)

func TestMigrationContainsTenantIndexesDigestAndAudit(t *testing.T) {
	t.Parallel()
	content, err := migrationFiles.ReadFile("migrations/001_client_credentials.sql")
	if err != nil {
		t.Fatalf("ReadFile() error = %v", err)
	}
	sql := string(content)
	for _, required := range []string{
		"tenant_id TEXT NOT NULL",
		"secret_sha256 BYTEA NOT NULL",
		"octet_length(secret_sha256) = 32",
		"gateway_client_credentials_tenant_created_idx",
		"gateway_client_credential_audit",
		"credential_id TEXT NOT NULL REFERENCES",
	} {
		if !strings.Contains(sql, required) {
			t.Fatalf("migration does not contain %q", required)
		}
	}
}

func TestPostgresCredentialLifecycleIsImmediatelyVisibleAcrossStores(t *testing.T) {
	url := os.Getenv("SPARKROUTE_TEST_POSTGRES_URL")
	if url == "" {
		t.Skip("SPARKROUTE_TEST_POSTGRES_URL is not set")
	}
	ctx := context.Background()
	first, err := Open(ctx, Options{
		URL: url, AutoMigrate: true, ExpectedPostgresMajor: 17, MaxConnections: 2,
	})
	if err != nil {
		t.Fatalf("Open(first) error = %v", err)
	}
	defer func() {
		if err := first.Close(); err != nil {
			t.Errorf("Close(first) error = %v", err)
		}
	}()
	second, err := Open(ctx, Options{
		URL: url, ExpectedPostgresMajor: 17, MaxConnections: 2,
	})
	if err != nil {
		t.Fatalf("Open(second) error = %v", err)
	}
	defer func() {
		if err := second.Close(); err != nil {
			t.Errorf("Close(second) error = %v", err)
		}
	}()
	firstManager, _ := clientcredentials.NewManager(first)
	secondManager, _ := clientcredentials.NewManager(second)
	issued, err := firstManager.Create(ctx, clientcredentials.CreateInput{
		Name: "replica integration", TenantID: "tenant-integration",
		PrincipalID: "worker-integration", Roles: []string{clientcredentials.RoleInference},
	}, "integration-operator")
	if err != nil {
		t.Fatalf("Create() error = %v", err)
	}
	principal, err := secondManager.AuthenticateBearer(ctx, issued.APIKey)
	if err != nil || principal.ID != "worker-integration" || principal.Tenant != "tenant-integration" {
		t.Fatalf("second AuthenticateBearer() = %#v, %v", principal, err)
	}
	if _, err := firstManager.SetState(ctx, issued.Credential.ID, clientcredentials.StateDisabled, "integration-operator"); err != nil {
		t.Fatalf("SetState() error = %v", err)
	}
	if _, err := secondManager.AuthenticateBearer(ctx, issued.APIKey); !errors.Is(err, identity.ErrInvalidCredentials) {
		t.Fatalf("disabled second AuthenticateBearer() error = %v", err)
	}
	audit, err := secondManager.ListAudit(ctx, clientcredentials.AuditQuery{
		CredentialID: issued.Credential.ID, Limit: 10,
	})
	if err != nil || len(audit.Events) != 2 || audit.Events[0].Action != "disabled" {
		t.Fatalf("ListAudit() = %#v, %v", audit, err)
	}
}
