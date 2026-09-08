import type { JSONObject, VirtualModelEditorContext } from "./extensions";

const object = (value: unknown): JSONObject => value && typeof value === "object" && !Array.isArray(value) ? value as JSONObject : {};
const phases = [{ key: "pre", label: "Request", help: "Run these checks before sending the request to its model." },
  { key: "post", label: "Response", help: "Run these checks before returning the response to the caller." }] as const;
type Phase = typeof phases[number]["key"];

export function GuardrailEditor({ model, modelNames, disabled, updateModel, profile = false }: VirtualModelEditorContext & { modelNames: string[]; profile?: boolean }) {
  const policy = object(model.guardrails);
  const names = [...new Set(modelNames)].sort();
  const shapeError = phases.some(({ key }) => policy[key] !== undefined &&
    (!Array.isArray(policy[key]) || (policy[key] as unknown[]).some(value => value === null || typeof value !== "object" || Array.isArray(value)))) ||
    policy.stream !== undefined && (policy.stream === null || typeof policy.stream !== "object" || Array.isArray(policy.stream));
  const entries = (phase: Phase): JSONObject[] => Array.isArray(policy[phase]) ? policy[phase] as JSONObject[] : [];
  const count = entries("pre").length + entries("post").length;

  function change(transform: (current: JSONObject) => JSONObject) {
    if (disabled || shapeError) return;
    updateModel(current => {
      const guardrails = transform({ ...object(current.guardrails) });
      const result = { ...current };
      if (Object.keys(guardrails).length) result.guardrails = guardrails; else delete result.guardrails;
      return result;
    });
  }
  function changeEntries(phase: Phase, transform: (values: JSONObject[]) => JSONObject[]) {
    change(current => {
      const next = transform([...(current[phase] as JSONObject[] ?? [])]);
      if (next.length) current[phase] = next; else delete current[phase];
      if (phase === "post" && !next.length) delete current.stream;
      return current;
    });
  }
  function set(phase: Phase, index: number, field: string, value: unknown) {
    changeEntries(phase, values => values.map((entry, i) => {
      if (i !== index) return entry;
      const next = { ...entry, [field]: value };
      if (value === undefined) delete next[field];
      return next;
    }));
  }
  function add(phase: Phase) {
    const used = new Set([...entries("pre"), ...entries("post")].map(entry => entry.name));
    const base = phase === "pre" ? "request-check" : "response-check";
    let name = base;
    for (let i = 2; used.has(name); i++) name = `${base}-${i}`;
    changeEntries(phase, values => [...values, { name, model: names.find(name => name !== model.name) ?? names[0] ?? "" }]);
  }
  function move(phase: Phase, index: number, offset: number) {
    changeEntries(phase, values => {
      const [entry] = values.splice(index, 1);
      values.splice(index + offset, 0, entry!);
      return values;
    });
  }
  function streamSize(field: string, value: string) {
    change(current => {
      const stream = { ...object(current.stream), [field]: Number(value) };
      if (value === "") delete stream[field];
      return { ...current, stream };
    });
  }

  return <details className="model-section guardrail-settings" open={profile || undefined}>
    <summary><span>Guardrails</span><small>{count ? `${entries("pre").length} request, ${entries("post").length} response` : "Not configured"}</small></summary>
    <p className="section-help">Apply checks to {profile ? "every model assigned this profile" : "this model and its request profiles"} to allow, block, or optionally rewrite content. Checks run in the order shown and may send content to the selected model’s provider.</p>
    {shapeError ? <p className="notice error" role="alert">Guardrail lists or streaming settings have an invalid shape. Use JSON mode to repair them; existing values are preserved.</p> : <fieldset disabled={disabled} className="policy-editor-fields">
      {phases.map(({ key, label, help }) => <section className="guardrail-phase" key={key} aria-label={`${label} guardrails`}>
        <div className="model-section-heading"><h4>{label} checks</h4>
          <button type="button" className="secondary-button" disabled={!names.length || entries(key).length >= 8} onClick={() => add(key)}>Add {label.toLowerCase()} guardrail</button>
        </div>
        <p className="section-help">{help}{!entries(key).length && " No checks configured."}</p>
        {entries(key).map((entry, index) => <article className="guardrail-card" key={index} aria-label={`${label} guardrail ${index + 1}`}>
          <div className="guardrail-card-heading"><strong>{index + 1}. {String(entry.name || "Unnamed check")}</strong>
            <div className="guardrail-actions">
              <button type="button" className="text-button" aria-label={`Move ${label.toLowerCase()} guardrail ${index + 1} up`} disabled={index === 0} onClick={() => move(key, index, -1)}>↑</button>
              <button type="button" className="text-button" aria-label={`Move ${label.toLowerCase()} guardrail ${index + 1} down`} disabled={index === entries(key).length - 1} onClick={() => move(key, index, 1)}>↓</button>
              <button type="button" className="text-button danger-text" aria-label={`Remove ${label.toLowerCase()} guardrail ${index + 1}`} onClick={() => changeEntries(key, values => values.filter((_, i) => i !== index))}>Remove</button>
            </div>
          </div>
          <div className="model-field-grid">
            <label>Check name<input value={String(entry.name ?? "")} onChange={e => set(key, index, "name", e.target.value)} required /></label>
            <label>Guardrail model<select value={String(entry.model ?? "")} onChange={e => set(key, index, "model", e.target.value)}>
              {!names.includes(String(entry.model ?? "")) && <option value={String(entry.model ?? "")}>{entry.model ? `${entry.model} (unavailable)` : "Choose a virtual model"}</option>}
              {names.map(name => <option value={name} key={name}>{name}</option>)}
            </select><small>Choose a model that can evaluate content and return JSON verdicts. Hidden models and aliases are supported.</small></label>
            <label className="wide">Instructions<textarea rows={3} value={String(entry.prompt ?? "")} onChange={e => set(key, index, "prompt", e.target.value || undefined)} placeholder="Describe what the check should allow or block." />
              <small>Optional. SparkRoute supplies the verdict format; add your policy here.</small></label>
            <label>If the check fails<select value={String(entry.failure_mode || "fail_closed")} onChange={e => set(key, index, "failure_mode", e.target.value)}>
              <option value="fail_closed">Block the request or response</option><option value="fail_open">Continue without this check</option>
            </select><small>Applies when the check errors or returns an invalid verdict. An explicit block verdict always blocks.</small></label>
            <label className="checkbox-field"><input type="checkbox" checked={entry.allow_replacement === true} onChange={e => set(key, index, "allow_replacement", e.target.checked)} /><span>Allow content replacement</span></label>
          </div>
        </article>)}
      </section>)}
      <div className="guardrail-stream">
        <label className="checkbox-field"><input type="checkbox" checked={policy.stream !== undefined} disabled={!entries("post").length && policy.stream === undefined} onChange={e => change(current => { if (e.target.checked) current.stream = {}; else delete current.stream; return current; })} /><span>Screen streaming responses</span></label>
        <p className="section-help">Check buffered output before releasing it. Requires a response guardrail; removing the last response guardrail turns this off. Streaming checks allow or block content and cannot replace it.</p>
        {policy.stream !== undefined && <div className="model-field-grid">
          <label>Screening window (bytes)<input type="number" min={1024} max={1048576} step={1} placeholder="16384" value={Number(object(policy.stream).window_bytes) || ""} onChange={e => streamSize("window_bytes", e.target.value)} /><small>Default: 16 KiB. Allowed: 1 KiB–1 MiB.</small></label>
          <label>Approved context (bytes)<input type="number" min={0} max={4194304} step={1} placeholder="65536" value={Number(object(policy.stream).context_bytes) || ""} onChange={e => streamSize("context_bytes", e.target.value)} /><small>Default: 64 KiB of previously approved output; maximum 4 MiB.</small></label>
        </div>}
      </div>
    </fieldset>}
  </details>;
}
