import { RequestProfileEditor } from "./RequestProfileEditor";
import { capabilityOptions, capabilitySelectionSummary } from "./capabilities";
import { useEffect, useMemo, useState } from "react";
import type { JSONObject, VirtualModelEditorExtension } from "./extensions";
import { deploymentChoices } from "./deploymentTitles";
import type { ConfigurationDocument } from "./types";

// Keep affinity configuration available in JSON while its controls are hidden.
const PROMPT_CACHE_AFFINITY_VISIBLE = false;

export function VirtualModelEditor({
  document,
  disabled,
  extensions = [],
  onChange,
  referencedDeployments = [],
  reservedModelNames = [],
  readOnlyDocument,
}: {
  document: ConfigurationDocument;
  disabled: boolean;
  extensions?: VirtualModelEditorExtension[];
  onChange: (document: ConfigurationDocument) => void;
  referencedDeployments?: Array<{ name: string; source: string }>;
  reservedModelNames?: string[];
  readOnlyDocument?: ConfigurationDocument;
}) {
  const shape = useMemo(() => inspectDocument(document, extensions), [document, extensions]);
  const generatedShape = useMemo(() => inspectDocument(readOnlyDocument ?? { deployments: [], virtual_models: [] }, extensions), [readOnlyDocument, extensions]);
  const deployments = [...new Set([...(shape.deployments ?? []), ...(generatedShape.deployments ?? []), ...referencedDeployments.map((deployment) => deployment.name)])];
  const titles = deploymentChoices(document, readOnlyDocument);
  const [selectedIndex, setSelectedIndex] = useState(0);
  const [confirmRemove, setConfirmRemove] = useState(false);
  const editableModels = shape.models ?? [];
  const models = [...editableModels, ...(generatedShape.models ?? [])];
  const readOnlySelected = selectedIndex >= editableModels.length;
  const formDisabled = disabled || readOnlySelected;
  const selected = models[selectedIndex];
  // Profiles remain explicit virtual models in the document; group named
  // children here without changing their routing or generated-set ownership.
  const selectedName = stringValue(selected?.name);
  const parentName = selectedName.includes(":") ? selectedName.slice(0, selectedName.lastIndexOf(":")) : undefined;
  const profileBase = selected?.request_overrides ? models.find((model) => model.name === parentName) ?? selected : selected;
  const profileRows = profileBase ? models.flatMap((model, index) => model.request_overrides &&
    (model.name === profileBase.name || stringValue(model.name).startsWith(`${profileBase.name}:`))
    ? [{ model, readOnly: index >= editableModels.length }] : []) : [];
  const aliases = selected ? stringArray(selected.aliases) : [];
  const selection = selected ? objectValue(selected.selection) : {};
  const selectionMode = stringValue(selection.mode) || "weighted_random";
  const promptCacheAffinity = objectValue(selection.prompt_cache_affinity);
  const [aliasDraft, setAliasDraft] = useState(aliases.join(", "));

  useEffect(() => {
    if (selectedIndex >= models.length) setSelectedIndex(Math.max(0, models.length - 1));
  }, [models.length, selectedIndex]);

  useEffect(() => {
    setAliasDraft(aliases.join(", "));
    setConfirmRemove(false);
  }, [selectedIndex, selected?.name]);

  if (shape.error) {
    return (
      <div className="structured-unavailable">
        <strong>Structured editor unavailable</strong>
        <p>{shape.error} Switch to JSON to repair the document without losing data.</p>
      </div>
    );
  }

  const updateModel = (transform: (model: JSONObject) => JSONObject) => {
    if (!selected || formDisabled) return;
    const nextModels = [...editableModels];
    nextModels[selectedIndex] = transform({ ...selected });
    onChange({ ...document, virtual_models: nextModels });
  };

  const addModel = () => {
    const firstDeployment = deployments[0];
    if (!firstDeployment) return;
    const names = new Set([...reservedModelNames, ...models.flatMap((model) => [stringValue(model.name), ...stringArray(model.aliases)])]);
    const name = uniqueName("new-model", names);
    const next: JSONObject = {
      name,
      visibility: "public",
      response_model: "virtual",
      selection: { mode: "weighted_random" },
      pools: [{
        priority: 0,
        targets: [{ deployment: firstDeployment, weight: 100 }],
      }],
    };
    onChange({ ...document, virtual_models: [...editableModels, next] });
    setSelectedIndex(editableModels.length);
  };

  const removeModel = () => {
    if (formDisabled) return;
    if (!selected || !confirmRemove) {
      setConfirmRemove(true);
      return;
    }
    const nextModels = editableModels.filter((_, index) => index !== selectedIndex);
    onChange({ ...document, virtual_models: nextModels });
    setSelectedIndex(Math.max(0, selectedIndex - 1));
    setConfirmRemove(false);
  };

  return (
    <div className="structured-editor" aria-disabled={disabled}>
      <aside className="model-list" aria-label="Virtual models">
        <div className="model-list-heading">
          <div>
            <span>Virtual models</span>
            <strong>{models.length}</strong>
          </div>
          <button
            className="icon-button"
            disabled={disabled || !deployments.length}
            onClick={addModel}
            title={deployments.length ? "Add virtual model" : "Add a deployment first"}
            type="button"
          >
            +
          </button>
        </div>
        <div className="model-list-items">
          {models.map((model, index) => (
            <button
              aria-current={index === selectedIndex ? "true" : undefined}
              className={[index === selectedIndex ? "active" : "", index >= editableModels.length ? "generated-entry" : ""].filter(Boolean).join(" ")}
              key={`${stringValue(model.name)}-${index}`}
              onClick={() => setSelectedIndex(index)}
              type="button"
            >
              <span>{stringValue(model.name) || `Model ${index + 1}`}</span>
              <small>{effectiveVisibility(model)}</small>
              {index >= editableModels.length ? <small className="ownership-label">sparkrun · Read only</small> : null}
            </button>
          ))}
          {!models.length ? (
            <p>No virtual models. Add one after defining a deployment.</p>
          ) : null}
        </div>
      </aside>

      <div className={readOnlySelected ? "model-form generated-form" : "model-form"}>
        {selected && readOnlySelected ? <p className="read-only-note">sparkrun generated · Read only</p> : null}
        {selected ? (
          <>
            <div className="model-form-heading">
              <div>
                <p className="eyebrow">Logical product and routing boundary</p>
                <h3>{stringValue(selected.name) || "Unnamed virtual model"}</h3>
              </div>
              <button
                className={confirmRemove ? "danger-button confirm" : "danger-button"}
                disabled={formDisabled}
                onBlur={() => setConfirmRemove(false)}
                onClick={removeModel}
                type="button"
              >
                {confirmRemove ? "Confirm remove" : "Remove model"}
              </button>
            </div>

            <fieldset disabled={formDisabled}>
              <div className="model-field-grid">
                <Field label="Canonical name">
                  <input
                    onChange={(event) => updateModel((model) => setString(model, "name", event.target.value, true))}
                    value={stringValue(selected.name)}
                  />
                </Field>
                <Field label="Visibility">
                  <select
                    onChange={(event) => updateModel((model) => setString(model, "visibility", event.target.value))}
                    value={stringValue(selected.visibility) || "public"}
                  >
                    <option value="public">Public</option>
                    <option value="hidden">Hidden</option>
                    <option value="internal">Internal</option>
                  </select>
                </Field>
                <Field label="Response model name">
                  <select
                    onChange={(event) => updateModel((model) => setString(model, "response_model", event.target.value))}
                    value={stringValue(selected.response_model) || "virtual"}
                  >
                    <option value="virtual">Canonical virtual model</option>
                    <option value="requested">Exact requested alias</option>
                    <option value="upstream">Upstream model</option>
                  </select>
                </Field>
                <Field label="Selection">
                  <select
                    onChange={(event) => updateModel((model) => setSelectionMode(model, event.target.value))}
                    value={stringValue(objectValue(selected.selection).mode) || "weighted_random"}
                  >
                    <option value="weighted_random">Weighted random</option>
                    <option value="weighted_hash">Weighted hash</option>
                  </select>
                </Field>
                {selectionMode === "weighted_hash" ? (
                  <Field label="Stable hash key">
                    <select
                      onChange={(event) => updateModel((model) => setNestedString(model, "selection", "hash_key", event.target.value))}
                      value={stringValue(selection.hash_key)}
                    >
                      <option value="">Select trusted attribution</option>
                      <option value="thread_id">Thread ID</option>
                      <option value="session">Session</option>
                      <option value="task_id">Task ID</option>
                      <option value="workspace">Workspace</option>
                      <option value="experiment">Experiment</option>
                      <option value="user">User</option>
                      <option value="principal">Principal</option>
                      <option value="tenant">Tenant</option>
                    </select>
                    <small>Missing trusted attribution falls back to weighted random for that request.</small>
                  </Field>
                ) : null}
                <Field label="Aliases" wide>
                  <input
                    onBlur={() => updateModel((model) => setStringArray(model, "aliases", splitList(aliasDraft)))}
                    onChange={(event) => setAliasDraft(event.target.value)}
                    placeholder="default, chat, production"
                    value={aliasDraft}
                  />
                  <small>Comma or newline separated; aliases are unique across the document.</small>
                </Field>
              </div>

            </fieldset>
            {profileBase && <RequestProfileEditor
              key={selectedName}
              model={profileBase}
              profiles={profileRows}
              names={[...reservedModelNames, ...models.flatMap((model) => [stringValue(model.name), ...stringArray(model.aliases)])]}
              disabled={disabled}
              onAdd={(profile) => {
                onChange({ ...document, virtual_models: [...editableModels, profile] });
                if (readOnlySelected) setSelectedIndex(selectedIndex + 1);
              }}
              onChange={(name, profile) => onChange({ ...document, virtual_models: editableModels.map((model) => model.name === name ? profile : model) })}
              onDelete={(name) => {
                const nextModels = editableModels.flatMap((model) => {
                  if (model.name !== name) return [model];
                  if (name !== profileBase.name) return [];
                  const next = { ...model }; delete next.request_overrides; return [next];
                });
                onChange({ ...document, virtual_models: nextModels });
                const selectedStillPresent = [...nextModels, ...(generatedShape.models ?? [])].findIndex((model) => model.name === selectedName);
                setSelectedIndex(selectedStillPresent >= 0 ? selectedStillPresent : Math.max(0, models.indexOf(profileBase)));
              }}
            />}
            <fieldset disabled={formDisabled}>
              <section className="model-section">
                <div className="model-section-heading">
                  <div>
                    <p className="eyebrow">Fallback order and weighted selection</p>
                    <h4>Routing pools</h4>
                  </div>
                  <button
                    className="secondary-button"
                    disabled={!availableDeployments(selected, deployments).length}
                    onClick={() => updateModel((model) => addPool(model, deployments))}
                    type="button"
                  >
                    Add fallback pool
                  </button>
                </div>
                <RoutingPools
                  deployments={deployments}
                  deploymentTitles={titles}
                  model={selected}
                  onChange={updateModel}
                />
              </section>

              {PROMPT_CACHE_AFFINITY_VISIBLE && <details className="model-section policy-section">
                <summary>
                  <span>Prompt-cache route affinity</span>
                  <small>{booleanValue(promptCacheAffinity.enabled) ? "Enabled" : "Disabled by default"}</small>
                </summary>
                <p className="section-help">
                  Best-effort reuse of a recent deployment for matching message prefixes. It never crosses routing priorities and normal failover remains active.
                </p>
                <div className="policy-fields">
                  <label className="checkbox-field">
                    <input
                      checked={booleanValue(promptCacheAffinity.enabled)}
                      onChange={(event) => updateModel((model) => setSelectionPromptBoolean(model, "enabled", event.target.checked))}
                      type="checkbox"
                    />
                    <span>Enable prompt-cache affinity</span>
                  </label>
                  <Field label="Successful-route TTL">
                    <input
                      placeholder="5m"
                      value={stringValue(promptCacheAffinity.ttl)}
                      onChange={(event) => updateModel((model) => setSelectionPromptString(model, "ttl", event.target.value))}
                    />
                  </Field>
                  <Field label="Minimum prefix bytes">
                    <input
                      min="0"
                      max={32 << 20}
                      placeholder="4096"
                      step="1"
                      type="number"
                      value={numberInput(promptCacheAffinity.min_prefix_bytes)}
                      onChange={(event) => updateModel((model) => setSelectionPromptNumber(model, "min_prefix_bytes", event.target.value))}
                    />
                  </Field>
                  <Field label="Maximum prefix boundaries">
                    <input
                      min="0"
                      max="64"
                      placeholder="8"
                      step="1"
                      type="number"
                      value={numberInput(promptCacheAffinity.max_prefixes_per_request)}
                      onChange={(event) => updateModel((model) => setSelectionPromptNumber(model, "max_prefixes_per_request", event.target.value))}
                    />
                  </Field>
                  <Field label="Isolation scope">
                    <select
                      value={stringValue(promptCacheAffinity.scope) || "caller"}
                      onChange={(event) => updateModel((model) => setSelectionPromptString(model, "scope", event.target.value))}
                    >
                      <option value="caller">Caller</option>
                      <option value="tenant">Tenant</option>
                    </select>
                  </Field>
                </div>
              </details>}

              <details className="model-section capability-section" open>
                <summary>
                  <span>Required capabilities</span>
                  <small>{capabilitySelectionSummary(stringArray(selected.required_capabilities))}</small>
                </summary>
                <p className="section-help">
                  Every eligible target must support the full required set. Per-request capabilities are added automatically. Other configured requirements are preserved and available in JSON.
                </p>
                <div className="capability-grid">
                  {capabilityOptions.map(({ value: capability, label }) => (
                    <label key={capability}>
                      <input
                        checked={stringArray(selected.required_capabilities).includes(capability)}
                        onChange={(event) => updateModel((model) => toggleCapability(model, capability, event.target.checked))}
                        type="checkbox"
                      />
                      <span>{label}</span>
                    </label>
                  ))}
                </div>
              </details>

              <details className="model-section policy-section">
                <summary>
                  <span>Timeout and retry policy</span>
                  <small>Zero or blank values use gateway defaults</small>
                </summary>
                <PolicyFields model={selected} onChange={updateModel} />
              </details>

              {extensions.map((extension) => (
                <extension.Component
                  disabled={formDisabled}
                  key={extension.id}
                  model={selected}
                  updateModel={updateModel}
                />
              ))}

              {selected.guardrails ? (
                <div className="preserved-policy-note">
                  <strong>Guardrails preserved</strong>
                  <span>{guardrailSummary(selected.guardrails)}</span>
                  <small>Use JSON mode for specialized guardrail policy changes.</small>
                </div>
              ) : null}
            </fieldset>
          </>
        ) : (
          <div className="structured-empty">
            <strong>No virtual model selected</strong>
            <p>Add a virtual model to define caller-facing aliases and routing pools.</p>
          </div>
        )}
      </div>
    </div>
  );
}

function RoutingPools({
  model,
  deployments,
  deploymentTitles,
  onChange,
}: {
  model: JSONObject;
  deployments: string[];
  deploymentTitles: Record<string, string>;
  onChange: (transform: (model: JSONObject) => JSONObject) => void;
}) {
  const pools = objectArray(model.pools);
  return (
    <div className="routing-pools">
      {pools.map((pool, poolIndex) => {
        const targets = objectArray(pool.targets);
        const totalWeight = targets.reduce((total, target) => total + numberValue(target.weight), 0);
        return (
          <article className="routing-pool" key={poolIndex}>
            <div className="pool-heading">
              <label>
                <span>{poolIndex === 0 ? "Primary priority" : `Fallback ${poolIndex} priority`}</span>
                <input
                  min="0"
                  onChange={(event) => onChange((current) => updatePool(current, poolIndex, (value) => setNumber(value, "priority", event.target.value, true)))}
                  step="1"
                  type="number"
                  value={numberInput(pool.priority)}
                />
              </label>
              <span>{totalWeight} total weight</span>
              <button
                className="text-button danger-text"
                onClick={() => onChange((current) => removePool(current, poolIndex))}
                type="button"
              >
                Remove pool
              </button>
            </div>
            <div className="target-list">
              {targets.map((target, targetIndex) => {
                const used = usedDeployments(model, stringValue(target.deployment));
                return (
                  <div className="target-row" key={targetIndex}>
                    <Field label="Deployment">
                      <select
                        aria-label="Deployment"
                        onChange={(event) => onChange((current) => updateTarget(current, poolIndex, targetIndex, (value) => setString(value, "deployment", event.target.value, true)))}
                        value={stringValue(target.deployment)}
                      >
                        {stringValue(target.deployment) && !deployments.includes(stringValue(target.deployment)) ? (
                          <option value={stringValue(target.deployment)}>{stringValue(target.deployment)} (unknown)</option>
                        ) : null}
                        {deployments.map((deployment) => (
                          <option disabled={used.has(deployment)} key={deployment} value={deployment}>{deploymentTitles[deployment] ?? deployment}</option>
                        ))}
                      </select>
                    </Field>
                    <Field label="Relative weight">
                      <input
                        min="1"
                        onChange={(event) => onChange((current) => updateTarget(current, poolIndex, targetIndex, (value) => setNumber(value, "weight", event.target.value, true)))}
                        step="1"
                        type="number"
                        value={numberInput(target.weight)}
                      />
                    </Field>
                    <button
                      aria-label={`Remove target ${stringValue(target.deployment) || targetIndex + 1}`}
                      className="icon-button remove"
                      onClick={() => onChange((current) => removeTarget(current, poolIndex, targetIndex))}
                      type="button"
                    >
                      ×
                    </button>
                  </div>
                );
              })}
              {!targets.length ? <p className="pool-empty">This pool needs at least one target.</p> : null}
            </div>
            <button
              className="text-button add-target"
              disabled={!availableDeployments(model, deployments).length}
              onClick={() => onChange((current) => addTarget(current, poolIndex, deployments))}
              type="button"
            >
              + Add target
            </button>
          </article>
        );
      })}
      {!pools.length ? <div className="pool-empty large">At least one routing pool is required.</div> : null}
    </div>
  );
}

function PolicyFields({ model, onChange }: { model: JSONObject; onChange: (transform: (model: JSONObject) => JSONObject) => void }) {
  const limits = objectValue(model.limits);
  const retry = objectValue(model.retry);
  const budget = objectValue(retry.budget);
  return (
    <div className="policy-fields">
      <Field label="Maximum attempts">
        <input min="0" max="16" type="number" value={numberInput(limits.max_attempts)} onChange={(event) => onChange((current) => setNestedNumber(current, "limits", "max_attempts", event.target.value))} />
      </Field>
      <Field label="Overall timeout">
        <input placeholder="10m" value={stringValue(limits.overall_timeout)} onChange={(event) => onChange((current) => setNestedString(current, "limits", "overall_timeout", event.target.value))} />
      </Field>
      <Field label="Per-try timeout">
        <input placeholder="2m" value={stringValue(limits.per_try_timeout)} onChange={(event) => onChange((current) => setNestedString(current, "limits", "per_try_timeout", event.target.value))} />
      </Field>
      <Field label="Stream idle timeout">
        <input placeholder="2m" value={stringValue(limits.stream_idle_timeout)} onChange={(event) => onChange((current) => setNestedString(current, "limits", "stream_idle_timeout", event.target.value))} />
      </Field>
      <label className="checkbox-field">
        <input checked={booleanValue(retry.disabled)} onChange={(event) => onChange((current) => setNestedBoolean(current, "retry", "disabled", event.target.checked))} type="checkbox" />
        <span>Disable retry and fallback attempts</span>
      </label>
      <Field label="Base backoff">
        <input placeholder="100ms" value={stringValue(retry.base_backoff)} onChange={(event) => onChange((current) => setNestedString(current, "retry", "base_backoff", event.target.value))} />
      </Field>
      <Field label="Maximum backoff">
        <input placeholder="2s" value={stringValue(retry.max_backoff)} onChange={(event) => onChange((current) => setNestedString(current, "retry", "max_backoff", event.target.value))} />
      </Field>
      <Field label="Maximum Retry-After">
        <input placeholder="30s" value={stringValue(retry.max_retry_after)} onChange={(event) => onChange((current) => setNestedString(current, "retry", "max_retry_after", event.target.value))} />
      </Field>
      <label className="checkbox-field">
        <input checked={booleanValue(budget.disabled)} onChange={(event) => onChange((current) => setDoubleNestedBoolean(current, "retry", "budget", "disabled", event.target.checked))} type="checkbox" />
        <span>Disable shared retry budget</span>
      </label>
      <Field label="Retry budget ratio">
        <input min="0" max="10" step="0.01" type="number" value={numberInput(budget.ratio)} onChange={(event) => onChange((current) => setDoubleNestedNumber(current, "retry", "budget", "ratio", event.target.value))} />
      </Field>
      <Field label="Minimum retry concurrency">
        <input min="0" step="1" type="number" value={numberInput(budget.min_concurrency)} onChange={(event) => onChange((current) => setDoubleNestedNumber(current, "retry", "budget", "min_concurrency", event.target.value))} />
      </Field>
    </div>
  );
}

function Field({ label, wide = false, children }: { label: string; wide?: boolean; children: React.ReactNode }) {
  return <label className={wide ? "wide" : ""}><span>{label}</span>{children}</label>;
}

function inspectDocument(
  document: ConfigurationDocument,
  extensions: VirtualModelEditorExtension[],
): { deployments?: string[]; models?: JSONObject[]; error?: string } {
  if (!Array.isArray(document.deployments)) return { error: "deployments must be an array." };
  if (!Array.isArray(document.virtual_models)) return { error: "virtual_models must be an array." };
  const deployments: string[] = [];
  for (const [index, value] of document.deployments.entries()) {
    if (!isObject(value) || typeof value.name !== "string") return { error: `deployments[${index}] must be an object with a string name.` };
    deployments.push(value.name);
  }
  const models: JSONObject[] = [];
  for (const [modelIndex, value] of document.virtual_models.entries()) {
    if (!isObject(value)) return { error: `virtual_models[${modelIndex}] must be an object.` };
    if (!validOptionalStringArray(value.aliases) || !validOptionalStringArray(value.required_capabilities)) {
      return { error: `virtual_models[${modelIndex}] aliases and required_capabilities must be string arrays.` };
    }
    for (const field of ["selection", "limits", "retry", "guardrails"] as const) {
      if (value[field] !== undefined && !isObject(value[field])) return { error: `virtual_models[${modelIndex}].${field} must be an object.` };
    }
    const selection = objectValue(value.selection);
    if (selection.prompt_cache_affinity !== undefined && !isObject(selection.prompt_cache_affinity)) {
      return { error: `virtual_models[${modelIndex}].selection.prompt_cache_affinity must be an object.` };
    }
    const retry = objectValue(value.retry);
    if (retry.budget !== undefined && !isObject(retry.budget)) return { error: `virtual_models[${modelIndex}].retry.budget must be an object.` };
    for (const extension of extensions) {
      const error = extension.validateModel?.(value, modelIndex);
      if (error) return { error };
    }
    if (!Array.isArray(value.pools)) return { error: `virtual_models[${modelIndex}].pools must be an array.` };
    for (const [poolIndex, pool] of value.pools.entries()) {
      if (!isObject(pool) || !Array.isArray(pool.targets)) return { error: `virtual_models[${modelIndex}].pools[${poolIndex}] must contain a targets array.` };
      for (const [targetIndex, target] of pool.targets.entries()) {
        if (!isObject(target)) return { error: `virtual_models[${modelIndex}].pools[${poolIndex}].targets[${targetIndex}] must be an object.` };
      }
    }
    models.push(value);
  }
  return { deployments, models };
}

function updatePool(model: JSONObject, index: number, transform: (pool: JSONObject) => JSONObject) {
  const pools = [...objectArray(model.pools)];
  pools[index] = transform({ ...pools[index] });
  return { ...model, pools };
}

function updateTarget(model: JSONObject, poolIndex: number, targetIndex: number, transform: (target: JSONObject) => JSONObject) {
  return updatePool(model, poolIndex, (pool) => {
    const targets = [...objectArray(pool.targets)];
    targets[targetIndex] = transform({ ...targets[targetIndex] });
    return { ...pool, targets };
  });
}

function addPool(model: JSONObject, deployments: string[]) {
  const pools = objectArray(model.pools);
  const priorities = pools.map((pool) => numberValue(pool.priority));
  const deployment = availableDeployments(model, deployments)[0];
  if (!deployment) return model;
  return {
    ...model,
    pools: [...pools, {
      priority: priorities.length ? Math.max(...priorities) + 1 : 0,
      targets: [{ deployment, weight: 100 }],
    }],
  };
}

function removePool(model: JSONObject, index: number) {
  return { ...model, pools: objectArray(model.pools).filter((_, current) => current !== index) };
}

function addTarget(model: JSONObject, poolIndex: number, deployments: string[]) {
  const deployment = availableDeployments(model, deployments)[0];
  if (!deployment) return model;
  return updatePool(model, poolIndex, (pool) => ({
    ...pool,
    targets: [...objectArray(pool.targets), { deployment, weight: 100 }],
  }));
}

function removeTarget(model: JSONObject, poolIndex: number, targetIndex: number) {
  return updatePool(model, poolIndex, (pool) => ({
    ...pool,
    targets: objectArray(pool.targets).filter((_, current) => current !== targetIndex),
  }));
}

function usedDeployments(model: JSONObject, except = "") {
  const used = new Set<string>();
  objectArray(model.pools).forEach((pool) => objectArray(pool.targets).forEach((target) => {
    const deployment = stringValue(target.deployment);
    if (deployment && deployment !== except) used.add(deployment);
  }));
  return used;
}

function availableDeployments(model: JSONObject, deployments: string[]) {
  const used = usedDeployments(model);
  return deployments.filter((deployment) => !used.has(deployment));
}

function toggleCapability(model: JSONObject, capability: string, enabled: boolean) {
  const current = stringArray(model.required_capabilities);
  const next = enabled
    ? [...new Set([...current, capability])]
    : current.filter((value) => value !== capability);
  return setStringArray(model, "required_capabilities", next);
}

function setString(model: JSONObject, field: string, value: string, required = false) {
  const next = { ...model };
  if (value || required) next[field] = value;
  else delete next[field];
  return next;
}

function setNumber(model: JSONObject, field: string, value: string, required = false) {
  const next = { ...model };
  if (value !== "" || required) next[field] = value === "" ? 0 : Number(value);
  else delete next[field];
  return next;
}

function setStringArray(model: JSONObject, field: string, values: string[]) {
  const next = { ...model };
  if (values.length) next[field] = values;
  else delete next[field];
  return next;
}

function setNestedString(model: JSONObject, parent: string, field: string, value: string) {
  return setNested(model, parent, (nested) => setString(nested, field, value));
}

function setNestedNumber(model: JSONObject, parent: string, field: string, value: string) {
  return setNested(model, parent, (nested) => setNumber(nested, field, value));
}

function setNestedBoolean(model: JSONObject, parent: string, field: string, value: boolean) {
  return setNested(model, parent, (nested) => setBoolean(nested, field, value));
}

function setDoubleNestedNumber(model: JSONObject, parent: string, child: string, field: string, value: string) {
  return setNested(model, parent, (nested) => setNested(nested, child, (inner) => setNumber(inner, field, value)));
}

function setDoubleNestedBoolean(model: JSONObject, parent: string, child: string, field: string, value: boolean) {
  return setNested(model, parent, (nested) => setNested(nested, child, (inner) => setBoolean(inner, field, value)));
}

function setSelectionPromptString(model: JSONObject, field: string, value: string) {
  return setNested(model, "selection", (selection) => setNested(selection, "prompt_cache_affinity", (affinity) => setString(affinity, field, value)));
}

function setSelectionMode(model: JSONObject, value: string) {
  return setNested(model, "selection", (selection) => {
    const next = setString(selection, "mode", value);
    if (value !== "weighted_hash") delete next.hash_key;
    return next;
  });
}

function setSelectionPromptNumber(model: JSONObject, field: string, value: string) {
  return setNested(model, "selection", (selection) => setNested(selection, "prompt_cache_affinity", (affinity) => setNumber(affinity, field, value)));
}

function setSelectionPromptBoolean(model: JSONObject, field: string, value: boolean) {
  return setNested(model, "selection", (selection) => setNested(selection, "prompt_cache_affinity", (affinity) => setBoolean(affinity, field, value)));
}

function setNested(model: JSONObject, parent: string, transform: (nested: JSONObject) => JSONObject) {
  const next = { ...model };
  const nested = transform({ ...objectValue(next[parent]) });
  if (Object.keys(nested).length) next[parent] = nested;
  else delete next[parent];
  return next;
}

function setBoolean(model: JSONObject, field: string, value: boolean) {
  const next = { ...model };
  if (value) next[field] = true;
  else delete next[field];
  return next;
}

function objectArray(value: unknown): JSONObject[] {
  return Array.isArray(value) ? value.filter(isObject) : [];
}

function objectValue(value: unknown): JSONObject {
  return isObject(value) ? value : {};
}

function stringArray(value: unknown): string[] {
  return Array.isArray(value) ? value.filter((current): current is string => typeof current === "string") : [];
}

function validOptionalStringArray(value: unknown) {
  return value === undefined || (Array.isArray(value) && value.every((current) => typeof current === "string"));
}

function stringValue(value: unknown) {
  return typeof value === "string" ? value : "";
}

function numberValue(value: unknown) {
  return typeof value === "number" && Number.isFinite(value) ? value : 0;
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

function splitList(value: string) {
  return [...new Set(value.split(/[\n,]/).map((current) => current.trim()).filter(Boolean))];
}

function uniqueName(prefix: string, names: Set<string>) {
  if (!names.has(prefix)) return prefix;
  let suffix = 2;
  while (names.has(`${prefix}-${suffix}`)) suffix += 1;
  return `${prefix}-${suffix}`;
}

function effectiveVisibility(model: JSONObject) {
  return stringValue(model.visibility) || "public";
}

function guardrailSummary(value: unknown) {
  const policy = objectValue(value);
  const pre = Array.isArray(policy.pre) ? policy.pre.length : 0;
  const post = Array.isArray(policy.post) ? policy.post.length : 0;
  return `${pre} pre, ${post} post${policy.stream ? ", streaming policy" : ""}`;
}

function humanize(value: string) {
  if (value === "ipv4") return "IPv4";
  if (value === "ssn") return "SSN";
  return value.split("_").map((part) => part[0]?.toUpperCase() + part.slice(1)).join(" ");
}
