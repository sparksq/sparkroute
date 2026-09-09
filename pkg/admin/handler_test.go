package admin

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sparksq/sparkroute/pkg/adminapi"
	"github.com/sparksq/sparkroute/pkg/clientcredentials"
	clientcredentialsqlite "github.com/sparksq/sparkroute/pkg/clientcredentials/sqlite"
	"github.com/sparksq/sparkroute/pkg/config"
	"github.com/sparksq/sparkroute/pkg/config/managed"
	configsqlite "github.com/sparksq/sparkroute/pkg/config/sqlite"
	"github.com/sparksq/sparkroute/pkg/credentials"
	"github.com/sparksq/sparkroute/pkg/identity"
	"github.com/sparksq/sparkroute/pkg/mmprojection"
	"github.com/sparksq/sparkroute/pkg/modelrouter"
	"github.com/sparksq/sparkroute/pkg/routing"
)

type staticTargetStatuses []routing.TargetStatus

func (s staticTargetStatuses) Statuses() []routing.TargetStatus {
	return append([]routing.TargetStatus(nil), s...)
}

type staticCredentialStatus credentials.Status

func (s staticCredentialStatus) CredentialStatus() credentials.Status {
	return credentials.Status(s)
}

type fakeProjectionProber struct {
	status mmprojection.Status
	calls  int
}

type metadataTestAuthenticator struct{}

func (metadataTestAuthenticator) Authenticate(
	_ context.Context,
	request *http.Request,
) (identity.Principal, error) {
	if request.Header.Get("Authorization") != "Bearer metadata-token" {
		return identity.Principal{}, identity.ErrInvalidCredentials
	}
	return identity.Principal{ID: "metadata-reader", Roles: []string{RoleConfigRead}}, nil
}

func TestAuthenticatedAdminExposesDiscoveredModelMetadata(t *testing.T) {
	t.Parallel()

	router, err := modelrouter.NewRouterManager(modelrouter.RoutingPolicy{
		Version: modelrouter.RoutingPolicyVersion, Revision: 2, DefaultVirtualModel: "auto",
		VirtualModels: map[string]modelrouter.VirtualModel{
			"auto": {Strategy: "smallest", Models: []string{"local"}},
		},
		Models: map[string]modelrouter.ModelMetadata{"local": {Enabled: true}},
	})
	if err != nil {
		t.Fatal(err)
	}
	contextLimit := 32_768
	if err := router.ReplaceDiscoveredMetadata(modelrouter.DiscoveredMetadataSnapshot{
		Source: "sparkrun:test", Models: map[string]modelrouter.DiscoveredModelMetadata{
			"local": {Context: &contextLimit, Tags: []string{"local"}},
		},
	}); err != nil {
		t.Fatal(err)
	}
	handler := NewHandler(config.Document{}, "revision", Options{
		Authenticator: metadataTestAuthenticator{}, ModelMetadata: router,
	})
	request := func(path string) *httptest.ResponseRecorder {
		current := httptest.NewRequest(http.MethodGet, path, nil)
		current.Header.Set("Authorization", "Bearer metadata-token")
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, current)
		return response
	}
	bootstrapResponse := request("/v1/ui/bootstrap")
	var bootstrap adminapi.Bootstrap
	if bootstrapResponse.Code != http.StatusOK ||
		json.Unmarshal(bootstrapResponse.Body.Bytes(), &bootstrap) != nil ||
		!bootstrap.Features[adminapi.FeatureDiscoveredMetadata] {
		t.Fatalf("bootstrap = %d %#v %s", bootstrapResponse.Code, bootstrap, bootstrapResponse.Body)
	}
	metadataResponse := request("/v1/model-routing/discovered-metadata")
	var state modelrouter.DiscoveredMetadataState
	if metadataResponse.Code != http.StatusOK ||
		json.Unmarshal(metadataResponse.Body.Bytes(), &state) != nil ||
		len(state.Sources) != 1 || state.Effective["local"].Context == nil ||
		*state.Effective["local"].Context != contextLimit {
		t.Fatalf("metadata = %d %#v %s", metadataResponse.Code, state, metadataResponse.Body)
	}
}

func (p *fakeProjectionProber) Status() mmprojection.Status { return p.status }

func (p *fakeProjectionProber) Probe(context.Context) (mmprojection.Status, error) {
	p.calls++
	p.status.State = "healthy"
	p.status.ProjectionAPI = 1
	p.status.Models = 2
	return p.status, nil
}

func TestReadOnlyAdminExposesProjectionStatusAndProbe(t *testing.T) {
	t.Parallel()
	prober := &fakeProjectionProber{status: mmprojection.Status{
		Configured: true, State: "unknown", Provider: mmprojection.ProviderID,
	}}
	handler := NewHandler(config.Document{}, "revision", Options{MMProjection: prober})

	bootstrapResponse := httptest.NewRecorder()
	handler.ServeHTTP(bootstrapResponse, httptest.NewRequest(http.MethodGet, "/v1/ui/bootstrap", nil))
	var bootstrap adminapi.Bootstrap
	if json.Unmarshal(bootstrapResponse.Body.Bytes(), &bootstrap) != nil ||
		!bootstrap.Features[adminapi.FeatureMMProjectionProbe] {
		t.Fatalf("bootstrap = %d %#v %s", bootstrapResponse.Code, bootstrap, bootstrapResponse.Body)
	}
	statusResponse := httptest.NewRecorder()
	handler.ServeHTTP(statusResponse, httptest.NewRequest(http.MethodGet, "/v1/status", nil))
	var status Status
	if json.Unmarshal(statusResponse.Body.Bytes(), &status) != nil ||
		status.MMProjection.State != "unknown" {
		t.Fatalf("status = %d %#v %s", statusResponse.Code, status, statusResponse.Body)
	}
	probeResponse := httptest.NewRecorder()
	handler.ServeHTTP(probeResponse, httptest.NewRequest(http.MethodPost, "/v1/mm-projection/probe", nil))
	var probed mmprojection.Status
	if probeResponse.Code != http.StatusOK ||
		json.Unmarshal(probeResponse.Body.Bytes(), &probed) != nil ||
		probed.State != "healthy" || prober.calls != 1 {
		t.Fatalf("probe = %d %#v calls=%d %s", probeResponse.Code, probed, prober.calls, probeResponse.Body)
	}
}

func TestReadOnlyAdminStatusAndBootstrap(t *testing.T) {
	t.Parallel()

	recentFailures := make([]routing.RecentTargetFailure, 10)
	for index := range recentFailures {
		recentFailures[index] = routing.RecentTargetFailure{
			OccurredAt:   time.Unix(int64(100-index), 0).UTC(),
			FailureClass: "upstream_http_503",
		}
	}
	recentFailures[0].FailureClass = "secret provider response\n"
	document := config.Document{
		Providers: []config.Provider{{
			Name:    "provider",
			BaseURL: "https://provider.example/v1",
		}},
		Deployments: []config.Deployment{{
			Name: "deployment",
		}},
		VirtualModels: []config.VirtualModel{{
			Name: "public", Aliases: []string{"public-alias"},
		}, {
			Name: "hidden", Aliases: []string{"hidden-alias"},
			Visibility: config.ModelVisibilityHidden,
		}, {
			Name: "internal", Aliases: []string{"internal-alias"},
			Visibility: config.ModelVisibilityInternal,
		}},
	}
	handler := NewHandler(document, "revision-a", Options{
		GatewayVersion: "1.2.3",
		Credentials: staticCredentialStatus{
			ObservedAt:           time.Unix(90, 0).UTC(),
			Status:               credentials.ResolutionHealthy,
			ConfiguredReferences: 1,
			ResolvedReferences:   1,
			Sources: []credentials.SourceStatus{{
				Scheme:               "env",
				Status:               credentials.ResolutionHealthy,
				ConfiguredReferences: 1,
				ResolvedReferences:   1,
			}},
		},
		Targets: staticTargetStatuses{{
			Deployment:     "deployment",
			CircuitState:   routing.CircuitClosed,
			ActiveRequests: 2,
			MaxConcurrency: 8,
			RecentFailures: recentFailures,
		}},
	})

	bootstrapResponse := httptest.NewRecorder()
	handler.ServeHTTP(
		bootstrapResponse,
		httptest.NewRequest(http.MethodGet, "/v1/ui/bootstrap", nil),
	)
	var bootstrap adminapi.Bootstrap
	if bootstrapResponse.Code != http.StatusOK ||
		json.Unmarshal(bootstrapResponse.Body.Bytes(), &bootstrap) != nil ||
		bootstrap.Edition != "standalone" ||
		bootstrap.GatewayVersion != "1.2.3" ||
		!bootstrap.Features[adminapi.FeatureStatus] ||
		!bootstrap.Features[adminapi.FeatureRoutingSimulation] {
		t.Fatalf(
			"bootstrap = %d %#v %s",
			bootstrapResponse.Code,
			bootstrap,
			bootstrapResponse.Body,
		)
	}

	statusResponse := httptest.NewRecorder()
	handler.ServeHTTP(
		statusResponse,
		httptest.NewRequest(http.MethodGet, "/v1/status", nil),
	)
	var status Status
	if statusResponse.Code != http.StatusOK ||
		json.Unmarshal(statusResponse.Body.Bytes(), &status) != nil ||
		status.Providers != 1 ||
		status.Credentials.Status != credentials.ResolutionHealthy ||
		strings.Join(status.ServedModelNames, ",") != "hidden,hidden-alias,public,public-alias" ||
		len(status.Credentials.Sources) != 1 ||
		len(status.Targets) != 1 ||
		status.Targets[0].ActiveRequests != 2 ||
		len(status.Targets[0].RecentFailures) != routing.MaxRecentTargetFailures ||
		status.Targets[0].RecentFailures[0].FailureClass != "upstream_failure" ||
		strings.Contains(statusResponse.Body.String(), "secret provider") ||
		strings.Contains(statusResponse.Body.String(), "internal-alias") ||
		strings.Contains(statusResponse.Body.String(), "provider.example") {
		t.Fatalf(
			"status = %d %#v %s",
			statusResponse.Code,
			status,
			statusResponse.Body,
		)
	}
}

func TestReadOnlyAdminSimulatesDraftModelRouting(t *testing.T) {
	t.Parallel()

	document := validManagedDocument("simulation")
	model := document.VirtualModels[0].Name
	document.ModelRouting = &modelrouter.RoutingPolicy{
		Version: modelrouter.RoutingPolicyVersion, Revision: 4,
		DefaultVirtualModel: "auto",
		VirtualModels: map[string]modelrouter.VirtualModel{
			"auto": {Strategy: "round_robin", Models: []string{model}},
		},
		Models: map[string]modelrouter.ModelMetadata{model: {Enabled: true}},
	}
	body := mustAdminJSON(t, adminapi.RoutingSimulationInput{
		Document: document, RequestedModel: "auto", RoutingText: "hello",
	})
	response := httptest.NewRecorder()
	request := httptest.NewRequest(
		http.MethodPost,
		"/v1/config/simulate-routing",
		strings.NewReader(body),
	)
	request.Header.Set("Content-Type", "application/json")
	NewHandler(config.Document{}, "revision", Options{}).ServeHTTP(
		response,
		request,
	)
	var result adminapi.RoutingSimulationResult
	if response.Code != http.StatusOK ||
		json.Unmarshal(response.Body.Bytes(), &result) != nil ||
		result.Decision.ResolvedModel != model ||
		result.StateMode != "stateless" {
		t.Fatalf("simulation = %d %#v %s", response.Code, result, response.Body)
	}
}

func mustAdminJSON(t *testing.T, value any) string {
	t.Helper()
	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatalf("Marshal() error = %v", err)
	}
	return string(raw)
}

func TestManagedAdminRegistersAuthorizedCredentialAPI(t *testing.T) {
	t.Parallel()
	directory := t.TempDir()
	if err := os.Chmod(directory, 0o700); err != nil {
		t.Fatalf("Chmod() error = %v", err)
	}
	store, err := clientcredentialsqlite.Open(context.Background(), clientcredentialsqlite.Options{
		Path: filepath.Join(directory, "credentials.sqlite"),
	})
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	defer func() { _ = store.Close() }()
	manager, _ := clientcredentials.NewManager(store)
	adminCredential, err := manager.Create(context.Background(), clientcredentials.CreateInput{
		Name: "admin", PrincipalID: "admin-a",
		Roles: []string{RoleStatusRead, clientcredentials.RoleRead, clientcredentials.RoleWrite},
	}, "bootstrap-cli")
	if err != nil {
		t.Fatalf("Create() error = %v", err)
	}
	handler := NewHandler(config.Document{}, "revision", Options{
		Authenticator:     clientcredentials.Authenticator{Manager: manager},
		ClientCredentials: manager,
	})
	unauthorized := httptest.NewRecorder()
	handler.ServeHTTP(unauthorized, httptest.NewRequest(http.MethodGet, "/v1/ui/bootstrap", nil))
	if unauthorized.Code != http.StatusUnauthorized {
		t.Fatalf("unauthorized bootstrap = %d %s", unauthorized.Code, unauthorized.Body)
	}

	bootstrapRequest := httptest.NewRequest(http.MethodGet, "/v1/ui/bootstrap", nil)
	bootstrapRequest.Header.Set("Authorization", "Bearer "+adminCredential.APIKey)
	bootstrapResponse := httptest.NewRecorder()
	handler.ServeHTTP(bootstrapResponse, bootstrapRequest)
	var bootstrap adminapi.Bootstrap
	if bootstrapResponse.Code != http.StatusOK ||
		json.Unmarshal(bootstrapResponse.Body.Bytes(), &bootstrap) != nil ||
		!bootstrap.Features[adminapi.FeatureClientCredentials] ||
		!bootstrap.Features[adminapi.FeatureCredentialsWrite] ||
		bootstrap.Principal == nil || bootstrap.Principal.Tenant != "" {
		t.Fatalf("managed bootstrap = %d %#v %s", bootstrapResponse.Code, bootstrap, bootstrapResponse.Body)
	}

	createRequest := httptest.NewRequest(http.MethodPost, "/v1/client-credentials", strings.NewReader(`{
		"name":"worker","principal_id":"worker-a","roles":["inference"]
	}`))
	createRequest.Header.Set("Authorization", "Bearer "+adminCredential.APIKey)
	createRequest.Header.Set("Content-Type", "application/json")
	createResponse := httptest.NewRecorder()
	handler.ServeHTTP(createResponse, createRequest)
	if createResponse.Code != http.StatusCreated ||
		strings.Contains(createResponse.Body.String(), "tenant_id") ||
		createResponse.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("managed create = %d %#v %s", createResponse.Code, createResponse.Header(), createResponse.Body)
	}
}

func TestManagedConfigurationAPIEnforcesOwnerRoles(t *testing.T) {
	t.Parallel()
	directory := filepath.Join(t.TempDir(), "private")
	if err := os.Mkdir(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	credentialStore, err := clientcredentialsqlite.Open(
		context.Background(),
		clientcredentialsqlite.Options{Path: filepath.Join(directory, "credentials.sqlite")},
	)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = credentialStore.Close() }()
	manager, err := clientcredentials.NewManager(credentialStore)
	if err != nil {
		t.Fatal(err)
	}
	operator, err := manager.Create(context.Background(), clientcredentials.CreateInput{
		Name: "operator", PrincipalID: "operator-a",
		Roles: []string{RoleConfigRead, RoleConfigWrite},
	}, "bootstrap-cli")
	if err != nil {
		t.Fatal(err)
	}
	sparkrun, err := manager.Create(context.Background(), clientcredentials.CreateInput{
		Name: "sparkrun", PrincipalID: "sparkrun-a",
		Roles: []string{RoleConfigReconcileSparkrun},
	}, "bootstrap-cli")
	if err != nil {
		t.Fatal(err)
	}
	writerOnly, err := manager.Create(context.Background(), clientcredentials.CreateInput{
		Name: "writer-only", PrincipalID: "writer-only-a",
		Roles: []string{RoleConfigWrite},
	}, "bootstrap-cli")
	if err != nil {
		t.Fatal(err)
	}
	configStore, err := configsqlite.Open(context.Background(), configsqlite.Options{
		Path: filepath.Join(directory, "config.sqlite"),
	})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = configStore.Close() }()
	if _, err := configStore.Initialize(context.Background(), managed.EmptyDocument(), "bootstrap", ""); err != nil {
		t.Fatal(err)
	}
	initial, active, err := configStore.Load(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	handler := NewHandler(initial, active, Options{
		Authenticator: clientcredentials.Authenticator{Manager: manager},
		ManagedConfig: configStore,
	})
	request := func(method, path, token string, body any) *httptest.ResponseRecorder {
		var reader io.Reader
		if body != nil {
			raw, marshalErr := json.Marshal(body)
			if marshalErr != nil {
				t.Fatal(marshalErr)
			}
			reader = strings.NewReader(string(raw))
		}
		current := httptest.NewRequest(method, path, reader)
		current.Header.Set("Authorization", "Bearer "+token)
		if body != nil {
			current.Header.Set("Content-Type", "application/json")
		}
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, current)
		return response
	}

	bootstrap := request(http.MethodGet, "/v1/ui/bootstrap", operator.APIKey, nil)
	var bootstrapBody adminapi.Bootstrap
	if bootstrap.Code != http.StatusOK || json.Unmarshal(bootstrap.Body.Bytes(), &bootstrapBody) != nil ||
		!bootstrapBody.Features[adminapi.FeatureConfigManagedSets] ||
		!bootstrapBody.Features[adminapi.FeatureConfigWrite] ||
		bootstrapBody.Features[adminapi.FeatureConfigHistory] {
		t.Fatalf("operator bootstrap = %d %#v %s", bootstrap.Code, bootstrapBody, bootstrap.Body)
	}
	for _, path := range []string{
		"/v1/config/revisions", "/v1/config/activations",
		"/v1/config/managed-set-events",
	} {
		response := request(http.MethodGet, path, operator.APIKey, nil)
		if response.Code != http.StatusNotFound {
			t.Fatalf("standalone history route %s = %d %s", path, response.Code, response.Body)
		}
	}

	writerSets := request(http.MethodGet, "/v1/config/managed-sets", writerOnly.APIKey, nil)
	for _, token := range []string{operator.APIKey, writerOnly.APIKey} {
		if response := request(http.MethodGet, "/v1/config/presets", token, nil); response.Code != http.StatusOK {
			t.Fatalf("presets read = %d %s", response.Code, response.Body)
		}
	}
	for _, path := range []string{"/v1/config/presets", "/v1/config/presets/save", "/v1/config/presets/activate", "/v1/config/presets/rename", "/v1/config/presets/delete"} {
		method := http.MethodPost
		if path == "/v1/config/presets" {
			method = http.MethodGet
		}
		if response := request(method, path, sparkrun.APIKey, map[string]any{}); response.Code != http.StatusForbidden {
			t.Fatalf("reconcile-only presets %s = %d %s", path, response.Code, response.Body)
		}
	}
	if writerSets.Code != http.StatusOK || !strings.Contains(writerSets.Body.String(), `"owner":"operator"`) ||
		strings.Contains(writerSets.Body.String(), `"owner":"sparkrun"`) {
		t.Fatalf("writer-only managed sets = %d %s", writerSets.Code, writerSets.Body)
	}
	if response := request(http.MethodGet, "/v1/config/managed-sets/operator", writerOnly.APIKey, nil); response.Code != http.StatusOK {
		t.Fatalf("writer-only operator read = %d %s", response.Code, response.Body)
	}
	if response := request(http.MethodGet, "/v1/config/managed-sets/sparkrun", writerOnly.APIKey, nil); response.Code != http.StatusForbidden {
		t.Fatalf("writer-only sparkrun read = %d %s", response.Code, response.Body)
	}
	if response := request(http.MethodGet, "/v1/config", writerOnly.APIKey, nil); response.Code != http.StatusForbidden {
		t.Fatalf("writer-only merged read = %d %s", response.Code, response.Body)
	}
	operatorDocument := validManagedDocument("operator")
	replaced := request(http.MethodPut, "/v1/config/managed-sets/operator", operator.APIKey, map[string]any{
		"document": operatorDocument, "expected_active_revision": active, "reason": "operator edit",
	})
	if replaced.Code != http.StatusOK || !strings.Contains(replaced.Body.String(), `"changed":true`) {
		t.Fatalf("operator replace = %d %s", replaced.Code, replaced.Body)
	}
	_, active, err = configStore.Load(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	forbidden := request(http.MethodPut, "/v1/config/managed-sets/sparkrun", operator.APIKey, map[string]any{
		"document": managed.EmptyDocument(), "expected_active_revision": active,
	})
	if forbidden.Code != http.StatusForbidden {
		t.Fatalf("operator wrote sparkrun set = %d %s", forbidden.Code, forbidden.Body)
	}
	forbidden = request(http.MethodGet, "/v1/config/managed-sets/operator", sparkrun.APIKey, nil)
	if forbidden.Code != http.StatusForbidden {
		t.Fatalf("sparkrun read operator set = %d %s", forbidden.Code, forbidden.Body)
	}
	ownedSets := request(http.MethodGet, "/v1/config/managed-sets", sparkrun.APIKey, nil)
	var ownedSetsBody struct {
		ActiveRevision config.Version        `json:"active_revision"`
		ManagedSets    []managed.SetMetadata `json:"managed_sets"`
	}
	if ownedSets.Code != http.StatusOK || json.Unmarshal(ownedSets.Body.Bytes(), &ownedSetsBody) != nil ||
		ownedSetsBody.ActiveRevision != active || len(ownedSetsBody.ManagedSets) != 1 ||
		ownedSetsBody.ManagedSets[0].Owner != managed.OwnerSparkrun {
		t.Fatalf("sparkrun managed sets = %d %#v %s", ownedSets.Code, ownedSetsBody, ownedSets.Body)
	}
	sparkrunBootstrap := request(http.MethodGet, "/v1/ui/bootstrap", sparkrun.APIKey, nil)
	var sparkrunBootstrapBody adminapi.Bootstrap
	if sparkrunBootstrap.Code != http.StatusOK ||
		json.Unmarshal(sparkrunBootstrap.Body.Bytes(), &sparkrunBootstrapBody) != nil ||
		!sparkrunBootstrapBody.Features[adminapi.FeatureRoutingSimulation] {
		t.Fatalf("sparkrun bootstrap = %d %#v %s", sparkrunBootstrap.Code, sparkrunBootstrapBody, sparkrunBootstrap.Body)
	}
	validated := request(http.MethodPost, "/v1/config/managed-sets/sparkrun/validate", sparkrun.APIKey, map[string]any{
		"document": validManagedDocument("generated"), "expected_active_revision": active,
	})
	if validated.Code != http.StatusOK || !strings.Contains(validated.Body.String(), `"valid":true`) {
		t.Fatalf("sparkrun validate = %d %s", validated.Code, validated.Body)
	}
	replaced = request(http.MethodPut, "/v1/config/managed-sets/sparkrun", sparkrun.APIKey, map[string]any{
		"document": validManagedDocument("generated"), "expected_active_revision": active,
	})
	if replaced.Code != http.StatusOK {
		t.Fatalf("sparkrun replace = %d %s", replaced.Code, replaced.Body)
	}
	merged, _, err := configStore.Load(context.Background())
	if err != nil || len(merged.Providers) != 2 || len(merged.VirtualModels) != 2 {
		t.Fatalf("merged configuration = %#v, %v", merged, err)
	}
	_, active, err = configStore.Load(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	operatorCandidate := validManagedDocument("operator")
	operatorCandidate.ModelRouting = &modelrouter.RoutingPolicy{
		Version:             modelrouter.RoutingPolicyVersion,
		Revision:            1,
		DefaultVirtualModel: "auto",
		VirtualModels: map[string]modelrouter.VirtualModel{
			"auto": {Strategy: "lowest_cost", Models: []string{"generated-model"}},
		},
		Models: map[string]modelrouter.ModelMetadata{
			"generated-model": {Enabled: true, Weight: 100},
		},
	}
	simulated := request(http.MethodPost, "/v1/config/managed-sets/operator/simulate-routing", operator.APIKey, map[string]any{
		"document": operatorCandidate, "expected_active_revision": active, "requested_model": "auto",
	})
	if simulated.Code != http.StatusOK ||
		!strings.Contains(simulated.Body.String(), `"resolved_model":"generated-model"`) ||
		simulated.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("managed routing simulation = %d %#v %s", simulated.Code, simulated.Header(), simulated.Body)
	}
	_, afterSimulation, err := configStore.Load(context.Background())
	if err != nil || afterSimulation != active {
		t.Fatalf("active after managed routing simulation = %s, %v; want %s", afterSimulation, err, active)
	}
}

func TestExplicitInsecureAdminGetsFullManagedConfigurationAccess(t *testing.T) {
	t.Parallel()
	directory := filepath.Join(t.TempDir(), "private")
	if err := os.Mkdir(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	configStore, err := configsqlite.Open(context.Background(), configsqlite.Options{
		Path: filepath.Join(directory, "config.sqlite"),
	})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = configStore.Close() }()
	if _, err := configStore.Initialize(context.Background(), managed.EmptyDocument(), "bootstrap", ""); err != nil {
		t.Fatal(err)
	}
	initial, active, err := configStore.Load(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	handler := NewHandler(initial, active, Options{
		ManagedConfig:      configStore,
		AllowInsecureAdmin: true,
	})

	bootstrapResponse := httptest.NewRecorder()
	handler.ServeHTTP(bootstrapResponse, httptest.NewRequest(http.MethodGet, "/v1/ui/bootstrap", nil))
	var bootstrap adminapi.Bootstrap
	if bootstrapResponse.Code != http.StatusOK || json.Unmarshal(bootstrapResponse.Body.Bytes(), &bootstrap) != nil {
		t.Fatalf("bootstrap = %d %s", bootstrapResponse.Code, bootstrapResponse.Body)
	}
	if !bootstrap.Features[adminapi.FeatureConfigRead] || !bootstrap.Features[adminapi.FeatureConfigWrite] ||
		bootstrap.Principal == nil || bootstrap.Principal.ID != "local-insecure-operator" {
		t.Fatalf("bootstrap = %#v", bootstrap)
	}

	setsResponse := httptest.NewRecorder()
	handler.ServeHTTP(setsResponse, httptest.NewRequest(http.MethodGet, "/v1/config/managed-sets", nil))
	if setsResponse.Code != http.StatusOK {
		t.Fatalf("managed sets = %d %s", setsResponse.Code, setsResponse.Body)
	}
}

func validManagedDocument(prefix string) config.Document {
	return config.Document{
		Providers: []config.Provider{{
			Name: prefix + "-provider", Type: "openai_compatible", BaseURL: "http://127.0.0.1/v1",
		}},
		Deployments: []config.Deployment{{
			Name: prefix + "-deployment", Provider: prefix + "-provider", Model: "upstream",
		}},
		VirtualModels: []config.VirtualModel{{
			Name: prefix + "-model", Pools: []config.RoutingPool{{
				Targets: []config.WeightedTarget{{Deployment: prefix + "-deployment", Weight: 1}},
			}},
		}},
	}
}

func TestReadOnlyAdminServesEmbeddedApplication(t *testing.T) {
	t.Parallel()

	handler := NewHandler(config.Document{}, "revision", Options{})
	response := httptest.NewRecorder()
	handler.ServeHTTP(
		response,
		httptest.NewRequest(http.MethodGet, "/admin/", nil),
	)
	if response.Code != http.StatusOK ||
		!strings.Contains(response.Body.String(), "sparkroute-admin-root") {
		t.Fatalf(
			"admin app = %d %#v %s",
			response.Code,
			response.Header(),
			response.Body,
		)
	}
}

func TestAdminRootRedirectsToApplication(t *testing.T) {
	t.Parallel()

	handler := NewHandler(config.Document{}, "revision", Options{})
	response := httptest.NewRecorder()
	handler.ServeHTTP(
		response,
		httptest.NewRequest(http.MethodGet, "/?view=config", nil),
	)
	if response.Code != http.StatusPermanentRedirect ||
		response.Header().Get("Location") != "/admin?view=config" {
		t.Fatalf("root redirect = %d %#v", response.Code, response.Header())
	}
}

func TestReadOnlyAdminValidatesWithoutMutatingConfiguration(t *testing.T) {
	t.Parallel()

	document := config.Document{
		Providers: []config.Provider{{
			Name:    "provider",
			Type:    "openai_compatible",
			BaseURL: "https://example.com/v1",
		}},
		Deployments: []config.Deployment{{
			Name:     "deployment",
			Provider: "provider",
			Model:    "upstream",
		}},
		VirtualModels: []config.VirtualModel{{
			Name: "public",
			Pools: []config.RoutingPool{{
				Targets: []config.WeightedTarget{{
					Deployment: "deployment",
					Weight:     1,
				}},
			}},
		}},
	}
	handler := NewHandler(config.Document{}, "active", Options{})
	serve := func(current config.Document) *httptest.ResponseRecorder {
		raw, err := json.Marshal(map[string]any{"document": current})
		if err != nil {
			t.Fatalf("json.Marshal() error = %v", err)
		}
		request := httptest.NewRequest(
			http.MethodPost,
			"/v1/config/validate",
			strings.NewReader(string(raw)),
		)
		request.Header.Set("Content-Type", "application/json")
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		return response
	}
	valid := serve(document)
	if valid.Code != http.StatusOK ||
		!strings.Contains(valid.Body.String(), `"valid":true`) ||
		!strings.Contains(valid.Body.String(), `"revision":`) {
		t.Fatalf("valid response = %d %s", valid.Code, valid.Body)
	}
	document.Deployments[0].Provider = "unknown"
	invalid := serve(document)
	if invalid.Code != http.StatusBadRequest ||
		!strings.Contains(
			invalid.Body.String(),
			`"code":"invalid_configuration"`,
		) {
		t.Fatalf("invalid response = %d %s", invalid.Code, invalid.Body)
	}
}
