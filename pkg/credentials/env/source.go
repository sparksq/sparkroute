// SPDX-FileCopyrightText: 2026 Scitrera LLC
// SPDX-FileCopyrightText: 2026 Fox Engine Ltd.
// SPDX-License-Identifier: AGPL-3.0-only

// Package env resolves environment-variable credential references.
package env

import (
	"context"
	"fmt"
	"os"
	"strings"
	"unicode"

	"github.com/sparksq/sparkroute/pkg/credentials"
)

type Source struct{}

func (Source) Resolve(ctx context.Context, ref credentials.Ref) (credentials.Material, error) {
	if err := ctx.Err(); err != nil {
		return credentials.Material{}, err
	}
	raw := string(ref)
	if !strings.HasPrefix(strings.ToLower(raw), "env://") {
		return credentials.Material{}, fmt.Errorf("unsupported environment credential reference")
	}
	name := raw[len("env://"):]
	if err := validateName(name); err != nil {
		return credentials.Material{}, err
	}
	value, exists := os.LookupEnv(name)
	if !exists || value == "" {
		return credentials.Material{}, fmt.Errorf("environment variable %q is not set", name)
	}
	return credentials.Material{Value: []byte(value)}, nil
}

func validateName(name string) error {
	if name == "" {
		return fmt.Errorf("environment variable name is required")
	}
	for index, ch := range name {
		if ch == '_' || unicode.IsLetter(ch) || index > 0 && unicode.IsDigit(ch) {
			continue
		}
		return fmt.Errorf("invalid environment variable name %q", name)
	}
	return nil
}

var _ credentials.Source = Source{}
