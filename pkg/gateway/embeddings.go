package gateway

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"github.com/sparksq/sparkroute/pkg/config"
	"github.com/sparksq/sparkroute/pkg/routing"
	"github.com/sparksq/sparkroute/pkg/telemetry"
)

func newEmbeddingsHandler(
	snapshot *routing.Snapshot,
	targets *routing.TargetManager,
	retryBudget routing.RetryBudget,
	options DataOptions,
	instrumentation *telemetry.Instrumentation,
) http.Handler {
	return newOpenAIHandler(
		openAIOperationEmbeddings,
		snapshot,
		targets,
		retryBudget,
		options,
		instrumentation,
	)
}

func decodeEmbeddingsRequest(
	raw []byte,
) (map[string]json.RawMessage, string, bool, error) {
	envelope, model, streaming, err := decodeChatRequest(raw)
	if err != nil {
		return nil, "", false, err
	}
	if streaming {
		return nil, "", false, unsupportedFeature(
			"streaming is not supported by the Embeddings API",
		)
	}
	if !rawNonNull(envelope["input"]) {
		return nil, "", false, fmt.Errorf("input is required")
	}
	return envelope, model, false, nil
}

func detectEmbeddingsCapabilities(
	_ map[string]json.RawMessage,
	_ bool,
) ([]config.Capability, error) {
	return []config.Capability{config.CapabilitySingleVectorEmbedding}, nil
}

func embeddingsURL(baseURL string) (string, error) {
	parsed, err := url.Parse(baseURL)
	if err != nil {
		return "", err
	}
	parsed.Path = strings.TrimRight(parsed.Path, "/") + "/embeddings"
	parsed.RawPath = ""
	parsed.RawQuery = ""
	parsed.Fragment = ""
	return parsed.String(), nil
}
