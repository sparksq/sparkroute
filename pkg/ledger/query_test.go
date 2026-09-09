// SPDX-FileCopyrightText: 2026 Scitrera LLC
// SPDX-FileCopyrightText: 2026 Fox Engine Ltd.
// SPDX-License-Identifier: AGPL-3.0-only

package ledger

import (
	"strings"
	"testing"
	"time"
)

func TestValidateAggregateQueryRequiresBoundedWindow(t *testing.T) {
	t.Parallel()

	end := time.Now().UTC()
	valid, err := ValidateAggregateQuery(AggregateQuery{
		StartedAtOrAfter: end.Add(-24 * time.Hour),
		StartedAtBefore:  end,
		TenantID:         "tenant-a",
		Provider:         "provider-a",
	})
	if err != nil {
		t.Fatalf("ValidateAggregateQuery() error = %v", err)
	}
	if valid.StartedAtBefore.Location() != time.UTC {
		t.Fatalf("validated location = %v, want UTC", valid.StartedAtBefore.Location())
	}

	tests := []struct {
		name  string
		query AggregateQuery
		want  string
	}{
		{name: "missing bounds", query: AggregateQuery{}, want: "both time bounds"},
		{
			name: "reversed",
			query: AggregateQuery{
				StartedAtOrAfter: end,
				StartedAtBefore:  end.Add(-time.Hour),
			},
			want: "before",
		},
		{
			name: "too wide",
			query: AggregateQuery{
				StartedAtOrAfter: end.Add(-MaxAggregateRange - time.Nanosecond),
				StartedAtBefore:  end,
			},
			want: "exceeds",
		},
		{
			name: "invalid outcome",
			query: AggregateQuery{
				StartedAtOrAfter: end.Add(-time.Hour),
				StartedAtBefore:  end,
				Outcome:          "unknown",
			},
			want: "outcome",
		},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			_, err := ValidateAggregateQuery(test.query)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("ValidateAggregateQuery() error = %v, want %q", err, test.want)
			}
		})
	}
}
