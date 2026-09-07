// Package telemetry contains the optional MLflow telemetry conventions.
package telemetry

import (
	"go.opentelemetry.io/otel/attribute"
)

const mlflowSpanType = "mlflow.spanType"

// MLflowConventions keeps the gateway's upstream attempt visible as one LLM
// span in MLflow. The value is intentionally the bare enum string: MLflow's raw
// OTLP/HTTP ingestion stores it verbatim and dashboard filters expect "LLM",
// not the JSON-encoded string "\"LLM\"".
type MLflowConventions struct{}

var _ Conventions = MLflowConventions{}

func (MLflowConventions) Request(RequestStart) []attribute.KeyValue {
	return nil
}

func (MLflowConventions) Attempt(AttemptStart) []attribute.KeyValue {
	return []attribute.KeyValue{attribute.String(mlflowSpanType, "LLM")}
}
