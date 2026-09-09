// SPDX-FileCopyrightText: 2026 Scitrera LLC
// SPDX-FileCopyrightText: 2026 Fox Engine Ltd.
// SPDX-License-Identifier: AGPL-3.0-only

import type { ConfigurationDocument } from "./types";

type Fields = Record<string, unknown>;
const attributes = [
  ["size_b", "Model size (B parameters)"],
  ["context", "Context length (tokens)"],
  ["input_price", "Input price / million tokens"],
  ["output_price", "Output price / million tokens"],
] as const;

export function DeploymentMetadataEditor({deployment, disabled = false, onChange}: {
  deployment: Fields; disabled?: boolean; onChange: (transform: (current: Fields) => Fields) => void;
}) {
  const fields = object(deployment.model_metadata);
  const update = (key: string, value: unknown) => {
    if (disabled) return;
    onChange(current => {
      const next = {...object(current.model_metadata), [key]: value};
      if (value === undefined) delete next[key];
      const result = {...current, model_metadata: next};
      if (!Object.keys(next).length) delete (result as Fields).model_metadata;
      return result;
    });
  };
  return <details className="model-section deployment-metadata" open>
    <summary><span>Model metadata</span><small>Used by model routing</small></summary>
    <p className="section-help">Leave unknown values blank. Enter prices in the same currency across deployments; zero means no per-token charge. A virtual model with multiple targets uses the smallest known context and the largest known size and prices.</p>
    <fieldset disabled={disabled} className="model-field-grid">
      {attributes.map(([key, label]) => <label key={key}>{label}
        <input type="number" min={key === "context" ? 1 : key === "size_b" ? 0.000001 : 0} step={key === "context" ? 1 : "any"}
          value={typeof fields[key] === "number" ? fields[key] as number : ""}
          placeholder="Unknown" onChange={event => update(key, event.target.value === "" ? undefined : Number(event.target.value))} />
      </label>)}
      <label>Model tags<input defaultValue={strings(fields.tags).join(", ")} key={JSON.stringify(fields.tags)} placeholder="local, coding"
        onBlur={event => {const tags = [...new Set(event.target.value.split(",").map(tag => tag.trim()).filter(Boolean))]; update("tags", tags.length ? tags : undefined);}} /></label>
    </fieldset>
  </details>;
}

// Mirror the backend's conservative reduction for draft previews. Runtime
// discovery fills unknown fields; authored deployment values take precedence.
export function deploymentMetadata(document: ConfigurationDocument, additional?: ConfigurationDocument): Record<string, Fields> {
  const deployments = new Map([...rows(document.deployments), ...rows(additional?.deployments)].map(d => [d.name, object(d.model_metadata)]));
  const result: Record<string, Fields> = {};
  for (const model of [...rows(document.virtual_models), ...rows(additional?.virtual_models)]) {
    const merged: Fields = {};
    for (const pool of rows(model.pools)) for (const target of rows(pool.targets)) {
      const fields = deployments.get(target.deployment) ?? {};
      for (const [key] of attributes) {
        const next = fields[key], current = merged[key];
        if (typeof next === "number") merged[key] = typeof current === "number" ? (key === "context" ? Math.min : Math.max)(current, next) : next;
      }
      const tags = [...new Set([...strings(merged.tags), ...strings(fields.tags)])].sort();
      if (tags.length) merged.tags = tags;
    }
    for (const name of [model.name, ...strings(model.aliases)]) if (typeof name === "string") result[name] = merged;
  }
  return result;
}

function object(value: unknown): Fields { return value && typeof value === "object" && !Array.isArray(value) ? value as Fields : {}; }
function rows(value: unknown): Fields[] { return Array.isArray(value) ? value.map(object) : []; }
function strings(value: unknown): string[] { return Array.isArray(value) ? value.filter((v): v is string => typeof v === "string") : []; }
