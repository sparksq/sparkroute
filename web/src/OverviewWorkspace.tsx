// SPDX-FileCopyrightText: 2026 Scitrera LLC
// SPDX-FileCopyrightText: 2026 Fox Engine Ltd.
// SPDX-License-Identifier: AGPL-3.0-only

import { type ReactNode, useCallback, useMemo, useState } from "react";
import { fetchLifecycleStatus, fetchRuntimeEndpoints, fetchRuntimeEvents, fetchStatus } from "./api";
import { deploymentInventory, routingLabel, type DeploymentEntry } from "./deploymentInventory";
import { ControllerInventory, Empty, EndpointInventory, TransitionHistory, formatBytes, formatTime, humanize } from "./RuntimeWorkspace";
import { TargetFact, TargetHealthDetail } from "./TargetHealthDetail";
import { WorkloadControls, useWorkloadControls } from "./WorkloadControls";
import { useLiveResource } from "./useLiveResource";
import type { AdminBootstrap, GatewayStatus, RuntimeEndpoint, RuntimeEndpointFilters, RuntimeEventFilters } from "./types";

type View = "deployments" | "activity" | "diagnostics";
type Focus = "all" | "running" | "inactive" | "attention" | "requests" | "queued";
const stateLabels: Record<string, string> = { running: "Running", stopped: "Stopped", sleeping: "Sleeping", starting: "Starting", stopping: "Stopping", draining: "Draining", failed: "Failed", unknown: "Unknown", external: "Externally managed" };

async function allEndpoints(token: string, signal: AbortSignal) {
  const endpoints = new Map<string, RuntimeEndpoint>();
  const cursors = new Set<string>();
  let cursor: string | undefined;
  do {
    signal.throwIfAborted();
    const page = await fetchRuntimeEndpoints(token, { limit: 100, cursor }, signal);
    for (const endpoint of page.endpoints) endpoints.set(endpoint.endpoint_id, endpoint);
    cursor = page.next_cursor;
    if (cursor && cursors.has(cursor)) throw new Error("Endpoint inventory returned a repeated page. Refresh to try again.");
    if (cursor) cursors.add(cursor);
  } while (cursor);
  return [...endpoints.values()];
}

export function OverviewWorkspace({ bootstrap, token, initialStatus, refreshKey = 0 }: {
  bootstrap: AdminBootstrap; token: string; initialStatus?: GatewayStatus; refreshKey?: number;
}) {
  const features = bootstrap.features;
  const [view, setView] = useState<View>("deployments");
  const [focus, setFocus] = useState<Focus>("all");
  const [query, setQuery] = useState("");
  const [lifecycleFilter, setLifecycleFilter] = useState("all");
  const [modeFilter, setModeFilter] = useState("all");
  const [availabilityFilter, setAvailabilityFilter] = useState("all");
  const [circuitFilter, setCircuitFilter] = useState("all");
  const [failureFilter, setFailureFilter] = useState("all");
  const [selected, setSelected] = useState("");
  const [eventFilters, setEventFilters] = useState<RuntimeEventFilters>({ limit: 100 });
  const [endpointFilters, setEndpointFilters] = useState<RuntimeEndpointFilters>({ limit: 100 });
  const statusLoader = useCallback((signal: AbortSignal) => fetchStatus(token, signal), [token]);
  const lifecycleLoader = useCallback((signal: AbortSignal) => fetchLifecycleStatus(token, signal), [token]);
  const endpointLoader = useCallback((signal: AbortSignal) => allEndpoints(token, signal), [token]);
  const status = useLiveResource(Boolean(features.status), statusLoader, initialStatus, refreshKey);
  const lifecycle = useLiveResource(Boolean(features.lifecycle_status), lifecycleLoader, undefined, refreshKey);
  const endpoints = useLiveResource(Boolean(features.endpoint_inventory), endpointLoader, undefined, refreshKey);
  const refresh = useCallback(() => Promise.all([status.refresh(), lifecycle.refresh(), endpoints.refresh()]), [status.refresh, lifecycle.refresh, endpoints.refresh]);
  const controls = useWorkloadControls(token, refresh);
  const rows = useMemo(() => deploymentInventory(status.data, lifecycle.data, endpoints.data), [status.data, lifecycle.data, endpoints.data]);
  const matchesFocus = (row: DeploymentEntry, value: Focus) => value === "all" || (value === "running" ? row.state === "running"
    : value === "inactive" ? row.activatable && ["stopped", "sleeping"].includes(row.state)
    : value === "attention" ? row.needsAttention : value === "requests" ? row.requests > 0 : row.queued > 0);
  const filtered = rows.filter(row => {
    if (!matchesFocus(row, focus)) return false;
    if (query.trim() && ![row.id, row.title, row.target?.provider, ...row.models, ...row.clusters].join(" ").toLowerCase().includes(query.trim().toLowerCase())) return false;
    if (lifecycleFilter !== "all" && row.state !== lifecycleFilter) return false;
    if (modeFilter === "activatable" && !row.activatable || modeFilter === "external" && row.activatable) return false;
    if (availabilityFilter === "admitting" && routingLabel(row) !== "Admitting" || availabilityFilter === "unavailable" && routingLabel(row) === "Admitting") return false;
    if (circuitFilter !== "all" && row.target?.circuit_state !== circuitFilter) return false;
    const failures = (row.target?.recent_failures?.length ?? 0) > 0;
    return !(failureFilter === "recent" && !failures || failureFilter === "none" && failures);
  });
  const metrics: { id: Focus; label: string; value: number | string }[] = [
    { id: "running", label: "Running workloads", value: rows.filter(row => matchesFocus(row, "running")).length },
    { id: "inactive", label: "Inactive activatable", value: rows.filter(row => matchesFocus(row, "inactive")).length },
    { id: "attention", label: "Needs attention", value: rows.filter(row => row.needsAttention).length },
    { id: "requests", label: "Requests in flight", value: features.status || features.endpoint_inventory ? rows.reduce((n, row) => n + row.requests, 0) : "—" },
    { id: "queued", label: "Queued requests", value: features.lifecycle_status ? rows.reduce((n, row) => n + row.queued, 0) : "—" },
  ];
  const controllersWithIssues = lifecycle.data?.controllers.filter(c => c.health !== "healthy") ?? [];
  const selectedRow = filtered.find(row => row.id === selected);
  const showActivity = (deployment: string) => { setEventFilters({ deployment, limit: 100 }); setView("activity"); };
  const clearFilters = () => { setFocus("all"); setQuery(""); setLifecycleFilter("all"); setModeFilter("all"); setAvailabilityFilter("all"); setCircuitFilter("all"); setFailureFilter("all"); };
  const endpointMatches = (endpoints.data ?? []).filter(e => (!endpointFilters.target || e.target === endpointFilters.target) && (!endpointFilters.controller || e.controller === endpointFilters.controller) && (!endpointFilters.state || e.state === endpointFilters.state));
  const endpointOffset = Number(endpointFilters.cursor ?? 0);
  const hasRuntime = features.lifecycle_status || features.endpoint_inventory || features.runtime_events;
  const availableViews: { id: View; label: string }[] = [
    { id: "deployments", label: "Deployments" },
    ...(features.runtime_events ? [{ id: "activity" as const, label: "Activity" }] : []),
    ...(features.lifecycle_status || features.endpoint_inventory ? [{ id: "diagnostics" as const, label: "Diagnostics" }] : []),
  ];
  const hasData = status.data || lifecycle.data || endpoints.data;
  const loading = status.loading || lifecycle.loading || endpoints.loading;

  return <div className="runtime-workspace overview-workspace">
    {hasRuntime ? <div className="workspace-tabs" role="tablist" aria-label="Overview views">
      {availableViews.map((item, index) =>
        <button key={item.id} type="button" role="tab" tabIndex={view === item.id ? 0 : -1} aria-selected={view === item.id} aria-controls={`overview-${item.id}`} id={`overview-tab-${item.id}`} className={view === item.id ? "active" : ""} onClick={() => setView(item.id)}
          onKeyDown={event => {
            const next = event.key === "ArrowRight" ? (index + 1) % availableViews.length : event.key === "ArrowLeft" ? (index - 1 + availableViews.length) % availableViews.length : event.key === "Home" ? 0 : event.key === "End" ? availableViews.length - 1 : undefined;
            if (next === undefined) return;
            event.preventDefault();
            const id = availableViews[next]!.id;
            setView(id);
            document.getElementById(`overview-tab-${id}`)?.focus();
          }}>{item.label}</button>)}
    </div> : null}
    {[{ name: "Routing health", resource: status }, { name: "Lifecycle status", resource: lifecycle }, { name: "Endpoint inventory", resource: endpoints }].filter(item => item.resource.error).map(item =>
      <div className="notice error overview-source-error" role="alert" key={item.name}>
        <span>{item.name} {item.resource.data ? "is stale; showing the last successful snapshot" : "is unavailable"}. {item.resource.error}</span>
        <button type="button" className="text-button" onClick={() => void item.resource.refresh()}>Retry {item.name.toLowerCase()}</button>
      </div>)}
    {controllersWithIssues.length ? <div className="notice warning overview-controller-notice">
      <span>{controllersWithIssues.map(c => `${c.controller}: ${humanize(c.health)}${c.reason ? ` · ${c.reason.replaceAll("_", " ")}` : ""}`).join("; ")}</span>
      <button type="button" className="text-button" onClick={() => setView("diagnostics")}>View diagnostics</button>
    </div> : null}
    {view === "deployments" ? <div id="overview-deployments" role={hasRuntime ? "tabpanel" : undefined} aria-labelledby={hasRuntime ? "overview-tab-deployments" : undefined} className="overview-deployments">
      <section className="metric-grid overview-metrics" aria-label="Deployment activity">
        {metrics.map(metric => <button type="button" key={metric.id} className={`metric${focus === metric.id ? " accent" : ""}`} aria-pressed={focus === metric.id} aria-label={`${metric.label} ${!hasData && loading ? "—" : metric.value}`}
          onClick={() => { clearFilters(); setFocus(focus === metric.id ? "all" : metric.id); }}><span>{metric.label}</span><strong>{!hasData && loading ? "—" : metric.value}</strong></button>)}
      </section>
      <section className="panel target-panel">
        <div className="panel-heading"><div><h2>Deployments &amp; workloads</h2>
          <p className="overview-inventory-note">{rows.length} deployments{status.data ? ` · ${status.data.virtual_models} virtual models · ${status.data.providers} providers` : ""}</p></div>
          <span className="activity-summary">{loading ? "Refreshing…" : "Updates every 5s"}</span></div>
        <div className="target-filters overview-filters">
          <label className="wide"><span>Filter deployments</span><input placeholder="Model, deployment, provider, or cluster" value={query} onChange={e => setQuery(e.target.value)} /></label>
          <label><span>Lifecycle</span><select value={lifecycleFilter} onChange={e => setLifecycleFilter(e.target.value)}><option value="all">All states</option>{Object.entries(stateLabels).map(([value, label]) => <option key={value} value={value}>{label}</option>)}</select></label>
          <label><span>Management</span><select value={modeFilter} onChange={e => setModeFilter(e.target.value)}><option value="all">All deployments</option><option value="activatable">Activatable</option><option value="external">Externally managed</option></select></label>
          <label><span>Circuit state</span><select value={circuitFilter} onChange={e => setCircuitFilter(e.target.value)}><option value="all">All circuits</option><option value="closed">Closed</option><option value="open">Open</option><option value="half_open">Half open</option></select></label>
        </div>
        <div className="overview-filter-footer">
          <span className="filter-count">{filtered.length} of {rows.length}{focus !== "all" ? ` · ${metrics.find(m => m.id === focus)?.label}` : ""}</span>
          <details className="overview-health-filters"><summary>More health filters</summary><div className="target-filters">
            <label><span>Availability</span><select value={availabilityFilter} onChange={e => setAvailabilityFilter(e.target.value)}><option value="all">All deployments</option><option value="admitting">Admitting only</option><option value="unavailable">Not admitting now</option></select></label>
            <label><span>Recent failures</span><select value={failureFilter} onChange={e => setFailureFilter(e.target.value)}><option value="all">Any history</option><option value="recent">Has recent failures</option><option value="none">No recent failures</option></select></label>
          </div></details>
          <button type="button" className="text-button" onClick={clearFilters}>Clear filters</button>
        </div>
        {!hasData && loading ? <p className="runtime-loading">Loading deployments…</p> : filtered.length ? <div className="table-wrap"><table className="target-table deployment-table">
          <thead><tr><th>Deployment / model</th><th>Lifecycle</th><th>Routing / circuit</th><th>Activity</th><th>Controls</th></tr></thead>
          <tbody>{filtered.map(row => <tr key={row.id} className={selected === row.id ? "selected" : ""}>
              <td><button className="target-link" type="button" title={row.id} aria-expanded={selected === row.id} aria-controls="deployment-details" onClick={() => setSelected(selected === row.id ? "" : row.id)}>{row.title}</button>
                {row.models.length ? <small title={row.models.join(", ")}>{row.models.join(", ")}</small> : null}
                {row.clusters.length ? <small title={row.clusters.join(", ")}>{row.clusters.join(", ")}</small> : null}
              </td>
              <td data-label="Lifecycle"><Pill tone={row.state === "running" ? "good" : ["failed", "unknown"].includes(row.state) ? "warning" : "neutral"}>{stateLabels[row.state]}</Pill>
                {row.activatable ? <small>{row.target?.cold_start === "reject" ? "Manual activation" : "Activatable"}</small> : null}
                {row.binding?.owned !== undefined ? <small>{row.binding.owned ? "Started by SparkRoute" : "Adopted workload"}</small> : null}
                {row.controllerIssue ? <small className="overview-row-warning">{row.controllerIssue}</small> : null}
                {row.binding?.reason && ["failed", "starting", "unknown"].includes(row.state) ? <small>{row.binding.reason.replaceAll("_", " ")}</small> : null}
                {lifecycle.error && row.activatable ? <small className="overview-row-warning">Lifecycle status stale</small> : null}
              </td>
              <td data-label="Routing / circuit"><Pill tone={routingLabel(row) === "Admitting" ? "good" : ["Blocked", "Unavailable", "Recovery probe"].includes(routingLabel(row)) ? "warning" : "neutral"}>{routingLabel(row)}</Pill>
                <small>{row.target ? `Circuit ${humanize(row.target.circuit_state)}` : "Circuit not reported"}</small>
                {row.target?.recent_failures?.length ? <small className="overview-row-warning">{row.target.recent_failures.length} recent failures</small> : null}
                {status.error && row.target ? <small className="overview-row-warning">Routing health stale</small> : null}
              </td>
              <td data-label="Activity">{row.target || row.endpoints.length ? <>{row.requests} requests{row.target?.max_concurrency ? <small>Limit {row.target.max_concurrency}</small> : null}</> : "Requests not reported"}
                {row.binding ? <small>{row.queued} queued{row.binding.max_queued_waiters ? ` / ${row.binding.max_queued_waiters}` : ""}</small> : null}
                {row.binding?.activation_deadline && row.state === "starting" ? <small>Deadline {formatTime(row.binding.activation_deadline)}</small> : null}
              </td>
              <td data-label="Controls">{row.binding ? <WorkloadControls binding={row.binding} canControl={Boolean(features.sparkrun_controls)} stale={Boolean(lifecycle.error || status.error || endpoints.error)} activeRequests={row.requests}
                operation={controls.operations[row.id]} error={controls.errors[row.id]} onAction={operation => void controls.act(row.binding!, operation)} />
                : <small>{row.activatable ? "Lifecycle status unavailable" : "Managed externally"}</small>}</td>
            </tr>
          )}</tbody>
        </table></div> : <Empty title={rows.length ? "No deployments match these filters" : "No deployments reported"} detail={rows.length ? "Adjust or clear the filters to see more deployments." : "Add a deployment to the active gateway configuration. Activatable workloads appear before their first start."} />}
        {selectedRow ? <DeploymentDetails row={selectedRow} token={token} hasEvents={Boolean(features.runtime_events)} hasEndpoints={Boolean(features.endpoint_inventory)} onClose={() => setSelected("")} onActivity={() => showActivity(selectedRow.id)} /> : null}
      </section>
    </div> : null}
    {view === "diagnostics" ? <div id="overview-diagnostics" role="tabpanel" aria-labelledby="overview-tab-diagnostics" className="overview-diagnostics">
      {lifecycle.data ? <ControllerInventory snapshot={lifecycle.data} /> : null}
      {features.endpoint_inventory ? <EndpointInventory filters={endpointFilters} onApply={setEndpointFilters} page={{ endpoints: endpointMatches.slice(endpointOffset, endpointOffset + 100), next_cursor: endpointOffset + 100 < endpointMatches.length ? String(endpointOffset + 100) : undefined }} /> : null}
    </div> : null}
    {view === "activity" ? <div id="overview-activity" role="tabpanel" aria-labelledby="overview-tab-activity"><ActivityView key={JSON.stringify(eventFilters)} token={token} filters={eventFilters} refreshKey={refreshKey} onApply={setEventFilters} /></div> : null}
  </div>;
}

function DeploymentDetails({ row, token, hasEvents, hasEndpoints, onClose, onActivity }: { row: DeploymentEntry; token: string; hasEvents: boolean; hasEndpoints: boolean; onClose: () => void; onActivity: () => void }) {
  const binding = row.binding;
  return <section id="deployment-details" className="deployment-detail" aria-label={`${row.id} deployment details`}>
    <div className="target-detail-heading"><h3>{row.title}</h3><button type="button" className="text-button" onClick={onClose}>Close details</button></div>
    <dl className="target-detail-grid">
      <TargetFact label="Deployment ID" value={row.id} /><TargetFact label="Provider" value={row.target?.provider || "Not reported"} />
      <TargetFact label="Model names" value={row.models.join(", ") || "Not reported"} /><TargetFact label={binding?.cluster_candidates?.length ? "Cluster candidates" : "Clusters"} value={row.clusters.join(", ") || "Not reported"} />
      {binding ? <><TargetFact label="Controller" value={binding.controller} /><TargetFact label="Job" value={binding.job_id || "Not started"} />
        <TargetFact label="Ownership" value={binding.owned === true ? "Started by SparkRoute" : binding.owned === false ? "Adopted; stop managed externally" : "Not reported"} />
        <TargetFact label="Plugins in use" value={binding.plugins_in_use?.join(", ") || "None reported"} />
      </> : null}
    </dl>
    {binding ? <div className="deployment-activation-detail"><h4>Activation</h4>
      <dl className="target-detail-grid">
        <TargetFact label="Queue" value={`${binding.queued_waiters} / ${binding.max_queued_waiters || "unlimited"}`} />
        <TargetFact label="Queued bytes" value={`${formatBytes(binding.queued_body_bytes)} / ${binding.max_queued_body_bytes ? formatBytes(binding.max_queued_body_bytes) : "unlimited"}`} />
        <TargetFact label="Active leases" value={String(binding.active_leases)} /><TargetFact label="Phase" value={humanize(binding.phase || binding.state)} />
        <TargetFact label="Activation started" value={binding.activation_started_at ? formatTime(binding.activation_started_at) : "—"} />
        <TargetFact label="Deadline" value={binding.activation_deadline ? formatTime(binding.activation_deadline) : "—"} />
        <TargetFact label="Last result" value={binding.reason?.replaceAll("_", " ") || "—"} /><TargetFact label="Binding revision" value={binding.binding_revision} />
      </dl>
      <p className="overview-policy-note">{row.target?.cold_start === "wait" ? "Requests can start this workload again after it stops." : row.target?.cold_start === "reject" ? "Start this workload manually before sending requests. Cold requests are rejected." : "Stopping keeps this deployment configured. Its activation policy determines whether requests can start it again."}</p>
    </div> : null}
    {binding?.recovery ? <section aria-label="Automatic recovery"><h4>Automatic recovery</h4><dl className="target-detail-grid">
      <TargetFact label="Recovery action" value={binding.recovery.action === "restart" ? "Restart immediately" : "Stop until next demand"} />
      <TargetFact label="Recovery phase" value={humanize(binding.recovery.phase)} />
      <TargetFact label="Recovery attempts" value={`${binding.recovery.attempts} / ${binding.recovery.max_restarts}`} />
      <TargetFact label="Failed recovery probes" value={String(binding.recovery.failed_probes)} />
      <TargetFact label="Next recovery attempt" value={binding.recovery.next_attempt_at ? formatTime(binding.recovery.next_attempt_at) : "—"} />
      <TargetFact label="Recovery result" value={humanize(binding.recovery.reason || "—")} />
    </dl>{["failed", "exhausted"].includes(binding.recovery.phase) ? <p className="notice error">Automatic recovery is blocked. Inspect the workload, then use Start to retry explicitly or Stop to shut it down.</p> : null}</section> : null}
    {row.target ? <TargetHealthDetail target={row.target} /> : <p className="overview-policy-note">Routing health is not available for this deployment.</p>}
    {hasEndpoints ? <div className="deployment-endpoints"><h4>Serving endpoints</h4>
      {row.endpoints.length ? <ul>{row.endpoints.map(endpoint => <li key={endpoint.endpoint_id}><code>{endpoint.endpoint_id}</code><span>{endpoint.expired ? "Expired" : humanize(endpoint.state)} · {endpoint.active_requests} requests{endpoint.heartbeat_at ? ` · Last heartbeat ${formatTime(endpoint.heartbeat_at)}` : ""}</span></li>)}</ul> : <p>No serving endpoints are registered.</p>}
    </div> : null}
    {hasEvents ? <RecentActivity key={`${row.id}/${binding?.state ?? ""}/${binding?.phase ?? ""}`} token={token} deployment={row.id} onActivity={onActivity} /> : null}
  </section>;
}

function RecentActivity({ token, deployment, onActivity }: { token: string; deployment: string; onActivity: () => void }) {
  const loader = useCallback((signal: AbortSignal) => fetchRuntimeEvents(token, { deployment, limit: 5 }, signal), [token, deployment]);
  const resource = useLiveResource(true, loader);
  return <div className="deployment-recent-activity"><div className="target-detail-heading"><h4>Recent activity</h4><button type="button" className="text-button" onClick={onActivity}>View all activity</button></div>
    {resource.error ? <p className="form-error" role="alert">Recent activity unavailable. {resource.error}</p> : null}
    {resource.data?.records.length ? <ol>{resource.data.records.map(event => <li key={event.event_id}><span>{humanize(event.prior_state || "unknown")} → {humanize(event.new_state)} · {humanize(event.outcome)}{event.reason ? ` · ${event.reason.replaceAll("_", " ")}` : ""}</span><time dateTime={event.occurred_at}>{formatTime(event.occurred_at)}</time></li>)}</ol>
      : <p>{resource.loading ? "Loading activity…" : "No lifecycle transitions recorded."}</p>}
  </div>;
}

function ActivityView({ token, filters, refreshKey, onApply }: { token: string; filters: RuntimeEventFilters; refreshKey: number; onApply: (filters: RuntimeEventFilters) => void }) {
  const loader = useCallback((signal: AbortSignal) => fetchRuntimeEvents(token, filters, signal), [token, filters]);
  const resource = useLiveResource(true, loader, undefined, refreshKey);
  return <>{resource.error ? <p className="notice error" role="alert">Activity {resource.data ? "is stale" : "is unavailable"}. {resource.error} <button type="button" className="text-button" onClick={() => void resource.refresh()}>Retry activity</button></p> : null}
    {resource.loading && !resource.data ? <p className="runtime-loading">Loading activity…</p> : <TransitionHistory filters={filters} page={resource.data ?? { records: [] }} onApply={onApply} />}</>;
}

function Pill({ children, tone }: { children: ReactNode; tone: "good" | "warning" | "neutral" }) {
  return <span className={`status-pill ${tone}`}><i />{children}</span>;
}
