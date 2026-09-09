// Package adminapi defines profile-neutral administration API contracts shared
// by the standalone and cluster gateway compositions.
package adminapi

const BootstrapSchemaVersion = 1

const (
	FeatureStatus             = "status"
	FeatureConfigRead         = "config_read"
	FeatureConfigValidate     = "config_validate"
	FeatureRoutingSimulation  = "config_routing_simulation"
	FeatureDiscoveredMetadata = "model_routing_discovered_metadata"
	FeatureConfigWrite        = "config_write"
	FeatureConfigHistory      = "config_history"
	FeatureConfigManagedSets  = "config_managed_sets"
	FeatureConfigPresets      = "config_presets"
	FeatureLedgerQuery        = "ledger_query"
	FeatureLedgerAggregate    = "ledger_aggregate"
	FeatureSavedTraceExport   = "saved_trace_export"
	FeatureLifecycle          = "lifecycle"
	FeatureEndpointInventory  = "endpoint_inventory"
	FeatureLifecycleStatus    = "lifecycle_status"
	FeatureRuntimeEvents      = "runtime_events"
	FeatureClientCredentials  = "client_credentials"
	FeatureCredentialsWrite   = "client_credentials_write"
	FeatureMMProjectionProbe  = "mm_projection_probe"
	FeaturePrivacyPII         = "privacy_pii"
	FeatureProviderAuth       = "provider_auth"
)

// Bootstrap is the authenticated description of the admin surface compiled
// into and enabled by one gateway process. The UI uses it for presentation;
// individual API handlers remain authoritative for authorization.
type Bootstrap struct {
	SchemaVersion  int               `json:"schema_version"`
	Edition        string            `json:"edition"`
	GatewayVersion string            `json:"gateway_version,omitempty"`
	Build          map[string]string `json:"build,omitempty"`
	ConfigRevision string            `json:"config_revision"`
	Features       map[string]bool   `json:"features"`
	Principal      *Principal        `json:"principal,omitempty"`
}

// Principal contains only the current administrator's presentation-safe
// identity and roles. It never contains credentials or attribution policy.
type Principal struct {
	ID     string   `json:"id"`
	Tenant string   `json:"tenant,omitempty"`
	Roles  []string `json:"roles"`
}
