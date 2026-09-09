// SPDX-FileCopyrightText: 2026 Scitrera LLC
// SPDX-FileCopyrightText: 2026 Fox Engine Ltd.
// SPDX-License-Identifier: AGPL-3.0-only

package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	openaiauth "github.com/scitrera/go-llm/auth/openai"
	ossadmin "github.com/sparksq/sparkroute/pkg/admin"
	"github.com/sparksq/sparkroute/pkg/adminapi"
	"github.com/sparksq/sparkroute/pkg/clientcredentials"
	clientcredentialsqlite "github.com/sparksq/sparkroute/pkg/clientcredentials/sqlite"
	"github.com/sparksq/sparkroute/pkg/config"
	configfile "github.com/sparksq/sparkroute/pkg/config/file"
	"github.com/sparksq/sparkroute/pkg/config/managed"
	configsqlite "github.com/sparksq/sparkroute/pkg/config/sqlite"
	"github.com/sparksq/sparkroute/pkg/credentials"
	credentialbuiltin "github.com/sparksq/sparkroute/pkg/credentials/builtin"
	credentialfile "github.com/sparksq/sparkroute/pkg/credentials/file"
	credentialkubernetes "github.com/sparksq/sparkroute/pkg/credentials/kubernetes"
	"github.com/sparksq/sparkroute/pkg/gateway"
	"github.com/sparksq/sparkroute/pkg/identity"
	"github.com/sparksq/sparkroute/pkg/ledger"
	ledgersqlite "github.com/sparksq/sparkroute/pkg/ledger/sqlite"
	"github.com/sparksq/sparkroute/pkg/lifecycle"
	"github.com/sparksq/sparkroute/pkg/mmprojection"
	"github.com/sparksq/sparkroute/pkg/modelrouter"
	"github.com/sparksq/sparkroute/pkg/promptcache"
	"github.com/sparksq/sparkroute/pkg/providerauth"
	"github.com/sparksq/sparkroute/pkg/responsesstate"
	"github.com/sparksq/sparkroute/pkg/savedtrace"
	tracefilesystem "github.com/sparksq/sparkroute/pkg/savedtrace/filesystem"
	sparkrunruntime "github.com/sparksq/sparkroute/pkg/sparkrun"
	"github.com/sparksq/sparkroute/pkg/telemetry/otlpexport"
	"github.com/sparksq/sparkroute/pkg/version"
)

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stderr, nil))
	slog.SetDefault(logger)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	if err := run(ctx, os.Args[1:], os.Stdout, logger); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return
		}
		logger.Error("gateway stopped", slog.Any("err", err))
		os.Exit(1)
	}
}

func run(ctx context.Context, args []string, stdout io.Writer, logger *slog.Logger) error {
	if len(args) >= 2 && args[0] == "traces" && args[1] == "export" {
		return runTraceExport(ctx, args[2:], stdout)
	}
	if len(args) >= 1 && args[0] == "client-credentials" {
		return runClientCredentialCommand(ctx, args[1:], stdout)
	}
	fileAllowGroupReadDefault, err := envBool(
		"SPARKROUTE_CREDENTIAL_FILE_ALLOW_GROUP_READ",
		false,
	)
	if err != nil {
		return err
	}
	operationsPprofDefault, err := envBool(
		"SPARKROUTE_OPERATIONS_PPROF",
		false,
	)
	if err != nil {
		return err
	}
	allowInsecureAdminNonLoopbackDefault, err := envBool(
		"SPARKROUTE_ALLOW_INSECURE_ADMIN_NONLOOPBACK",
		false,
	)
	if err != nil {
		return err
	}
	traceQueueCapacityDefault, err := envInt(
		"SPARKROUTE_TRACE_QUEUE_CAPACITY",
		savedtrace.DefaultAsyncQueueCapacity,
	)
	if err != nil {
		return err
	}
	traceWriteWorkersDefault, err := envInt(
		"SPARKROUTE_TRACE_WRITE_WORKERS",
		0,
	)
	if err != nil {
		return err
	}
	traceBatchSizeDefault, err := envInt(
		"SPARKROUTE_TRACE_BATCH_SIZE",
		savedtrace.DefaultAsyncBatchSize,
	)
	if err != nil {
		return err
	}
	traceBatchIntervalDefault, err := envDuration(
		"SPARKROUTE_TRACE_BATCH_INTERVAL",
		savedtrace.DefaultAsyncBatchInterval,
	)
	if err != nil {
		return err
	}
	traceSessionIntervalDefault, err := envDuration(
		"SPARKROUTE_TRACE_SESSION_INTERVAL", savedtrace.DefaultSessionInterval,
	)
	if err != nil {
		return err
	}
	mmProjectionTimeoutDefault, err := time.ParseDuration(env(
		"SPARKROUTE_MM_PROJECTION_TIMEOUT",
		env("MM_PROJECTION_TIMEOUT", "10m"),
	))
	if err != nil {
		return fmt.Errorf("parse MM projection timeout: %w", err)
	}
	legacyProjectionTokenRef := ""
	if strings.TrimSpace(os.Getenv("MM_PROJECTION_TOKEN")) != "" {
		legacyProjectionTokenRef = "env://MM_PROJECTION_TOKEN"
	}
	flags := flag.NewFlagSet("sparkroute", flag.ContinueOnError)
	flags.SetOutput(stdout)
	configPath := flags.String("config", env("SPARKROUTE_CONFIG", ""), "path to gateway configuration")
	piiSQLitePath := flags.String("pii-sqlite", env("SPARKROUTE_PII_SQLITE", ""), "private PII mapping SQLite path; defaults beside persistent configuration when conversation scope is used")
	piiKeyringFile := flags.String("pii-keyring-file", env("SPARKROUTE_PII_KEYRING_FILE", ""), "private PII mapping keyring file; empty generates and retains a local key beside the mapping store")
	configSourceMode := flags.String(
		"config-source",
		env("SPARKROUTE_CONFIG_SOURCE", "file"),
		"configuration source: file or sqlite",
	)
	configSQLitePath := flags.String(
		"config-sqlite",
		env("SPARKROUTE_CONFIG_SQLITE", ""),
		"private SQLite managed-configuration path used with -config-source sqlite",
	)
	configBootstrapPath := flags.String(
		"config-bootstrap",
		env("SPARKROUTE_CONFIG_BOOTSTRAP", ""),
		"optional operator configuration loaded only when a SQLite store is empty",
	)
	routingPolicyPath := flags.String(
		"routing-policy",
		env("SPARKROUTE_ROUTING_POLICY", ""),
		"optional SparkRoute-compatible JSON routing policy; publishes selector models such as auto",
	)
	mmProjectionURL := flags.String(
		"mm-projection-url",
		env("SPARKROUTE_MM_PROJECTION_URL", env("MM_PROJECTION_URL", "")),
		"MMBridge /v1 base URL; empty disables multimedia projection execution",
	)
	mmProjectionTokenRef := flags.String(
		"mm-projection-token-ref",
		env("SPARKROUTE_MM_PROJECTION_TOKEN_REF", legacyProjectionTokenRef),
		"credential reference containing the MMBridge shared bearer token",
	)
	mmProjectionAnalyzerModel := flags.String(
		"mm-projection-analyzer-model",
		env("SPARKROUTE_MM_PROJECTION_ANALYZER_MODEL", env("MM_PROJECTION_ANALYZER_MODEL", "")),
		"default analyzer logical model used when routing policy does not override it",
	)
	mmProjectionTimeout := flags.Duration(
		"mm-projection-timeout",
		mmProjectionTimeoutDefault,
		"default MMBridge request timeout",
	)
	dataAddress := flags.String(
		"data-address",
		env("SPARKROUTE_DATA_ADDRESS", "127.0.0.1:8080"),
		"data listener address",
	)
	operationsAddress := flags.String(
		"operations-address",
		env("SPARKROUTE_OPERATIONS_ADDRESS", ""),
		"optional operations listener address; empty serves operations routes on the data listener",
	)
	operationsPprof := flags.Bool(
		"operations-pprof",
		operationsPprofDefault,
		"expose Go profiles on the operations surface; disabled by default",
	)
	adminAddress := flags.String(
		"admin-address",
		env("SPARKROUTE_ADMIN_ADDRESS", ""),
		"optional admin/UI listener address; empty serves admin routes on the data listener",
	)
	adminTokenFile := flags.String(
		"admin-token-file",
		env("SPARKROUTE_ADMIN_TOKEN_FILE", ""),
		"live bearer token file used by token-file admin authentication; missing or empty disables authentication",
	)
	clientCredentialSQLitePath := flags.String(
		"client-credentials-sqlite",
		env("SPARKROUTE_CLIENT_CREDENTIALS_SQLITE", ""),
		"private SQLite client-credential path; empty disables managed credentials",
	)
	callerAuthMode := flags.String(
		"caller-auth-mode",
		env("SPARKROUTE_CALLER_AUTH_MODE", "disabled"),
		"data-plane caller authentication: disabled, managed, or token-file",
	)
	callerTokenFile := flags.String(
		"caller-token-file",
		env("SPARKROUTE_CALLER_TOKEN_FILE", ""),
		"bearer token file used by token-file caller authentication",
	)
	adminAuthMode := flags.String(
		"admin-auth-mode",
		env("SPARKROUTE_ADMIN_AUTH_MODE", "local"),
		"admin authentication: local read-only compatibility, managed, token-file, or disabled (unsafe full admin; loopback-only unless separately allowed)",
	)
	allowInsecureAdminNonLoopback := flags.Bool(
		"allow-insecure-admin-nonloopback",
		allowInsecureAdminNonLoopbackDefault,
		"allow an admin mode that may be unauthenticated on a non-loopback listener (dangerous; explicit opt-in)",
	)
	ledgerSQLitePath := flags.String(
		"ledger-sqlite",
		env("SPARKROUTE_LEDGER_SQLITE", ""),
		"SQLite usage-ledger path; empty disables persistence, :memory: is ephemeral",
	)
	traceStorage := flags.String(
		"trace-storage",
		env("SPARKROUTE_TRACE_STORAGE", "disabled"),
		"saved trace storage: disabled, database, or filesystem",
	)
	traceDatabasePath := flags.String(
		"trace-database",
		env("SPARKROUTE_TRACE_DATABASE", ""),
		"SQLite saved-trace path; empty reuses -ledger-sqlite",
	)
	traceFilesystemPath := flags.String(
		"trace-filesystem",
		env("SPARKROUTE_TRACE_FILESYSTEM", ""),
		"owner-only saved-trace directory used with filesystem storage",
	)
	traceMaxBodyBytes := flags.Int64(
		"trace-max-body-bytes",
		savedtrace.DefaultMaxBodyBytes,
		"maximum saved bytes for each request and response body",
	)
	traceQueueCapacity := flags.Int(
		"trace-queue-capacity",
		traceQueueCapacityDefault,
		"bounded saved-trace queue capacity",
	)
	traceWriteWorkers := flags.Int(
		"trace-write-workers",
		traceWriteWorkersDefault,
		"concurrent saved-trace writers; 0 selects a backend-aware default",
	)
	traceOverflowPolicy := flags.String(
		"trace-overflow-policy",
		env("SPARKROUTE_TRACE_OVERFLOW_POLICY", string(savedtrace.OverflowBlock)),
		"saved-trace queue behavior: block or drop",
	)
	traceBatchSize := flags.Int(
		"trace-batch-size",
		traceBatchSizeDefault,
		"maximum records per saved-trace storage commit",
	)
	traceBatchInterval := flags.Duration(
		"trace-batch-interval",
		traceBatchIntervalDefault,
		"maximum saved-trace group-commit delay",
	)
	traceSessionInterval := flags.Duration(
		"trace-session-interval", traceSessionIntervalDefault,
		"capture-session durability checkpoint interval",
	)
	sparkrunEnabled := flags.Bool("sparkrun", false, "enable sparkrun integration, recipe catalog, and on-demand lifecycle")
	sparkrunCommand := flags.String(
		"sparkrun-command",
		env("SPARKROUTE_SPARKRUN_COMMAND", "sparkrun"),
		"Sparkrun executable used for the hidden gateway bridge",
	)
	sparkrunEndpointTTL := flags.Duration(
		"sparkrun-endpoint-ttl",
		90*time.Second,
		"lifetime of a health-checked Sparkrun endpoint registration",
	)
	sparkrunReconcileInterval := flags.Duration(
		"sparkrun-reconcile-interval",
		30*time.Second,
		"interval for discovering and refreshing Sparkrun endpoints",
	)
	sparkrunStopTimeout := flags.Duration(
		"sparkrun-stop-timeout",
		2*time.Minute,
		"maximum time for an idle or operator-requested Sparkrun stop",
	)
	credentialFileRoots := flags.String(
		"credential-file-roots",
		env("SPARKROUTE_CREDENTIAL_FILE_ROOTS", ""),
		"OS path-list of allowlisted roots for file:// credentials",
	)
	credentialFileAllowGroupRead := flags.Bool(
		"credential-file-allow-group-read",
		fileAllowGroupReadDefault,
		"allow group-read-only file credentials; other group access remains rejected",
	)
	credentialKubernetesAPIURL := flags.String(
		"credential-kubernetes-api-url",
		env("SPARKROUTE_CREDENTIAL_KUBERNETES_API_URL", ""),
		"Kubernetes API URL; empty uses the in-cluster service",
	)
	credentialKubernetesNamespaces := flags.String(
		"credential-kubernetes-namespaces",
		env("SPARKROUTE_CREDENTIAL_KUBERNETES_NAMESPACES", ""),
		"comma-separated namespace allowlist; empty uses the pod namespace",
	)
	configCheck := flags.Bool("config-check", false, "validate configuration and exit")
	showVersion := flags.Bool("version", false, "print version and exit")
	providerAuthPath := flags.String("provider-auth-sqlite", "", "private provider sign-in store; defaults to <config-sqlite>.provider-auth.db for managed configuration")
	showBuildInfo := flags.Bool("build-info", false, "print release/source identity as JSON and exit")
	includeAliases := flags.Bool("list-aliases", false, "include aliases in GET /v1/models")
	if err := flags.Parse(args); err != nil {
		return fmt.Errorf("parse flags: %w", err)
	}
	if *operationsPprof && strings.TrimSpace(*operationsAddress) == "" &&
		!gateway.IsLoopbackAddress(*dataAddress) {
		return fmt.Errorf(
			"-operations-pprof requires a dedicated -operations-address or a loopback data listener",
		)
	}
	if *showVersion {
		_, err := fmt.Fprintln(stdout, version.Version)
		return err
	}
	if *showBuildInfo {
		return json.NewEncoder(stdout).Encode(version.BuildInfo())
	}
	var providerAuth *providerauth.Service
	if *providerAuthPath == "" && strings.EqualFold(strings.TrimSpace(*configSourceMode), "sqlite") && *configSQLitePath != "" && *configSQLitePath != ":memory:" {
		*providerAuthPath = *configSQLitePath + ".provider-auth.db"
	}
	if *providerAuthPath != "" && !*configCheck {
		store, err := providerauth.OpenStore(ctx, *providerAuthPath)
		if err != nil {
			return fmt.Errorf("open provider sign-in store: %w", err)
		}
		providerAuth = providerauth.New(ctx, store, openaiauth.Config{})
		defer func() {
			providerAuth.Close()
			if err := store.Close(); err != nil {
				logger.Error("close provider sign-in store", slog.Any("err", err))
			}
		}()
	}
	var clientCredentialManager *clientcredentials.Manager
	var clientCredentialStore *clientcredentialsqlite.Store
	if *clientCredentialSQLitePath != "" {
		clientCredentialStore, err = clientcredentialsqlite.Open(ctx, clientcredentialsqlite.Options{
			Path: *clientCredentialSQLitePath,
		})
		if err != nil {
			return fmt.Errorf("open standalone client credential store: %w", err)
		}
		defer func() {
			if err := clientCredentialStore.Close(); err != nil {
				logger.Error("close client credential store", slog.Any("err", err))
			}
		}()
		clientCredentialManager, err = clientcredentials.NewManager(clientCredentialStore)
		if err != nil {
			return err
		}
	}
	var callerAuthentication identity.Authenticator
	callerMode := strings.ToLower(strings.TrimSpace(*callerAuthMode))
	switch callerMode {
	case "disabled":
	case "managed":
		if clientCredentialManager == nil {
			return fmt.Errorf("-caller-auth-mode managed requires -client-credentials-sqlite")
		}
		callerAuthentication = clientcredentials.Authenticator{
			Manager: clientCredentialManager, RequiredRole: clientcredentials.RoleInference,
		}
	case "token-file":
		if strings.TrimSpace(*callerTokenFile) == "" {
			return fmt.Errorf("-caller-auth-mode token-file requires -caller-token-file")
		}
		callerAuthentication = identity.FileBearerAuthenticator{
			Path: *callerTokenFile,
			Principal: identity.Principal{
				ID:    "shared-token-caller",
				Roles: []string{clientcredentials.RoleInference},
			},
		}
	default:
		return fmt.Errorf("-caller-auth-mode must be disabled, managed, or token-file")
	}
	if callerMode != "token-file" && strings.TrimSpace(*callerTokenFile) != "" {
		return fmt.Errorf("-caller-token-file requires -caller-auth-mode token-file")
	}
	var adminAuthentication identity.Authenticator
	insecureAdmin := false
	adminMode := strings.ToLower(strings.TrimSpace(*adminAuthMode))
	switch adminMode {
	case "local":
	case "disabled":
		insecureAdmin = true
	case "managed":
		if clientCredentialManager == nil {
			return fmt.Errorf("-admin-auth-mode managed requires -client-credentials-sqlite")
		}
		adminAuthentication = clientcredentials.Authenticator{Manager: clientCredentialManager}
	case "token-file":
		if strings.TrimSpace(*adminTokenFile) == "" {
			return fmt.Errorf("-admin-auth-mode token-file requires -admin-token-file")
		}
		insecureAdmin = true
		adminAuthentication = standaloneTokenFileAdminAuthenticator(*adminTokenFile)
	default:
		return fmt.Errorf("-admin-auth-mode must be local, managed, token-file, or disabled")
	}
	if adminMode != "token-file" && strings.TrimSpace(*adminTokenFile) != "" {
		return fmt.Errorf("-admin-token-file requires -admin-auth-mode token-file")
	}
	if insecureAdmin {
		adminBind := strings.TrimSpace(*adminAddress)
		if adminBind == "" {
			adminBind = *dataAddress
		}
		if !gateway.IsLoopbackAddress(adminBind) && !*allowInsecureAdminNonLoopback {
			return fmt.Errorf("-admin-auth-mode %s requires a loopback admin listener or explicit -allow-insecure-admin-nonloopback", adminMode)
		}
	} else if *allowInsecureAdminNonLoopback {
		return fmt.Errorf("-allow-insecure-admin-nonloopback requires -admin-auth-mode disabled or token-file")
	}
	if !insecureAdmin && strings.TrimSpace(*adminAddress) == "" && adminAuthentication == nil &&
		!gateway.IsLoopbackAddress(*dataAddress) {
		return fmt.Errorf("co-locating local unauthenticated admin routes requires a loopback -data-address; configure -admin-address or managed admin authentication")
	}
	credentialOptions := credentialbuiltin.Options{
		File: credentialfile.Options{
			Roots:          splitPathList(*credentialFileRoots),
			AllowGroupRead: *credentialFileAllowGroupRead,
		},
		Kubernetes: credentialkubernetes.Options{
			APIURL:            *credentialKubernetesAPIURL,
			AllowedNamespaces: splitCommaList(*credentialKubernetesNamespaces),
		},
	}
	var document config.Document
	var configVersion config.Version
	var managedConfigStore managed.Store
	switch strings.ToLower(strings.TrimSpace(*configSourceMode)) {
	case "file":
		if *configPath == "" {
			return fmt.Errorf("-config or SPARKROUTE_CONFIG is required for file configuration")
		}
		if *configSQLitePath != "" || *configBootstrapPath != "" {
			return fmt.Errorf("-config-sqlite and -config-bootstrap require -config-source sqlite")
		}
		document, configVersion, err = (configfile.Source{Path: *configPath}).Load(ctx)
	case "sqlite":
		if *configSQLitePath == "" {
			return fmt.Errorf("-config-source sqlite requires -config-sqlite")
		}
		if *configPath != "" {
			return fmt.Errorf("-config is not used with -config-source sqlite; use -config-bootstrap")
		}
		if adminMode != "managed" && adminMode != "disabled" && adminMode != "token-file" {
			return fmt.Errorf("-config-source sqlite requires managed, token-file, or explicit loopback-only disabled admin authentication")
		}
		if adminMode == "managed" && clientCredentialManager == nil {
			return fmt.Errorf("-config-source sqlite with managed admin authentication requires -client-credentials-sqlite")
		}
		store, openErr := configsqlite.Open(ctx, configsqlite.Options{
			Path: *configSQLitePath,
			Validator: func(candidate config.Document) error {
				return validateDocumentForIntegration(candidate, credentialOptions, *sparkrunEnabled)
			},
		})
		if openErr != nil {
			return fmt.Errorf("open standalone SQLite configuration store: %w", openErr)
		}
		managedConfigStore = store
		defer func() {
			if err := managedConfigStore.Close(); err != nil {
				logger.Error("close managed configuration store", slog.Any("err", err))
			}
		}()
		_, _, loadErr := store.Load(ctx)
		if errors.Is(loadErr, configsqlite.ErrNoActiveConfiguration) {
			bootstrap := managed.EmptyDocument()
			if *configBootstrapPath != "" {
				bootstrap, _, err = (configfile.Source{Path: *configBootstrapPath}).Load(ctx)
				if err != nil {
					return fmt.Errorf("load SQLite operator bootstrap: %w", err)
				}
			}
			if _, err = store.Initialize(
				ctx, bootstrap, "deployment/bootstrap", "initialize managed configuration",
			); err != nil {
				return err
			}
		} else if loadErr != nil {
			return loadErr
		}
		document, configVersion, err = store.Load(ctx)
	default:
		return fmt.Errorf("-config-source must be file or sqlite")
	}
	if err != nil {
		return err
	}
	if err := validateDocumentForIntegration(document, credentialOptions, *sparkrunEnabled); err != nil {
		return err
	}
	var requestModelRouter modelrouter.Router
	if strings.TrimSpace(*routingPolicyPath) != "" {
		manager, loadErr := modelrouter.LoadRouterManager(*routingPolicyPath)
		if loadErr != nil {
			return fmt.Errorf("load routing policy: %w", loadErr)
		}
		requestModelRouter = manager
	}
	var projectionClient *mmprojection.Client
	var projectionCredentialStatus credentials.StatusSource
	projectionConfigured := strings.TrimSpace(*mmProjectionURL) != "" ||
		strings.TrimSpace(*mmProjectionTokenRef) != "" ||
		strings.TrimSpace(*mmProjectionAnalyzerModel) != ""
	if projectionConfigured && strings.TrimSpace(*mmProjectionURL) == "" {
		return fmt.Errorf("-mm-projection-url is required when other MM projection options are set")
	}
	if strings.TrimSpace(*mmProjectionURL) != "" {
		if strings.TrimSpace(*mmProjectionTokenRef) == "" {
			return fmt.Errorf("-mm-projection-token-ref is required with -mm-projection-url")
		}
		projectionCredentials, projectionErr := credentialbuiltin.NewRegistryForReferences(
			[]credentials.Ref{credentials.Ref(*mmProjectionTokenRef)},
			credentialOptions,
		)
		if projectionErr != nil {
			return fmt.Errorf("configure MM projection credential: %w", projectionErr)
		}
		projectionCredentialStatus = projectionCredentials
		projectionClient, projectionErr = mmprojection.New(mmprojection.Options{
			URL:                  *mmProjectionURL,
			TokenReference:       credentials.Ref(*mmProjectionTokenRef),
			Credentials:          projectionCredentials,
			DefaultAnalyzerModel: *mmProjectionAnalyzerModel,
			DefaultTimeout:       *mmProjectionTimeout,
		})
		if projectionErr != nil {
			return fmt.Errorf("configure MM projection: %w", projectionErr)
		}
	}
	if *configCheck {
		_, err := fmt.Fprintf(stdout, "config ok %s\n", configVersion)
		return err
	}
	var usageStore ledger.Store = ledger.DiscardStore{}
	if *ledgerSQLitePath != "" {
		sqliteStore, err := ledgersqlite.Open(ctx, ledgersqlite.Options{
			Path: *ledgerSQLitePath,
		})
		if err != nil {
			return fmt.Errorf("open SQLite usage ledger: %w", err)
		}
		usageStore = sqliteStore
	}
	responsesState := responsesstate.Store(
		responsesstate.NewMemoryStore(responsesstate.MemoryOptions{}),
	)
	if durableState, ok := usageStore.(responsesstate.Store); ok {
		responsesState = durableState
	}
	usageWriter, err := ledger.NewAsyncWriter(usageStore, ledger.AsyncOptions{
		OnLoss: func(event ledger.LossEvent) {
			logger.Error(
				"ledger records lost",
				slog.String("reason", string(event.Reason)),
				slog.Uint64("count", event.Count),
				slog.Any("err", event.Err),
			)
		},
	})
	if err != nil {
		_ = usageStore.Close()
		return fmt.Errorf("construct usage ledger writer: %w", err)
	}
	traceStore, traceStoreShared, err := configureStandaloneSavedTraceStore(
		ctx,
		*traceStorage,
		*traceDatabasePath,
		*traceFilesystemPath,
		*ledgerSQLitePath,
		usageStore,
	)
	if err != nil {
		_ = closeLedgerWriter(logger, usageWriter)
		return err
	}
	var traceRecorder *savedtrace.AsyncRecorder
	if traceStore != nil {
		if *traceMaxBodyBytes <= 0 || *traceMaxBodyBytes > savedtrace.MaxBodyBytes {
			_ = closeLedgerWriter(logger, usageWriter)
			if !traceStoreShared {
				_ = traceStore.Close()
			}
			return fmt.Errorf(
				"-trace-max-body-bytes must be between 1 and %d",
				savedtrace.MaxBodyBytes,
			)
		}
		overflowPolicy, err := savedtrace.ParseOverflowPolicy(*traceOverflowPolicy)
		if err != nil {
			_ = closeLedgerWriter(logger, usageWriter)
			if !traceStoreShared {
				_ = traceStore.Close()
			}
			return fmt.Errorf("parse -trace-overflow-policy: %w", err)
		}
		workers := resolveTraceWriteWorkers(*traceStorage, *traceWriteWorkers, 1)
		traceRecorder, err = savedtrace.NewAsyncRecorder(
			traceStore,
			savedtrace.AsyncOptions{
				QueueCapacity:   *traceQueueCapacity,
				Workers:         workers,
				Overflow:        overflowPolicy,
				BatchSize:       *traceBatchSize,
				BatchInterval:   *traceBatchInterval,
				SessionInterval: *traceSessionInterval,
				OnLoss: func(event savedtrace.LossEvent) {
					logger.Error(
						"saved trace lost",
						slog.String("reason", string(event.Reason)),
						slog.Any("err", event.Err),
					)
				},
			},
		)
		if err != nil {
			_ = closeLedgerWriter(logger, usageWriter)
			if !traceStoreShared {
				_ = traceStore.Close()
			}
			return fmt.Errorf("construct saved trace recorder: %w", err)
		}
		logger.Info(
			"saved trace recorder enabled",
			slog.String("storage", strings.ToLower(strings.TrimSpace(*traceStorage))),
			slog.String("overflow_policy", string(overflowPolicy)),
			slog.Int("queue_capacity", *traceQueueCapacity),
			slog.Int("write_workers", workers),
			slog.Int("batch_size", *traceBatchSize),
			slog.Duration("batch_interval", *traceBatchInterval),
			slog.Duration("session_interval", *traceSessionInterval),
		)
	}
	promptCache, err := promptcache.NewDirectory(
		promptcache.NewMemoryStore(promptcache.MemoryOptions{}),
		promptcache.DirectoryOptions{
			OnError: func(err error) {
				logger.Warn("prompt-cache affinity unavailable", slog.Any("err", err))
			},
			OnLoss: func() {
				logger.Warn("prompt-cache affinity observation dropped")
			},
		},
	)
	if err != nil {
		_ = closeSavedTraceRecorder(logger, traceRecorder)
		_ = closeLedgerWriter(logger, usageWriter)
		if traceStore != nil && !traceStoreShared {
			_ = traceStore.Close()
		}
		return fmt.Errorf("construct prompt-cache affinity directory: %w", err)
	}
	promptFingerprinter, err := promptcache.NewRandomFingerprinter()
	if err != nil {
		_ = closePromptCache(logger, promptCache)
		_ = closeSavedTraceRecorder(logger, traceRecorder)
		_ = closeLedgerWriter(logger, usageWriter)
		if traceStore != nil && !traceStoreShared {
			_ = traceStore.Close()
		}
		return err
	}
	telemetryRuntime, err := otlpexport.FromEnvironment(
		ctx,
		"sparkroute",
		version.Version,
	)
	if err != nil {
		_ = closePromptCache(logger, promptCache)
		_ = closeSavedTraceRecorder(logger, traceRecorder)
		_ = closeLedgerWriter(logger, usageWriter)
		if traceStore != nil && !traceStoreShared {
			_ = traceStore.Close()
		}
		return fmt.Errorf("configure OpenTelemetry: %w", err)
	}
	workloads := sparkrunruntime.NewWorkloads()
	defer workloads.Close()
	tracePath := *traceFilesystemPath
	if *traceStorage == "database" {
		tracePath = *traceDatabasePath
		if tracePath == "" {
			tracePath = *ledgerSQLitePath
		}
	}
	if *piiSQLitePath == "" {
		source := *configPath
		if strings.EqualFold(strings.TrimSpace(*configSourceMode), "sqlite") {
			source = *configSQLitePath
		}
		if source != "" && source != ":memory:" {
			*piiSQLitePath = filepath.Join(source+".pii", "mappings.sqlite")
		}
	}
	privacyRuntime := &privacyRuntime{ctx: ctx, path: *piiSQLitePath, keyPath: *piiKeyringFile}
	defer func() {
		if err := privacyRuntime.Close(); err != nil {
			logger.Error("close PII mapping storage", "error", err)
		}
	}()
	runtimeOptions := runtimeBuildOptions{
		Privacy:     privacyRuntime,
		TraceStores: newTraceStores(*traceStorage, tracePath, traceStore), TraceReader: traceStore,
		SparkrunWorkloads: workloads,
		SparkrunEnabled:   *sparkrunEnabled,
		Context:           ctx, Logger: logger, CredentialOptions: credentialOptions,
		Ledger: usageWriter, ResponsesState: responsesState,
		SavedTraces: traceRecorder, MaxSavedTraceBytes: *traceMaxBodyBytes,
		PromptCache: promptCache, PromptFingerprinter: promptFingerprinter,
		Telemetry: telemetryRuntime, IncludeAliases: *includeAliases,
		ModelRouter:             requestModelRouter,
		MMProjection:            projectionClient,
		MMProjectionCredentials: projectionCredentialStatus,
		CallerAuthentication:    callerAuthentication,
		AdminEnabled:            true, AdminAuthentication: adminAuthentication,
		AllowInsecureAdmin: insecureAdmin,
		ClientCredentials:  clientCredentialManager, ManagedConfig: managedConfigStore,
		ProviderAuth:    providerAuth,
		SparkrunCommand: *sparkrunCommand, SparkrunEndpointTTL: *sparkrunEndpointTTL,
		SparkrunReconcile:   *sparkrunReconcileInterval,
		SparkrunStopTimeout: *sparkrunStopTimeout,
	}
	initialRuntime, err := buildRuntimeGeneration(document, configVersion, runtimeOptions)
	if err != nil {
		_ = closeTelemetry(logger, telemetryRuntime)
		_ = closePromptCache(logger, promptCache)
		_ = closeSavedTraceRecorder(logger, traceRecorder)
		_ = closeLedgerWriter(logger, usageWriter)
		if traceStore != nil && !traceStoreShared {
			_ = traceStore.Close()
		}
		return err
	}
	runtimeSlot := newRuntimeSlot(initialRuntime)
	health := gateway.NewHealth()
	health.SetTargetStatusSource(runtimeSlot)
	health.SetCredentialStatusSource(runtimeSlot)
	defer health.SetReady(false)

	listeners := gateway.AssembleListeners(
		gateway.ListenerSpec{
			Name: "data", Address: *dataAddress, Handler: runtimeSlot.dataHandler(),
		},
		gateway.ListenerSurface{
			Listener: gateway.ListenerSpec{
				Name: "admin", Address: *adminAddress, Handler: runtimeSlot.adminHandler(),
			},
			MatchPath: adminapi.IsPath,
		},
		gateway.ListenerSurface{
			Listener: gateway.ListenerSpec{
				Name: "operations", Address: *operationsAddress,
				Handler: gateway.NewOperationsHandler(health.Handler(), *operationsPprof),
			},
			MatchPath: gateway.IsOperationsPath,
		},
	)
	var cancelReload context.CancelFunc
	var reloadDone chan struct{}
	if managedConfigStore != nil {
		var reloadContext context.Context
		reloadContext, cancelReload = context.WithCancel(ctx)
		reloadDone = make(chan struct{})
		go func() {
			defer close(reloadDone)
			reconcileManagedConfiguration(
				reloadContext, managedConfigStore, runtimeSlot, runtimeOptions, logger,
			)
		}()
	}
	group := gateway.ServerGroup{
		Listeners: listeners,
		OnReady: func(listeners []gateway.BoundListener) {
			health.SetReady(true)
			for _, listener := range listeners {
				logger.Info(
					"listener ready",
					slog.String("name", listener.Name),
					slog.String("address", listener.Address),
					slog.String("config_revision", string(configVersion)),
				)
			}
		},
	}
	runErr := group.Run(ctx)
	if cancelReload != nil {
		cancelReload()
		<-reloadDone
	}
	runtimeSlot.Close()
	promptCacheErr := closePromptCache(logger, promptCache)
	traceErr := closeSavedTraceRecorder(logger, traceRecorder)
	closeErr := closeLedgerWriter(logger, usageWriter)
	var traceStoreErr error
	if traceStore != nil && !traceStoreShared {
		traceStoreErr = traceStore.Close()
	}
	telemetryErr := closeTelemetry(logger, telemetryRuntime)
	return errors.Join(runErr, promptCacheErr, traceErr, closeErr, traceStoreErr, telemetryErr)
}

func standaloneTokenFileAdminAuthenticator(path string) identity.FileBearerAuthenticator {
	roles := []string{
		ossadmin.RoleStatusRead,
		ossadmin.RoleConfigRead,
		ossadmin.RoleConfigWrite,
		ossadmin.RoleTraceReadAll,
		ossadmin.RoleConfigReconcileSparkrun,
		clientcredentials.RoleRead,
		clientcredentials.RoleWrite,
	}
	return identity.FileBearerAuthenticator{
		Path: path,
		Principal: identity.Principal{
			ID:    "shared-token-operator",
			Roles: append([]string(nil), roles...),
		},
		Optional: true,
		FallbackPrincipal: identity.Principal{
			ID:    "open-local-operator",
			Roles: append([]string(nil), roles...),
		},
	}
}

func validateStandaloneLifecycleTargets(targets []lifecycle.Target) error {
	for _, target := range targets {
		if target.Controller != sparkrunruntime.ControllerName {
			return fmt.Errorf(
				"standalone gateway does not support runtime controller %q",
				target.Controller,
			)
		}
		if target.Source == lifecycle.EndpointActivatable && target.Binding.RecipeRevision == "" {
			return fmt.Errorf(
				"the Sparkrun deployment %q requires endpoint_source.recipe_revision",
				target.Deployment,
			)
		}
	}
	return nil
}

func configureStandaloneSavedTraceStore(
	ctx context.Context,
	mode string,
	databasePath string,
	filesystemPath string,
	ledgerPath string,
	usageStore ledger.Store,
) (savedtrace.Store, bool, error) {
	switch strings.ToLower(strings.TrimSpace(mode)) {
	case "", "disabled":
		if databasePath != "" || filesystemPath != "" {
			return nil, false, fmt.Errorf(
				"saved trace paths require -trace-storage database or filesystem",
			)
		}
		return nil, false, nil
	case "database":
		if filesystemPath != "" {
			return nil, false, fmt.Errorf(
				"-trace-filesystem is valid only with filesystem trace storage",
			)
		}
		path := databasePath
		if path == "" {
			path = ledgerPath
		}
		if path == "" {
			return nil, false, fmt.Errorf(
				"database trace storage requires -trace-database or -ledger-sqlite",
			)
		}
		if path == ledgerPath {
			store, ok := usageStore.(savedtrace.Store)
			if !ok {
				return nil, false, fmt.Errorf("SQLite usage store does not support saved traces")
			}
			return store, true, nil
		}
		store, err := ledgersqlite.Open(ctx, ledgersqlite.Options{Path: path})
		if err != nil {
			return nil, false, fmt.Errorf("open SQLite saved trace store: %w", err)
		}
		return store, false, nil
	case "filesystem":
		if databasePath != "" {
			return nil, false, fmt.Errorf(
				"-trace-database is valid only with database trace storage",
			)
		}
		store, err := tracefilesystem.Open(filesystemPath)
		if err != nil {
			return nil, false, err
		}
		return store, false, nil
	default:
		return nil, false, fmt.Errorf(
			"-trace-storage must be disabled, database, or filesystem",
		)
	}
}

func closeLedgerWriter(logger *slog.Logger, writer *ledger.AsyncWriter) error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	err := writer.Close(ctx)
	if err != nil {
		logger.Error("close usage ledger writer", slog.Any("err", err))
	}
	return err
}

func closeSavedTraceRecorder(
	logger *slog.Logger,
	recorder *savedtrace.AsyncRecorder,
) error {
	if recorder == nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	err := recorder.Close(ctx)
	stats := recorder.Stats()
	logger.Info(
		"saved trace recorder stopped",
		slog.Uint64("accepted", stats.Accepted),
		slog.Uint64("persisted", stats.Persisted),
		slog.Uint64("pending", stats.Pending),
		slog.Uint64("lost", stats.Lost),
		slog.Uint64("queue_full", stats.QueueFull),
		slog.Uint64("store_failures", stats.StoreFailures),
	)
	if err != nil {
		logger.Error("close saved trace recorder", slog.Any("err", err))
	}
	return err
}

func closePromptCache(logger *slog.Logger, directory *promptcache.Directory) error {
	if directory == nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	err := directory.Close(ctx)
	if err != nil {
		logger.Error("close prompt-cache affinity directory", slog.Any("err", err))
	}
	return err
}

func closeTelemetry(logger *slog.Logger, runtime *otlpexport.Runtime) error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	err := runtime.Shutdown(ctx)
	if err != nil {
		logger.Error("close OpenTelemetry providers", slog.Any("err", err))
	}
	return err
}

func env(name, fallback string) string {
	if value := os.Getenv(name); value != "" {
		return value
	}
	return fallback
}

func envBool(name string, fallback bool) (bool, error) {
	value := env(name, "")
	if value == "" {
		return fallback, nil
	}
	parsed, err := strconv.ParseBool(value)
	if err != nil {
		return false, fmt.Errorf("%s must be a boolean: %w", name, err)
	}
	return parsed, nil
}

func envInt(name string, fallback int) (int, error) {
	value := env(name, "")
	if value == "" {
		return fallback, nil
	}
	parsed, err := strconv.Atoi(value)
	if err != nil {
		return 0, fmt.Errorf("%s must be an integer: %w", name, err)
	}
	return parsed, nil
}

func envDuration(name string, fallback time.Duration) (time.Duration, error) {
	value := env(name, "")
	if value == "" {
		return fallback, nil
	}
	parsed, err := time.ParseDuration(value)
	if err != nil {
		return 0, fmt.Errorf("%s must be a duration: %w", name, err)
	}
	return parsed, nil
}

func resolveTraceWriteWorkers(storage string, configured, databaseWorkers int) int {
	if configured != 0 {
		return configured
	}
	if strings.EqualFold(strings.TrimSpace(storage), "filesystem") {
		return 1
	}
	return max(1, databaseWorkers)
}

func splitPathList(value string) []string {
	if value == "" {
		return nil
	}
	result := make([]string, 0)
	for _, item := range filepath.SplitList(value) {
		if item = strings.TrimSpace(item); item != "" {
			result = append(result, item)
		}
	}
	return result
}

func splitCommaList(value string) []string {
	if value == "" {
		return nil
	}
	result := make([]string, 0)
	for _, item := range strings.Split(value, ",") {
		if item = strings.TrimSpace(item); item != "" {
			result = append(result, item)
		}
	}
	return result
}
