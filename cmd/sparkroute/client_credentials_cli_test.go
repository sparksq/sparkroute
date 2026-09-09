// SPDX-FileCopyrightText: 2026 Scitrera LLC
// SPDX-FileCopyrightText: 2026 Fox Engine Ltd.
// SPDX-License-Identifier: AGPL-3.0-only

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/sparksq/sparkroute/pkg/clientcredentials"
	clientcredentialsqlite "github.com/sparksq/sparkroute/pkg/clientcredentials/sqlite"
)

func TestClientCredentialBootstrapCommandCreatesUsableAdmin(t *testing.T) {
	t.Parallel()
	directory := t.TempDir()
	if err := os.Chmod(directory, 0o700); err != nil {
		t.Fatalf("Chmod() error = %v", err)
	}
	database := filepath.Join(directory, "credentials.sqlite")
	var stdout bytes.Buffer
	if err := runClientCredentialCommand(context.Background(), []string{
		"create",
		"-database", database,
		"-name", "local admin",
		"-principal-id", "local-admin",
		"-roles", "status_read,credentials_read,credentials_write",
	}, &stdout); err != nil {
		t.Fatalf("runClientCredentialCommand() error = %v", err)
	}
	var issued clientcredentials.IssuedCredential
	if err := json.Unmarshal(stdout.Bytes(), &issued); err != nil {
		t.Fatalf("Unmarshal() error = %v", err)
	}
	store, err := clientcredentialsqlite.Open(context.Background(), clientcredentialsqlite.Options{Path: database})
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	defer func() { _ = store.Close() }()
	manager, _ := clientcredentials.NewManager(store)
	principal, err := manager.AuthenticateBearer(context.Background(), issued.APIKey)
	if err != nil {
		t.Fatalf("AuthenticateBearer() error = %v", err)
	}
	if principal.ID != "local-admin" || !clientcredentials.HasRole(principal, clientcredentials.RoleWrite) {
		t.Fatalf("principal = %#v", principal)
	}
}

func TestClientCredentialEnsureAdoptsAndRotatesExistingNamedCredential(t *testing.T) {
	t.Parallel()
	directory := t.TempDir()
	if err := os.Chmod(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	database := filepath.Join(directory, "credentials.sqlite")
	store, err := clientcredentialsqlite.Open(context.Background(), clientcredentialsqlite.Options{Path: database})
	if err != nil {
		t.Fatal(err)
	}
	manager, _ := clientcredentials.NewManager(store)
	existing, err := manager.Create(context.Background(), clientcredentials.CreateInput{
		Name: "sparkrun-operator", PrincipalID: "sparkrun-operator",
		Roles: []string{"status_read", "config_read", "config_write"},
	}, "test")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	var stdout bytes.Buffer
	if err := runClientCredentialCommand(context.Background(), []string{
		"ensure", "-database", database,
		"-name", "sparkrun-operator",
		"-principal-id", "sparkrun-operator",
		"-roles", "status_read,config_read,config_write",
	}, &stdout); err != nil {
		t.Fatal(err)
	}
	var adopted clientcredentials.IssuedCredential
	if err := json.Unmarshal(stdout.Bytes(), &adopted); err != nil {
		t.Fatal(err)
	}
	if adopted.Credential.ID != existing.Credential.ID || adopted.APIKey == existing.APIKey {
		t.Fatalf("adopted = %#v", adopted)
	}

	store, err = clientcredentialsqlite.Open(context.Background(), clientcredentialsqlite.Options{Path: database})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close() }()
	manager, _ = clientcredentials.NewManager(store)
	if _, err := manager.AuthenticateBearer(context.Background(), existing.APIKey); err == nil {
		t.Fatal("old API key remained valid after ensure")
	}
	if _, err := manager.AuthenticateBearer(context.Background(), adopted.APIKey); err != nil {
		t.Fatalf("new API key does not authenticate: %v", err)
	}
}

func TestClientCredentialRotateCommandInvalidatesPreviousSecret(t *testing.T) {
	t.Parallel()
	directory := t.TempDir()
	if err := os.Chmod(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	database := filepath.Join(directory, "credentials.sqlite")
	store, err := clientcredentialsqlite.Open(context.Background(), clientcredentialsqlite.Options{Path: database})
	if err != nil {
		t.Fatal(err)
	}
	manager, _ := clientcredentials.NewManager(store)
	existing, err := manager.Create(context.Background(), clientcredentials.CreateInput{
		Name: "admin", PrincipalID: "admin", Roles: []string{"status_read"},
	}, "test")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	var stdout bytes.Buffer
	if err := runClientCredentialCommand(context.Background(), []string{
		"rotate", "-database", database, "-credential-id", existing.Credential.ID,
	}, &stdout); err != nil {
		t.Fatal(err)
	}
	var rotated clientcredentials.IssuedCredential
	if err := json.Unmarshal(stdout.Bytes(), &rotated); err != nil {
		t.Fatal(err)
	}
	if rotated.Credential.ID != existing.Credential.ID || rotated.APIKey == existing.APIKey {
		t.Fatalf("rotated = %#v", rotated)
	}
}
