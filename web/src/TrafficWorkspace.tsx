import {
  type FormEvent,
  useCallback,
  useEffect,
  useState,
} from "react";
import {
  fetchAttempts,
  fetchLedgerSummary,
  fetchRequest,
  fetchRequests,
} from "./api";
import type {
  AdminBootstrap,
  AttemptFilters,
  AttemptRecord,
  LedgerAggregateBreakdown,
  LedgerAggregateFilters,
  LedgerAggregateSummary,
  RequestFilters,
  RequestRecord,
  TokenUsage,
} from "./types";

const outcomes = [
  "success",
  "rejected",
  "upstream_error",
  "transport_error",
  "configuration_error",
  "client_cancelled",
  "stream_error",
  "attempts_exhausted",
];

interface RequestForm {
  requestID: string;
  startedAfter: string;
  startedBefore: string;
  tenant: string;
  virtualModel: string;
  outcome: string;
  limit: number;
}

interface AttemptForm {
  requestID: string;
  startedAfter: string;
  startedBefore: string;
  provider: string;
  deployment: string;
  outcome: string;
  limit: number;
}

interface DashboardForm {
  startedAfter: string;
  startedBefore: string;
  tenant: string;
  virtualModel: string;
  provider: string;
  deployment: string;
  outcome: string;
}

type TrafficView = "dashboard" | "requests" | "attempts";

const initialRequestForm: RequestForm = {
  requestID: "",
  startedAfter: "",
  startedBefore: "",
  tenant: "",
  virtualModel: "",
  outcome: "",
  limit: 50,
};

const initialAttemptForm: AttemptForm = {
  requestID: "",
  startedAfter: "",
  startedBefore: "",
  provider: "",
  deployment: "",
  outcome: "",
  limit: 50,
};

function initialDashboardForm(): DashboardForm {
  const before = new Date();
  return {
    startedAfter: datetimeLocal(new Date(before.valueOf() - 24 * 60 * 60 * 1_000)),
    startedBefore: datetimeLocal(before),
    tenant: "",
    virtualModel: "",
    provider: "",
    deployment: "",
    outcome: "",
  };
}

export function TrafficWorkspace({
  bootstrap,
  token,
}: {
  bootstrap: AdminBootstrap;
  token: string;
}) {
  const roles = bootstrap.principal?.roles ?? [];
  const canReadAll = roles.includes("ledger_read_all");
  const dashboardAvailable = Boolean(bootstrap.features.ledger_aggregate);
  const [view, setView] = useState<TrafficView>(
    dashboardAvailable ? "dashboard" : "requests",
  );
  const [dashboardForm, setDashboardForm] = useState(initialDashboardForm);
  const [summary, setSummary] = useState<LedgerAggregateSummary>();
  const [requestForm, setRequestForm] = useState(initialRequestForm);
  const [requestQuery, setRequestQuery] = useState<RequestFilters>({ limit: 50 });
  const [requests, setRequests] = useState<RequestRecord[]>([]);
  const [requestCursor, setRequestCursor] = useState("");
  const [attemptForm, setAttemptForm] = useState(initialAttemptForm);
  const [attemptQuery, setAttemptQuery] = useState<AttemptFilters>();
  const [attempts, setAttempts] = useState<AttemptRecord[]>([]);
  const [attemptCursor, setAttemptCursor] = useState("");
  const [selected, setSelected] = useState<RequestRecord>();
  const [selectedAttempts, setSelectedAttempts] = useState<AttemptRecord[]>([]);
  const [selectedAttemptCursor, setSelectedAttemptCursor] = useState("");
  const [notice, setNotice] = useState("");
  const [busy, setBusy] = useState("");

  const loadSummary = useCallback(
    async (filters: LedgerAggregateFilters, signal?: AbortSignal) => {
      setBusy("dashboard");
      setNotice("");
      try {
        setSummary(await fetchLedgerSummary(token, filters, signal));
      } catch (error) {
        if (!isAbort(error)) setNotice(errorMessage(error));
      } finally {
        if (!signal?.aborted) setBusy("");
      }
    },
    [token],
  );

  const loadRequests = useCallback(
    async (
      query: RequestFilters,
      append = false,
      signal?: AbortSignal,
    ) => {
      setBusy(append ? "more-requests" : "requests");
      setNotice("");
      try {
        const page = await fetchRequests(token, query, signal);
        setRequests((current) => (append ? [...current, ...page.records] : page.records));
        setRequestCursor(page.next_cursor ?? "");
      } catch (error) {
        if (!isAbort(error)) setNotice(errorMessage(error));
      } finally {
        if (!signal?.aborted) setBusy("");
      }
    },
    [token],
  );

  useEffect(() => {
    if (dashboardAvailable) return undefined;
    const controller = new AbortController();
    void loadRequests({ limit: 50 }, false, controller.signal);
    return () => controller.abort();
  }, [dashboardAvailable, loadRequests]);

  useEffect(() => {
    if (!dashboardAvailable) return undefined;
    const controller = new AbortController();
    void loadSummary(
      aggregateFilters(initialDashboardForm(), canReadAll),
      controller.signal,
    );
    return () => controller.abort();
  }, [canReadAll, dashboardAvailable, loadSummary]);

  const refreshDashboard = async (event: FormEvent) => {
    event.preventDefault();
    try {
      await loadSummary(aggregateFilters(dashboardForm, canReadAll));
    } catch (error) {
      setNotice(errorMessage(error));
    }
  };

  const searchRequests = async (event: FormEvent) => {
    event.preventDefault();
    try {
      if (requestForm.requestID.trim()) {
        await openRequest(requestForm.requestID.trim());
        return;
      }
      const query = requestFilters(requestForm, canReadAll);
      setRequestQuery(query);
      setSelected(undefined);
      await loadRequests(query);
    } catch (error) {
      setNotice(errorMessage(error));
    }
  };

  const loadAttempts = async (query: AttemptFilters, append = false) => {
    setBusy(append ? "more-attempts" : "attempts");
    setNotice("");
    try {
      const page = await fetchAttempts(token, query);
      setAttempts((current) => (append ? [...current, ...page.records] : page.records));
      setAttemptCursor(page.next_cursor ?? "");
    } catch (error) {
      setNotice(errorMessage(error));
    } finally {
      setBusy("");
    }
  };

  const searchAttempts = async (event: FormEvent) => {
    event.preventDefault();
    try {
      if (!canReadAll && !attemptForm.requestID.trim()) {
        throw new Error("Request ID is required for tenant-scoped attempt searches.");
      }
      const query = attemptFilters(attemptForm);
      setAttemptQuery(query);
      await loadAttempts(query);
    } catch (error) {
      setNotice(errorMessage(error));
    }
  };

  const openRequest = async (requestID: string) => {
    setBusy("detail");
    setNotice("");
    try {
      const [record, page] = await Promise.all([
        fetchRequest(token, requestID),
        fetchAttempts(token, { request_id: requestID, limit: 100 }),
      ]);
      setSelected(record);
      setSelectedAttempts(page.records);
      setSelectedAttemptCursor(page.next_cursor ?? "");
      setView("requests");
    } catch (error) {
      setNotice(errorMessage(error));
    } finally {
      setBusy("");
    }
  };

  const loadMoreSelectedAttempts = async () => {
    if (!selected || !selectedAttemptCursor) return;
    setBusy("more-detail-attempts");
    try {
      const page = await fetchAttempts(token, {
        request_id: selected.request_id,
        limit: 100,
        cursor: selectedAttemptCursor,
      });
      setSelectedAttempts((current) => [...current, ...page.records]);
      setSelectedAttemptCursor(page.next_cursor ?? "");
    } catch (error) {
      setNotice(errorMessage(error));
    } finally {
      setBusy("");
    }
  };

  const principalTenant = bootstrap.principal?.tenant;
  return (
    <div className="traffic-workspace">
      <section className="scope-banner">
        <div>
          <p className="eyebrow">Content-free operational history</p>
          <strong>Requests and provider attempts</strong>
        </div>
        <span>
          {canReadAll
            ? "Cross-tenant role enabled"
            : `Tenant enforced: ${principalTenant || "unavailable"}`}
        </span>
      </section>

      <div className="workspace-tabs" role="tablist" aria-label="Traffic views">
        {dashboardAvailable ? (
          <button
            aria-selected={view === "dashboard"}
            className={view === "dashboard" ? "active" : ""}
            onClick={() => setView("dashboard")}
            role="tab"
            type="button"
          >
            Dashboard
          </button>
        ) : null}
        <button
          aria-selected={view === "requests"}
          className={view === "requests" ? "active" : ""}
          onClick={() => {
            setView("requests");
            if (!requests.length) void loadRequests({ limit: 50 });
          }}
          role="tab"
          type="button"
        >
          Requests
        </button>
        <button
          aria-selected={view === "attempts"}
          className={view === "attempts" ? "active" : ""}
          onClick={() => setView("attempts")}
          role="tab"
          type="button"
        >
          Attempts
        </button>
      </div>

      {notice ? <div className="notice error">{notice}</div> : null}

      {view === "dashboard" ? (
        <TrafficDashboard
          busy={busy === "dashboard"}
          canReadAll={canReadAll}
          form={dashboardForm}
          onChange={setDashboardForm}
          onSubmit={refreshDashboard}
          summary={summary}
        />
      ) : view === "requests" ? (
        <>
          <RequestSearch
            canReadAll={canReadAll}
            form={requestForm}
            onChange={setRequestForm}
            onSubmit={searchRequests}
            searching={busy === "requests" || busy === "detail"}
          />
          <RequestResults
            busy={busy}
            nextCursor={requestCursor}
            onLoadMore={() =>
              void loadRequests(
                { ...requestQuery, cursor: requestCursor },
                true,
              )
            }
            onOpen={(requestID) => void openRequest(requestID)}
            records={requests}
          />
          {selected ? (
            <RequestDetail
              attemptCursor={selectedAttemptCursor}
              attempts={selectedAttempts}
              busy={busy === "more-detail-attempts"}
              onClose={() => setSelected(undefined)}
              onLoadMore={() => void loadMoreSelectedAttempts()}
              record={selected}
            />
          ) : null}
        </>
      ) : (
        <>
          <AttemptSearch
            canReadAll={canReadAll}
            form={attemptForm}
            onChange={setAttemptForm}
            onSubmit={searchAttempts}
            searching={busy === "attempts"}
          />
          <AttemptResults
            busy={busy}
            nextCursor={attemptCursor}
            onLoadMore={() => {
              if (attemptQuery) {
                void loadAttempts(
                  { ...attemptQuery, cursor: attemptCursor },
                  true,
                );
              }
            }}
            onOpenRequest={(requestID) => void openRequest(requestID)}
            records={attempts}
          />
        </>
      )}
    </div>
  );
}

function TrafficDashboard({
  form,
  canReadAll,
  busy,
  summary,
  onChange,
  onSubmit,
}: {
  form: DashboardForm;
  canReadAll: boolean;
  busy: boolean;
  summary?: LedgerAggregateSummary;
  onChange: (form: DashboardForm) => void;
  onSubmit: (event: FormEvent) => void;
}) {
  return (
    <>
      <form className="panel filter-panel" onSubmit={onSubmit}>
        <div className="panel-heading">
          <div>
            <p className="eyebrow">Server-side bounded aggregate</p>
            <h2>Dashboard scope</h2>
          </div>
          <span className="activity-summary">Maximum window: 31 days</span>
        </div>
        <div className="filter-grid">
          <DateField
            label="Started at or after"
            onChange={(startedAfter) => onChange({ ...form, startedAfter })}
            value={form.startedAfter}
          />
          <DateField
            label="Started before"
            onChange={(startedBefore) => onChange({ ...form, startedBefore })}
            value={form.startedBefore}
          />
          {canReadAll ? (
            <Field label="Tenant">
              <input
                onChange={(event) => onChange({ ...form, tenant: event.target.value })}
                placeholder="Blank includes all tenants"
                value={form.tenant}
              />
            </Field>
          ) : null}
          <Field label="Virtual model">
            <input
              onChange={(event) => onChange({ ...form, virtualModel: event.target.value })}
              value={form.virtualModel}
            />
          </Field>
          <Field label="Final provider">
            <input
              onChange={(event) => onChange({ ...form, provider: event.target.value })}
              value={form.provider}
            />
          </Field>
          <Field label="Final deployment">
            <input
              onChange={(event) => onChange({ ...form, deployment: event.target.value })}
              value={form.deployment}
            />
          </Field>
          <OutcomeField
            onChange={(outcome) => onChange({ ...form, outcome })}
            value={form.outcome}
          />
        </div>
        <div className="filter-actions">
          <button className="primary-button" disabled={busy} type="submit">
            {busy ? "Refreshing…" : "Refresh dashboard"}
          </button>
        </div>
      </form>

      {summary ? <DashboardSummary summary={summary} /> : (
        <section className="panel result-panel">
          <div className="list-empty">
            {busy ? "Loading dashboard…" : "No aggregate is available for this scope."}
          </div>
        </section>
      )}
    </>
  );
}

function DashboardSummary({ summary }: { summary: LedgerAggregateSummary }) {
  const successRate = summary.requests
    ? summary.successful_requests / summary.requests
    : 0;
  const completeRate = summary.requests
    ? summary.usage_completeness.complete / summary.requests
    : 0;
  return (
    <div className="dashboard-results">
      <section className="dashboard-metrics" aria-label="Traffic summary">
        <DashboardMetric label="Requests" value={formatCount(summary.requests)} />
        <DashboardMetric label="Success rate" value={formatPercent(successRate)} />
        <DashboardMetric label="Attempts" value={formatCount(summary.attempts)} />
        <DashboardMetric label="Retries" value={formatCount(summary.retries)} />
        <DashboardMetric label="Retried requests" value={formatCount(summary.retried_requests)} />
        <DashboardMetric label="P95 latency" value={formatDuration(summary.latency.p95)} />
      </section>

      <section className="panel aggregate-panel">
        <div className="panel-heading">
          <div>
            <p className="eyebrow">Normalized request measurements</p>
            <h2>Latency and token usage</h2>
          </div>
          <span className="activity-summary">
            {formatPercent(completeRate)} complete usage
          </span>
        </div>
        <div className="aggregate-stat-grid">
          <DashboardMetric label="Average latency" value={formatDuration(summary.latency.average)} />
          <DashboardMetric label="P50 latency" value={formatDuration(summary.latency.p50)} />
          <DashboardMetric label="P95 latency" value={formatDuration(summary.latency.p95)} />
          <DashboardMetric label="P99 latency" value={formatDuration(summary.latency.p99)} />
          <DashboardMetric label="Input tokens" value={formatCount(summary.tokens.input_tokens)} />
          <DashboardMetric label="Output tokens" value={formatCount(summary.tokens.output_tokens)} />
          <DashboardMetric label="Total tokens" value={formatCount(summary.tokens.total_tokens)} />
          <DashboardMetric label="Cached input" value={formatCount(summary.tokens.cached_input_tokens)} />
        </div>
        <div className="usage-coverage" aria-label="Usage completeness">
          <CoverageItem label="Complete" value={summary.usage_completeness.complete} total={summary.requests} />
          <CoverageItem label="Partial" value={summary.usage_completeness.partial} total={summary.requests} />
          <CoverageItem label="Missing" value={summary.usage_completeness.missing} total={summary.requests} />
        </div>
        <p className="content-boundary-note">
          Token totals sum only normalized values reported by providers. Coverage counts show where missing fields contributed zero. No prompt or response content is queried.
        </p>
      </section>

      <section className="aggregate-breakdown-grid" aria-label="Traffic breakdowns">
        <Breakdown title="Outcomes" breakdown={summary.by_outcome} />
        <Breakdown title="Virtual models" breakdown={summary.by_virtual_model} />
        <Breakdown title="Final providers" breakdown={summary.by_provider} />
        <Breakdown title="Final deployments" breakdown={summary.by_deployment} />
      </section>
    </div>
  );
}

function DashboardMetric({ label, value }: { label: string; value: string }) {
  return <div className="dashboard-metric"><span>{label}</span><strong>{value}</strong></div>;
}

function CoverageItem({
  label,
  value,
  total,
}: {
  label: string;
  value: number;
  total: number;
}) {
  const ratio = total ? Math.min(1, value / total) : 0;
  return (
    <div>
      <span><strong>{label}</strong><small>{formatCount(value)} · {formatPercent(ratio)}</small></span>
      <div className="coverage-track"><span style={{ width: `${ratio * 100}%` }} /></div>
    </div>
  );
}

function Breakdown({
  title,
  breakdown,
}: {
  title: string;
  breakdown: LedgerAggregateBreakdown;
}) {
  return (
    <section className="panel breakdown-panel">
      <div className="panel-heading compact-heading">
        <h2>{title}</h2>
        {breakdown.truncated ? <span className="activity-summary">Top 20</span> : null}
      </div>
      {breakdown.groups.length ? (
        <ol>
          {breakdown.groups.map((group) => (
            <li key={group.value || "(unset)"}>
              <span>{group.value || "(unset)"}</span>
              <strong>{formatCount(group.count)}</strong>
            </li>
          ))}
        </ol>
      ) : <div className="list-empty">No matching values.</div>}
    </section>
  );
}

function RequestSearch({
  form,
  canReadAll,
  searching,
  onChange,
  onSubmit,
}: {
  form: RequestForm;
  canReadAll: boolean;
  searching: boolean;
  onChange: (form: RequestForm) => void;
  onSubmit: (event: FormEvent) => void;
}) {
  return (
    <form className="panel filter-panel" onSubmit={onSubmit}>
      <div className="panel-heading">
        <div>
          <p className="eyebrow">Reverse chronological ledger</p>
          <h2>Request search</h2>
        </div>
      </div>
      <div className="filter-grid">
        <Field label="Exact request ID">
          <input
            onChange={(event) => onChange({ ...form, requestID: event.target.value })}
            placeholder="Open one request directly"
            value={form.requestID}
          />
        </Field>
        {canReadAll ? (
          <Field label="Tenant">
            <input
              onChange={(event) => onChange({ ...form, tenant: event.target.value })}
              placeholder="Blank searches all tenants"
              value={form.tenant}
            />
          </Field>
        ) : null}
        <Field label="Virtual model">
          <input
            onChange={(event) => onChange({ ...form, virtualModel: event.target.value })}
            value={form.virtualModel}
          />
        </Field>
        <OutcomeField
          onChange={(outcome) => onChange({ ...form, outcome })}
          value={form.outcome}
        />
        <DateField
          label="Started at or after"
          onChange={(startedAfter) => onChange({ ...form, startedAfter })}
          value={form.startedAfter}
        />
        <DateField
          label="Started before"
          onChange={(startedBefore) => onChange({ ...form, startedBefore })}
          value={form.startedBefore}
        />
        <LimitField
          onChange={(limit) => onChange({ ...form, limit })}
          value={form.limit}
        />
      </div>
      <div className="filter-actions">
        <button className="primary-button" disabled={searching} type="submit">
          {searching ? "Searching…" : form.requestID.trim() ? "Open request" : "Search requests"}
        </button>
      </div>
    </form>
  );
}

function AttemptSearch({
  form,
  canReadAll,
  searching,
  onChange,
  onSubmit,
}: {
  form: AttemptForm;
  canReadAll: boolean;
  searching: boolean;
  onChange: (form: AttemptForm) => void;
  onSubmit: (event: FormEvent) => void;
}) {
  return (
    <form className="panel filter-panel" onSubmit={onSubmit}>
      <div className="panel-heading">
        <div>
          <p className="eyebrow">Provider traffic</p>
          <h2>Attempt search</h2>
        </div>
        {!canReadAll ? <span className="activity-summary">Request ID required</span> : null}
      </div>
      <div className="filter-grid">
        <Field label="Request ID">
          <input
            required={!canReadAll}
            onChange={(event) => onChange({ ...form, requestID: event.target.value })}
            value={form.requestID}
          />
        </Field>
        <Field label="Provider">
          <input
            onChange={(event) => onChange({ ...form, provider: event.target.value })}
            value={form.provider}
          />
        </Field>
        <Field label="Deployment">
          <input
            onChange={(event) => onChange({ ...form, deployment: event.target.value })}
            value={form.deployment}
          />
        </Field>
        <OutcomeField
          onChange={(outcome) => onChange({ ...form, outcome })}
          value={form.outcome}
        />
        <DateField
          label="Started at or after"
          onChange={(startedAfter) => onChange({ ...form, startedAfter })}
          value={form.startedAfter}
        />
        <DateField
          label="Started before"
          onChange={(startedBefore) => onChange({ ...form, startedBefore })}
          value={form.startedBefore}
        />
        <LimitField
          onChange={(limit) => onChange({ ...form, limit })}
          value={form.limit}
        />
      </div>
      <div className="filter-actions">
        <button className="primary-button" disabled={searching} type="submit">
          {searching ? "Searching…" : "Search attempts"}
        </button>
      </div>
    </form>
  );
}

function RequestResults({
  records,
  nextCursor,
  busy,
  onOpen,
  onLoadMore,
}: {
  records: RequestRecord[];
  nextCursor: string;
  busy: string;
  onOpen: (requestID: string) => void;
  onLoadMore: () => void;
}) {
  return (
    <section className="panel result-panel">
      <div className="panel-heading">
        <div>
          <p className="eyebrow">Normalized request records</p>
          <h2>Requests</h2>
        </div>
        <span className="activity-summary">{records.length} loaded</span>
      </div>
      {records.length ? (
        <div className="table-wrap">
          <table className="traffic-table">
            <thead>
              <tr>
                <th>Started</th>
                <th>Request</th>
                <th>Model</th>
                <th>Final route</th>
                <th>Outcome</th>
                <th>Latency</th>
                <th>Attempts</th>
              </tr>
            </thead>
            <tbody>
              {records.map((record) => (
                <tr key={record.request_id}>
                  <td>{formatTime(record.started_at)}</td>
                  <td>
                    <button
                      className="record-link"
                      onClick={() => onOpen(record.request_id)}
                      title={record.request_id}
                      type="button"
                    >
                      {compactID(record.request_id)}
                    </button>
                  </td>
                  <td>{record.virtual_model || record.requested_model || "—"}</td>
                  <td>{routeName(record)}</td>
                  <td><OutcomePill outcome={record.outcome} /></td>
                  <td>{formatDuration(record.latency)}</td>
                  <td>{record.attempt_count}</td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      ) : (
        <div className="list-empty">
          {busy === "requests" ? "Loading requests…" : "No matching requests."}
        </div>
      )}
      {nextCursor ? (
        <div className="pagination-bar">
          <button
            className="secondary-button"
            disabled={busy === "more-requests"}
            onClick={onLoadMore}
            type="button"
          >
            {busy === "more-requests" ? "Loading…" : "Load older requests"}
          </button>
        </div>
      ) : null}
    </section>
  );
}

function AttemptResults({
  records,
  nextCursor,
  busy,
  onOpenRequest,
  onLoadMore,
}: {
  records: AttemptRecord[];
  nextCursor: string;
  busy: string;
  onOpenRequest: (requestID: string) => void;
  onLoadMore: () => void;
}) {
  return (
    <section className="panel result-panel">
      <div className="panel-heading">
        <div>
          <p className="eyebrow">Normalized upstream records</p>
          <h2>Attempts</h2>
        </div>
        <span className="activity-summary">{records.length} loaded</span>
      </div>
      {records.length ? (
        <AttemptTable records={records} onOpenRequest={onOpenRequest} />
      ) : (
        <div className="list-empty">
          {busy === "attempts" ? "Loading attempts…" : "Run an attempt search."}
        </div>
      )}
      {nextCursor ? (
        <div className="pagination-bar">
          <button
            className="secondary-button"
            disabled={busy === "more-attempts"}
            onClick={onLoadMore}
            type="button"
          >
            {busy === "more-attempts" ? "Loading…" : "Load older attempts"}
          </button>
        </div>
      ) : null}
    </section>
  );
}

function RequestDetail({
  record,
  attempts,
  attemptCursor,
  busy,
  onClose,
  onLoadMore,
}: {
  record: RequestRecord;
  attempts: AttemptRecord[];
  attemptCursor: string;
  busy: boolean;
  onClose: () => void;
  onLoadMore: () => void;
}) {
  return (
    <section className="panel request-detail">
      <div className="panel-heading">
        <div>
          <p className="eyebrow">Request drill-down</p>
          <h2 title={record.request_id}>{compactID(record.request_id, 28)}</h2>
        </div>
        <button className="text-button" onClick={onClose} type="button">Close</button>
      </div>
      <dl className="detail-grid">
        <Detail label="Tenant" value={record.tenant_id} />
        <Detail label="Principal" value={record.principal_id} />
        <Detail label="Protocol / operation" value={`${record.protocol} / ${record.operation}`} />
        <Detail label="Streaming" value={record.stream ? "Yes" : "No"} />
        <Detail label="Requested model" value={record.requested_model} />
        <Detail label="Virtual model" value={record.virtual_model} />
        <Detail label="Presented model" value={record.response_presented_model} />
        <Detail label="Final route" value={routeName(record)} />
        <Detail label="Upstream model" value={record.final_upstream_model} />
        <Detail label="HTTP status" value={record.http_status?.toString()} />
        <Detail label="Outcome" value={humanize(record.outcome)} />
        <Detail label="Failure class" value={record.failure_class} />
        <Detail label="Latency" value={formatDuration(record.latency)} />
        <Detail label="Time to first byte" value={formatDuration(record.time_to_first_byte)} />
        <Detail label="Started" value={formatTime(record.started_at)} />
        <Detail label="Completed" value={formatTime(record.completed_at)} />
        <Detail label="Config revision" value={record.config_revision} mono />
      </dl>
      <UsageSummary usage={record.usage} />
      <div className="subsection-heading">
        <div>
          <p className="eyebrow">Routing execution</p>
          <h3>Provider attempts</h3>
        </div>
        <span>{attempts.length} loaded</span>
      </div>
      {attempts.length ? <AttemptTable records={attempts} /> : <div className="list-empty">No attempts recorded.</div>}
      {attemptCursor ? (
        <div className="pagination-bar">
          <button className="secondary-button" disabled={busy} onClick={onLoadMore} type="button">
            {busy ? "Loading…" : "Load older attempts"}
          </button>
        </div>
      ) : null}
      <p className="content-boundary-note">
        The usage ledger contains operational metadata and normalized token counts only. Prompt and response content is not available on this screen.
      </p>
    </section>
  );
}

function AttemptTable({
  records,
  onOpenRequest,
}: {
  records: AttemptRecord[];
  onOpenRequest?: (requestID: string) => void;
}) {
  return (
    <div className="table-wrap">
      <table className="traffic-table">
        <thead>
          <tr>
            <th>Started</th>
            <th>Request</th>
            <th>#</th>
            <th>Provider / deployment</th>
            <th>Model</th>
            <th>Outcome</th>
            <th>Latency</th>
            <th>Retried</th>
          </tr>
        </thead>
        <tbody>
          {records.map((record) => (
            <tr key={`${record.request_id}-${record.attempt}`}>
              <td>{formatTime(record.started_at)}</td>
              <td>
                {onOpenRequest ? (
                  <button className="record-link" onClick={() => onOpenRequest(record.request_id)} type="button">
                    {compactID(record.request_id)}
                  </button>
                ) : compactID(record.request_id)}
              </td>
              <td>{record.attempt}</td>
              <td>{record.provider} / {record.deployment}</td>
              <td>{record.upstream_model}</td>
              <td><OutcomePill outcome={record.outcome} /></td>
              <td>{formatDuration(record.latency)}</td>
              <td>{record.retried ? "Yes" : "No"}</td>
            </tr>
          ))}
        </tbody>
      </table>
    </div>
  );
}

function UsageSummary({ usage }: { usage: TokenUsage }) {
  const values = [
    ["Input", usage.input_tokens],
    ["Output", usage.output_tokens],
    ["Total", usage.total_tokens],
    ["Cached input", usage.cached_input_tokens],
    ["Reasoning", usage.reasoning_tokens],
  ] as const;
  return (
    <div className="usage-summary">
      <div>
        <span>Usage completeness</span>
        <strong>{humanize(usage.completeness || "missing")}</strong>
      </div>
      {values.map(([label, value]) => (
        <div key={label}>
          <span>{label} tokens</span>
          <strong>{value?.toLocaleString() ?? "—"}</strong>
        </div>
      ))}
    </div>
  );
}

function Field({ label, children }: { label: string; children: React.ReactNode }) {
  return <label><span>{label}</span>{children}</label>;
}

function DateField({
  label,
  value,
  onChange,
}: {
  label: string;
  value: string;
  onChange: (value: string) => void;
}) {
  return <Field label={label}><input type="datetime-local" step="0.001" value={value} onChange={(event) => onChange(event.target.value)} /></Field>;
}

function OutcomeField({ value, onChange }: { value: string; onChange: (value: string) => void }) {
  return (
    <Field label="Outcome">
      <select value={value} onChange={(event) => onChange(event.target.value)}>
        <option value="">Any outcome</option>
        {outcomes.map((outcome) => <option key={outcome} value={outcome}>{humanize(outcome)}</option>)}
      </select>
    </Field>
  );
}

function LimitField({ value, onChange }: { value: number; onChange: (value: number) => void }) {
  return (
    <Field label="Page size">
      <select value={value} onChange={(event) => onChange(Number(event.target.value))}>
        {[25, 50, 100, 250, 500].map((limit) => <option key={limit} value={limit}>{limit}</option>)}
      </select>
    </Field>
  );
}

function Detail({ label, value, mono = false }: { label: string; value?: string; mono?: boolean }) {
  return <div><dt>{label}</dt><dd className={mono ? "mono" : ""}>{value || "—"}</dd></div>;
}

function OutcomePill({ outcome }: { outcome: string }) {
  const good = outcome === "success";
  return <span className={good ? "outcome-pill success" : "outcome-pill failure"}>{humanize(outcome)}</span>;
}

function requestFilters(form: RequestForm, canReadAll: boolean): RequestFilters {
  return {
    started_at_or_after: localTime(form.startedAfter),
    started_at_before: localTime(form.startedBefore),
    tenant_id: canReadAll ? form.tenant.trim() : undefined,
    virtual_model: form.virtualModel.trim(),
    outcome: form.outcome,
    limit: form.limit,
  };
}

function aggregateFilters(
  form: DashboardForm,
  canReadAll: boolean,
): LedgerAggregateFilters {
  const after = localTime(form.startedAfter);
  const before = localTime(form.startedBefore);
  if (!after || !before) throw new Error("Dashboard start and end times are required.");
  return {
    started_at_or_after: after,
    started_at_before: before,
    tenant_id: canReadAll ? form.tenant.trim() : undefined,
    virtual_model: form.virtualModel.trim(),
    provider: form.provider.trim(),
    deployment: form.deployment.trim(),
    outcome: form.outcome,
  };
}

function attemptFilters(form: AttemptForm): AttemptFilters {
  return {
    started_at_or_after: localTime(form.startedAfter),
    started_at_before: localTime(form.startedBefore),
    request_id: form.requestID.trim(),
    provider: form.provider.trim(),
    deployment: form.deployment.trim(),
    outcome: form.outcome,
    limit: form.limit,
  };
}

function localTime(value: string) {
  if (!value) return undefined;
  const parsed = new Date(value);
  if (Number.isNaN(parsed.valueOf())) throw new Error("Time filters must be valid dates.");
  return parsed.toISOString();
}

function datetimeLocal(value: Date) {
  const local = new Date(value.valueOf() - value.getTimezoneOffset() * 60_000);
  return local.toISOString().slice(0, 16);
}

function routeName(record: RequestRecord) {
  if (!record.final_provider && !record.final_deployment) return "—";
  return [record.final_provider, record.final_deployment].filter(Boolean).join(" / ");
}

function compactID(value: string, maximum = 18) {
  return value.length > maximum ? `${value.slice(0, maximum - 1)}…` : value;
}

function formatTime(value: string) {
  const parsed = new Date(value);
  return Number.isNaN(parsed.valueOf()) ? value : parsed.toLocaleString();
}

function formatDuration(nanoseconds?: number) {
  if (nanoseconds === undefined || nanoseconds === null) return "—";
  if (nanoseconds < 1_000) return `${nanoseconds} ns`;
  if (nanoseconds < 1_000_000) return `${(nanoseconds / 1_000).toFixed(1)} µs`;
  if (nanoseconds < 1_000_000_000) return `${(nanoseconds / 1_000_000).toFixed(1)} ms`;
  return `${(nanoseconds / 1_000_000_000).toFixed(2)} s`;
}

function formatCount(value: number) {
  return value.toLocaleString();
}

function formatPercent(ratio: number) {
  return new Intl.NumberFormat(undefined, {
    style: "percent",
    maximumFractionDigits: 1,
  }).format(ratio);
}

function humanize(value: string) {
  return value
    .split("_")
    .filter(Boolean)
    .map((part) => part[0]?.toUpperCase() + part.slice(1))
    .join(" ");
}

function errorMessage(error: unknown) {
  return error instanceof Error ? error.message : "Traffic history request failed.";
}

function isAbort(error: unknown) {
  return error instanceof DOMException && error.name === "AbortError";
}
