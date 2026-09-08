package main

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	ossadmin "github.com/sparksq/sparkroute/pkg/admin"
	"github.com/sparksq/sparkroute/pkg/clientcredentials"
	"github.com/sparksq/sparkroute/pkg/config"
	"github.com/sparksq/sparkroute/pkg/config/managed"
	"github.com/sparksq/sparkroute/pkg/credentials"
	credentialbuiltin "github.com/sparksq/sparkroute/pkg/credentials/builtin"
	"github.com/sparksq/sparkroute/pkg/endpointregistry"
	"github.com/sparksq/sparkroute/pkg/gateway"
	"github.com/sparksq/sparkroute/pkg/identity"
	"github.com/sparksq/sparkroute/pkg/ledger"
	"github.com/sparksq/sparkroute/pkg/lifecycle"
	"github.com/sparksq/sparkroute/pkg/mmprojection"
	"github.com/sparksq/sparkroute/pkg/modelrouter"
	"github.com/sparksq/sparkroute/pkg/promptcache"
	"github.com/sparksq/sparkroute/pkg/providerauth"
	"github.com/sparksq/sparkroute/pkg/responsesstate"
	"github.com/sparksq/sparkroute/pkg/routing"
	"github.com/sparksq/sparkroute/pkg/savedtrace"
	sparkrunruntime "github.com/sparksq/sparkroute/pkg/sparkrun"
	gatewaytelemetry "github.com/sparksq/sparkroute/pkg/telemetry"
	"github.com/sparksq/sparkroute/pkg/telemetry/otlpexport"
	"github.com/sparksq/sparkroute/pkg/version"
)

type runtimeBuildOptions struct {
	TraceStores             *traceStores
	TraceReader             savedtrace.Reader
	SparkrunWorkloads       *sparkrunruntime.Workloads
	Context                 context.Context
	Logger                  *slog.Logger
	CredentialOptions       credentialbuiltin.Options
	Ledger                  ledger.Recorder
	ResponsesState          responsesstate.Store
	SavedTraces             *savedtrace.AsyncRecorder
	MaxSavedTraceBytes      int64
	PromptCache             *promptcache.Directory
	PromptFingerprinter     *promptcache.Fingerprinter
	Telemetry               *otlpexport.Runtime
	IncludeAliases          bool
	ModelRouter             modelrouter.Router
	MMProjection            *mmprojection.Client
	MMProjectionCredentials credentials.StatusSource
	CallerAuthentication    identity.Authenticator
	AdminEnabled            bool
	AdminAuthentication     identity.Authenticator
	AllowInsecureAdmin      bool
	ClientCredentials       *clientcredentials.Manager
	ProviderAuth            *providerauth.Service
	ManagedConfig           managed.Store
	SparkrunEnabled         bool
	SparkrunCommand         string
	SparkrunEndpointTTL     time.Duration
	SparkrunReconcile       time.Duration
	SparkrunStopTimeout     time.Duration
}

type runtimeGeneration struct {
	revision    config.Version
	data        http.Handler
	admin       http.Handler
	targets     routing.TargetStatusSource
	credentials credentials.StatusSource
	// sparkrun retains the replica-local controller status boundary alongside
	// its close function; command acceptance waits for completed controller
	// transitions rather than treating subprocess acknowledgement as completion.
	sparkrun   *sparkrunruntime.Controller
	closeFn    func()
	activateFn func()

	active    atomic.Int64
	retired   atomic.Bool
	closeOnce sync.Once
}

func buildRuntimeGeneration(
	document config.Document,
	revision config.Version,
	options runtimeBuildOptions,
) (*runtimeGeneration, error) {
	if err := validateDocumentForIntegration(document, options.CredentialOptions, options.SparkrunEnabled); err != nil {
		return nil, err
	}
	requestModelRouter := options.ModelRouter
	if requestModelRouter == nil && document.ModelRouting != nil {
		manager, err := modelrouter.NewRouterManager(*document.ModelRouting)
		if err != nil {
			return nil, fmt.Errorf("construct model router: %w", err)
		}
		requestModelRouter = manager
	}
	credentialRegistry, err := credentialbuiltin.NewRegistry(document, options.CredentialOptions)
	if err != nil {
		return nil, err
	}
	lifecycleTargets, _ := lifecycle.TargetsFromDocument(document)
	var runtimeController *sparkrunruntime.Controller
	var endpointRegistry *endpointregistry.Memory
	var admissionCoordinator *lifecycle.AdmissionCoordinator
	if len(lifecycleTargets) > 0 {
		bridge, err := sparkrunruntime.NewClient(options.SparkrunCommand)
		if err != nil {
			return nil, err
		}
		registry := endpointregistry.NewMemory()
		endpointRegistry = registry
		metadataPublisher, _ := requestModelRouter.(modelrouter.DiscoveredMetadataPublisher)
		runtimeController, err = sparkrunruntime.New(sparkrunruntime.Options{
			Bridge: bridge, Registry: registry, Targets: lifecycleTargets, Workloads: options.SparkrunWorkloads,
			MetadataPublisher: metadataPublisher,
			MetadataSource:    "sparkrun:" + string(revision),
			EndpointTTL:       options.SparkrunEndpointTTL,
			ReconcileInterval: options.SparkrunReconcile,
			StopTimeout:       options.SparkrunStopTimeout,
		})
		if err != nil {
			return nil, err
		}
		capabilityCtx, cancelCapabilities := context.WithTimeout(options.Context, 10*time.Second)
		err = runtimeController.CheckCapabilities(capabilityCtx)
		cancelCapabilities()
		if err != nil {
			runtimeController.Close()
			return nil, fmt.Errorf("check Sparkrun gateway bridge: %w", err)
		}
		admissionCoordinator, err = lifecycle.NewAdmissionCoordinator(
			lifecycleTargets,
			lifecycle.AdmissionOptions{
				Registry: registry, Inspector: registry,
				Controllers: map[string]lifecycle.Controller{
					sparkrunruntime.ControllerName: runtimeController,
				},
				Authorizer: runtimeController,
				Recorder:   options.Ledger,
			},
		)
		if err != nil {
			runtimeController.Close()
			return nil, fmt.Errorf("construct Sparkrun lifecycle coordinator: %w", err)
		}
	}
	closeObservability, err := configureGenerationObservability(document, &options)
	if err != nil {
		if runtimeController != nil {
			runtimeController.Close()
		}
		return nil, fmt.Errorf("configure tracing: %w", err)
	}
	accepted := false
	defer func() {
		if !accepted {
			closeObservability()
		}
	}()
	dataOptions := gateway.DataOptions{
		Models:      gateway.ModelListOptions{IncludeAliases: options.IncludeAliases},
		Credentials: credentialRegistry, Ledger: options.Ledger, ProviderAuth: options.ProviderAuth,
		ResponsesState: options.ResponsesState, ConfigRevision: string(revision),
		MaxSavedTraceBytes: options.MaxSavedTraceBytes,
		PromptCache:        options.PromptCache, PromptFingerprinter: options.PromptFingerprinter,
		Telemetry: gatewaytelemetry.Options{
			TracerProvider: options.Telemetry.TracerProvider,
			MeterProvider:  options.Telemetry.MeterProvider,
			Propagator:     options.Telemetry.Propagator,
		},
		Lifecycle:    admissionCoordinator,
		ModelRouter:  requestModelRouter,
		MMProjection: options.MMProjection,
	}
	// Keep disabled saved traces as a nil interface. Assigning a nil pointer to
	// the Recorder interface would make it appear enabled and panic when the
	// request finalizer calls Record.
	if options.SavedTraces != nil {
		dataOptions.SavedTraces = options.SavedTraces
	}
	dataPlane, err := gateway.NewDataPlane(document, dataOptions)
	if err != nil {
		if runtimeController != nil {
			runtimeController.Close()
		}
		return nil, fmt.Errorf("construct data handler: %w", err)
	}
	rawDataHandler := dataPlane.Handler
	dataHandler := rawDataHandler
	if options.CallerAuthentication != nil {
		dataHandler = clientcredentials.WrapDataPlane(options.CallerAuthentication, dataHandler)
	}
	dataHandler = gateway.WrapMMProjectionAnalyzer(
		dataHandler,
		rawDataHandler,
		options.MMProjection,
		requestModelRouter,
	)
	generation := &runtimeGeneration{
		revision: revision, data: dataHandler,
		targets:  dataPlane.Targets,
		sparkrun: runtimeController,
		credentials: credentials.AggregateStatusSources(
			credentialRegistry,
			options.MMProjectionCredentials,
		),
	}
	accepted = true // The generation owns cleanup from this point.
	generation.closeFn = closeObservability
	if runtimeController != nil {
		generation.closeFn = func() { runtimeController.Close(); closeObservability() }
		runtimeController.Start(options.Context, func(err error) {
			options.Logger.Warn("Sparkrun endpoint reconciliation failed", slog.Any("err", err))
		})
	}
	generation.activateFn = func() {
		if options.SparkrunWorkloads != nil {
			options.SparkrunWorkloads.Configure(lifecycleTargets)
		}
	}
	if options.AdminEnabled {
		adminOptions := ossadmin.Options{
			TraceReader: options.TraceReader,
			Targets:     dataPlane.Targets, Credentials: generation.credentials,
			Privacy:        dataPlane.Privacy,
			GatewayVersion: version.Version,
			BuildInfo:      version.BuildInfo(),
			ValidateConfig: func(candidate config.Document) error {
				return validateDocumentForIntegration(candidate, options.CredentialOptions, options.SparkrunEnabled)
			},
			Authenticator:      options.AdminAuthentication,
			AllowInsecureAdmin: options.AllowInsecureAdmin,
			ClientCredentials:  options.ClientCredentials,
			ProviderAuth:       options.ProviderAuth,
			ManagedConfig:      options.ManagedConfig,
		}
		if admissionCoordinator != nil {
			adminOptions.Lifecycle = admissionCoordinator
			adminOptions.SparkrunControl = runtimeController
		}
		if endpointRegistry != nil {
			adminOptions.Endpoints = endpointRegistry
		}
		if options.SparkrunEnabled {
			catalog, err := sparkrunruntime.NewClient(options.SparkrunCommand)
			if err != nil {
				generation.close()
				return nil, err
			}
			adminOptions.SparkrunCatalog = catalog
		}
		adminOptions.ModelMetadata, _ = requestModelRouter.(modelrouter.DiscoveredMetadataInspector)
		if options.MMProjection != nil {
			adminOptions.MMProjection = options.MMProjection
		}
		generation.admin = ossadmin.NewHandler(document, revision, adminOptions)
	}
	accepted = true
	return generation, nil
}

func validateDocumentForIntegration(document config.Document, credentials credentialbuiltin.Options, enabled bool) error {
	if err := validateTraceEnvironment(document.Observability); err != nil {
		return err
	}
	if !enabled {
		for _, deployment := range document.Deployments {
			if deployment.EndpointSource.Controller == "sparkrun" {
				return fmt.Errorf("sparkrun integration is disabled; start SparkRoute with -sparkrun to use deployment %s", deployment.Name)
			}
		}
	}
	return validateRuntimeDocument(document, credentials)
}

func validateRuntimeDocument(
	document config.Document,
	credentialOptions credentialbuiltin.Options,
) error {
	if err := sparkrunruntime.ValidateWorkloadSharing(document); err != nil {
		return err
	}
	if err := gateway.ValidatePrivacyProvider(document, nil); err != nil {
		return err
	}
	if _, err := routing.Compile(document); err != nil {
		return err
	}
	targets, err := lifecycle.TargetsFromDocument(document)
	if err != nil {
		return err
	}
	if err := validateStandaloneLifecycleTargets(targets); err != nil {
		return err
	}
	_, err = credentialbuiltin.NewRegistry(document, credentialOptions)
	return err
}

func (g *runtimeGeneration) acquire() bool {
	g.active.Add(1)
	if !g.retired.Load() {
		return true
	}
	g.release()
	return false
}

func (g *runtimeGeneration) release() {
	if g.active.Add(-1) == 0 && g.retired.Load() {
		g.close()
	}
}

func (g *runtimeGeneration) retire() {
	g.retired.Store(true)
	if g.active.Load() == 0 {
		g.close()
	}
}

func (g *runtimeGeneration) close() {
	g.closeOnce.Do(func() {
		if g.closeFn != nil {
			g.closeFn()
		}
	})
}

type runtimeSlot struct {
	current atomic.Pointer[runtimeGeneration]
}

func newRuntimeSlot(initial *runtimeGeneration) *runtimeSlot {
	if initial.activateFn != nil {
		initial.activateFn()
	}
	slot := &runtimeSlot{}
	slot.current.Store(initial)
	return slot
}

func (s *runtimeSlot) replace(next *runtimeGeneration) {
	if next.activateFn != nil {
		next.activateFn()
	}
	previous := s.current.Swap(next)
	if previous != nil {
		previous.retire()
	}
}

func (s *runtimeSlot) acquire() *runtimeGeneration {
	for {
		generation := s.current.Load()
		if generation == nil {
			return nil
		}
		if generation.acquire() {
			return generation
		}
	}
}

func (s *runtimeSlot) Close() {
	current := s.current.Swap(nil)
	if current != nil {
		current.retire()
	}
}

func (s *runtimeSlot) dataHandler() http.Handler {
	return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		generation := s.acquire()
		if generation == nil {
			http.Error(writer, "gateway runtime unavailable", http.StatusServiceUnavailable)
			return
		}
		defer generation.release()
		generation.data.ServeHTTP(writer, request)
	})
}

func (s *runtimeSlot) adminHandler() http.Handler {
	return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		generation := s.acquire()
		if generation == nil || generation.admin == nil {
			if generation != nil {
				generation.release()
			}
			http.Error(writer, "gateway admin runtime unavailable", http.StatusServiceUnavailable)
			return
		}
		defer generation.release()
		generation.admin.ServeHTTP(writer, request)
	})
}

func (s *runtimeSlot) Statuses() []routing.TargetStatus {
	generation := s.acquire()
	if generation == nil {
		return nil
	}
	defer generation.release()
	return generation.targets.Statuses()
}

func (s *runtimeSlot) CredentialStatus() credentials.Status {
	generation := s.acquire()
	if generation == nil {
		return credentials.Status{ObservedAt: time.Now().UTC(), Status: credentials.ResolutionUnavailable}
	}
	defer generation.release()
	return generation.credentials.CredentialStatus()
}

func reconcileManagedConfiguration(
	ctx context.Context,
	store managed.Store,
	slot *runtimeSlot,
	options runtimeBuildOptions,
	logger *slog.Logger,
) {
	changes, err := store.Watch(ctx)
	if err != nil {
		logger.Error("watch managed configuration", slog.Any("err", err))
		return
	}
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case _, ok := <-changes:
			if !ok {
				return
			}
		case <-ticker.C:
		}
		document, revision, err := store.Load(ctx)
		if err != nil {
			logger.Error("load managed configuration", slog.Any("err", err))
			continue
		}
		current := slot.current.Load()
		if current != nil && current.revision == revision {
			continue
		}
		next, err := buildRuntimeGeneration(document, revision, options)
		if err != nil {
			logger.Error(
				"apply managed configuration",
				slog.String("config_revision", string(revision)),
				slog.Any("err", err),
			)
			continue
		}
		slot.replace(next)
		logger.Info("managed configuration applied", slog.String("config_revision", string(revision)))
	}
}
