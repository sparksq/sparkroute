// SPDX-FileCopyrightText: 2026 Scitrera LLC
// SPDX-FileCopyrightText: 2026 Fox Engine Ltd.
// SPDX-License-Identifier: AGPL-3.0-only

package file

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSourceLoad(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "config.yaml")
	raw := `{
		"providers": [{"name":"p","type":"openai_compatible","base_url":"https://example.com/v1"}],
		"deployments": [{"name":"d","provider":"p","model":"upstream"}],
		"virtual_models": [{
			"name":"public",
			"pools":[{"priority":0,"targets":[{"deployment":"d","weight":1}]}]
		}]
	}`
	if err := os.WriteFile(path, []byte(raw), 0o600); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}

	document, version, err := (Source{Path: path}).Load(context.Background())
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if len(document.VirtualModels) != 1 || document.VirtualModels[0].Name != "public" {
		t.Fatalf("VirtualModels = %#v", document.VirtualModels)
	}
	if len(version) != 64 {
		t.Fatalf("version length = %d, want 64", len(version))
	}
}

func TestSourceLoadRejectsUnknownField(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte(`{"unknown":true}`), 0o600); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}

	_, _, err := (Source{Path: path}).Load(context.Background())
	if err == nil || !strings.Contains(err.Error(), "unknown field") {
		t.Fatalf("Load() error = %v, want unknown-field error", err)
	}
}
