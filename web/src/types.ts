export type AdminEdition = "standalone" | "cluster" | string;

export interface AdminPrincipal {
  id: string;
  tenant?: string;
  roles: string[];
}

export interface AdminBootstrap {
  schema_version: number;
  edition: AdminEdition;
  gateway_version?: string;
  build?: { name: string; version: string; commit: string; source: string; license: string };
  config_revision: string;
  features: Record<string, boolean>;
  principal?: AdminPrincipal;
}

export interface TargetStatus {
  deployment: string;
  title?: string;
  circuit_state: "closed" | "open" | "half_open" | string;
  admission_available: boolean;
  active_requests: number;
  max_concurrency: number;
  consecutive_failures: number;
  samples: number;
  failures: number;
  ejection_count: number;
  ejected_until?: string;
  half_open_probe_active: boolean;
  recent_failures?: TargetFailureStatus[];
}

export interface TargetFailureStatus {
  occurred_at: string;
  failure_class: string;
}

export type CredentialResolutionStatus =
  | "healthy"
  | "degraded"
  | "unavailable"
  | "unknown"
  | "unused"
  | string;

export interface CredentialSourceStatus {
  scheme: string;
  status: CredentialResolutionStatus;
  configured_references: number;
  resolved_references: number;
  failing_references: number;
  unresolved_references: number;
  resolution_attempts: number;
  resolution_failures: number;
  rotations: number;
  last_resolved_at?: string;
  last_failed_at?: string;
  last_rotated_at?: string;
}

export interface CredentialStatus {
  observed_at: string;
  status: CredentialResolutionStatus;
  configured_references: number;
  resolved_references: number;
  failing_references: number;
  unresolved_references: number;
  sources: CredentialSourceStatus[];
}

export interface MMProjectionStatus {
  configured: boolean;
  state: "disabled" | "unknown" | "healthy" | "unavailable" | "circuit_open" | string;
  provider?: string;
  analyzer_model?: string;
  projection_api?: number;
  models: number;
  consecutive_failures: number;
  circuit_open: boolean;
  circuit_open_until?: string;
  last_attempt_at?: string;
  last_success_at?: string;
  last_http_status?: number;
  last_latency_ms?: number;
  last_error?: string;
}

export interface PrivacyStatus {
  observed_at: string;
  state: "disabled" | "healthy" | "unavailable" | string;
  enabled: boolean;
  configured_models: number;
  conversation_models: number;
  media_required_models: number;
  file_inspection_models: number;
  detector_entities: string[];
  backend: "disabled" | "none" | "sqlite" | "postgres" | "unavailable" | string;
  live_mappings: number;
  live_conversations: number;
  live_original_bytes: number;
  expired_pending_prune: number;
  maximum_mappings: number;
  maximum_original_bytes: number;
  maximum_conversations: number;
  sliding_ttl_seconds: number;
  absolute_ttl_seconds: number;
  inspections: number;
  failures: number;
}

export interface GatewayStatus {
  config_revision: string;
  providers: number;
  deployments: number;
  virtual_models: number;
  served_model_names?: string[];
  targets: TargetStatus[];
  credentials: CredentialStatus;
  mm_projection: MMProjectionStatus;
  privacy?: PrivacyStatus;
}

export type ConfigurationDocument = Record<string, unknown>;

export interface ActiveConfiguration {
  revision: string;
  document: ConfigurationDocument;
}

export interface ConfigurationValidation {
  valid: true;
  revision: string;
}

export interface RoutingCandidateTrace {
  model: string;
  eligible: boolean;
  reason?: string;
  score?: number;
  scores?: Record<string, number>;
}

export interface RoutingDecision {
  requested_model: string;
  resolved_model: string;
  virtual_model?: string;
  virtual_alias?: string;
  routing_preset?: string;
  router_kwargs?: Record<string, unknown>;
  provider_priority?: string[];
  strategy: string;
  matched_rule?: string;
  reason: string;
  revision: number;
  candidates: RoutingCandidateTrace[];
  at: string;
}

export interface RoutingSimulationResult {
  decision: RoutingDecision;
  available_models: string[];
  required_capabilities?: string[];
  state_mode: "stateless" | string;
}

export interface DiscoveredModelMetadata {
  size_b?: number;
  input_price?: number;
  output_price?: number;
  context?: number;
  tags?: string[];
}

export interface DiscoveredMetadataSnapshot {
  source: string;
  observed_at: string;
  models: Record<string, DiscoveredModelMetadata>;
}

export interface DiscoveredMetadataState {
  generation: number;
  sources: DiscoveredMetadataSnapshot[];
  effective: Record<string, DiscoveredModelMetadata>;
}


export type ManagedConfigurationOwner = "operator" | "sparkrun";

export interface ManagedConfigurationSetMetadata {
  owner: ManagedConfigurationOwner;
  revision: string;
  updated_at: string;
  updated_by: string;
  reason?: string;
}

export interface ManagedConfigurationSet extends ManagedConfigurationSetMetadata {
  document: ConfigurationDocument;
}

export interface ManagedConfigurationValidation {
  owner: ManagedConfigurationOwner;
  set_revision: string;
  active_revision: string;
  candidate_revision: string;
  valid: true;
}

export interface ManagedConfigurationCurrent {
  revision: string;
  updated_at: string;
  updated_by: string;
  reason?: string;
}

export interface ManagedConfigurationReplaceResult {
  managed_set: ManagedConfigurationSetMetadata;
  current: ManagedConfigurationCurrent;
  changed: boolean;
}


export interface TokenUsage {
  input_tokens?: number;
  output_tokens?: number;
  total_tokens?: number;
  cached_input_tokens?: number;
  cache_creation_tokens?: number;
  reasoning_tokens?: number;
  tool_use_prompt_tokens?: number;
  accepted_prediction_tokens?: number;
  rejected_prediction_tokens?: number;
  provider_components?: Record<string, number>;
  completeness: "missing" | "partial" | "complete" | string;
  normalization_version?: string;
}

export interface RequestRecord {
  request_id: string;
  started_at: string;
  completed_at: string;
  principal_id?: string;
  principal_type?: string;
  principal_subject?: string;
  principal_roles?: string[];
  tenant_id?: string;
  attribution?: Record<string, string>;
  protocol: string;
  operation: string;
  stream: boolean;
  requested_model?: string;
  virtual_model?: string;
  response_presented_model?: string;
  config_revision?: string;
  final_provider?: string;
  final_deployment?: string;
  final_upstream_model?: string;
  attempt_count: number;
  http_status?: number;
  outcome: string;
  failure_class?: string;
  latency: number;
  time_to_first_byte?: number;
  usage: TokenUsage;
}

export interface AttemptRecord {
  request_id: string;
  attempt: number;
  started_at: string;
  first_byte_at?: string;
  completed_at: string;
  provider: string;
  deployment: string;
  upstream_model: string;
  pool_priority: number;
  http_status?: number;
  upstream_request_id?: string;
  outcome: string;
  failure_class?: string;
  retried: boolean;
  latency: number;
  usage: TokenUsage;
}

export interface RequestPage {
  records: RequestRecord[];
  next_cursor?: string;
}

export interface AttemptPage {
  records: AttemptRecord[];
  next_cursor?: string;
}

export interface RequestFilters {
  started_at_or_after?: string;
  started_at_before?: string;
  tenant_id?: string;
  virtual_model?: string;
  outcome?: string;
  limit?: number;
  cursor?: string;
}

export interface AttemptFilters {
  started_at_or_after?: string;
  started_at_before?: string;
  request_id?: string;
  provider?: string;
  deployment?: string;
  outcome?: string;
  limit?: number;
  cursor?: string;
}

export type RuntimeState =
  | "offline"
  | "activating"
  | "ready"
  | "draining"
  | "deactivating"
  | "failed"
  | "unknown"
  | string;

export interface RuntimeEndpoint {
  endpoint_id: string;
  target: string;
  protocol?: string;
  served_models: string[];
  controller?: string;
  cluster_id?: string;
  job_id?: string;
  binding_revision?: string;
  recipe_revision?: string;
  state: RuntimeState;
  active_requests: number;
  max_concurrency: number;
  registered_at?: string;
  heartbeat_at?: string;
  expires_at?: string;
  expired: boolean;
}

export interface RuntimeEndpointFilters {
  target?: string;
  controller?: string;
  state?: string;
  limit?: number;
  cursor?: string;
}

export interface RuntimeEndpointPage {
  endpoints: RuntimeEndpoint[];
  next_cursor?: string;
}

export interface LifecycleControllerStatus {
  controller: string;
  health: "healthy" | "degraded" | "unavailable" | "unknown" | string;
  updated_at: string;
  bindings: number;
  endpoints: number;
  ready_endpoints: number;
  active_leases: number;
  queued_waiters: number;
  queued_body_bytes: number;
  reason?: string;
}

export interface LifecycleBindingStatus {
  controller: string;
  binding_revision: string;
  virtual_model?: string;
  deployment: string;
  state: RuntimeState;
  updated_at: string;
  activation_started_at?: string;
  activation_deadline?: string;
  endpoint_id?: string;
  active_leases: number;
  queued_waiters: number;
  queued_body_bytes: number;
  max_queued_waiters: number;
  max_queued_body_bytes: number;
  reason?: string;
}

export interface LifecycleSnapshot {
  observed_at: string;
  controllers: LifecycleControllerStatus[];
  bindings: LifecycleBindingStatus[];
}

export interface RuntimeEventRecord {
  event_id: string;
  occurred_at: string;
  controller: string;
  binding_revision?: string;
  virtual_model?: string;
  deployment?: string;
  endpoint_instance?: string;
  cluster_id?: string;
  job_id?: string;
  recipe_revision?: string;
  prior_state?: string;
  new_state: string;
  reason?: string;
  latency?: number;
  queue_depth: number;
  waiter_count: number;
  outcome: string;
}

export interface RuntimeEventFilters {
  occurred_at_or_after?: string;
  occurred_at_before?: string;
  controller?: string;
  virtual_model?: string;
  deployment?: string;
  outcome?: string;
  limit?: number;
  cursor?: string;
}

export interface RuntimeEventPage {
  records: RuntimeEventRecord[];
  next_cursor?: string;
}

export interface LedgerAggregateFilters {
	started_at_or_after: string;
	started_at_before: string;
	tenant_id?: string;
	virtual_model?: string;
	provider?: string;
	deployment?: string;
	outcome?: string;
}

export interface LedgerAggregateBreakdown {
	groups: Array<{ value: string; count: number }>;
	truncated: boolean;
}

export interface LedgerAggregateSummary {
	started_at_or_after: string;
	started_at_before: string;
	requests: number;
	successful_requests: number;
	streaming_requests: number;
	attempts: number;
	retried_requests: number;
	retries: number;
	latency: {
		average: number;
		p50: number;
		p95: number;
		p99: number;
	};
	tokens: {
		input_tokens: number;
		output_tokens: number;
		total_tokens: number;
		cached_input_tokens: number;
		cache_creation_tokens: number;
		reasoning_tokens: number;
		tool_use_prompt_tokens: number;
		accepted_prediction_tokens: number;
		rejected_prediction_tokens: number;
	};
	usage_completeness: {
		missing: number;
		partial: number;
		complete: number;
	};
	by_outcome: LedgerAggregateBreakdown;
	by_virtual_model: LedgerAggregateBreakdown;
	by_provider: LedgerAggregateBreakdown;
	by_deployment: LedgerAggregateBreakdown;
}

export interface SavedTraceExportFilters {
  started_at_or_after?: string;
  started_at_before?: string;
  tenant_id?: string;
  conversation_id?: string;
  response_id?: string;
  parent_response_id?: string;
  session_id?: string;
  protocol?: string;
  operation?: string;
  requested_model?: string;
  virtual_model?: string;
  provider?: string;
  deployment?: string;
  outcome?: string;
  capture_outcome?: string;
  metadata?: string[];
  max_records?: number;
  format?: "jsonl" | "dataset";
  projection?: "canonical" | "deepspec" | "mlflow";
  require_complete?: boolean;
}

export type ClientCredentialState = "active" | "disabled" | "revoked";

export interface ClientCredential {
  id: string;
  name: string;
  tenant_id?: string;
  principal_id: string;
  principal_type?: string;
  principal_subject?: string;
  roles: string[];
  allowed_attribution?: string[];
  fixed_attribution?: Record<string, string>;
  state: ClientCredentialState;
  created_at: string;
  created_by: string;
  updated_at: string;
  updated_by: string;
  rotated_at?: string;
  expires_at?: string;
  revoked_at?: string;
}

export interface ClientCredentialCreateInput {
  name: string;
  tenant_id?: string;
  principal_id: string;
  principal_type?: string;
  principal_subject?: string;
  roles: string[];
  allowed_attribution?: string[];
  fixed_attribution?: Record<string, string>;
  expires_at?: string;
}

export interface IssuedClientCredential {
  credential: ClientCredential;
  api_key: string;
}

export interface ClientCredentialFilters {
  tenant_id?: string;
  principal_id?: string;
  state?: ClientCredentialState | "";
  limit?: number;
}

export interface ClientCredentialAuditEvent {
  id: number;
  credential_id: string;
  tenant_id?: string;
  action: string;
  occurred_at: string;
  actor: string;
  details?: Record<string, unknown>;
}

export interface AdminAppOptions {
  productName: string;
  extensions?: import("./extensions").AdminConsoleExtensions;
}
