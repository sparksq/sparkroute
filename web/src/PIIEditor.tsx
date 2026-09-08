import type { JSONObject, VirtualModelEditorContext, VirtualModelEditorExtension } from "./extensions";

const defaultPIIEntities = ["email", "phone", "ssn", "credit_card", "ipv4"] as const;

export const piiVirtualModelExtension: VirtualModelEditorExtension = {
  id: "privacy-pii",
  available: bootstrap => Boolean(bootstrap.features.privacy_pii),
  Component: PIIVirtualModelSection,
  validateModel: validatePIIModel,
};

export function PIIVirtualModelSection({ model, disabled, updateModel }: VirtualModelEditorContext) {
  const privacy = objectValue(model.privacy);
  const pii = objectValue(privacy.pii);
  const files = objectValue(pii.files);
  const enabled = isObject(privacy.pii) && stringValue(pii.mode) !== "disabled";
  const fileInspection = (stringValue(files.text) !== "" && stringValue(files.text) !== "disabled") || booleanValue(files.metadata);
  const entities = enabled && stringArray(pii.entities).length
    ? stringArray(pii.entities)
    : [...defaultPIIEntities];
  return (
    <details className="model-section pii-settings">
      <summary>
        <span>PII privacy</span>
        <small>{enabled ? `${entities.length} entities · ${stringValue(pii.scope) || "request"} scope` : "Disabled"}</small>
      </summary>
      <p className="section-help">
        Replace detected personal information with opaque tokens before sending content to providers. Changes apply to this model and its request profiles. Restore known values for the caller or keep them masked. Detection is best effort and does not cover every kind of personal information.
      </p>
      {enabled && <p className="section-help">Conversation scope uses encrypted mappings and requires caller authentication. In standalone SparkRoute, send X-SparkRoute-Thread-Id with a stable conversation ID; mappings stay isolated per caller. Request scope works without persistent mappings. File inspection selects conversation scope automatically.</p>}
      <div className="policy-fields">
        <label className="checkbox-field wide">
          <input checked={enabled} disabled={disabled} onChange={(event) => updateModel((current) => setPIIEnabled(current, event.target.checked))} type="checkbox" />
          <span>Enable PII substitution</span>
        </label>
        {enabled ? (
          <>
            <Field label="Mapping scope">
              <select disabled={disabled} value={stringValue(pii.scope) || "request"} onChange={(event) => updateModel((current) => setPIIString(current, "scope", event.target.value))}>
                <option disabled={fileInspection} value="request">This request only</option>
                <option value="conversation">Across the conversation</option>
              </select>
            </Field>
            <Field label="Caller response">
              <select disabled={disabled} value={stringValue(pii.response) || "restore"} onChange={(event) => updateModel((current) => setPIIString(current, "response", event.target.value))}>
                <option value="restore">Restore known tokens</option>
                <option value="masked">Keep masked</option>
              </select>
            </Field>
            <Field label="Saved trace content">
              <select disabled={disabled} value={stringValue(pii.trace_content) || "masked"} onChange={(event) => updateModel((current) => setPIIString(current, "trace_content", event.target.value))}>
                <option value="masked">Masked</option>
                <option value="disabled">Disabled</option>
                <option value="caller_visible">What the caller sees (may contain PII)</option>
              </select>
            </Field>
            <Field label="Inspection failure">
              <select disabled={disabled} value={stringValue(pii.failure_mode) || "fail_closed"} onChange={(event) => updateModel((current) => setPIIString(current, "failure_mode", event.target.value))}>
                <option value="fail_closed">Block if inspection fails</option>
                <option value="fail_open">Continue if inspection fails</option>
              </select>
            </Field>
            <Field label="Multimedia extracted text">
              <select disabled={disabled} value={stringValue(pii.media_text) || "best_effort"} onChange={(event) => updateModel((current) => setPIIString(current, "media_text", event.target.value))}>
                <option value="best_effort">Best effort</option>
                <option value="required">Require MMBridge attestation</option>
              </select>
            </Field>
            <Field label="Text file contents">
              <select disabled={disabled} value={stringValue(files.text) || "disabled"} onChange={(event) => updateModel((current) => setPIIFileText(current, event.target.value))}>
                <option value="disabled">Disabled</option>
                <option value="best_effort">Best effort</option>
                <option value="required">Required typed text</option>
              </select>
            </Field>
            <Field label="Maximum text-file bytes">
              <input disabled={disabled} min="1" max={32 << 20} placeholder={String(4 << 20)} step="1" type="number" value={numberInput(files.max_text_bytes)} onChange={(event) => updateModel((current) => setPIIFileNumber(current, "max_text_bytes", event.target.value))} />
            </Field>
            <label className="checkbox-field">
              <input checked={booleanValue(files.metadata)} disabled={disabled} onChange={(event) => updateModel((current) => setPIIFileBoolean(current, "metadata", event.target.checked))} type="checkbox" />
              <span>Inspect filename and purpose metadata</span>
            </label>
            <div className="wide pii-entities">
              <span className="field-label">Detected entities</span>
              <div className="capability-grid">
                {defaultPIIEntities.map((entity) => (
                  <label key={entity}>
                    <input checked={entities.includes(entity)} disabled={disabled || entities.length === 1 && entities.includes(entity)} onChange={(event) => updateModel((current) => togglePIIEntity(current, entity, event.target.checked))} type="checkbox" />
                    <span>{humanize(entity)}</span>
                  </label>
                ))}
              </div>
              <small>Keep at least one entity selected, or turn substitution off. The built-in detector covers these five entity types. Names and street addresses require an additional detector.</small>
            </div>
          </>
        ) : null}
      </div>
    </details>
  );
}

function validatePIIModel(model: JSONObject, index: number) {
  if (model.privacy !== undefined && !isObject(model.privacy)) return `virtual_models[${index}].privacy must be an object.`;
  const privacy = objectValue(model.privacy);
  if (privacy.pii !== undefined && !isObject(privacy.pii)) return `virtual_models[${index}].privacy.pii must be an object.`;
  const pii = objectValue(privacy.pii);
  if (pii.files !== undefined && !isObject(pii.files)) return `virtual_models[${index}].privacy.pii.files must be an object.`;
  return undefined;
}

function Field({ label, children }: { label: string; children: React.ReactNode }) {
  return <label><span>{label}</span>{children}</label>;
}

function setPIIEnabled(model: JSONObject, enabled: boolean) {
  return setNested(model, "privacy", (privacy) => ({ ...privacy, pii: enabled ? { mode: "substitute" } : { mode: "disabled" } }));
}

function updatePII(model: JSONObject, transform: (pii: JSONObject) => JSONObject) {
  return setNested(model, "privacy", (privacy) => setNested(privacy, "pii", transform));
}

function setPIIString(model: JSONObject, field: string, value: string) {
  return updatePII(model, (pii) => setString(pii, field, value));
}

function updatePIIFiles(model: JSONObject, transform: (files: JSONObject) => JSONObject) {
  return updatePII(model, (pii) => setNested(pii, "files", transform));
}

function setPIIFileText(model: JSONObject, value: string) {
  let next = updatePIIFiles(model, (files) => setString(files, "text", value));
  if (value !== "disabled") next = setPIIString(next, "scope", "conversation");
  return next;
}

function setPIIFileNumber(model: JSONObject, field: string, value: string) {
  return updatePIIFiles(model, (files) => setNumber(files, field, value));
}

function setPIIFileBoolean(model: JSONObject, field: string, value: boolean) {
  let next = updatePIIFiles(model, (files) => setBoolean(files, field, value));
  if (value) next = setPIIString(next, "scope", "conversation");
  return next;
}

function togglePIIEntity(model: JSONObject, entity: string, enabled: boolean) {
  const pii = objectValue(objectValue(model.privacy).pii);
  const configured = stringArray(pii.entities);
  const current = configured.length ? configured : [...defaultPIIEntities];
  const next = enabled ? [...new Set([...current, entity])] : current.filter((value) => value !== entity);
  return updatePII(model, (policy) => setStringArray(policy, "entities", next));
}

function setNested(model: JSONObject, parent: string, transform: (nested: JSONObject) => JSONObject) {
  const next = { ...model };
  const nested = transform({ ...objectValue(next[parent]) });
  if (Object.keys(nested).length) next[parent] = nested;
  else delete next[parent];
  return next;
}

function setString(model: JSONObject, field: string, value: string) {
  const next = { ...model };
  if (value) next[field] = value;
  else delete next[field];
  return next;
}

function setNumber(model: JSONObject, field: string, value: string) {
  const next = { ...model };
  if (value !== "") next[field] = Number(value);
  else delete next[field];
  return next;
}

function setBoolean(model: JSONObject, field: string, value: boolean) {
  const next = { ...model };
  if (value) next[field] = true;
  else delete next[field];
  return next;
}

function setStringArray(model: JSONObject, field: string, values: string[]) {
  const next = { ...model };
  if (values.length) next[field] = values;
  else delete next[field];
  return next;
}

function objectValue(value: unknown): JSONObject {
  return isObject(value) ? value : {};
}

function stringArray(value: unknown): string[] {
  return Array.isArray(value) ? value.filter((current): current is string => typeof current === "string") : [];
}

function stringValue(value: unknown) {
  return typeof value === "string" ? value : "";
}

function numberInput(value: unknown) {
  return typeof value === "number" && Number.isFinite(value) ? value : "";
}

function booleanValue(value: unknown) {
  return value === true;
}

function isObject(value: unknown): value is JSONObject {
  return Boolean(value) && typeof value === "object" && !Array.isArray(value);
}

function humanize(value: string) {
  const labels: Record<string, string> = { ipv4: "IPv4", postgres: "PostgreSQL", sqlite: "SQLite", ssn: "SSN" };
  if (labels[value]) return labels[value];
  return value.split("_").filter(Boolean).map((part) => part[0]?.toUpperCase() + part.slice(1)).join(" ");
}
