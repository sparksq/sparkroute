package telemetry

import (
	"testing"
)

func TestMLflowAttemptTypeIsBareLLM(t *testing.T) {
	attrs := (MLflowConventions{}).Attempt(AttemptStart{})
	if len(attrs) != 1 {
		t.Fatalf("attribute count = %d, want 1", len(attrs))
	}
	if got := string(attrs[0].Key); got != "mlflow.spanType" {
		t.Fatalf("attribute key = %q", got)
	}
	if got := attrs[0].Value.AsString(); got != "LLM" {
		t.Fatalf("attribute value = %q, want bare LLM", got)
	}
}
