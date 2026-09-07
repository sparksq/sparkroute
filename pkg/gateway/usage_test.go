package gateway

import (
	"testing"

	"github.com/sparksq/sparkroute/pkg/ledger"
)

func TestExtractOpenAIChatUsage(t *testing.T) {
	t.Parallel()

	usage, found := extractOpenAIChatUsage([]byte(`{
		"id":"chatcmpl-1",
		"usage":{
			"prompt_tokens":12,
			"completion_tokens":5,
			"total_tokens":17,
			"prompt_tokens_details":{"cached_tokens":3},
			"completion_tokens_details":{
				"reasoning_tokens":2,
				"accepted_prediction_tokens":1,
				"rejected_prediction_tokens":0
			},
			"future_provider_field":7
		}
	}`))
	if !found {
		t.Fatal("usage not found")
	}
	assertTokenValue(t, "input", usage.InputTokens, 12)
	assertTokenValue(t, "output", usage.OutputTokens, 5)
	assertTokenValue(t, "total", usage.TotalTokens, 17)
	assertTokenValue(t, "cached", usage.CachedInputTokens, 3)
	assertTokenValue(t, "reasoning", usage.ReasoningTokens, 2)
	assertTokenValue(t, "accepted", usage.AcceptedPredictionTokens, 1)
	assertTokenValue(t, "rejected", usage.RejectedPredictionTokens, 0)
	if usage.Completeness != ledger.UsageComplete {
		t.Fatalf("Completeness = %q", usage.Completeness)
	}
	if len(usage.Raw) == 0 {
		t.Fatal("raw usage was not retained")
	}
}

func TestExtractOpenAIChatUsageMissing(t *testing.T) {
	t.Parallel()

	usage, found := extractOpenAIChatUsage([]byte(`{"id":"chatcmpl-1"}`))
	if found {
		t.Fatal("found = true, want false")
	}
	if usage.Completeness != ledger.UsageMissing {
		t.Fatalf("Completeness = %q", usage.Completeness)
	}
}

func assertTokenValue(t *testing.T, name string, value *int64, want int64) {
	t.Helper()
	if value == nil || *value != want {
		t.Fatalf("%s tokens = %v, want %d", name, value, want)
	}
}
