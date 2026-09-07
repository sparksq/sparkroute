import { type FormEvent, useCallback, useEffect, useMemo, useState } from "react";
import {
  fetchLifecycleStatus,
  fetchRuntimeEndpoints,
  fetchRuntimeEvents,
} from "./api";
import type {
  AdminBootstrap,
  LifecycleSnapshot,
  RuntimeEndpointFilters,
  RuntimeEndpointPage,
  RuntimeEventFilters,
  RuntimeEventPage,
} from "./types";

type RuntimeView = "overview" | "endpoints" | "transitions";

const emptyEndpoints: RuntimeEndpointPage = { endpoints: [] };
const emptyEvents: RuntimeEventPage = { records: [] };

export function RuntimeWorkspace({
  bootstrap,
  token,
}: {
  bootstrap: AdminBootstrap;
  token: string;
}) {
  const hasStatus = Boolean(bootstrap.features.lifecycle_status);
  const hasEndpoints = Boolean(bootstrap.features.endpoint_inventory);
  const hasEvents = Boolean(bootstrap.features.runtime_events);
  const [view, setView] = useState<RuntimeView>(
    hasStatus ? "overview" : hasEndpoints ? "endpoints" : "transitions",
  );
  const [endpointFilters, setEndpointFilters] = useState<RuntimeEndpointFilters>({ limit: 100 });
  const [eventFilters, setEventFilters] = useState<RuntimeEventFilters>({ limit: 100 });
  const [endpoints, setEndpoints] = useState(emptyEndpoints);
  const [events, setEvents] = useState(emptyEvents);
  const [status, setStatus] = useState<LifecycleSnapshot>();
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState("");

  const load = useCallback(async (signal?: AbortSignal) => {
    setLoading(true);
    setError("");
    try {
      const [nextStatus, nextEndpoints, nextEvents] = await Promise.all([
        hasStatus ? fetchLifecycleStatus(token, signal) : Promise.resolve(undefined),
        hasEndpoints
          ? fetchRuntimeEndpoints(token, endpointFilters, signal)
          : Promise.resolve(emptyEndpoints),
        hasEvents
          ? fetchRuntimeEvents(token, eventFilters, signal)
          : Promise.resolve(emptyEvents),
      ]);
      if (signal?.aborted) return;
      setStatus(nextStatus);
      setEndpoints(nextEndpoints);
      setEvents(nextEvents);
    } catch (caught) {
      if (signal?.aborted) return;
      setError(caught instanceof Error ? caught.message : "Runtime status request failed");
    } finally {
      if (!signal?.aborted) setLoading(false);
    }
  }, [endpointFilters, eventFilters, hasEndpoints, hasEvents, hasStatus, token]);

  useEffect(() => {
    const controller = new AbortController();
    void load(controller.signal);
    return () => controller.abort();
  }, [load]);

  const readyEndpoints = endpoints.endpoints.filter(
    (endpoint) => endpoint.state === "ready" && !endpoint.expired,
  ).length;
  const queuedWaiters = status?.controllers.reduce(
    (total, controller) => total + controller.queued_waiters,
    0,
  ) ?? 0;
  const availableViews = useMemo(() => [
    ...(hasStatus ? [{ id: "overview" as const, label: "Controllers & queues" }] : []),
    ...(hasEndpoints ? [{ id: "endpoints" as const, label: "Endpoints" }] : []),
    ...(hasEvents ? [{ id: "transitions" as const, label: "Transitions" }] : []),
  ], [hasEndpoints, hasEvents, hasStatus]);

  return (
    <div className="runtime-workspace">
      <section className="scope-banner">
        <div>
          <p className="eyebrow">Content-free runtime operations</p>
          <strong>Lifecycle controllers, cold-start admission, and serving instances</strong>
        </div>
        <span>URLs, credentials, raw errors, and registry metadata are excluded.</span>
      </section>

      <section className="metric-grid" aria-label="Runtime inventory">
        <Metric label="Controllers" value={status?.controllers.length ?? 0} />
        <Metric label="Endpoints" value={endpoints.endpoints.length} />
        <Metric label="Ready endpoints" value={readyEndpoints} accent={readyEndpoints > 0} />
        <Metric label="Queued waiters" value={queuedWaiters} accent={queuedWaiters === 0} />
      </section>

      <div className="workspace-tabs" role="tablist" aria-label="Runtime views">
        {availableViews.map((item) => (
          <button
            aria-selected={view === item.id}
            className={view === item.id ? "active" : ""}
            key={item.id}
            onClick={() => setView(item.id)}
            role="tab"
            type="button"
          >
            {item.label}
          </button>
        ))}
        <button className="runtime-refresh" onClick={() => void load()} type="button">
          Refresh runtime
        </button>
      </div>

      {error ? <p className="form-error panel-error">{error}</p> : null}
      {loading ? <section className="panel runtime-loading">Loading runtime state…</section> : null}
      {!loading && !error && view === "overview" && status ? (
        <LifecycleOverview snapshot={status} />
      ) : null}
      {!loading && !error && view === "endpoints" ? (
        <EndpointInventory
          filters={endpointFilters}
          page={endpoints}
          onApply={setEndpointFilters}
        />
      ) : null}
      {!loading && !error && view === "transitions" ? (
        <TransitionHistory
          filters={eventFilters}
          page={events}
          onApply={setEventFilters}
        />
      ) : null}
    </div>
  );
}

function LifecycleOverview({ snapshot }: { snapshot: LifecycleSnapshot }) {
  return (
    <>
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

      <section className="panel">
        <div className="panel-heading">
          <div><p className="eyebrow">Bounded cold-start admission</p><h2>Activation bindings</h2></div>
          <span className="activity-summary">{snapshot.bindings.length} bindings</span>
        </div>
        {snapshot.bindings.length ? (
          <div className="table-wrap"><table><thead><tr>
            <th>Deployment</th><th>Model</th><th>State</th><th>Queue</th><th>Leases</th><th>Deadline</th>
          </tr></thead><tbody>
            {snapshot.bindings.map((binding) => <tr key={`${binding.controller}/${binding.binding_revision}`}>
              <td className="deployment-name">{binding.deployment}<small>{binding.controller}</small></td>
              <td>{binding.virtual_model || "—"}</td>
              <td><StatePill value={binding.state} good={binding.state === "ready"} /></td>
              <td>{binding.queued_waiters} / {binding.max_queued_waiters || "∞"}<small>{formatBytes(binding.queued_body_bytes)} / {binding.max_queued_body_bytes ? formatBytes(binding.max_queued_body_bytes) : "∞"}</small></td>
              <td>{binding.active_leases}</td>
              <td>{binding.activation_deadline ? formatTime(binding.activation_deadline) : "—"}</td>
            </tr>)}
          </tbody></table></div>
        ) : <Empty title="No activation bindings" detail="No cold-start-capable binding has published status." />}
      </section>
    </>
  );
}

function EndpointInventory({
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

function TransitionHistory({
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

function Metric({ label, value, accent = false }: { label: string; value: number; accent?: boolean }) {
  return <article className={accent ? "metric accent" : "metric"}><span>{label}</span><strong>{value}</strong></article>;
}

function StatePill({ value, good }: { value: string; good: boolean }) {
  return <span className={good ? "status-pill good" : "status-pill warning"}><i />{humanize(value)}</span>;
}

function Empty({ title, detail }: { title: string; detail: string }) {
  return <div className="empty-state compact"><strong>{title}</strong><span>{detail}</span></div>;
}

function formatTime(value: string) {
  const parsed = new Date(value);
  return Number.isNaN(parsed.valueOf()) ? value : new Intl.DateTimeFormat(undefined, { dateStyle: "medium", timeStyle: "medium" }).format(parsed);
}

function formatBytes(value: number) {
  if (value < 1024) return `${value} B`;
  if (value < 1024 * 1024) return `${(value / 1024).toFixed(1)} KiB`;
  return `${(value / (1024 * 1024)).toFixed(1)} MiB`;
}

function formatDuration(nanoseconds: number) {
  if (nanoseconds < 1_000_000) return `${Math.round(nanoseconds / 1_000)} µs`;
  if (nanoseconds < 1_000_000_000) return `${(nanoseconds / 1_000_000).toFixed(1)} ms`;
  return `${(nanoseconds / 1_000_000_000).toFixed(2)} s`;
}

function humanize(value: string) {
  return value.split("_").filter(Boolean).map((part) => part[0]?.toUpperCase() + part.slice(1)).join(" ");
}
