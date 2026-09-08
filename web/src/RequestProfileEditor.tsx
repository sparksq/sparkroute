import { useEffect, useState } from "react";
import type { JSONObject } from "./extensions";

export function RequestProfileEditor({model, names, disabled, readOnly, onAdd, onChange}: {
  model: JSONObject; names: string[]; disabled: boolean; readOnly: boolean;
  onAdd: (profile: JSONObject) => void; onChange: (profile: JSONObject) => void;
}) {
  const [suffix, setSuffix] = useState("low");
  const [operation, setOperation] = useState("chat_completions");
  const [parameters, setParameters] = useState('{"reasoning_effort":"low"}');
  const [error, setError] = useState("");
  const [editing, setEditing] = useState(false);
  useEffect(() => {setEditing(false); setError("");}, [model.name]);
  function values() {
    const parsed: unknown = JSON.parse(parameters);
    if (!parsed || Array.isArray(parsed) || typeof parsed !== "object") throw new Error("Parameters must be a JSON object.");
    return parsed;
  }
  function apply(add: boolean) {
    try {
      const overrides = values();
      const name = add ? `${model.name}:${suffix.trim()}` : String(model.name);
      if (add && (!/^[A-Za-z0-9][A-Za-z0-9_-]*$/.test(suffix.trim()) || names.includes(name))) throw new Error("Choose a unique profile suffix using letters, numbers, underscores, or hyphens.");
      const existing = model.request_overrides && typeof model.request_overrides === "object" ? model.request_overrides as JSONObject : {};
      const result = {...model, name, ...(add ? {aliases: []} : {}), request_overrides: {...existing, [operation]: overrides}};
      if (add) onAdd(result); else onChange(result);
      setEditing(false); setError("");
    } catch (e) {setError(e instanceof Error ? e.message : "Invalid profile parameters.");}
  }
  return <section className="request-profiles">
    <h4>Request profiles</h4>
    <p className="section-help">A name such as coding:xhigh uses the same deployment with model-specific request parameters; it shares startup, idle time, and workload ownership.</p>
    {Boolean(model.request_overrides) && <><pre className="profile-summary">{JSON.stringify(model.request_overrides, null, 2)}</pre>
      {!readOnly && <button type="button" disabled={disabled} onClick={() => {const entries = Object.entries(model.request_overrides as JSONObject); const [key, value] = entries[0] ?? ["chat_completions", {}]; setOperation(key); setParameters(JSON.stringify(value, null, 2)); setEditing(true);}}>Edit profile parameters</button>}</>}
    <details open={editing || undefined}><summary>{editing ? "Edit parameters" : "Add a named profile"}</summary>
      <fieldset disabled={disabled} className="recipe-fieldset">
        {!editing && <label>Profile suffix<input value={suffix} onChange={(e) => setSuffix(e.target.value)} placeholder="xhigh" /><small>Public name: {String(model.name)}:{suffix}</small></label>}
        <label>Request API<select aria-label="Request API" value={operation} onChange={(e) => {setOperation(e.target.value); setParameters(e.target.value.startsWith("responses") ? '{"reasoning":{"effort":"low"}}' : e.target.value === "chat_completions" ? '{"reasoning_effort":"low"}' : "{}");}}>
          <option value="chat_completions">OpenAI chat</option><option value="responses">OpenAI responses</option><option value="responses_compact">OpenAI responses compact</option><option value="messages">Anthropic messages</option>
          <option value="generate_content">Gemini generate content</option><option value="stream_generate_content">Gemini streaming</option><option value="converse">Bedrock converse</option><option value="converse_stream">Bedrock streaming</option>
        </select></label>
        <label>Request parameters (JSON)<textarea aria-label="Request parameters (JSON)" rows={4} spellCheck={false} value={parameters} onChange={(e) => setParameters(e.target.value)} /></label>
        <p className="section-help">These values override caller parameters for the selected API before protocol translation; for Responses, use {`{"reasoning":{"effort":"xhigh"}}`}. Choose values supported by the underlying model.</p>
        {error && <p className="notice error" role="alert">{error}</p>}
        <div className="recipe-actions"><button type="button" onClick={() => apply(!editing)}>{editing ? "Apply parameters to draft" : "Add profile to draft"}</button>
        {editing && <button type="button" onClick={() => setEditing(false)}>Cancel</button>}</div>
      </fieldset>
    </details>
  </section>;
}
