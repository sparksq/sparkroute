// SPDX-FileCopyrightText: 2026 Scitrera LLC
// SPDX-FileCopyrightText: 2026 Fox Engine Ltd.
// SPDX-License-Identifier: AGPL-3.0-only

import { type FormEvent, useState } from "react";
import type { LifecycleSnapshot, RuntimeEndpointFilters, RuntimeEndpointPage, RuntimeEventFilters, RuntimeEventPage } from "./types";

export function ControllerInventory({ snapshot }: { snapshot: LifecycleSnapshot }) {
  return (
      <section className="panel">
        <div className="panel-heading">
          <div><p className="eyebrow">Controller observations</p><h2>Runtime controllers</h2></div>
          <span className="activity-summary">Observed {formatTime(snapshot.observed_at)}</span>
        </div>
        {snapshot.controllers.length ? (
          <div className="table-wrap"><table><thead><tr>
            <th>Controller</th><th>Health</th><th>Ready</th><th>Leases</th><th>Queued</th><th>Reason</th>
          </tr></thead><tbody>
            {snapshot.controllers.map((controller) => <tr key={controller.controller}>
              <td className="deployment-name">{controller.controller}</td>
              <td><StatePill value={controller.health} good={controller.health === "healthy"} /></td>
              <td>{controller.ready_endpoints} / {controller.endpoints}</td>
              <td>{controller.active_leases}</td>
              <td>{controller.queued_waiters} · {formatBytes(controller.queued_body_bytes)}</td>
              <td>{controller.reason || "—"}</td>
            </tr>)}
          </tbody></table></div>
        ) : <Empty title="No controllers reported" detail="No lifecycle controller has published status." />}
      </section>

  );
}

export function EndpointInventory({
  filters,
  page,
  onApply,
}: {
  filters: RuntimeEndpointFilters;
  page: RuntimeEndpointPage;
  onApply: (filters: RuntimeEndpointFilters) => void;
}) {
  const [draft, setDraft] = useState(filters);
  const submit = (event: FormEvent) => {
    event.preventDefault();
    onApply({ ...draft, cursor: undefined, limit: 100 });
  };
  return <section className="panel">
    <div className="panel-heading"><div><p className="eyebrow">Live registry</p><h2>Serving endpoints</h2></div><span className="activity-summary">Current registrations</span></div>
    <form className="target-filters runtime-filters" onSubmit={submit}>
      <label className="wide"><span>Target</span><input aria-label="Runtime target" value={draft.target ?? ""} onChange={(event) => setDraft({ ...draft, target: event.target.value })} /></label>
      <label><span>Controller</span><input aria-label="Runtime controller" value={draft.controller ?? ""} onChange={(event) => setDraft({ ...draft, controller: event.target.value })} /></label>
      <label><span>State</span><select aria-label="Runtime state" value={draft.state ?? ""} onChange={(event) => setDraft({ ...draft, state: event.target.value })}>
        <option value="">All states</option>{["offline", "activating", "ready", "draining", "deactivating", "failed", "unknown"].map((state) => <option key={state}>{state}</option>)}
      </select></label>
      <button className="secondary-button" type="submit">Apply filters</button>
    </form>
    {page.endpoints.length ? <div className="table-wrap"><table><thead><tr>
      <th>Endpoint</th><th>Target</th><th>State</th><th>Admission</th><th>Controller</th><th>Heartbeat</th>
    </tr></thead><tbody>
      {page.endpoints.map((endpoint) => <tr key={endpoint.endpoint_id}>
        <td className="deployment-name">{endpoint.endpoint_id}<small>{endpoint.served_models.join(", ") || "No model advertised"}</small></td>
        <td>{endpoint.target}<small>{endpoint.protocol || "—"}</small></td>
        <td><StatePill value={endpoint.expired ? "expired" : endpoint.state} good={endpoint.state === "ready" && !endpoint.expired} /></td>
        <td>{endpoint.active_requests} / {endpoint.max_concurrency || "∞"}</td>
        <td>{endpoint.controller || "static"}<small>{endpoint.cluster_id || endpoint.job_id || "—"}</small></td>
        <td>{endpoint.heartbeat_at ? formatTime(endpoint.heartbeat_at) : "—"}</td>
      </tr>)}
    </tbody></table></div> : <Empty title="No endpoints match" detail="No current registration matches these filters." />}
    {page.next_cursor ? <div className="pagination-bar"><button className="secondary-button" onClick={() => onApply({ ...filters, cursor: page.next_cursor })} type="button">Next page</button></div> : null}
  </section>;
}

export function TransitionHistory({
  filters,
  page,
  onApply,
}: {
  filters: RuntimeEventFilters;
  page: RuntimeEventPage;
  onApply: (filters: RuntimeEventFilters) => void;
}) {
  const [draft, setDraft] = useState(filters);
  const submit = (event: FormEvent) => {
    event.preventDefault();
    onApply({ ...draft, cursor: undefined, limit: 100 });
  };
  return <section className="panel">
    <div className="panel-heading"><div><p className="eyebrow">Persisted operational history</p><h2>Lifecycle transitions</h2></div><span className="activity-summary">Newest first</span></div>
    <form className="target-filters runtime-event-filters" onSubmit={submit}>
      <label><span>Controller</span><input aria-label="Transition controller" value={draft.controller ?? ""} onChange={(event) => setDraft({ ...draft, controller: event.target.value })} /></label>
      <label><span>Virtual model</span><input aria-label="Transition model" value={draft.virtual_model ?? ""} onChange={(event) => setDraft({ ...draft, virtual_model: event.target.value })} /></label>
      <label><span>Deployment</span><input aria-label="Transition deployment" value={draft.deployment ?? ""} onChange={(event) => setDraft({ ...draft, deployment: event.target.value })} /></label>
      <label><span>Outcome</span><select aria-label="Transition outcome" value={draft.outcome ?? ""} onChange={(event) => setDraft({ ...draft, outcome: event.target.value })}>
        <option value="">All outcomes</option>{["success", "failure", "timeout", "cancelled", "rejected"].map((outcome) => <option key={outcome}>{outcome}</option>)}
      </select></label>
      <button className="secondary-button" type="submit">Apply filters</button>
    </form>
    {page.records.length ? <div className="table-wrap"><table><thead><tr>
      <th>Occurred</th><th>Controller</th><th>Deployment</th><th>Transition</th><th>Outcome</th><th>Queue</th><th>Latency</th>
    </tr></thead><tbody>
      {page.records.map((event) => <tr key={event.event_id}>
        <td>{formatTime(event.occurred_at)}<small>{event.event_id}</small></td>
        <td className="deployment-name">{event.controller}<small>{event.binding_revision || "—"}</small></td>
        <td>{event.deployment || "—"}<small>{event.virtual_model || "—"}</small></td>
        <td>{event.prior_state || "—"} → {event.new_state}<small>{event.reason || "—"}</small></td>
        <td><StatePill value={event.outcome} good={event.outcome === "success"} /></td>
        <td>{event.waiter_count} waiters<small>depth {event.queue_depth}</small></td>
        <td>{event.latency === undefined ? "—" : formatDuration(event.latency)}</td>
      </tr>)}
    </tbody></table></div> : <Empty title="No lifecycle transitions" detail="No persisted runtime event matches these filters." />}
    {page.next_cursor ? <div className="pagination-bar"><button className="secondary-button" onClick={() => onApply({ ...filters, cursor: page.next_cursor })} type="button">Next page</button></div> : null}
  </section>;
}

export function StatePill({ value, good }: { value: string; good: boolean }) {
  return <span className={good ? "status-pill good" : "status-pill warning"}><i />{humanize(value)}</span>;
}

export function Empty({ title, detail }: { title: string; detail: string }) {
  return <div className="empty-state compact"><strong>{title}</strong><span>{detail}</span></div>;
}

export function formatTime(value: string) {
  const parsed = new Date(value);
  return Number.isNaN(parsed.valueOf()) ? value : new Intl.DateTimeFormat(undefined, { dateStyle: "medium", timeStyle: "medium" }).format(parsed);
}

export function formatBytes(value: number) {
  if (value < 1024) return `${value} B`;
  if (value < 1024 * 1024) return `${(value / 1024).toFixed(1)} KiB`;
  return `${(value / (1024 * 1024)).toFixed(1)} MiB`;
}

function formatDuration(nanoseconds: number) {
  if (nanoseconds < 1_000_000) return `${Math.round(nanoseconds / 1_000)} µs`;
  if (nanoseconds < 1_000_000_000) return `${(nanoseconds / 1_000_000).toFixed(1)} ms`;
  return `${(nanoseconds / 1_000_000_000).toFixed(2)} s`;
}

export function humanize(value: string) {
  return value.split("_").filter(Boolean).map((part) => part[0]?.toUpperCase() + part.slice(1)).join(" ");
}
