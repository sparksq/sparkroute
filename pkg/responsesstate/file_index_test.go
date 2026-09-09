// SPDX-FileCopyrightText: 2026 Scitrera LLC
// SPDX-FileCopyrightText: 2026 Fox Engine Ltd.
// SPDX-License-Identifier: AGPL-3.0-only

package responsesstate_test

import (
	"testing"

	"github.com/sparksq/sparkroute/pkg/responsesstate"
	"github.com/sparksq/sparkroute/pkg/responsesstate/responsesstatetest"
)

func TestMemoryFileStoreConformance(t *testing.T) {
	t.Parallel()
	responsesstatetest.RunFileStoreConformance(
		t,
		responsesstate.NewMemoryStore(responsesstate.MemoryOptions{}),
	)
}
