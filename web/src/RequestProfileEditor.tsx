// SPDX-FileCopyrightText: 2026 Scitrera LLC
// SPDX-FileCopyrightText: 2026 Fox Engine Ltd.
// SPDX-License-Identifier: AGPL-3.0-only

import { Fragment, useEffect, useId, useState } from "react";
import type { JSONObject } from "./extensions";

type Profile = { model: JSONObject; readOnly: boolean };
const operations = [
  ["chat_completions", "OpenAI chat"], ["responses", "OpenAI responses"],
  ["responses_compact", "OpenAI responses compact"], ["messages", "Anthropic messages"],
  ["generate_content", "Gemini generate content"], ["stream_generate_content", "Gemini streaming"],
  ["converse", "Bedrock converse"], ["converse_stream", "Bedrock streaming"],
] as const;
const objectValue = (value: unknown): JSONObject => value && typeof value === "object" && !Array.isArray(value) ? value as JSONObject : {};
const defaults = (operation: string) => operation.startsWith("responses") ? { reasoning: { effort: "low" } }
  : operation === "chat_completions" ? { reasoning_effort: "low" } : {};

export function RequestProfileEditor({ model, profiles, names, disabled, onAdd, onChange, onDelete }: {
  model: JSONObject; profiles: Profile[]; names: string[]; disabled: boolean;
  onAdd: (profile: JSONObject) => void;
  onChange: (name: string, profile: JSONObject) => void;
  onDelete: (name: string) => void;
}) {
  const [editor, setEditor] = useState<{ originalName: string | null; profile: JSONObject } | null>(null);
  const [selector, setSelector] = useState("low");
  const [operation, setOperation] = useState("chat_completions");
  const [parameters, setParameters] = useState("");
  const [overrides, setOverrides] = useState<JSONObject>({});
  const [error, setError] = useState("");
  const id = useId();
  useEffect(() => { setEditor(null); setError(""); }, [model.name]);
  const suffix = (name: string) => name.startsWith(`${model.name}:`) ? name.slice(String(model.name).length + 1) : name;

  function open(profile?: JSONObject) {
    const current = objectValue(profile?.request_overrides);
    const api = Object.keys(current)[0] ?? "chat_completions";
    setEditor({ originalName: profile ? String(profile.name) : null, profile: profile ?? model });
    setSelector(profile ? suffix(String(profile.name)) : "low");
    setOperation(api);
    setOverrides(current);
    setParameters(JSON.stringify(current[api] ?? defaults(api), null, 2));
    setError("");
  }
  function parsedParameters(): JSONObject {
    const parsed: unknown = JSON.parse(parameters);
    if (!parsed || Array.isArray(parsed) || typeof parsed !== "object") throw new Error("Parameters must be a JSON object.");
    return parsed as JSONObject;
  }
  function changeOperation(api: string) {
    try {
      const parsed = parsedParameters();
      const changed = operation in overrides || JSON.stringify(parsed) !== JSON.stringify(defaults(operation));
      const next = changed ? { ...overrides, [operation]: parsed } : overrides;
      setOverrides(next);
      setOperation(api);
      setParameters(JSON.stringify(next[api] ?? defaults(api), null, 2));
      setError("");
    } catch (e) { setError(e instanceof Error ? e.message : "Invalid profile parameters."); }
  }
  function apply() {
    if (!editor || disabled) return;
    try {
      const values = parsedParameters();
      // A base model with overrides is editable without changing its public name.
      const isBase = editor.originalName === String(model.name);
      const name = isBase ? String(model.name) : `${model.name}:${selector.trim()}`;
      if (!isBase && (!/^[A-Za-z0-9][A-Za-z0-9_-]*$/.test(selector.trim()) || name !== editor.originalName && names.includes(name))) {
        throw new Error("Choose a unique profile selector using letters, numbers, underscores, or hyphens.");
      }
      const current = editor.originalName === null ? model : profiles.find(({ model: profile }) => profile.name === editor.originalName)?.model ?? editor.profile;
      const result = { ...current, name, ...(editor.originalName === null ? { aliases: [] } : {}), request_overrides: { ...overrides, [operation]: values } };
      if (editor.originalName === null) onAdd(result); else onChange(editor.originalName, result);
      setEditor(null); setError("");
    } catch (e) { setError(e instanceof Error ? e.message : "Invalid profile parameters."); }
  }
  const editForm = editor && <div className="profile-editor" id={`${id}-editor`}>
    <h5>{editor.originalName === null ? "Add request profile" : "Edit profile parameters"}</h5>
    <fieldset disabled={disabled}>
      <div className="model-field-grid">
        {editor.originalName !== String(model.name) && <label>Selector
          <input aria-label="Profile selector" value={selector} onChange={(e) => setSelector(e.target.value)} placeholder="xhigh" />
          <small>Public name: {String(model.name)}:{selector}</small>
        </label>}
        <label>Request API<select aria-label="Request API" value={operation} onChange={(e) => changeOperation(e.target.value)}>
          {!operations.some(([value]) => value === operation) && <option value={operation}>{operation}</option>}
          {operations.map(([value, label]) => <option key={value} value={value}>{label}</option>)}
        </select></label>
        <label className="wide">Request parameters (JSON)
          <textarea aria-label="Request parameters (JSON)" rows={5} spellCheck={false} value={parameters} onChange={(e) => setParameters(e.target.value)} />
        </label>
      </div>
      <p className="section-help">Values override caller parameters for the selected API before protocol translation. Choose values supported by the underlying model.</p>
      {error && <p className="notice error" role="alert">{error}</p>}
      <div className="recipe-actions">
        <button className="secondary-button" type="button" onClick={apply}>{editor.originalName === null ? "Add profile to draft" : "Apply parameters to draft"}</button>
        <button className="secondary-button" type="button" onClick={() => { setEditor(null); setError(""); }}>Cancel</button>
      </div>
    </fieldset>
  </div>;

  return <section className="model-section request-profiles" aria-labelledby={`${id}-title`}>
    <div className="model-section-heading">
      <h4 id={`${id}-title`}>Request profiles</h4>
      <button className="secondary-button" type="button" disabled={disabled} onClick={() => open()}>Add profile</button>
    </div>
    <p className="section-help">Named profiles such as coding:xhigh share the deployment, startup, and idle policy, with model-specific request parameters.</p>
    <div className="profile-table-wrap"><table className="profile-table" aria-label="Request profiles">
      <colgroup><col className="profile-selector-column" /><col /><col className="profile-options-column" /></colgroup>
      <thead><tr><th scope="col">Selector</th><th scope="col">JSON</th><th scope="col">Options</th></tr></thead>
      <tbody>
        {profiles.map(({ model: profile, readOnly }) => {
          const name = String(profile.name), expanded = editor?.originalName === name;
          const summary = JSON.stringify(profile.request_overrides);
          return <Fragment key={name}>
            <tr className={readOnly ? "generated-entry" : undefined}>
              <th scope="row"><span className="profile-selector" title={name}>{name === String(model.name) ? "(default)" : suffix(name)}</span></th>
              <td><code className="profile-summary" title={summary}>{summary}</code></td>
              <td><div className="profile-row-actions">
                <button className="icon-button" type="button" title={`Edit profile ${name}`} aria-label={`Edit profile ${name}`} aria-expanded={expanded} aria-controls={expanded ? `${id}-editor` : undefined} disabled={disabled || readOnly} onClick={() => expanded ? setEditor(null) : open(profile)}>
                  <svg width="16" height="16" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="1.8" strokeLinecap="round" strokeLinejoin="round" aria-hidden="true"><path d="m15 5 4 4M4 20l4-1L20 7a2.8 2.8 0 0 0-4-4L4 15z" /></svg>
                </button>
                <button className="icon-button profile-delete" type="button" title={`Delete profile ${name}`} aria-label={`Delete profile ${name}`} disabled={disabled || readOnly} onClick={() => { onDelete(name); setEditor(null); setError(""); }}>
                  <svg width="16" height="16" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="1.8" strokeLinecap="round" aria-hidden="true"><path d="m6 6 12 12M18 6 6 18" /></svg>
                </button>
              </div></td>
            </tr>
            {expanded && <tr className="profile-expanded-row"><td colSpan={3}>{editForm}</td></tr>}
          </Fragment>;
        })}
        {!profiles.length && <tr><td colSpan={3} className="profile-empty">No request profiles configured.</td></tr>}
      </tbody>
    </table></div>
    {editor?.originalName === null && editForm}
  </section>;
}
