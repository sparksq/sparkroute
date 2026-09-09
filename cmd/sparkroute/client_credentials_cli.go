// SPDX-FileCopyrightText: 2026 Scitrera LLC
// SPDX-FileCopyrightText: 2026 Fox Engine Ltd.
// SPDX-License-Identifier: AGPL-3.0-only

package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/sparksq/sparkroute/pkg/clientcredentials"
	clientcredentialsqlite "github.com/sparksq/sparkroute/pkg/clientcredentials/sqlite"
)

func runClientCredentialCommand(
	ctx context.Context,
	args []string,
	stdout io.Writer,
) error {
	if len(args) == 0 {
		return fmt.Errorf("client-credentials command must be create, ensure, or rotate")
	}
	switch args[0] {
	case "create":
		return runClientCredentialCreate(ctx, args[1:], stdout, false)
	case "ensure":
		return runClientCredentialCreate(ctx, args[1:], stdout, true)
	case "rotate":
		return runClientCredentialRotate(ctx, args[1:], stdout)
	default:
		return fmt.Errorf("client-credentials command must be create, ensure, or rotate")
	}
}

func runClientCredentialCreate(
	ctx context.Context,
	args []string,
	stdout io.Writer,
	ensure bool,
) error {
	verb := "create"
	if ensure {
		verb = "ensure"
	}
	flags := flag.NewFlagSet("sparkroute client-credentials "+verb, flag.ContinueOnError)
	flags.SetOutput(stdout)
	database := flags.String("database", "", "private SQLite client-credential path")
	name := flags.String("name", "", "operator-facing credential name")
	principalID := flags.String("principal-id", "", "principal ID bound to the credential")
	principalType := flags.String("principal-type", "machine", "principal type")
	principalSubject := flags.String("principal-subject", "", "optional principal subject")
	roles := flags.String("roles", "", "comma-separated principal roles")
	expiresAt := flags.String("expires-at", "", "optional RFC3339 expiration time")
	if err := flags.Parse(args); err != nil {
		return fmt.Errorf("parse client credential flags: %w", err)
	}
	if flags.NArg() != 0 {
		return fmt.Errorf("client-credentials %s does not accept positional arguments", verb)
	}
	if *database == "" {
		return fmt.Errorf("-database is required")
	}
	var expiration *time.Time
	if *expiresAt != "" {
		parsed, err := time.Parse(time.RFC3339, *expiresAt)
		if err != nil {
			return fmt.Errorf("parse -expires-at: %w", err)
		}
		parsed = parsed.UTC()
		expiration = &parsed
	}
	store, err := clientcredentialsqlite.Open(ctx, clientcredentialsqlite.Options{Path: *database})
	if err != nil {
		return err
	}
	defer func() { _ = store.Close() }()
	manager, err := clientcredentials.NewManager(store)
	if err != nil {
		return err
	}
	input := clientcredentials.CreateInput{
		Name:             *name,
		PrincipalID:      *principalID,
		PrincipalType:    *principalType,
		PrincipalSubject: *principalSubject,
		Roles:            splitCredentialRoles(*roles),
		ExpiresAt:        expiration,
	}
	var issued clientcredentials.IssuedCredential
	if ensure {
		page, listErr := manager.List(ctx, clientcredentials.ListQuery{
			PrincipalID: strings.TrimSpace(*principalID),
			Limit:       500,
		})
		if listErr != nil {
			return listErr
		}
		matches := make([]clientcredentials.Credential, 0, 1)
		for _, credential := range page.Credentials {
			if credential.Name == strings.TrimSpace(*name) &&
				credential.State == clientcredentials.StateActive {
				matches = append(matches, credential)
			}
		}
		switch len(matches) {
		case 0:
			issued, err = manager.Create(ctx, input, "local-bootstrap-cli")
		case 1:
			issued, err = manager.Rotate(ctx, matches[0].ID, "local-bootstrap-cli")
		default:
			return fmt.Errorf(
				"client-credentials ensure found %d active credentials named %q for principal %q",
				len(matches), strings.TrimSpace(*name), strings.TrimSpace(*principalID),
			)
		}
	} else {
		issued, err = manager.Create(ctx, input, "local-bootstrap-cli")
	}
	if err != nil {
		return err
	}
	return writeIssuedCredential(stdout, issued)
}

func runClientCredentialRotate(
	ctx context.Context,
	args []string,
	stdout io.Writer,
) error {
	flags := flag.NewFlagSet("sparkroute client-credentials rotate", flag.ContinueOnError)
	flags.SetOutput(stdout)
	database := flags.String("database", "", "private SQLite client-credential path")
	credentialID := flags.String("credential-id", "", "credential ID to rotate")
	if err := flags.Parse(args); err != nil {
		return fmt.Errorf("parse client credential flags: %w", err)
	}
	if flags.NArg() != 0 {
		return fmt.Errorf("client-credentials rotate does not accept positional arguments")
	}
	if *database == "" {
		return fmt.Errorf("-database is required")
	}
	if strings.TrimSpace(*credentialID) == "" {
		return fmt.Errorf("-credential-id is required")
	}
	store, err := clientcredentialsqlite.Open(ctx, clientcredentialsqlite.Options{Path: *database})
	if err != nil {
		return err
	}
	defer func() { _ = store.Close() }()
	manager, err := clientcredentials.NewManager(store)
	if err != nil {
		return err
	}
	issued, err := manager.Rotate(ctx, strings.TrimSpace(*credentialID), "local-bootstrap-cli")
	if err != nil {
		return err
	}
	return writeIssuedCredential(stdout, issued)
}

func writeIssuedCredential(stdout io.Writer, issued clientcredentials.IssuedCredential) error {
	encoder := json.NewEncoder(stdout)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(issued); err != nil {
		return fmt.Errorf("write issued client credential: %w", err)
	}
	return nil
}

func splitCredentialRoles(value string) []string {
	result := make([]string, 0)
	for _, role := range strings.Split(value, ",") {
		if role = strings.TrimSpace(role); role != "" {
			result = append(result, role)
		}
	}
	return result
}
