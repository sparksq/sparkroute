// SPDX-FileCopyrightText: 2026 Scitrera LLC
// SPDX-FileCopyrightText: 2026 Fox Engine Ltd.
// SPDX-License-Identifier: AGPL-3.0-only

package config

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestDeploymentTitleIsPresentationMetadata(t *testing.T) {
	document := validDocument()
	identity := document.Deployments[0].Name
	document.Deployments[0].Title = "sparkrun:spark-a:org/model"
	if err := document.Validate(); err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(document)
	if err != nil {
		t.Fatal(err)
	}
	var loaded Document
	if err := json.Unmarshal(raw, &loaded); err != nil {
		t.Fatal(err)
	}
	if loaded.Deployments[0].Name != identity || loaded.Deployments[0].Title != document.Deployments[0].Title {
		t.Fatal("title round-trip changed identity or title")
	}
	if err := loaded.Validate(); err != nil {
		t.Fatal(err)
	}
	for _, title := range []string{"unsafe\nlabel", strings.Repeat("x", 4097)} {
		loaded.Deployments[0].Title = title
		if err := loaded.Validate(); err == nil {
			t.Fatal("invalid title was accepted")
		}
	}
}
