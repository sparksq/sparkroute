// Package admin provides the standalone administration surface.
package admin

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/sparksq/sparkroute/pkg/adminapi"
	"github.com/sparksq/sparkroute/pkg/adminui"
	"github.com/sparksq/sparkroute/pkg/clientcredentials"
	"github.com/sparksq/sparkroute/pkg/config"
	"github.com/sparksq/sparkroute/pkg/config/managed"
	"github.com/sparksq/sparkroute/pkg/credentials"
	"github.com/sparksq/sparkroute/pkg/endpointregistry"
	"github.com/sparksq/sparkroute/pkg/identity"
	"github.com/sparksq/sparkroute/pkg/lifecycle"
	"github.com/sparksq/sparkroute/pkg/mmprojection"
	"github.com/sparksq/sparkroute/pkg/modelrouter"
	"github.com/sparksq/sparkroute/pkg/privacy"
	"github.com/sparksq/sparkroute/pkg/providerauth"
	"github.com/sparksq/sparkroute/pkg/routing"
	"github.com/sparksq/sparkroute/pkg/savedtrace"
	"github.com/sparksq/sparkroute/pkg/sparkrun"
)

const (
	RoleStatusRead              = "status_read"
	RoleConfigRead              = "config_read"
	RoleConfigWrite             = "config_write"
	RoleConfigReconcileSparkrun = "config_reconcile:sparkrun"
)

type ReadyEndpoints interface {
	Ready(context.Context, string) ([]endpointregistry.Endpoint, error)
}

type Options struct {
	TraceReader     savedtrace.Reader
	SparkrunControl interface {
		WorkloadAction(context.Context, string, string, string) (sparkrun.WorkloadInfo, error)
	}
	Lifecycle         lifecycle.StatusSource
	SparkrunCatalog   sparkrun.Catalog
	Endpoints         ReadyEndpoints
	Targets           routing.TargetStatusSource
	Credentials       credentials.StatusSource
	GatewayVersion    string
	BuildInfo         map[string]string
	ValidateConfig    func(config.Document) error
	Authenticator     identity.Authenticator
	ClientCredentials *clientcredentials.Manager
	ManagedConfig     managed.Store
	MMProjection      mmprojection.StatusProber
	ModelMetadata     modelrouter.DiscoveredMetadataInspector
	Privacy           privacy.StatusSource
	ProviderAuth      *providerauth.Service
	// AllowInsecureAdmin grants the local operator full standalone admin
	// privileges without authentication. Callers must constrain the listener
	// to loopback; the executable rejects unsafe bind combinations.
	AllowInsecureAdmin bool
}

type Status struct {
	ConfigRevision   string              `json:"config_revision"`
	Providers        int                 `json:"providers"`
	Deployments      int                 `json:"deployments"`
	VirtualModels    int                 `json:"virtual_models"`
	ServedModelNames []string            `json:"served_model_names"`
	Targets          []TargetStatus      `json:"targets"`
	Credentials      credentials.Status  `json:"credentials"`
	MMProjection     mmprojection.Status `json:"mm_projection"`
	Privacy          privacy.Status      `json:"privacy"`
}

type TargetStatus struct {
	Deployment          string                `json:"deployment"`
	Title               string                `json:"title,omitempty"`
	CircuitState        routing.CircuitState  `json:"circuit_state"`
	AdmissionAvailable  bool                  `json:"admission_available"`
	ActiveRequests      int                   `json:"active_requests"`
	MaxConcurrency      int                   `json:"max_concurrency"`
	ConsecutiveFailures int                   `json:"consecutive_failures"`
	Samples             int                   `json:"samples"`
	Failures            int                   `json:"failures"`
	EjectionCount       int                   `json:"ejection_count"`
	EjectedUntil        *time.Time            `json:"ejected_until,omitempty"`
	HalfOpenProbeActive bool                  `json:"half_open_probe_active"`
	RecentFailures      []TargetFailureStatus `json:"recent_failures"`
}

type TargetFailureStatus struct {
	OccurredAt   time.Time `json:"occurred_at"`
	FailureClass string    `json:"failure_class"`
}

type handler struct {
	document      config.Document
	revision      config.Version
	status        Status
	options       Options
	ui            http.Handler
	bootstrap     adminapi.Bootstrap
	authenticated bool
	insecureAdmin bool
}

// NewHandler returns the standalone admin handler. Configuration mutation
// routes require either authentication or an explicitly enabled loopback-only
// insecure admin mode.
func NewHandler(
	document config.Document,
	revision config.Version,
	options Options,
) http.Handler {
	ui, err := adminui.NewEmbedded("/admin")
	if err != nil {
		panic("construct embedded admin UI: " + err.Error())
	}
	h := &handler{
		document: document,
		revision: revision,
		status: Status{
			ConfigRevision:   string(revision),
			Providers:        len(document.Providers),
			Deployments:      len(document.Deployments),
			VirtualModels:    len(document.VirtualModels),
			ServedModelNames: servedModelNames(document),
			Targets:          []TargetStatus{},
			Credentials:      emptyCredentialStatus(),
			MMProjection:     projectionStatus(options.MMProjection),
			Privacy:          privacyStatus(context.Background(), nil),
		},
		options:       options,
		ui:            ui,
		authenticated: options.Authenticator != nil,
		insecureAdmin: options.AllowInsecureAdmin,
		bootstrap: adminapi.Bootstrap{
			SchemaVersion:  adminapi.BootstrapSchemaVersion,
			Edition:        "standalone",
			GatewayVersion: options.GatewayVersion,
			Build:          options.BuildInfo,
			ConfigRevision: string(revision),
			Features: map[string]bool{
				adminapi.FeatureStatus:             true,
				adminapi.FeatureConfigRead:         false,
				adminapi.FeatureConfigValidate:     true,
				adminapi.FeatureRoutingSimulation:  true,
				adminapi.FeatureDiscoveredMetadata: false,
				adminapi.FeatureConfigWrite:        false,
				adminapi.FeatureConfigHistory:      false,
				adminapi.FeatureConfigManagedSets:  false,
				adminapi.FeatureLifecycle:          false,
				adminapi.FeatureEndpointInventory:  false,
				adminapi.FeatureLifecycleStatus:    false,
				adminapi.FeatureRuntimeEvents:      false,
				adminapi.FeatureClientCredentials:  false,
				adminapi.FeatureCredentialsWrite:   false,
				adminapi.FeatureMMProjectionProbe:  options.MMProjection != nil,
				adminapi.FeaturePrivacyPII:         options.Privacy != nil,
			},
			Principal: &adminapi.Principal{
				ID:    "local-operator",
				Roles: []string{RoleStatusRead},
			},
		},
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/ui/bootstrap", h.serveBootstrap)
	mux.HandleFunc("/v1/status", h.serveStatus)
	if options.MMProjection != nil {
		mux.HandleFunc("/v1/mm-projection/probe", h.probeMMProjection)
	}
	mux.HandleFunc("/v1/config/validate", h.validateConfiguration)
	mux.HandleFunc("/v1/config/simulate-routing", h.simulateRouting)
	privileged := h.authenticated || h.insecureAdmin
	if options.TraceReader != nil && privileged {
		mux.HandleFunc("/v1/saved-traces/export", h.exportSavedTraces)
	}
	if options.SparkrunCatalog != nil && privileged {
		mux.HandleFunc("/v1/sparkrun/catalog", h.sparkrunCatalog)
		if options.ManagedConfig != nil {
			mux.HandleFunc("/v1/sparkrun/recipe-draft", h.sparkrunRecipeDraft)
		}
	}
	if options.SparkrunControl != nil && privileged {
		mux.HandleFunc("/v1/sparkrun/workload", h.sparkrunWorkload)
	}
	if options.Lifecycle != nil && privileged {
		mux.HandleFunc("/v1/runtime/status", h.lifecycleStatus)
	}
	if options.ProviderAuth != nil && privileged {
		mux.HandleFunc("/v1/provider-auth/openai/", h.providerAuth)
	}
	if options.ManagedConfig != nil && privileged {
		mux.HandleFunc("/v1/config", h.activeConfiguration)
		mux.HandleFunc("/v1/config/managed-sets", h.managedSets)
		mux.HandleFunc("/v1/config/managed-sets/", h.managedSet)
	}
	if options.ModelMetadata != nil && privileged {
		mux.HandleFunc("/v1/model-routing/discovered-metadata", h.discoveredModelMetadata)
	}
	if options.ClientCredentials != nil && privileged {
		credentialHandler, err := clientcredentials.NewAdminHandler(clientcredentials.AdminOptions{
			Manager: options.ClientCredentials,
		})
		if err != nil {
			panic("construct standalone client credential admin handler: " + err.Error())
		}
		mux.Handle("/v1/client-credentials", credentialHandler)
		mux.Handle("/v1/client-credentials/", credentialHandler)
	}
	mux.HandleFunc("/", func(writer http.ResponseWriter, _ *http.Request) {
		writeError(writer, http.StatusNotFound, "not_found", "route not found")
	})
	var api http.Handler = mux
	if h.authenticated {
		api = h.authenticate(mux)
	} else if h.insecureAdmin {
		principal := identity.Principal{
			ID: "local-insecure-operator",
			Roles: []string{
				RoleStatusRead, RoleConfigRead, RoleConfigWrite, RoleTraceReadAll,
				clientcredentials.RoleRead, clientcredentials.RoleWrite,
			},
		}
		api = http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
			ctx := identity.WithContext(request.Context(), identity.Identity{Principal: principal})
			mux.ServeHTTP(writer, request.WithContext(ctx))
		})
	}
	return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path == "/" {
			target := "/admin"
			if request.URL.RawQuery != "" {
				target += "?" + request.URL.RawQuery
			}
			http.Redirect(writer, request, target, http.StatusPermanentRedirect)
			return
		}
		if request.URL.Path == "/admin" || strings.HasPrefix(request.URL.Path, "/admin/") {
			ui.ServeHTTP(writer, request)
			return
		}
		api.ServeHTTP(writer, request)
	})
}

// servedModelNames is the content-free externally callable catalog available
// to status readers. Hidden models are included because status_read is an
// administrative role; internal guardrail-only models remain excluded.
func servedModelNames(document config.Document) []string {
	seen := make(map[string]struct{})
	for _, model := range document.VirtualModels {
		if model.Visibility.Effective() == config.ModelVisibilityInternal {
			continue
		}
		seen[model.Name] = struct{}{}
		for _, alias := range model.Aliases {
			seen[alias] = struct{}{}
		}
	}
	result := make([]string, 0, len(seen))
	for name := range seen {
		result = append(result, name)
	}
	sort.Strings(result)
	return result
}

func (h *handler) validateConfiguration(
	writer http.ResponseWriter,
	request *http.Request,
) {
	if h.authenticated && !requireRole(writer, request, RoleConfigRead, RoleConfigWrite) {
		return
	}
	if request.Method != http.MethodPost {
		writer.Header().Set("Allow", http.MethodPost)
		writeError(
			writer,
			http.StatusMethodNotAllowed,
			"method_not_allowed",
			"method not allowed",
		)
		return
	}
	if request.URL.RawQuery != "" {
		writeError(
			writer,
			http.StatusBadRequest,
			"invalid_query",
			"query parameters are not supported",
		)
		return
	}
	var input struct {
		Document config.Document `json:"document"`
	}
	if !decodeRequestJSON(writer, request, &input) {
		return
	}
	if err := h.validateDocument(input.Document); err != nil {
		writeError(
			writer,
			http.StatusBadRequest,
			"invalid_configuration",
			err.Error(),
		)
		return
	}
	_, revision, err := config.EncodeCanonical(input.Document)
	if err != nil {
		writeError(
			writer,
			http.StatusBadRequest,
			"invalid_configuration",
			err.Error(),
		)
		return
	}
	writeJSON(writer, http.StatusOK, map[string]any{
		"valid":    true,
		"revision": revision,
	})
}

func (h *handler) simulateRouting(
	writer http.ResponseWriter,
	request *http.Request,
) {
	if h.authenticated && !requireRole(writer, request, RoleConfigRead, RoleConfigWrite) {
		return
	}
	if request.Method != http.MethodPost {
		writer.Header().Set("Allow", http.MethodPost)
		writeError(writer, http.StatusMethodNotAllowed, "method_not_allowed", "method not allowed")
		return
	}
	if request.URL.RawQuery != "" {
		writeError(writer, http.StatusBadRequest, "invalid_query", "query parameters are not supported")
		return
	}
	var input adminapi.RoutingSimulationInput
	if !decodeRequestJSON(writer, request, &input) {
		return
	}
	if err := h.validateDocument(input.Document); err != nil {
		writeError(writer, http.StatusBadRequest, "invalid_configuration", err.Error())
		return
	}
	result, err := adminapi.SimulateRoutingWithMetadata(
		input, config.UnknownCapabilityTry, discoveredMetadataState(h.options.ModelMetadata),
	)
	if err != nil {
		writeError(writer, http.StatusBadRequest, "routing_simulation_failed", err.Error())
		return
	}
	writeJSON(writer, http.StatusOK, result)
}

func discoveredMetadataState(source modelrouter.DiscoveredMetadataInspector) modelrouter.DiscoveredMetadataState {
	if source == nil {
		return modelrouter.DiscoveredMetadataState{}
	}
	return source.DiscoveredMetadata()
}

func (h *handler) validateDocument(document config.Document) error {
	if h.options.ValidateConfig != nil {
		return h.options.ValidateConfig(document)
	}
	_, err := routing.Compile(document)
	return err
}

func (h *handler) serveBootstrap(
	writer http.ResponseWriter,
	request *http.Request,
) {
	if !allowRead(writer, request) {
		return
	}
	if request.URL.RawQuery != "" {
		writeError(
			writer,
			http.StatusBadRequest,
			"invalid_query",
			"query parameters are not supported",
		)
		return
	}
	bootstrap := h.bootstrap
	bootstrap.Features = cloneFeatures(bootstrap.Features)
	if h.authenticated || h.insecureAdmin {
		principal, ok := principalFromRequest(request)
		if !ok {
			writeError(writer, http.StatusUnauthorized, "authentication_required", "admin authentication is required")
			return
		}
		bootstrap.Principal = &adminapi.Principal{
			ID: principal.ID, Roles: append([]string(nil), principal.Roles...),
		}
		bootstrap.Features[adminapi.FeatureSavedTraceExport] = h.options.TraceReader != nil && hasRole(principal, RoleTraceReadAll)
		bootstrap.Features["sparkrun_controls"] = h.options.SparkrunControl != nil && hasRole(principal, RoleConfigWrite)
		bootstrap.Features["sparkrun"] = h.options.SparkrunCatalog != nil
		bootstrap.Features["sparkrun_catalog"] = h.options.SparkrunCatalog != nil && hasRole(principal, RoleConfigRead)
		bootstrap.Features[adminapi.FeatureLifecycleStatus] = h.options.Lifecycle != nil && hasRole(principal, RoleStatusRead)
		bootstrap.Features[adminapi.FeatureLifecycle] = bootstrap.Features[adminapi.FeatureLifecycleStatus]
		bootstrap.Features[adminapi.FeatureStatus] = hasRole(principal, RoleStatusRead)
		bootstrap.Features[adminapi.FeatureProviderAuth] = h.options.ProviderAuth != nil && hasRole(principal, RoleConfigWrite)
		bootstrap.Features[adminapi.FeatureConfigValidate] =
			hasRole(principal, RoleConfigRead) || hasRole(principal, RoleConfigWrite)
		bootstrap.Features[adminapi.FeatureRoutingSimulation] =
			bootstrap.Features[adminapi.FeatureConfigValidate] ||
				(h.options.ManagedConfig != nil && hasRole(principal, RoleConfigReconcileSparkrun))
		bootstrap.Features[adminapi.FeatureConfigRead] = h.options.ManagedConfig != nil &&
			hasRole(principal, RoleConfigRead)
		bootstrap.Features[adminapi.FeatureConfigWrite] = h.options.ManagedConfig != nil &&
			hasRole(principal, RoleConfigWrite)
		bootstrap.Features[adminapi.FeatureConfigHistory] = false
		bootstrap.Features[adminapi.FeatureConfigManagedSets] = h.options.ManagedConfig != nil &&
			(hasRole(principal, RoleConfigRead) || hasRole(principal, RoleConfigWrite) ||
				hasRole(principal, RoleConfigReconcileSparkrun))
		bootstrap.Features[adminapi.FeatureClientCredentials] =
			h.options.ClientCredentials != nil && hasRole(principal, clientcredentials.RoleRead)
		bootstrap.Features[adminapi.FeatureCredentialsWrite] =
			h.options.ClientCredentials != nil && hasRole(principal, clientcredentials.RoleWrite)
		bootstrap.Features[adminapi.FeatureMMProjectionProbe] =
			h.options.MMProjection != nil && hasRole(principal, RoleStatusRead)
		bootstrap.Features[adminapi.FeatureDiscoveredMetadata] =
			h.options.ModelMetadata != nil && hasRole(principal, RoleConfigRead)
	}
	writeJSON(writer, http.StatusOK, bootstrap)
}

func (h *handler) discoveredModelMetadata(writer http.ResponseWriter, request *http.Request) {
	if !allowRead(writer, request) || !requireRole(writer, request, RoleConfigRead) {
		return
	}
	if request.URL.RawQuery != "" {
		writeError(writer, http.StatusBadRequest, "invalid_query", "query parameters are not supported")
		return
	}
	writer.Header().Set("Cache-Control", "no-store")
	writeJSON(writer, http.StatusOK, h.options.ModelMetadata.DiscoveredMetadata())
}

func (h *handler) serveStatus(
	writer http.ResponseWriter,
	request *http.Request,
) {
	if !allowRead(writer, request) {
		return
	}
	if h.authenticated && !requireRole(writer, request, RoleStatusRead) {
		return
	}
	if request.URL.RawQuery != "" {
		writeError(
			writer,
			http.StatusBadRequest,
			"invalid_query",
			"query parameters are not supported",
		)
		return
	}
	status := h.status
	status.Targets = targetStatuses(h.options.Targets)
	titles := make(map[string]string, len(h.document.Deployments))
	for _, deployment := range h.document.Deployments {
		titles[deployment.Name] = h.deploymentTitle(request.Context(), deployment)
	}
	for i := range status.Targets {
		status.Targets[i].Title = titles[status.Targets[i].Deployment]
	}
	status.Credentials = credentialStatus(h.options.Credentials)
	status.MMProjection = projectionStatus(h.options.MMProjection)
	status.Privacy = privacyStatus(request.Context(), h.options.Privacy)
	writeJSON(writer, http.StatusOK, status)
}

// Runtime placement enriches presentation only; stored configuration and IDs stay intact.
func (h *handler) deploymentTitle(ctx context.Context, deployment config.Deployment) string {
	if h.options.Endpoints == nil || !strings.HasPrefix(deployment.Name, "sparkrun:") || deployment.EndpointSource.Controller != "sparkrun" {
		return deployment.Title
	}
	endpoints, err := h.options.Endpoints.Ready(ctx, deployment.Name)
	if err != nil {
		return deployment.Title
	}
	names := make(map[string]bool)
	for _, endpoint := range endpoints {
		if endpoint.Controller != "sparkrun" {
			continue
		}
		if name := endpoint.Metadata["cluster_name"]; name != "" {
			names[name] = true
		}
	}
	if len(names) == 0 {
		return deployment.Title
	}
	sorted := make([]string, 0, len(names))
	for name := range names {
		sorted = append(sorted, name)
	}
	sort.Strings(sorted)
	return "sparkrun:" + strings.Join(sorted, ",") + ":" + deployment.Model
}

func privacyStatus(ctx context.Context, source privacy.StatusSource) privacy.Status {
	if source == nil {
		return privacy.DisabledStatus()
	}
	return source.PrivacyStatus(ctx)
}

func (h *handler) probeMMProjection(writer http.ResponseWriter, request *http.Request) {
	if request.Method != http.MethodPost {
		writer.Header().Set("Allow", http.MethodPost)
		writeError(writer, http.StatusMethodNotAllowed, "method_not_allowed", "method not allowed")
		return
	}
	if h.authenticated && !requireRole(writer, request, RoleStatusRead) {
		return
	}
	if request.URL.RawQuery != "" {
		writeError(writer, http.StatusBadRequest, "invalid_query", "query parameters are not supported")
		return
	}
	status, _ := h.options.MMProjection.Probe(request.Context())
	writeJSON(writer, http.StatusOK, status)
}

func projectionStatus(source mmprojection.StatusProber) mmprojection.Status {
	if source == nil {
		return mmprojection.DisabledStatus()
	}
	return source.Status()
}

func (h *handler) authenticate(next http.Handler) http.Handler {
	return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		principal, err := h.options.Authenticator.Authenticate(request.Context(), request)
		if err != nil {
			challenge := `Bearer realm="sparkroute-admin"`
			if provider, ok := h.options.Authenticator.(identity.ChallengeAuthenticator); ok {
				challenge = provider.AuthenticationChallenge("sparkroute-admin")
			}
			if challenge != "" {
				writer.Header().Set("WWW-Authenticate", challenge)
			}
			writeError(writer, http.StatusUnauthorized, "invalid_api_key", "admin authentication failed")
			return
		}
		nextRequest := request.Clone(identity.WithContext(
			request.Context(), identity.Identity{Principal: principal},
		))
		nextRequest.Header = request.Header.Clone()
		for _, name := range []string{"Authorization", "Cookie", "Proxy-Authorization", "X-Api-Key"} {
			nextRequest.Header.Del(name)
		}
		next.ServeHTTP(writer, nextRequest)
	})
}

func principalFromRequest(request *http.Request) (identity.Principal, bool) {
	value, ok := identity.FromContext(request.Context())
	return value.Principal, ok
}

func requireRole(writer http.ResponseWriter, request *http.Request, roles ...string) bool {
	principal, ok := principalFromRequest(request)
	if !ok {
		writeError(writer, http.StatusUnauthorized, "authentication_required", "admin authentication is required")
		return false
	}
	for _, role := range roles {
		if hasRole(principal, role) {
			return true
		}
	}
	writeError(writer, http.StatusForbidden, "forbidden", "the authenticated principal is not authorized for this operation")
	return false
}

func hasRole(principal identity.Principal, role string) bool {
	for _, current := range principal.Roles {
		if current == role {
			return true
		}
	}
	return false
}

func cloneFeatures(source map[string]bool) map[string]bool {
	result := make(map[string]bool, len(source))
	for key, value := range source {
		result[key] = value
	}
	return result
}

func credentialStatus(source credentials.StatusSource) credentials.Status {
	if source == nil {
		return emptyCredentialStatus()
	}
	return source.CredentialStatus()
}

func emptyCredentialStatus() credentials.Status {
	return credentials.Status{
		ObservedAt: time.Now().UTC(),
		Status:     credentials.ResolutionUnknown,
		Sources:    []credentials.SourceStatus{},
	}
}

func targetStatuses(source routing.TargetStatusSource) []TargetStatus {
	if source == nil {
		return []TargetStatus{}
	}
	current := append([]routing.TargetStatus(nil), source.Statuses()...)
	sort.Slice(current, func(i, j int) bool {
		return current[i].Deployment < current[j].Deployment
	})
	result := make([]TargetStatus, 0, len(current))
	for _, status := range current {
		recentFailures := status.RecentFailures
		if len(recentFailures) > routing.MaxRecentTargetFailures {
			recentFailures = recentFailures[:routing.MaxRecentTargetFailures]
		}
		item := TargetStatus{
			Deployment:          status.Deployment,
			CircuitState:        status.CircuitState,
			AdmissionAvailable:  status.AdmissionAvailable(),
			ActiveRequests:      status.ActiveRequests,
			MaxConcurrency:      status.MaxConcurrency,
			ConsecutiveFailures: status.ConsecutiveFailures,
			Samples:             status.Samples,
			Failures:            status.Failures,
			EjectionCount:       status.EjectionCount,
			HalfOpenProbeActive: status.HalfOpenProbeActive,
			RecentFailures:      make([]TargetFailureStatus, 0, len(recentFailures)),
		}
		for _, failure := range recentFailures {
			item.RecentFailures = append(item.RecentFailures, TargetFailureStatus{
				OccurredAt:   failure.OccurredAt,
				FailureClass: routing.SanitizeTargetFailureClass(failure.FailureClass),
			})
		}
		if !status.EjectedUntil.IsZero() {
			ejectedUntil := status.EjectedUntil
			item.EjectedUntil = &ejectedUntil
		}
		result = append(result, item)
	}
	return result
}

func allowRead(writer http.ResponseWriter, request *http.Request) bool {
	if request.Method == http.MethodGet || request.Method == http.MethodHead {
		return true
	}
	writer.Header().Set("Allow", "GET, HEAD")
	writeError(
		writer,
		http.StatusMethodNotAllowed,
		"method_not_allowed",
		"method not allowed",
	)
	return false
}

func decodeRequestJSON(
	writer http.ResponseWriter,
	request *http.Request,
	target any,
) bool {
	mediaType, _, err := mime.ParseMediaType(
		request.Header.Get("Content-Type"),
	)
	if err != nil || !strings.EqualFold(mediaType, "application/json") {
		writeError(
			writer,
			http.StatusUnsupportedMediaType,
			"unsupported_content_type",
			"Content-Type must be application/json",
		)
		return false
	}
	request.Body = http.MaxBytesReader(writer, request.Body, 9<<20)
	decoder := json.NewDecoder(request.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		var maxBytesError *http.MaxBytesError
		if errors.As(err, &maxBytesError) {
			writeError(
				writer,
				http.StatusRequestEntityTooLarge,
				"request_too_large",
				"request is too large",
			)
		} else {
			writeError(
				writer,
				http.StatusBadRequest,
				"invalid_json",
				"request body is invalid",
			)
		}
		return false
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		writeError(
			writer,
			http.StatusBadRequest,
			"invalid_json",
			"request body has trailing data",
		)
		return false
	}
	return true
}

func writeError(writer http.ResponseWriter, status int, code, message string) {
	writeJSON(writer, status, map[string]any{
		"error": map[string]string{
			"code":    code,
			"message": message,
		},
	})
}

func writeJSON(writer http.ResponseWriter, status int, value any) {
	writer.Header().Set("Content-Type", "application/json")
	writer.Header().Set("X-Content-Type-Options", "nosniff")
	writer.WriteHeader(status)
	_ = json.NewEncoder(writer).Encode(value)
}
