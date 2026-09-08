import { useEffect, useRef, useState } from "react";
import { fetchStatus, probeMMProjection } from "./api";
import type { ConfigurationDocument, MMProjectionStatus } from "./types";

type Settings = Record<string, unknown>;
const object = (value: unknown): Settings => value && typeof value === "object" && !Array.isArray(value) ? value as Settings : {};

export function MMProjectionSettingsEditor({ document, disabled, onChange, token, canReadStatus, runtimeRevision }: {
  document: ConfigurationDocument;
  disabled: boolean;
  onChange: (document: ConfigurationDocument) => void;
  token: string;
  canReadStatus: boolean;
  runtimeRevision: string;
}) {
  const settings = object(document.mm_projection);
  const mode = document.mm_projection == null ? "inherit" : settings.enabled ? "enabled" : "disabled";
  const [status, setStatus] = useState<MMProjectionStatus>();
  const [probing, setProbing] = useState(false);
  const [error, setError] = useState("");
  const requestID = useRef(0);

  useEffect(() => {
    const id = ++requestID.current;
    setStatus(undefined);
    setError("");
    setProbing(false);
    if (canReadStatus) {
      void fetchStatus(token).then((result) => {
        if (id === requestID.current) setStatus(result.mm_projection);
      }).catch((value: unknown) => {
        if (id === requestID.current) setError(value instanceof Error ? value.message : "Could not load bridge status");
      });
    }
    return () => { requestID.current++; };
  }, [token, canReadStatus, runtimeRevision]);

  function update(value: Settings | undefined) {
    const next = { ...document };
    if (value === undefined) delete next.mm_projection;
    else next.mm_projection = value;
    onChange(next);
  }
  function field(name: string, value: unknown) { update({ ...settings, [name]: value }); }
  async function probe() {
    const id = ++requestID.current;
    setProbing(true);
    setError("");
    try {
      const result = await probeMMProjection(token);
      if (id === requestID.current) setStatus(result);
    } catch (value) {
      if (id === requestID.current) setError(value instanceof Error ? value.message : "Bridge test failed");
    } finally {
      if (id === requestID.current) setProbing(false);
    }
  }

  return <section className="model-section mmbridge-settings" aria-label="MMBridge projection settings">
    <h3>MMBridge projection</h3>
    <p className="section-help">Connect to MMBridge to turn multimedia input into text for models that need it. Configure when projection runs, the analyzer, and failure behavior under Model Routing.</p>
    <fieldset disabled={disabled} className="recipe-fieldset">
      <label>Projection connection
        <select aria-label="Projection connection" value={mode} onChange={(event) => update(event.target.value === "inherit" ? undefined : { ...settings, enabled: event.target.value === "enabled" })}>
          <option value="inherit">Use startup settings</option>
          <option value="disabled">Disabled</option>
          <option value="enabled">Enabled</option>
        </select>
        <small>Use startup settings follows command-line flags or environment variables; projection is disabled if none are supplied.</small>
      </label>
      {mode === "enabled" ? <div className="model-field-grid">
        <label>Bridge base URL
          <input aria-label="Bridge base URL" type="url" value={String(settings.url ?? "")} placeholder="http://localhost:8100/v1" onChange={(event) => field("url", event.target.value)} />
          <small>The MMBridge /v1 base URL, reachable from the gateway host.</small>
        </label>
        <label>Bridge token reference
          <input aria-label="Bridge token reference" value={String(settings.token_ref ?? "")} placeholder="env://MMBRIDGE_TOKEN" autoComplete="off" spellCheck={false} onChange={(event) => field("token_ref", event.target.value)} />
          <small>Reference the shared bearer token, for example env://MMBRIDGE_TOKEN. Set the variable on the gateway host; enter a reference here, not the secret.</small>
        </label>
        <label>Default analyzer model
          <input aria-label="Default analyzer model" value={String(settings.analyzer_model ?? "")} placeholder="Selected by routing policy" onChange={(event) => field("analyzer_model", event.target.value)} />
          <small>A virtual model capable of analyzing the media. Routing policies can override this default.</small>
        </label>
        <label>Default projection timeout (seconds)
          <input aria-label="Default projection timeout (seconds)" type="number" min={1} max={900} step={1} value={Number(settings.timeout_ms || 600000) / 1000} onChange={(event) => field("timeout_ms", event.target.value === "" ? undefined : Number(event.target.value) * 1000)} />
          <small>Maximum wait for a projection, unless a routing policy overrides it. Default: 10 minutes.</small>
        </label>
      </div> : null}
    </fieldset>
    <p className="section-help">Validate, then Save to apply connection changes. Requests in progress finish with their original settings.</p>
    {canReadStatus ? <div className="mmbridge-active-connection">
      <div className="panel-heading">
        <h4>Active connection</h4>
        <div className="panel-actions">
          {status ? <span className={`status-pill ${status.state === "healthy" || status.state === "disabled" ? "good" : "warning"}`}>{status.state.replaceAll("_", " ")}</span> : <span>{error ? "Status unavailable" : "Loading status…"}</span>}
          <button className="secondary-button" type="button" disabled={probing || !status?.configured} onClick={() => void probe()}>{probing ? "Testing…" : "Test bridge"}</button>
        </div>
      </div>
      <p className="section-help">Test bridge checks the saved, active connection. Save changes before testing; this checks bridge authentication and protocol support, without sending model requests.</p>
      {status?.configured ? <dl className="projection-status-facts">
        <div><dt>Analyzer model</dt><dd>{status.analyzer_model || "Selected by routing policy"}</dd></div>
        <div><dt>Projection API</dt><dd>{status.projection_api ? `v${status.projection_api}` : "Not tested"}</dd></div>
        <div><dt>Discovered models</dt><dd>{status.projection_api ? status.models : "Not tested"}</dd></div>
        <div><dt>Circuit</dt><dd>{status.circuit_open ? "Open" : "Closed"}</dd></div>
        <div><dt>Last successful test or projection</dt><dd>{status.last_success_at ? new Date(status.last_success_at).toLocaleString() : "Not observed"}</dd></div>
      </dl> : null}
      {status?.last_error ? <p className="notice error">Last result: {status.last_error.replaceAll("_", " ")}</p> : null}
      {error ? <p className="notice error">{error}</p> : null}
    </div> : null}
  </section>;
}
