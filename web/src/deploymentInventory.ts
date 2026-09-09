import type { GatewayStatus, LifecycleBindingStatus, LifecycleSnapshot, RuntimeEndpoint, TargetStatus } from "./types";

export type WorkloadState = "running" | "stopped" | "sleeping" | "starting" | "stopping" | "draining" | "failed" | "unknown" | "external";
export interface DeploymentEntry {
  id: string;
  title: string;
  target?: TargetStatus;
  binding?: LifecycleBindingStatus;
  endpoints: RuntimeEndpoint[];
  models: string[];
  clusters: string[];
  activatable: boolean;
  state: WorkloadState;
  requests: number;
  queued: number;
  controllerIssue?: string;
  needsAttention: boolean;
}

export function bindingState(binding: LifecycleBindingStatus): WorkloadState {
  // Controller phases carry sleep/wake information that the admission state
  // alone cannot express. Transitions take precedence over a retained phase.
  if (binding.state === "activating" || ["activating", "starting", "waking"].includes(binding.phase ?? "")) return "starting";
  if (binding.state === "deactivating" || ["deactivating", "stopping"].includes(binding.phase ?? "")) return "stopping";
  if (binding.state === "draining") return "draining";
  if (binding.state === "failed") return "failed";
  if (["sleeping", "asleep", "sleep"].includes(binding.phase ?? "")) return "sleeping";
  if (binding.phase === "unknown" || binding.phase === "failed") return binding.phase;
  if (binding.phase === "offline") return "stopped";
  if (binding.state === "ready") return "running";
  if (binding.state === "offline") return "stopped";
  return "unknown";
}

export function deploymentInventory(status?: GatewayStatus, lifecycle?: LifecycleSnapshot, endpoints: RuntimeEndpoint[] = []): DeploymentEntry[] {
  const entries = new Map<string, DeploymentEntry>();
  const get = (id: string) => {
    let row = entries.get(id);
    if (!row) {
      row = { id, title: id, endpoints: [], models: [], clusters: [], activatable: false, state: "unknown", requests: 0, queued: 0, needsAttention: false };
      entries.set(id, row);
    }
    return row;
  };
  for (const target of status?.targets ?? []) { get(target.deployment).target = target; }
  for (const binding of lifecycle?.bindings ?? []) { get(binding.deployment).binding = binding; }
  for (const endpoint of endpoints) {
    if (!endpoint.expired || entries.has(endpoint.target)) get(endpoint.target).endpoints.push(endpoint);
  }
  for (const row of entries.values()) {
    const { target, binding } = row;
    row.title = target?.title || row.id;
    row.activatable = Boolean(binding || target?.endpoint_source === "activatable");
    row.models = [...new Set([...(target?.model_names ?? []), target?.model, binding?.virtual_model, ...row.endpoints.flatMap(e => e.served_models)].filter((s): s is string => Boolean(s)))];
    row.clusters = [...new Set(binding?.cluster_candidates?.length ? binding.cluster_candidates : row.endpoints.map(e => e.cluster_id).filter((s): s is string => Boolean(s)))];
    const live = row.endpoints.filter(e => !e.expired);
    row.state = binding ? bindingState(binding) : row.activatable ? "unknown"
      : live.some(e => e.state === "ready" && Boolean(e.controller)) ? "running"
      : target?.endpoint_source === "discovered" || row.endpoints.some(e => Boolean(e.controller)) ? "unknown" : "external";
    // Target requests and endpoint requests measure the same traffic. Never
    // add them together, and keep activation leases separate in the detail.
    row.requests = target?.active_requests ?? live.reduce((n, e) => n + e.active_requests, 0);
    row.queued = binding?.queued_waiters ?? 0;
    const controller = lifecycle?.controllers.find(c => c.controller === (binding?.controller || target?.controller || live[0]?.controller));
    if (controller && controller.health !== "healthy") row.controllerIssue = `${controller.controller}: ${controller.health}${controller.reason ? ` · ${controller.reason.replaceAll("_", " ")}` : ""}`;
    row.needsAttention = Boolean(row.controllerIssue || ["failed", "unknown"].includes(row.state)
      || target && (["open", "half_open"].includes(target.circuit_state) || target.consecutive_failures > 0 || (target.recent_failures?.length ?? 0) > 0));
  }
  return [...entries.values()].sort((a, b) => a.title.localeCompare(b.title) || a.id.localeCompare(b.id));
}

export function routingLabel(row: DeploymentEntry): string {
  const target = row.target;
  if (target?.circuit_state === "open") return "Blocked";
  if (target?.circuit_state === "half_open") return "Recovery probe";
  if (target && target.max_concurrency > 0 && row.requests >= target.max_concurrency) return "At capacity";
  if (row.state === "sleeping") return "Wake to serve";
  if (row.state === "stopped") return target?.cold_start === "wait" ? "Starts on request" : "Start to serve";
  if (row.state === "starting") return "Waiting for readiness";
  if (["stopping", "draining", "failed"].includes(row.state)) return "Unavailable";
  if (row.state === "unknown" && row.activatable) return "Unknown";
  return target ? target.admission_available ? "Admitting" : "Unavailable" : "Not reported";
}
