// SPDX-FileCopyrightText: 2026 Scitrera LLC
// SPDX-FileCopyrightText: 2026 Fox Engine Ltd.
// SPDX-License-Identifier: AGPL-3.0-only

import { type FormEvent, type ReactNode, useState } from "react";
import { fetchSavedTraceExport } from "./api";
import type { AdminBootstrap, SavedTraceExportFilters } from "./types";

interface ExportForm {
	format: "jsonl" | "dataset";
	projection: "canonical" | "deepspec" | "mlflow";
	requireComplete: boolean;
  startedAfter: string;
  startedBefore: string;
  tenant: string;
  conversationID: string;
  responseID: string;
  parentResponseID: string;
  sessionID: string;
  protocol: string;
  operation: string;
  requestedModel: string;
  virtualModel: string;
  provider: string;
  deployment: string;
  outcome: string;
  captureOutcome: string;
  metadata: string;
  maxRecords: string;
}

const initialForm: ExportForm = {
	format: "jsonl",
	projection: "canonical",
	requireComplete: false,
  startedAfter: "",
  startedBefore: "",
  tenant: "",
  conversationID: "",
  responseID: "",
  parentResponseID: "",
  sessionID: "",
  protocol: "",
  operation: "",
  requestedModel: "",
  virtualModel: "",
  provider: "",
  deployment: "",
  outcome: "",
  captureOutcome: "",
  metadata: "",
  maxRecords: "100",
};

export function TraceExportWorkspace({
  bootstrap,
  token,
}: {
  bootstrap: AdminBootstrap;
  token: string;
}) {
  const roles = bootstrap.principal?.roles ?? [];
  const canReadAll = roles.includes("trace_read_all");
  const [form, setForm] = useState(initialForm);
  const [confirmed, setConfirmed] = useState(false);
  const [notice, setNotice] = useState<{ kind: "success" | "error"; text: string }>();
  const [busy, setBusy] = useState(false);

  const exportTraces = async (event: FormEvent) => {
    event.preventDefault();
    if (!confirmed) {
      setNotice({ kind: "error", text: "Confirm the sensitive-data warning before exporting." });
      return;
    }
    setBusy(true);
    setNotice(undefined);
    try {
      const blob = await fetchSavedTraceExport(token, exportFilters(form, canReadAll));
      const url = URL.createObjectURL(blob);
      const anchor = document.createElement("a");
      anchor.href = url;
      anchor.download = form.format === "dataset" ? "sparkroute-trace-dataset.zip" : "sparkroute-traces.jsonl";
      anchor.rel = "noopener";
      anchor.click();
      setTimeout(() => URL.revokeObjectURL(url), 0);
      setNotice({
        kind: "success",
        text: `Exported ${blob.size.toLocaleString()} bytes. Store the file encrypted and access-controlled.`,
      });
    } catch (error) {
      setNotice({
        kind: "error",
        text: error instanceof Error ? error.message : "Saved-trace export failed.",
      });
    } finally {
      setBusy(false);
    }
  };

  return (
    <div className="trace-workspace">
      <section className="trace-warning" role="note">
        <div className="warning-icon" aria-hidden="true">!</div>
        <div>
          <strong>Exports contain caller-visible inference inputs and outputs</strong>
          <p>
            Use only for authorized training or reinforcement-learning workflows.
            Keep downloaded JSONL encrypted, access-controlled, and governed by a retention policy.
          </p>
        </div>
      </section>

      <form className="panel trace-export-panel" onSubmit={exportTraces}>
        <div className="panel-heading">
          <div>
            <p className="eyebrow">Exact-match saved-trace selection</p>
            <h2>{form.format === "dataset" ? "Export training dataset" : "Export trace JSONL"}</h2>
          </div>
          <span className="activity-summary">
            {canReadAll
              ? "Cross-tenant role enabled"
              : `Tenant enforced: ${bootstrap.principal?.tenant || "unavailable"}`}
          </span>
        </div>
        <div className="filter-grid trace-filter-grid">
          <Field label="Export format">
            <select value={form.format} onChange={(event) => setForm({ ...form, format: event.target.value as ExportForm["format"] })}>
              <option value="jsonl">Raw canonical JSONL</option>
              <option value="dataset">Manifest-backed dataset zip</option>
            </select>
          </Field>
          <Field label="Dataset projection">
            <select disabled={form.format !== "dataset"} value={form.projection} onChange={(event) => setForm({ ...form, projection: event.target.value as ExportForm["projection"] })}>
              <option value="canonical">Canonical trace records</option>
              <option value="deepspec">DeepSpec conversations</option>
              <option value="mlflow">MLflow inputs / outputs</option>
            </select>
          </Field>
          <DateField
            label="Started at or after"
            onChange={(startedAfter) => setForm({ ...form, startedAfter })}
            value={form.startedAfter}
          />
          <DateField
            label="Started before"
            onChange={(startedBefore) => setForm({ ...form, startedBefore })}
            value={form.startedBefore}
          />
          {canReadAll ? (
            <Field label="Tenant">
              <input
                onChange={(event) => setForm({ ...form, tenant: event.target.value })}
                placeholder="Blank exports all tenants"
                value={form.tenant}
              />
            </Field>
          ) : null}
          <Field label="Conversation ID">
            <input onChange={(event) => setForm({ ...form, conversationID: event.target.value })} value={form.conversationID} />
          </Field>
          <Field label="Response ID">
            <input onChange={(event) => setForm({ ...form, responseID: event.target.value })} value={form.responseID} />
          </Field>
          <Field label="Parent response ID">
            <input onChange={(event) => setForm({ ...form, parentResponseID: event.target.value })} value={form.parentResponseID} />
          </Field>
          <Field label="Caller session ID">
            <input onChange={(event) => setForm({ ...form, sessionID: event.target.value })} value={form.sessionID} />
          </Field>
          <Field label="Protocol">
            <input onChange={(event) => setForm({ ...form, protocol: event.target.value })} value={form.protocol} />
          </Field>
          <Field label="Operation">
            <input onChange={(event) => setForm({ ...form, operation: event.target.value })} placeholder="chat_completions" value={form.operation} />
          </Field>
          <Field label="Requested model">
            <input onChange={(event) => setForm({ ...form, requestedModel: event.target.value })} value={form.requestedModel} />
          </Field>
          <Field label="Virtual model">
            <input onChange={(event) => setForm({ ...form, virtualModel: event.target.value })} value={form.virtualModel} />
          </Field>
          <Field label="Provider">
            <input onChange={(event) => setForm({ ...form, provider: event.target.value })} value={form.provider} />
          </Field>
          <Field label="Deployment">
            <input onChange={(event) => setForm({ ...form, deployment: event.target.value })} value={form.deployment} />
          </Field>
          <Field label="Outcome">
            <input onChange={(event) => setForm({ ...form, outcome: event.target.value })} placeholder="success" value={form.outcome} />
          </Field>
          <Field label="Capture outcome">
            <select onChange={(event) => setForm({ ...form, captureOutcome: event.target.value })} value={form.captureOutcome}>
              <option value="">Any capture outcome</option>
              <option value="complete">Complete</option>
              <option value="truncated">Truncated</option>
              <option value="incomplete">Incomplete stream</option>
            </select>
          </Field>
          <Field label="Maximum records (blank or 0 means all)">
            <input
              min="0"
              onChange={(event) => setForm({ ...form, maxRecords: event.target.value })}
              step="1"
              type="number"
              value={form.maxRecords}
            />
          </Field>
          <Field label="Metadata filters, one key=value per line" wide>
            <textarea
              onChange={(event) => setForm({ ...form, metadata: event.target.value })}
              placeholder={"sparkroute.workspace=workspace-a\nsparkroute.task_id=task-a"}
              value={form.metadata}
            />
          </Field>
        </div>
        <div className="export-confirmation">
          <label>
            <input
              checked={confirmed}
              onChange={(event) => setConfirmed(event.target.checked)}
              type="checkbox"
            />
            <span>I understand this download can contain sensitive inference content.</span>
          </label>
          {form.format === "dataset" ? (
            <label title="Historical exports need an explicit end time covered by completed or later-updated recorder sessions.">
              <input checked={form.requireComplete} onChange={(event) => setForm({ ...form, requireComplete: event.target.checked })} type="checkbox" />
              <span>Reject unless capture completeness is proven.</span>
            </label>
          ) : null}
          <button className="primary-button" disabled={busy || !confirmed} type="submit">
            {busy ? "Preparing export…" : form.format === "dataset" ? "Export dataset" : "Export JSONL"}
          </button>
        </div>
        {notice ? <div className={`notice ${notice.kind}`} role="status">{notice.text}</div> : null}
        <p className="content-boundary-note">
          The browser does not preview trace payloads. Filters are conjunctive exact matches,
          and authorization is enforced again by the export API.
        </p>
      </form>
    </div>
  );
}

function Field({
  label,
  wide = false,
  children,
}: {
  label: string;
  wide?: boolean;
  children: ReactNode;
}) {
  return <label className={wide ? "wide" : ""}><span>{label}</span>{children}</label>;
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
  return (
    <Field label={label}>
      <input type="datetime-local" step="0.001" value={value} onChange={(event) => onChange(event.target.value)} />
    </Field>
  );
}

function exportFilters(form: ExportForm, canReadAll: boolean): SavedTraceExportFilters {
  const maximum = form.maxRecords.trim();
  let maxRecords: number | undefined;
  if (maximum) {
    maxRecords = Number(maximum);
    if (!Number.isSafeInteger(maxRecords) || maxRecords < 0) {
      throw new Error("Maximum records must be a non-negative integer.");
    }
  }
  return {
    started_at_or_after: localTime(form.startedAfter),
    started_at_before: localTime(form.startedBefore),
    tenant_id: canReadAll ? form.tenant.trim() : undefined,
    conversation_id: form.conversationID.trim(),
    response_id: form.responseID.trim(),
    parent_response_id: form.parentResponseID.trim(),
    session_id: form.sessionID.trim(),
    protocol: form.protocol.trim(),
    operation: form.operation.trim(),
    requested_model: form.requestedModel.trim(),
    virtual_model: form.virtualModel.trim(),
    provider: form.provider.trim(),
    deployment: form.deployment.trim(),
    outcome: form.outcome.trim(),
    capture_outcome: form.captureOutcome,
    metadata: metadataFilters(form.metadata),
    max_records: maxRecords,
    format: form.format,
    projection: form.format === "dataset" ? form.projection : undefined,
    require_complete: form.format === "dataset" ? form.requireComplete : undefined,
  };
}

function metadataFilters(value: string) {
  const filters: string[] = [];
  const keys = new Set<string>();
  value.split(/\r?\n/).forEach((current) => {
    const line = current.trim();
    if (!line) return;
    const separator = line.indexOf("=");
    const key = separator < 0 ? "" : line.slice(0, separator).trim();
    if (!key) throw new Error(`Metadata filter "${line}" must be key=value.`);
    if (keys.has(key)) throw new Error(`Metadata key "${key}" was supplied more than once.`);
    keys.add(key);
    filters.push(`${key}=${line.slice(separator + 1)}`);
  });
  return filters.length ? filters : undefined;
}

function localTime(value: string) {
  if (!value) return undefined;
  const parsed = new Date(value);
  if (Number.isNaN(parsed.valueOf())) throw new Error("Time filters must be valid dates.");
  return parsed.toISOString();
}
