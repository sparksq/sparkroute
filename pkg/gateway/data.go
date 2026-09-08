package gateway

import (
	"fmt"
	"net/http"
	"sort"
	"strings"

	"github.com/sparksq/sparkroute/pkg/config"
	"github.com/sparksq/sparkroute/pkg/credentials"
	"github.com/sparksq/sparkroute/pkg/ledger"
	"github.com/sparksq/sparkroute/pkg/lifecycle"
	"github.com/sparksq/sparkroute/pkg/mmprojection"
	"github.com/sparksq/sparkroute/pkg/modelcatalog"
	"github.com/sparksq/sparkroute/pkg/modelrouter"
	"github.com/sparksq/sparkroute/pkg/privacy"
	"github.com/sparksq/sparkroute/pkg/promptcache"
	"github.com/sparksq/sparkroute/pkg/providerauth"
	"github.com/sparksq/sparkroute/pkg/responsesstate"
	"github.com/sparksq/sparkroute/pkg/routing"
	"github.com/sparksq/sparkroute/pkg/savedtrace"
	"github.com/sparksq/sparkroute/pkg/telemetry"
)

type ModelListOptions struct {
	Owner          string
	IncludeAliases bool
}

type DataOptions struct {
	Models              ModelListOptions
	Credentials         credentials.Source
	HTTPClient          *http.Client
	ProviderAuth        *providerauth.Service
	RoutingPicker       routing.WeightedPicker
	Ledger              ledger.Recorder
	ConfigRevision      string
	MaxRequestBytes     int64
	MaxResponseBytes    int64
	RequestIDHeader     string
	AttemptCountHeader  string
	Telemetry           telemetry.Options
	OTLPTraceHandler    http.Handler
	TargetManager       *routing.TargetManager
	Lifecycle           *lifecycle.AdmissionCoordinator
	RetryBudget         routing.RetryBudget
	RetryDelayPicker    routing.WeightedPicker
	RetrySleeper        RetrySleeper
	ResponsesState      responsesstate.Store
	SavedTraces         savedtrace.Recorder
	MaxSavedTraceBytes  int64
	PromptCache         *promptcache.Directory
	PromptFingerprinter *promptcache.Fingerprinter
	ModelCatalog        *modelcatalog.Directory
	// ModelRouter optionally selects one configured logical virtual model before
	// ordinary capability filtering and provider/deployment plan construction.
	ModelRouter modelrouter.Router
	// MMProjection is the optional authenticated request transformer used only
	// when the selected model's routing policy enables multimedia projection.
	MMProjection *mmprojection.Client
	// Privacy is an optional implementation of the privacy
	// extension contract. The standalone command supplies pkg/pii.
	Privacy privacy.Provider
	// ModelCatalogCredentials resolves only references accepted by the catalog
	// policy. When nil, catalog planes use Credentials for compatibility.
	ModelCatalogCredentials credentials.Source
}

// DataPlane owns the HTTP handler and the live replica-local target state used
// by that handler. Targets may be observed by separate admin/operations
// listeners, but all admission decisions remain inside the data plane.
type DataPlane struct {
	Handler   http.Handler
	Targets   *routing.TargetManager
	Lifecycle *lifecycle.AdmissionCoordinator
	Privacy   privacy.StatusSource
}

type combinedEligibility struct {
	targets   routing.Eligibility
	lifecycle *lifecycle.AdmissionCoordinator
}

func (e combinedEligibility) Eligible(deployment string) bool {
	return e.targets.Eligible(deployment) &&
		(!e.lifecycle.IsDynamic(deployment) || e.lifecycle.Eligible(deployment))
}

// NewDataPlane constructs the multi-protocol data plane and exposes its
// content-free runtime target state.
func NewDataPlane(document config.Document, options DataOptions) (*DataPlane, error) {
	for _, provider := range document.Providers {
		if provider.Type == "openai_subscription" && options.ProviderAuth == nil {
			return nil, fmt.Errorf("subscription provider requires a private provider credential store")
		}
	}
	if err := ValidatePrivacyProvider(document, options.Privacy); err != nil {
		return nil, err
	}
	if options.ModelRouter == nil && document.ModelRouting != nil {
		manager, err := modelrouter.NewRouterManager(*document.ModelRouting)
		if err != nil {
			return nil, fmt.Errorf("construct model router: %w", err)
		}
		options.ModelRouter = manager
	}
	snapshot, err := routing.Compile(document)
	if err != nil {
		return nil, err
	}
	if publisher, ok := options.ModelRouter.(interface {
		ReplaceDeploymentMetadata(map[string]modelrouter.DiscoveredModelMetadata) error
	}); ok {
		if err := publisher.ReplaceDeploymentMetadata(document.DeploymentMetadata()); err != nil {
			return nil, fmt.Errorf("deployment metadata: %w", err)
		}
	}
	instrumentation, err := telemetry.New(options.Telemetry)
	if err != nil {
		return nil, err
	}
	targetManager := options.TargetManager
	if targetManager == nil {
		targetManager, err = routing.NewTargetManager(
			document.Deployments,
			routing.TargetManagerOptions{Observer: instrumentation},
		)
		if err != nil {
			return nil, err
		}
	}
	for _, deployment := range document.Deployments {
		if deployment.EndpointSource.Type.Effective() != config.EndpointSourceStatic &&
			options.Lifecycle == nil {
			return nil, fmt.Errorf(
				"deployment %q uses a dynamic endpoint source but no lifecycle coordinator is configured",
				deployment.Name,
			)
		}
		if deployment.EndpointSource.Type.Effective() != config.EndpointSourceStatic &&
			options.Lifecycle != nil && !options.Lifecycle.Eligible(deployment.Name) {
			return nil, fmt.Errorf(
				"deployment %q is missing from the lifecycle coordinator",
				deployment.Name,
			)
		}
	}
	retryBudget := options.RetryBudget
	if retryBudget == nil {
		retryBudget = routing.NewRetryBudgetManager(instrumentation)
	}
	if options.ResponsesState == nil {
		options.ResponsesState = responsesstate.NewMemoryStore(
			responsesstate.MemoryOptions{},
		)
	}
	owner := options.Models.Owner
	if owner == "" {
		owner = "sparkroute"
	}

	models := make([]modelObject, 0, len(document.VirtualModels))
	publishedModels := make(map[string]struct{}, len(document.VirtualModels))
	for _, model := range document.VirtualModels {
		if model.Visibility.Effective() != config.ModelVisibilityPublic {
			continue
		}
		models = append(models, modelObject{
			ID:      model.Name,
			Object:  "model",
			OwnedBy: owner,
		})
		publishedModels[model.Name] = struct{}{}
		if options.Models.IncludeAliases {
			for _, alias := range model.Aliases {
				models = append(models, modelObject{
					ID:      alias,
					Object:  "model",
					OwnedBy: owner,
				})
				publishedModels[alias] = struct{}{}
			}
		}
	}
	if publisher, ok := options.ModelRouter.(modelrouter.ModelPublisher); ok {
		for _, name := range publisher.PublicModels() {
			if _, exists := publishedModels[name]; exists {
				continue
			}
			models = append(models, modelObject{ID: name, Object: "model", OwnedBy: owner})
			publishedModels[name] = struct{}{}
		}
	}
	sort.Slice(models, func(i, j int) bool {
		return models[i].ID < models[j].ID
	})

	chat := newChatCompletionsHandler(
		snapshot,
		targetManager,
		retryBudget,
		options,
		instrumentation,
	)
	responses := newResponsesHandler(
		snapshot,
		targetManager,
		retryBudget,
		options,
		instrumentation,
	)
	conversations := newConversationsHandler(
		snapshot,
		targetManager,
		retryBudget,
		options,
		instrumentation,
	)
	embeddings := newEmbeddingsHandler(
		snapshot,
		targetManager,
		retryBudget,
		options,
		instrumentation,
	)
	files := newFilesHandler(
		snapshot,
		targetManager,
		retryBudget,
		options,
		instrumentation,
	)
	anthropicMessages := newAnthropicMessagesHandler(
		snapshot,
		targetManager,
		retryBudget,
		options,
		instrumentation,
	)
	anthropicCountTokens := newAnthropicCountTokensHandler(
		snapshot,
		targetManager,
		retryBudget,
		options,
		instrumentation,
	)
	gemini := newGeminiHandler(
		snapshot,
		targetManager,
		retryBudget,
		options,
		instrumentation,
	)
	bedrock := newBedrockHandler(
		snapshot,
		targetManager,
		retryBudget,
		options,
		instrumentation,
	)
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/models", func(w http.ResponseWriter, request *http.Request) {
		if request.Method != http.MethodGet {
			w.Header().Set("Allow", http.MethodGet)
			writeOpenAIError(w, http.StatusMethodNotAllowed, "method_not_allowed", "method not allowed")
			return
		}
		writeJSON(w, http.StatusOK, modelList{
			Object: "list",
			Data:   models,
		})
	})
	mux.HandleFunc("/v1/models/", func(w http.ResponseWriter, request *http.Request) {
		if request.Method != http.MethodGet {
			w.Header().Set("Allow", http.MethodGet)
			writeOpenAIError(w, http.StatusMethodNotAllowed, "method_not_allowed", "method not allowed")
			return
		}
		requested := strings.TrimPrefix(request.URL.Path, "/v1/models/")
		model, exists := document.CanonicalModel(requested)
		_, publishedByRouter := publishedModels[requested]
		if requested == "" ||
			(!exists && !publishedByRouter) ||
			model.Visibility.Effective() == config.ModelVisibilityInternal {
			writeOpenAIError(w, http.StatusNotFound, "model_not_found", "requested model was not found")
			return
		}
		writeJSON(w, http.StatusOK, modelObject{
			ID:      requested,
			Object:  "model",
			OwnedBy: owner,
		})
	})
	mux.Handle("/v1/chat/completions", chat)
	mux.Handle("/v1/responses", responses)
	mux.Handle("/v1/responses/", responses)
	mux.Handle("/v1/conversations", conversations)
	mux.Handle("/v1/conversations/", conversations)
	mux.Handle("/v1/embeddings", embeddings)
	mux.Handle("/v1/files", files)
	mux.Handle("/v1/files/", files)
	mux.Handle("/v1/messages", anthropicMessages)
	mux.Handle("/v1/messages/count_tokens", anthropicCountTokens)
	mux.Handle(geminiModelsPathPrefix, gemini)
	mux.Handle(bedrockModelsPathPrefix, bedrock)
	if options.OTLPTraceHandler != nil {
		mux.Handle("/v1/traces", options.OTLPTraceHandler)
	}
	mux.HandleFunc("/", func(w http.ResponseWriter, _ *http.Request) {
		writeOpenAIError(w, http.StatusNotFound, "route_not_found", "route not found")
	})
	handler := http.Handler(mux)
	if options.ModelCatalog != nil {
		handler = newModelCatalogHandler(document, handler, options)
	}
	return &DataPlane{
		Handler: handler, Targets: targetManager, Lifecycle: options.Lifecycle,
		Privacy: options.Privacy,
	}, nil
}

// NewDataHandler constructs the multi-protocol data plane. Callers that
// also expose runtime target state should use NewDataPlane.
func NewDataHandler(document config.Document, options DataOptions) (http.Handler, error) {
	plane, err := NewDataPlane(document, options)
	if err != nil {
		return nil, err
	}
	return plane.Handler, nil
}

type modelList struct {
	Object string        `json:"object"`
	Data   []modelObject `json:"data"`
}

type modelObject struct {
	ID      string `json:"id"`
	Object  string `json:"object"`
	Created int64  `json:"created"`
	OwnedBy string `json:"owned_by"`
}

type openAIErrorEnvelope struct {
	Error openAIError `json:"error"`
}

type openAIError struct {
	Message string `json:"message"`
	Type    string `json:"type"`
	Code    string `json:"code"`
}

func writeOpenAIError(w http.ResponseWriter, status int, code, message string) {
	errorType := "api_error"
	if status >= 400 && status < 500 {
		errorType = "invalid_request_error"
	}
	writeJSON(w, status, openAIErrorEnvelope{
		Error: openAIError{
			Message: message,
			Type:    errorType,
			Code:    code,
		},
	})
}
