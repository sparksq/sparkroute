// SPDX-FileCopyrightText: 2026 Scitrera LLC
// SPDX-FileCopyrightText: 2026 Fox Engine Ltd.
// SPDX-License-Identifier: AGPL-3.0-only

import { type FormEvent, useCallback, useEffect, useState } from "react";
import {
  createClientCredential,
  fetchClientCredentialAudit,
  fetchClientCredentials,
  mutateClientCredential,
} from "./api";
import type {
  AdminBootstrap,
  ClientCredential,
  ClientCredentialAuditEvent,
  ClientCredentialState,
  IssuedClientCredential,
} from "./types";

export function CredentialsWorkspace({
  bootstrap,
  token,
}: {
  bootstrap: AdminBootstrap;
  token: string;
}) {
  const writable = Boolean(bootstrap.features.client_credentials_write);
  const multiTenant = bootstrap.edition === "cluster";
  const principalTenant = bootstrap.principal?.tenant ?? "";
  const [credentials, setCredentials] = useState<ClientCredential[]>([]);
  const [audit, setAudit] = useState<ClientCredentialAuditEvent[]>([]);
  const [tenantFilter, setTenantFilter] = useState("");
  const [principalFilter, setPrincipalFilter] = useState("");
  const [stateFilter, setStateFilter] = useState<ClientCredentialState | "">("");
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState("");
  const [notice, setNotice] = useState("");
  const [issued, setIssued] = useState<IssuedClientCredential>();

  const load = useCallback(async (signal?: AbortSignal) => {
    setLoading(true);
    setError("");
    try {
      const tenant = principalTenant ? undefined : tenantFilter.trim() || undefined;
      const [credentialPage, auditPage] = await Promise.all([
        fetchClientCredentials(token, {
          tenant_id: tenant,
          principal_id: principalFilter.trim() || undefined,
          state: stateFilter,
          limit: 200,
        }, signal),
        fetchClientCredentialAudit(token, { tenant_id: tenant, limit: 100 }, signal),
      ]);
      setCredentials(credentialPage.credentials);
      setAudit(auditPage.events);
    } catch (current) {
      if (!signal?.aborted) {
        setError(current instanceof Error ? current.message : "Credential request failed");
      }
    } finally {
      if (!signal?.aborted) setLoading(false);
    }
  }, [principalFilter, principalTenant, stateFilter, tenantFilter, token]);

  useEffect(() => {
    const controller = new AbortController();
    void load(controller.signal);
    return () => controller.abort();
  }, [load]);

  const mutate = async (
    credential: ClientCredential,
    action: "rotate" | "enable" | "disable" | "revoke",
  ) => {
    const warning = action === "rotate"
      ? "Rotate this credential now? Its current API key will stop working immediately."
      : action === "revoke"
        ? "Permanently revoke this credential? It cannot be enabled or rotated afterward."
        : "";
    if (warning && !window.confirm(warning)) return;
    setError("");
    setNotice("");
    try {
      const result = await mutateClientCredential(token, credential.id, action);
      if ("api_key" in result) {
        setIssued(result);
        setNotice("Credential rotated. Capture the replacement API key before dismissing it.");
      } else {
        setNotice(`Credential ${action === "enable" ? "enabled" : action + "d"}.`);
      }
      await load();
    } catch (current) {
      setError(current instanceof Error ? current.message : "Credential mutation failed");
    }
  };

  return (
    <div className="credential-workspace">
      {issued ? <IssuedSecret issued={issued} onDismiss={() => setIssued(undefined)} /> : null}
      {error ? <p className="notice error">{error}</p> : null}
      {notice ? <p className="notice success">{notice}</p> : null}
      {writable ? (
        <CreateCredentialPanel
          multiTenant={multiTenant}
          principalTenant={principalTenant}
          token={token}
          onCreated={(current) => {
            setIssued(current);
            setNotice("Credential created. Capture its API key before dismissing it.");
            void load();
          }}
          onError={setError}
        />
      ) : null}

      <section className="panel credential-list-panel">
        <div className="panel-heading">
          <div>
            <p className="eyebrow">Gateway identity</p>
            <h2>Client credentials</h2>
          </div>
          <span className="activity-summary">{credentials.length} shown</span>
        </div>
        <div className="credential-filters">
          {multiTenant && !principalTenant ? (
            <label><span>Tenant</span><input value={tenantFilter} onChange={(event) => setTenantFilter(event.target.value)} placeholder="All tenants" /></label>
          ) : multiTenant ? <span className="scope-note">Tenant enforced: {principalTenant}</span> : <span className="scope-note">Single-tenant standalone scope</span>}
          <label><span>Principal</span><input value={principalFilter} onChange={(event) => setPrincipalFilter(event.target.value)} placeholder="All principals" /></label>
          <label><span>State</span><select value={stateFilter} onChange={(event) => setStateFilter(event.target.value as ClientCredentialState | "")}>
            <option value="">All states</option><option value="active">Active</option><option value="disabled">Disabled</option><option value="revoked">Revoked</option>
          </select></label>
          <button className="secondary-button" onClick={() => void load()} type="button">Refresh</button>
        </div>
        {loading ? <div className="empty-state compact"><strong>Loading credentials</strong></div> : credentials.length ? (
          <div className="table-wrap"><table className="credential-table"><thead><tr>
            <th>Name</th>{multiTenant ? <th>Tenant</th> : null}<th>Principal</th><th>Roles</th><th>State</th><th>Expires</th>{writable ? <th>Actions</th> : null}
          </tr></thead><tbody>{credentials.map((credential) => (
            <CredentialRow key={credential.id} credential={credential} multiTenant={multiTenant} writable={writable} onMutate={mutate} />
          ))}</tbody></table></div>
        ) : <div className="empty-state compact"><strong>No credentials match this scope</strong><span>Create a credential or adjust the filters.</span></div>}
      </section>

      <section className="panel credential-audit-panel">
        <div className="panel-heading"><div><p className="eyebrow">Immutable history</p><h2>Credential audit</h2></div><span className="activity-summary">Newest first</span></div>
        {audit.length ? <div className="table-wrap"><table><thead><tr><th>Time</th><th>Action</th><th>Credential</th>{multiTenant ? <th>Tenant</th> : null}<th>Actor</th></tr></thead><tbody>{audit.map((event) => (
          <tr key={event.id}><td>{formatTime(event.occurred_at)}</td><td>{event.action}</td><td className="mono-cell">{event.credential_id}</td>{multiTenant ? <td>{event.tenant_id || "—"}</td> : null}<td>{event.actor}</td></tr>
        ))}</tbody></table></div> : <div className="empty-state compact"><strong>No credential mutations recorded</strong></div>}
      </section>
    </div>
  );
}

function CreateCredentialPanel({
  multiTenant,
  principalTenant,
  token,
  onCreated,
  onError,
}: {
  multiTenant: boolean;
  principalTenant: string;
  token: string;
  onCreated: (issued: IssuedClientCredential) => void;
  onError: (message: string) => void;
}) {
  const [name, setName] = useState("");
  const [tenant, setTenant] = useState("");
  const [principal, setPrincipal] = useState("");
  const [principalType, setPrincipalType] = useState("machine");
  const [subject, setSubject] = useState("");
  const [roles, setRoles] = useState("inference");
  const [allowed, setAllowed] = useState("");
  const [fixed, setFixed] = useState("");
  const [expires, setExpires] = useState("");
  const [saving, setSaving] = useState(false);
  const submit = async (event: FormEvent<HTMLFormElement>) => {
    event.preventDefault();
    onError("");
    let fixedAttribution: Record<string, string> | undefined;
    try {
      if (fixed.trim()) {
        const decoded = JSON.parse(fixed) as unknown;
        if (!decoded || Array.isArray(decoded) || typeof decoded !== "object") throw new Error("Fixed attribution must be a JSON object");
        const entries = Object.entries(decoded);
        if (entries.some(([, value]) => typeof value !== "string")) {
          throw new Error("Fixed attribution values must be strings");
        }
        fixedAttribution = Object.fromEntries(entries) as Record<string, string>;
      }
      setSaving(true);
      const issued = await createClientCredential(token, {
        name: name.trim(),
        tenant_id: multiTenant && !principalTenant ? tenant.trim() : undefined,
        principal_id: principal.trim(),
        principal_type: principalType.trim() || undefined,
        principal_subject: subject.trim() || undefined,
        roles: splitList(roles),
        allowed_attribution: splitList(allowed),
        fixed_attribution: fixedAttribution,
        expires_at: expires ? new Date(expires).toISOString() : undefined,
      });
      setName(""); setPrincipal(""); setSubject(""); setAllowed(""); setFixed(""); setExpires("");
      onCreated(issued);
    } catch (current) {
      onError(current instanceof Error ? current.message : "Credential creation failed");
    } finally {
      setSaving(false);
    }
  };
  return <section className="panel credential-create-panel"><div className="panel-heading"><div><p className="eyebrow">Provision identity</p><h2>Create client credential</h2></div></div>
    <form className="credential-create-form" onSubmit={submit}>
      <label><span>Name</span><input required maxLength={128} value={name} onChange={(event) => setName(event.target.value)} /></label>
      {multiTenant && !principalTenant ? <label><span>Tenant</span><input required value={tenant} onChange={(event) => setTenant(event.target.value)} /></label> : null}
      <label><span>Principal ID</span><input required value={principal} onChange={(event) => setPrincipal(event.target.value)} /></label>
      <label><span>Principal type</span><input value={principalType} onChange={(event) => setPrincipalType(event.target.value)} /></label>
      <label><span>Subject</span><input value={subject} onChange={(event) => setSubject(event.target.value)} placeholder="Optional" /></label>
      <label className="wide"><span>Roles</span><input required value={roles} onChange={(event) => setRoles(event.target.value)} placeholder="inference, trace_ingest" /></label>
      <label className="wide"><span>Allowed attribution</span><input value={allowed} onChange={(event) => setAllowed(event.target.value)} placeholder="workspace, thread_id" /></label>
      <label className="wide"><span>Fixed attribution JSON</span><input value={fixed} onChange={(event) => setFixed(event.target.value)} placeholder='{"source":"training"}' /></label>
      <label><span>Expires</span><input type="datetime-local" value={expires} onChange={(event) => setExpires(event.target.value)} /></label>
      <div className="credential-create-actions"><button disabled={saving} type="submit">{saving ? "Creating…" : "Create credential"}</button></div>
    </form>
  </section>;
}

function CredentialRow({ credential, multiTenant, writable, onMutate }: {
  credential: ClientCredential;
  multiTenant: boolean;
  writable: boolean;
  onMutate: (credential: ClientCredential, action: "rotate" | "enable" | "disable" | "revoke") => void;
}) {
  const expired = Boolean(credential.expires_at && new Date(credential.expires_at) <= new Date());
  return <tr><td><strong>{credential.name}</strong><small className="credential-id">{credential.id}</small></td>
    {multiTenant ? <td>{credential.tenant_id || "—"}</td> : null}<td>{credential.principal_id}</td><td className="role-cell">{credential.roles.join(", ")}</td>
    <td><span className={`status-pill ${credential.state === "active" && !expired ? "good" : "warning"}`}><i />{expired ? "expired" : credential.state}</span></td>
    <td>{credential.expires_at ? formatTime(credential.expires_at) : "Never"}</td>
    {writable ? <td><div className="credential-row-actions">
      {credential.state !== "revoked" ? <button className="secondary-button" onClick={() => onMutate(credential, "rotate")} type="button">Rotate</button> : null}
      {credential.state === "active" ? <button className="secondary-button" onClick={() => onMutate(credential, "disable")} type="button">Disable</button> : credential.state === "disabled" ? <button className="secondary-button" onClick={() => onMutate(credential, "enable")} type="button">Enable</button> : null}
      {credential.state !== "revoked" ? <button className="danger-button" onClick={() => onMutate(credential, "revoke")} type="button">Revoke</button> : null}
    </div></td> : null}
  </tr>;
}

function IssuedSecret({ issued, onDismiss }: { issued: IssuedClientCredential; onDismiss: () => void }) {
  const [copied, setCopied] = useState(false);
  const copy = async () => {
    if (!navigator.clipboard) return;
    await navigator.clipboard.writeText(issued.api_key);
    setCopied(true);
  };
  return <section className="issued-secret" aria-live="polite"><div><p className="eyebrow">One-time secret</p><h2>Capture this API key now</h2><p>It cannot be retrieved again. Store it in the caller's secret manager before dismissing this panel.</p></div>
    <code>{issued.api_key}</code><div className="issued-secret-actions"><button onClick={() => void copy()} type="button">{copied ? "Copied" : "Copy API key"}</button><button className="secondary-button" onClick={onDismiss} type="button">Dismiss permanently</button></div>
  </section>;
}

function splitList(value: string) {
  return value.split(",").map((item) => item.trim()).filter(Boolean);
}

function formatTime(value: string) {
  return new Intl.DateTimeFormat(undefined, { dateStyle: "medium", timeStyle: "short" }).format(new Date(value));
}
