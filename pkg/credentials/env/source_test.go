// SPDX-FileCopyrightText: 2026 Scitrera LLC
// SPDX-FileCopyrightText: 2026 Fox Engine Ltd.
// SPDX-License-Identifier: AGPL-3.0-only

package env

import (
	"context"
	"strings"
	"testing"

	"github.com/sparksq/sparkroute/pkg/credentials"
)

func TestSourceResolve(t *testing.T) {
	t.Setenv("SPARKROUTE_TEST_SECRET", "secret")

	material, err := (Source{}).Resolve(
		context.Background(),
		credentials.Ref("env://SPARKROUTE_TEST_SECRET"),
	)
	if err != nil {
		t.Fatalf("Resolve() error = %v", err)
	}
	if string(material.Value) != "secret" {
		t.Fatalf("Value = %q, want secret", material.Value)
	}
}

func TestSourceRejectsInvalidName(t *testing.T) {
	t.Parallel()

	_, err := (Source{}).Resolve(context.Background(), credentials.Ref("env://BAD/NAME"))
	if err == nil || !strings.Contains(err.Error(), "invalid environment variable") {
		t.Fatalf("Resolve() error = %v, want invalid-name error", err)
	}
}
