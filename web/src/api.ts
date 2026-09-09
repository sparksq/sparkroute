// SPDX-FileCopyrightText: 2026 Scitrera LLC
// SPDX-FileCopyrightText: 2026 Fox Engine Ltd.
// SPDX-License-Identifier: AGPL-3.0-only

import type {
  ActiveConfiguration,
  AdminBootstrap,
  ConfigurationDocument,
  ConfigurationPresets,
  ConfigurationValidation,
  DiscoveredMetadataState,
  RoutingSimulationResult,
  RoutingStageScenario,
  AttemptFilters,
  AttemptPage,
	GatewayStatus,
	MMProjectionStatus,
	LedgerAggregateFilters,
	LedgerAggregateSummary,
  RequestFilters,
  RequestPage,
  RequestRecord,
  LifecycleSnapshot,
  RuntimeEndpointFilters,
  RuntimeEndpointPage,
  RuntimeEventFilters,
  RuntimeEventPage,
  SavedTraceExportFilters,
  ClientCredential,
  ClientCredentialAuditEvent,
  ClientCredentialCreateInput,
  ClientCredentialFilters,
  IssuedClientCredential,
  ManagedConfigurationOwner,
  ManagedConfigurationReplaceResult,
  ManagedConfigurationSet,
  ManagedConfigurationSetMetadata,
  ManagedConfigurationValidation,
} from "./types";

export class AdminAPIError extends Error {
  readonly status: number;

  constructor(status: number, message: string) {
    super(message);
    this.name = "AdminAPIError";
    this.status = status;
  }
}

async function requestJSON<T>(
  path: string,
  token: string,
  options: { method?: string; body?: unknown } = {},
  signal?: AbortSignal,
): Promise<T> {
  const headers = new Headers({ Accept: "application/json" });
  if (token) headers.set("Authorization", `Bearer ${token}`);
  if (options.body !== undefined) headers.set("Content-Type", "application/json");
  const response = await fetch(path, {
    body: options.body === undefined ? undefined : JSON.stringify(options.body),
    credentials: "same-origin",
    headers,
    method: options.method ?? "GET",
    signal,
  });
  if (!response.ok) {
    throw await responseError(response);
  }
  return (await response.json()) as T;
}

async function responseError(response: Response) {
  let message = `Admin request failed with status ${response.status}`;
  try {
    const body = (await response.json()) as {
      error?: { message?: string };
    };
    if (body.error?.message) message = body.error.message;
  } catch {
    // Keep the bounded status-only fallback; do not reflect arbitrary HTML.
  }
  return new AdminAPIError(response.status, message);
}

function queryPath<T extends object>(path: string, values: T) {
  const query = new URLSearchParams();
  Object.entries(values).forEach(([name, value]) => {
    if (value === undefined || value === null || value === "") return;
    if (Array.isArray(value)) {
      value.forEach((current) => query.append(name, String(current)));
      return;
    }
    query.set(name, String(value));
  });
  const encoded = query.toString();
  return encoded ? `${path}?${encoded}` : path;
}

export function fetchBootstrap(token: string, signal?: AbortSignal) {
  return requestJSON<AdminBootstrap>("/v1/ui/bootstrap", token, {}, signal);
}

export function fetchStatus(token: string, signal?: AbortSignal) {
  return requestJSON<GatewayStatus>("/v1/status", token, {}, signal);
}

export function probeMMProjection(token: string) {
  return requestJSON<MMProjectionStatus>(
    "/v1/mm-projection/probe",
    token,
    { method: "POST" },
  );
}

export function fetchActiveConfiguration(token: string, signal?: AbortSignal) {
  return requestJSON<ActiveConfiguration>("/v1/config", token, {}, signal);
}

export interface ProviderSignInStatus {
  profile: string;
  state: "signed_out" | "starting" | "pending" | "connected" | "failed" | "expired";
  email?: string;
  plan?: string;
  account_id?: string;
  expires_at?: string;
  verification_url?: string;
  user_code?: string;
  login_expires_at?: string;
  message?: string;
}

export function fetchProviderSignIn(token: string, profile: string, signal?: AbortSignal) {
  return requestJSON<ProviderSignInStatus>(`/v1/provider-auth/openai/${encodeURIComponent(profile)}`, token, {}, signal);
}

export function changeProviderSignIn(token: string, profile: string, action: "login" | "cancel" | "logout") {
  return requestJSON<ProviderSignInStatus>(`/v1/provider-auth/openai/${encodeURIComponent(profile)}/${action}`, token, { method: "POST", body: {} });
}

export function fetchDiscoveredModelMetadata(token: string, signal?: AbortSignal) {
  return requestJSON<DiscoveredMetadataState>(
    "/v1/model-routing/discovered-metadata",
    token,
    {},
    signal,
  );
}

export function validateConfiguration(
  token: string,
  document: ConfigurationDocument,
) {
  return requestJSON<ConfigurationValidation>(
    "/v1/config/validate",
    token,
    { method: "POST", body: { document } },
  );
}

export function simulateModelRouting(
  token: string,
  document: ConfigurationDocument,
  requestedModel: string,
  routingText: string,
  requiredCapabilities: string[],
  stageScenario?: RoutingStageScenario,
) {
  return requestJSON<RoutingSimulationResult>(
    "/v1/config/simulate-routing",
    token,
    {
      method: "POST",
      body: {
        document,
        requested_model: requestedModel,
        routing_text: routingText,
        required_capabilities: requiredCapabilities,
        stage_scenario: stageScenario,
      },
    },
  );
}

export function fetchManagedConfigurationSets(token: string) {
  return requestJSON<{
    active_revision: string;
    presets_revision?: number;
    active_preset?: string;
    managed_sets: ManagedConfigurationSetMetadata[];
  }>("/v1/config/managed-sets", token);
}

export function fetchManagedConfigurationSet(
  token: string,
  owner: ManagedConfigurationOwner,
) {
  return requestJSON<ManagedConfigurationSet>(
    `/v1/config/managed-sets/${owner}`,
    token,
  );
}

export function validateManagedConfigurationSet(
  token: string,
  owner: ManagedConfigurationOwner,
  document: ConfigurationDocument,
  expectedActiveRevision: string,
) {
  return requestJSON<ManagedConfigurationValidation>(
    `/v1/config/managed-sets/${owner}/validate`,
    token,
    {
      method: "POST",
      body: {
        document,
        expected_active_revision: expectedActiveRevision,
      },
    },
  );
}

export function simulateManagedModelRouting(
  token: string,
  owner: ManagedConfigurationOwner,
  document: ConfigurationDocument,
  expectedActiveRevision: string,
  requestedModel: string,
  routingText: string,
  requiredCapabilities: string[],
  stageScenario?: RoutingStageScenario,
) {
  return requestJSON<RoutingSimulationResult>(
    `/v1/config/managed-sets/${owner}/simulate-routing`,
    token,
    {
      method: "POST",
      body: {
        document,
        expected_active_revision: expectedActiveRevision,
        requested_model: requestedModel,
        routing_text: routingText,
        required_capabilities: requiredCapabilities,
        stage_scenario: stageScenario,
      },
    },
  );
}

export function replaceManagedConfigurationSet(
  token: string,
  owner: ManagedConfigurationOwner,
  document: ConfigurationDocument,
  expectedActiveRevision: string,
  reason: string,
  expectedPresetsRevision?: number,
) {
  return requestJSON<ManagedConfigurationReplaceResult>(
    `/v1/config/managed-sets/${owner}`,
    token,
    {
      method: "PUT",
      body: {
        document,
        expected_active_revision: expectedActiveRevision,
        reason,
        expected_presets_revision: expectedPresetsRevision,
      },
    },
  );
}

export function fetchConfigurationPresets(token: string) {
  return requestJSON<ConfigurationPresets>("/v1/config/presets", token);
}

export function mutateConfigurationPreset(
  token: string,
  operation: "save" | "activate" | "rename" | "delete",
  catalog: ConfigurationPresets,
  input: { id?: string; name?: string },
) {
  return requestJSON<unknown>(`/v1/config/presets/${operation}`, token, {
    method: "POST",
    body: { ...input, expected_active_revision: catalog.active_revision, expected_presets_revision: catalog.presets_revision },
  });
}

export function fetchRequests(
  token: string,
  filters: RequestFilters,
  signal?: AbortSignal,
) {
  return requestJSON<RequestPage>(
    queryPath("/v1/ledger/requests", filters),
    token,
    {},
    signal,
  );
}

export function fetchRequest(token: string, requestID: string, signal?: AbortSignal) {
  return requestJSON<RequestRecord>(
    `/v1/ledger/requests/${encodeURIComponent(requestID)}`,
    token,
    {},
    signal,
  );
}

export function fetchAttempts(
  token: string,
  filters: AttemptFilters,
  signal?: AbortSignal,
) {
  return requestJSON<AttemptPage>(
    queryPath("/v1/ledger/attempts", filters),
    token,
    {},
    signal,
  );
}

export function fetchLedgerSummary(
	token: string,
	filters: LedgerAggregateFilters,
	signal?: AbortSignal,
) {
	return requestJSON<LedgerAggregateSummary>(
		queryPath("/v1/ledger/summary", filters),
		token,
		{},
		signal,
	);
}

export function fetchRuntimeEndpoints(
  token: string,
  filters: RuntimeEndpointFilters,
  signal?: AbortSignal,
) {
  return requestJSON<RuntimeEndpointPage>(
    queryPath("/v1/runtime/endpoints", filters),
    token,
    {},
    signal,
  );
}

export function fetchLifecycleStatus(token: string, signal?: AbortSignal) {
  return requestJSON<LifecycleSnapshot>("/v1/runtime/status", token, {}, signal);
}

export function fetchRuntimeEvents(
  token: string,
  filters: RuntimeEventFilters,
  signal?: AbortSignal,
) {
  return requestJSON<RuntimeEventPage>(
    queryPath("/v1/runtime/events", filters),
    token,
    {},
    signal,
  );
}

export async function fetchSavedTraceExport(
  token: string,
  filters: SavedTraceExportFilters,
) {
  const dataset = filters.format === "dataset";
  const expectedContentType = dataset ? "application/zip" : "application/x-ndjson";
  const headers = new Headers({ Accept: expectedContentType });
  if (token) headers.set("Authorization", `Bearer ${token}`);
  const response = await fetch(
    queryPath("/v1/saved-traces/export", filters),
    { credentials: "same-origin", headers },
  );
  if (!response.ok) throw await responseError(response);
  const contentType = response.headers.get("Content-Type")?.split(";", 1)[0];
  if (contentType !== expectedContentType) {
    throw new AdminAPIError(response.status, "Trace export returned an unexpected content type");
  }
  const blob = await response.blob();
  if (response.headers.get("X-SparkRoute-Export-Error") === "incomplete") {
    throw new AdminAPIError(response.status, "Trace export was incomplete; discard the partial download");
  }
  return blob;
}

export function fetchClientCredentials(
  token: string,
  filters: ClientCredentialFilters,
  signal?: AbortSignal,
) {
  return requestJSON<{ credentials: ClientCredential[] }>(
    queryPath("/v1/client-credentials", filters),
    token,
    {},
    signal,
  );
}

export function createClientCredential(
  token: string,
  input: ClientCredentialCreateInput,
) {
  return requestJSON<IssuedClientCredential>(
    "/v1/client-credentials",
    token,
    { method: "POST", body: input },
  );
}

export function mutateClientCredential(
  token: string,
  id: string,
  action: "rotate" | "enable" | "disable" | "revoke",
) {
  return requestJSON<IssuedClientCredential | { credential: ClientCredential }>(
    `/v1/client-credentials/${encodeURIComponent(id)}/${action}`,
    token,
    { method: "POST" },
  );
}

export function fetchClientCredentialAudit(
  token: string,
  filters: { tenant_id?: string; credential_id?: string; action?: string; limit?: number },
  signal?: AbortSignal,
) {
  return requestJSON<{ events: ClientCredentialAuditEvent[] }>(
    queryPath("/v1/client-credentials/audit", filters),
    token,
    {},
    signal,
  );
}

export interface SparkrunRecipe {
  tp?: number | null; pp?: number | null; quantization?: string | null; context_length?: number | null; parameters_b?: number | null;
  reference: string;
  name: string;
  model: string;
  runtime: string;
  description: string;
  registry?: string | null;
  source_path: string;
  min_nodes: number;
}
export interface SparkrunRecipeDetails extends SparkrunRecipe {
  recipe_revision: string;
  native_api_options?: string[];
  native_protocols: string[];
  capabilities: string[];
  sparkroute?: { capabilities?: string[]; request_profiles?: Record<string, Record<string, Record<string, unknown>>> };
  required_plugins: string[];
  trusted: boolean;
  metadata?: {benchmarks?: Record<string, unknown>[]};
  hf_model?: string; defaults: Record<string, unknown>;
  issues: { severity: string; code: string; message: string }[];
}
export interface SparkrunOperation {
  operation_id: string;
  state: "running" | "succeeded" | "failed";
  phase: string;
  updated_at: number;
  result?: { updated?: Record<string, boolean>; failed?: string[] };
  error?: { code: string; message: string; retryable: boolean };
}
export function sparkRunCatalog<T>(token: string, operation: string, args: Record<string, unknown> = {}, signal?: AbortSignal) {
  return requestJSON<T>("/v1/sparkrun/catalog", token, { method: "POST", body: { operation, arguments: args } }, signal);
}
export function prepareSparkrunRecipe(token: string, document: ConfigurationDocument, expectedActiveRevision: string, recipe: Record<string, unknown>) {
  return requestJSON<{ document: ConfigurationDocument; deployment: string; reused: boolean }>("/v1/sparkrun/recipe-draft", token, {
    method: "POST", body: { document, expected_active_revision: expectedActiveRevision, recipe },
  });
}

export function controlSparkrunWorkload(token: string, deployment: string, job_id: string, action: string) {
 return requestJSON<{state: string}>("/v1/sparkrun/workload", token, {method:"POST", body:{deployment, job_id, action}});
}
