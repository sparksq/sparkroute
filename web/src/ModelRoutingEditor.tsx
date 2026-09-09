import { missingRoutingModels, reconcileRoutingMetadata } from "./modelRoutingReferences";
import { deploymentMetadata } from "./DeploymentMetadataEditor";
import { useEffect, useMemo, useState } from "react";
import type {
  ConfigurationDocument,
  DiscoveredMetadataState,
  RoutingSimulationResult,
} from "./types";
import { capabilityOptions } from "./capabilities";

// Keep the editor implementation available while the MMBridge configuration
// contract settles; existing document fields remain untouched and editable as JSON.
const MM_PROJECTION_CONFIGURATION_VISIBLE = false;

type JSONObject = Record<string, unknown>;

const strategies = [
  "random",
  "round_robin",
  "weighted_round_robin",
  "smallest",
  "largest",
  "lowest_cost",
  "fastest",
  "balanced",
  "stage_router",
] as const;

export type RoutingSimulator = (
  document: ConfigurationDocument,
  requestedModel: string,
  routingText: string,
  requiredCapabilities: string[],
) => Promise<RoutingSimulationResult>;

export function ModelRoutingEditor({
  canonicalModelNames,
  additionalDocument,
  discoveredMetadata,
  disabled,
  document,
  onChange,
  simulate,
}: {
  canonicalModelNames?: string[];
  additionalDocument?: ConfigurationDocument;
  discoveredMetadata?: DiscoveredMetadataState;
  disabled: boolean;
  document: ConfigurationDocument;
  onChange: (document: ConfigurationDocument) => void;
  simulate?: RoutingSimulator;
}) {
  const policy = objectValue(document.model_routing);
  const selectorMap = objectValue(policy.virtual_models);
  const selectors = useMemo(() => Object.keys(selectorMap).sort(), [selectorMap]);
  const canonicalModels = useMemo(() => [...new Set(
    canonicalModelNames ?? arrayValue(document.virtual_models)
      .map((model) => stringValue(model.name)),
  )].filter(Boolean).sort(), [canonicalModelNames, document.virtual_models]);
  const [selectedName, setSelectedName] = useState(selectors[0] ?? "auto");
  const [confirmRemove, setConfirmRemove] = useState(false);
  const [confirmRemovePolicy, setConfirmRemovePolicy] = useState(false);

  useEffect(() => {
    if (!selectors.includes(selectedName)) setSelectedName(selectors[0] ?? "auto");
  }, [selectedName, selectors]);

  if (!document.model_routing || !isObject(document.model_routing)) {
    return (
      <div className="routing-policy-empty">
        <div>
          <p className="eyebrow">Model-policy layer</p>
          <h3>No model-routing selectors configured</h3>
          <p>
            Model routing is optional. Enable it to choose between virtual models through
            a public selector such as <code>auto</code>. Virtual models and aliases
            can also be called directly.
          </p>
        </div>
        <button
          className="primary-button"
          disabled={disabled || canonicalModels.length === 0}
          onClick={() => onChange({
            ...document,
            model_routing: newPolicy(canonicalModels),
          })}
          type="button"
        >
          Enable model routing
        </button>
        {!canonicalModels.length ? <small>Add a virtual model first.</small> : null}
      </div>
    );
  }

  const selected = objectValue(selectorMap[selectedName]);
  const metadata = objectValue(policy.models);
  const missingModels = missingRoutingModels(policy, canonicalModels);
  const candidateNames = [...new Set([...canonicalModels, ...stringArray(selected.models)])].sort();
  const removePolicy = () => {
    if (disabled) return;
    if (!confirmRemovePolicy) { setConfirmRemovePolicy(true); return; }
    const next = { ...document };
    delete next.model_routing;
    onChange(next);
    setConfirmRemovePolicy(false);
  };
  const updatePolicy = (transform: (current: JSONObject) => JSONObject) => {
    onChange({ ...document, model_routing: reconcileRoutingMetadata(transform({ ...policy }), canonicalModels) });
  };
  const updateSelector = (transform: (current: JSONObject) => JSONObject) => {
    if (!selectedName) return;
    updatePolicy((current) => ({
      ...current,
      virtual_models: {
        ...objectValue(current.virtual_models),
        [selectedName]: transform({ ...selected }),
      },
    }));
  };
  const addSelector = () => {
    const name = uniqueName("selector", new Set(selectors));
    updatePolicy((current) => ({
      ...current,
      virtual_models: {
        ...objectValue(current.virtual_models),
        [name]: { strategy: "balanced", models: [...canonicalModels] },
      },
    }));
    setSelectedName(name);
  };
  const renameSelector = (name: string) => {
    if (!selectedName || selectedName === "auto" || name === selectedName) return;
    const currentSelectors = objectValue(policy.virtual_models);
    const nextSelectors: JSONObject = {};
    Object.entries(currentSelectors).forEach(([key, value]) => {
      nextSelectors[key === selectedName ? name : key] = value;
    });
    const rules = arrayValue(policy.keyword_rules).map((rule) => (
      stringValue(rule.virtual_model) === selectedName
        ? { ...rule, virtual_model: name }
        : rule
    ));
    updatePolicy((current) => ({
      ...current,
      default_virtual_model:
        stringValue(current.default_virtual_model) === selectedName
          ? name
          : current.default_virtual_model,
      virtual_models: nextSelectors,
      ...(rules.length ? { keyword_rules: rules } : {}),
    }));
    setSelectedName(name);
  };
  const removeSelector = () => {
    if (!confirmRemove || selectedName === "auto") {
      setConfirmRemove(selectedName !== "auto");
      return;
    }
    const nextSelectors = { ...selectorMap };
    delete nextSelectors[selectedName];
    const rules = arrayValue(policy.keyword_rules).filter(
      (rule) => stringValue(rule.virtual_model) !== selectedName,
    );
    updatePolicy((current) => ({
      ...current,
      default_virtual_model:
        stringValue(current.default_virtual_model) === selectedName ? "auto" : current.default_virtual_model,
      virtual_models: nextSelectors,
      ...(rules.length ? { keyword_rules: rules } : { keyword_rules: undefined }),
    }));
    setConfirmRemove(false);
  };
  const toggleCandidate = (name: string, checked: boolean) => {
    const current = stringArray(selected.models);
    const models = checked ? [...new Set([...current, name])] : current.filter((item) => item !== name);
    updatePolicy((next) => {
      const nextMetadata = { ...objectValue(next.models) };
      if (checked && !isObject(nextMetadata[name])) nextMetadata[name] = { enabled: true, weight: 100 };
      return {
        ...next,
        models: nextMetadata,
        virtual_models: {
          ...objectValue(next.virtual_models),
          [selectedName]: { ...selected, models },
        },
      };
    });
  };
  const updateMetadata = (name: string, transform: (current: JSONObject) => JSONObject) => {
    updatePolicy((current) => ({
      ...current,
      models: {
        ...objectValue(current.models),
        [name]: transform({ ...objectValue(objectValue(current.models)[name]) }),
      },
    }));
  };

  return (
    <div className="model-routing-workspace">
      <div className="routing-policy-bar">
        <label>
          Policy revision
          <input
            disabled={disabled}
            min="1"
            onChange={(event) => updatePolicy((current) => ({
              ...current,
              revision: integerValue(event.target.value, 1),
            }))}
            type="number"
            value={numberValue(policy.revision) || 1}
          />
        </label>
        <label>
          Default selector
          <select
            disabled={disabled}
            onChange={(event) => updatePolicy((current) => ({
              ...current,
              default_virtual_model: event.target.value,
            }))}
            value={stringValue(policy.default_virtual_model) || "auto"}
          >
            {selectors.map((name) => <option key={name} value={name}>{name}</option>)}
          </select>
        </label>
        <span>Policy v{numberValue(policy.version) || 2}</span>
        <button className={confirmRemovePolicy ? "danger-button confirm" : "danger-button"} disabled={disabled}
          onClick={removePolicy} onBlur={() => setConfirmRemovePolicy(false)} type="button">
          {confirmRemovePolicy ? "Confirm remove model routing" : "Remove model routing"}
        </button>
      </div>

      {confirmRemovePolicy ? <p className="notice info routing-policy-notice" role="status">
        Remove all routing selectors, routing preferences, presets, and keyword rules from this draft.
        Direct virtual models, their aliases, and deployments remain available. Validate, then Save to apply.
      </p> : null}
      {missingModels.length ? <section className="notice error routing-policy-notice" aria-label="Missing routing models">
        <strong>Model routing references models that are no longer available</strong>
        <ul>{missingModels.map(({ name, locations }) => <li key={name}>
          <code>{name}</code>: {locations.length ? locations.join("; ") : "Unused routing preferences"}
        </li>)}</ul>
        <p>Update the candidates or stage roles below, choose another routing strategy, or remove model routing.
          Preset and inline keyword-rule references can also be edited in JSON.</p>
        {missingModels.some(({ locations }) => !locations.length) ? <button className="secondary-button" disabled={disabled}
          onClick={() => updatePolicy((current) => current)} type="button">Remove unused preferences</button> : null}
      </section> : null}

      <div className="structured-editor routing-editor" aria-disabled={disabled}>
        <aside className="model-list" aria-label="Model-routing selectors">
          <div className="model-list-heading">
            <div><span>Selectors</span><strong>{selectors.length}</strong></div>
            <button className="icon-button" disabled={disabled} onClick={addSelector} type="button">+</button>
          </div>
          <div className="model-list-items">
            {selectors.map((name) => (
              <button
                aria-current={name === selectedName ? "true" : undefined}
                className={name === selectedName ? "active" : ""}
                key={name}
                onClick={() => setSelectedName(name)}
                type="button"
              >
                <span>{name}</span>
                <small>{stringValue(objectValue(selectorMap[name]).strategy)}</small>
              </button>
            ))}
          </div>
        </aside>

        <div className="model-form routing-selector-form">
          <div className="model-form-heading">
            <div>
              <p className="eyebrow">Caller-facing model selector</p>
              <h3>{selectedName}</h3>
            </div>
            {selectedName === "auto" ? <small className="routing-required-selector">Required while model routing is enabled</small> : <button
              className={confirmRemove ? "danger-button confirm" : "danger-button"}
              disabled={disabled}
              onBlur={() => setConfirmRemove(false)}
              onClick={removeSelector}
              type="button"
            >
              {confirmRemove ? "Confirm remove" : "Remove selector"}
            </button>}
          </div>
          <fieldset disabled={disabled}>
            <div className="model-field-grid">
              <label>
                Selector name
                <input
                  onBlur={(event) => renameSelector(event.target.value.trim())}
                  onChange={() => undefined}
                  defaultValue={selectedName}
                  readOnly={selectedName === "auto"}
                  key={selectedName}
                />
              </label>
              <label>
                Routing strategy
                <select
                  onChange={(event) => updateSelector((current) => setRoutingStrategy(
                    current,
                    event.target.value,
                    canonicalModels,
                  ))}
                  value={stringValue(selected.strategy)}
                >
                  {strategies.map((strategy) => <option key={strategy} value={strategy}>{label(strategy)}</option>)}
                </select>
              </label>
              <label className="wide">
                Aliases
                <input
                  onBlur={(event) => updateSelector((current) => ({
                    ...current,
                    aliases: splitList(event.target.value),
                  }))}
                  defaultValue={stringArray(selected.aliases).join(", ")}
                  key={`${selectedName}-aliases`}
                  placeholder="smart, recommended"
                />
              </label>
            </div>

            {stringValue(selected.strategy) === "stage_router" ? (
              <StageRouterEditor
                canonicalModels={canonicalModels}
                onChange={updateSelector}
                onUseBalanced={() => updateSelector((current) => setRoutingStrategy(current, "balanced", canonicalModels))}
                selector={selected}
              />
            ) : (
              <section className="routing-candidate-section">
                <div className="section-title">
                  <div><strong>Candidate virtual models</strong><small>Capability filtering happens before this policy runs.</small></div>
                </div>
                <div className="capability-grid routing-candidate-grid">
                  {candidateNames.map((name) => (
                    <label key={name}>
                      <input
                        checked={stringArray(selected.models).includes(name)}
                        onChange={(event) => toggleCandidate(name, event.target.checked)}
                        type="checkbox"
                      />
                      <span>{name}{!canonicalModels.includes(name) ? " (missing model)" : ""}</span>
                    </label>
                  ))}
                </div>
              </section>
            )}

            {MM_PROJECTION_CONFIGURATION_VISIBLE ? (
              <MMProjectionPolicyEditor
                enabled={isObject(selected.mm_projection)}
                onEnabledChange={(enabled) => updateSelector((current) => setProjectionMode(current, enabled ? "enabled" : "inherit"))}
                onPolicyChange={(transform) => updateSelector((current) => updateProjection(current, transform))}
                policy={objectValue(selected.mm_projection)}
                scope="selector"
              />
            ) : null}

            <ModelMetadataTable
              deploymentFields={deploymentMetadata(document, additionalDocument)}
              candidates={canonicalModels}
              discoveredMetadata={discoveredMetadata}
              metadata={metadata}
              onChange={updateMetadata}
            />

            <KeywordRuleEditor
              onChange={(rules) => updatePolicy((current) => ({ ...current, keyword_rules: rules }))}
              rules={arrayValue(policy.keyword_rules)}
              selectors={selectors}
            />
          </fieldset>
        </div>
      </div>

      <RoutingSimulationPanel
        document={document}
        endpoints={routingEndpoints(selectorMap)}
        simulate={simulate}
      />
      <p className="routing-advanced-note">
        Named kwargs/presets and provider priority are preserved by this editor;
        use JSON mode for those advanced fields in this release.
      </p>
    </div>
  );
}

function StageRouterEditor({ canonicalModels, selector, onChange, onUseBalanced }: {
  canonicalModels: string[];
  selector: JSONObject;
  onChange: (transform: (current: JSONObject) => JSONObject) => void;
  onUseBalanced: () => void;
}) {
  const stage = objectValue(selector.stage_router);
  const models = stringArray(selector.models);
  const capable = stringValue(stage.capable_model) || models[0] || "";
  const efficient = stringValue(stage.efficient_model) || models[1] || "";
  const updateRole = (role: "capable_model" | "efficient_model", model: string) => {
    onChange((current) => {
      const currentStage = objectValue(current.stage_router);
      const otherRole = role === "capable_model" ? "efficient_model" : "capable_model";
      const previous = stringValue(currentStage[role]);
      const other = stringValue(currentStage[otherRole]);
      const nextStage = { ...currentStage, [role]: model };
      if (model === other) nextStage[otherRole] = previous;
      return {
        ...current,
        models: [stringValue(nextStage.capable_model), stringValue(nextStage.efficient_model)].filter(Boolean),
        stage_router: nextStage,
      };
    });
  };
  return (
    <section className="routing-candidate-section stage-router-section">
      <div className="section-title">
        <div>
          <strong>Agent-stage routing</strong>
          <small>Uses a content-free projection of recent tool results; raw prompts, tool names, arguments, and output are not retained.</small>
        </div>
      </div>
      {canonicalModels.length < 2 || capable === efficient || !canonicalModels.includes(capable) || !canonicalModels.includes(efficient) ? (
        <div className="routing-stage-warning">
          <p>Stage routing requires two different, available virtual models. Add or select a replacement model,
            use Balanced routing with the remaining models, or remove model routing.</p>
          <button className="secondary-button" onClick={onUseBalanced} type="button">Use Balanced routing</button>
        </div>
      ) : null}
      <div className="routing-projection-fields stage-router-fields">
        <label>
          Capable model
          <select aria-label="Capable model" onChange={(event) => updateRole("capable_model", event.target.value)} value={capable}>
            {!capable ? <option value="">Choose a model</option> : !canonicalModels.includes(capable) ? <option value={capable}>{capable} (missing model)</option> : null}
            {canonicalModels.map((model) => <option key={model} value={model}>{model}</option>)}
          </select>
        </label>
        <label>
          Efficient model
          <select aria-label="Efficient model" onChange={(event) => updateRole("efficient_model", event.target.value)} value={efficient}>
            {!efficient ? <option value="">Choose a model</option> : !canonicalModels.includes(efficient) ? <option value={efficient}>{efficient} (missing model)</option> : null}
            {canonicalModels.map((model) => <option key={model} value={model}>{model}</option>)}
          </select>
        </label>
        <label>
          Ambiguous-signal default
          <select
            aria-label="Ambiguous-signal default"
            onChange={(event) => onChange((current) => updateStageRouter(current, (value) => setOptionalString(value, "picker", event.target.value)))}
            value={stringValue(stage.picker) || "efficient_first"}
          >
            <option value="efficient_first">Efficient first</option>
            <option value="capable_first">Capable first</option>
          </select>
        </label>
        <label>
          Confidence threshold
          <input
            aria-label="Stage confidence threshold"
            max="1"
            min="0"
            onChange={(event) => onChange((current) => updateStageRouter(current, (value) => setOptionalNumber(value, "confidence_threshold", event.target.value)))}
            placeholder="Default: 0.5"
            step="0.05"
            type="number"
            value={numberOrBlank(stage.confidence_threshold)}
          />
        </label>
        <label>
          Recent turn window
          <input
            aria-label="Stage recent turn window"
            max="32"
            min="1"
            onChange={(event) => onChange((current) => updateStageRouter(current, (value) => setOptionalNumber(value, "recent_turn_window", event.target.value)))}
            placeholder="Default: 3"
            step="1"
            type="number"
            value={numberOrBlank(stage.recent_turn_window)}
          />
        </label>
      </div>
      <p className="routing-projection-help">
        Critical failures and compaction select the capable tier. Passing tests with settled production work select the efficient tier. An unavailable tier falls back only to the other configured model.
      </p>
    </section>
  );
}

function ModelMetadataTable({
  deploymentFields,
  candidates,
  discoveredMetadata,
  metadata,
  onChange,
}: {
  deploymentFields: Record<string, JSONObject>;
  candidates: string[];
  discoveredMetadata?: DiscoveredMetadataState;
  metadata: JSONObject;
  onChange: (name: string, transform: (current: JSONObject) => JSONObject) => void;
}) {
  return (
    <section className="routing-metadata-section">
      <div className="section-title">
        <div>
          <strong>Routing preferences</strong>
          <small>Weights and priorities are shared across selectors. Size, context, prices, and tags are configured in Model Deployments.</small>
        </div>
        {discoveredMetadata ? (
          <span className="metadata-discovery-summary">
            {discoveredMetadata.sources.length} source{discoveredMetadata.sources.length === 1 ? "" : "s"}
            {" · "}{Object.keys(discoveredMetadata.effective).length} model{Object.keys(discoveredMetadata.effective).length === 1 ? "" : "s"}
          </span>
        ) : null}
      </div>
      <div className="routing-metadata-table">
        {candidates.map((name) => {
          const model = objectValue(metadata[name]);
          const discovered = discoveredMetadata?.effective[name];
          const inherited = deploymentFields[name] ?? {};
          const effective: JSONObject = {...(model.discovery_disabled === true ? {} : discovered)};
          for (const field of ["size_b", "context", "input_price", "output_price", "tags"]) {
            if (field === "tags" ? stringArray(model[field]).length > 0 : typeof model[field] === "number" && model[field] !== 0) effective[field] = model[field];
          }
          Object.assign(effective, inherited);
          const sources = discoveredMetadata?.sources.filter((source) => source.models[name]);
          return (
            <article key={name}>
              <header>
                <strong>{name}</strong>
                <div className="metadata-model-controls">
                  {discovered ? <span className="metadata-origin">Discovered</span> : null}
                  <label>
                    <input
                      checked={model.enabled === true}
                      onChange={(event) => onChange(name, (current) => ({ ...current, enabled: event.target.checked }))}
                      type="checkbox"
                    /> Enabled
                  </label>
                </div>
              </header>
              <div>
                <NumericMetadata label="Weight" name="weight" model={model} onChange={onChange} modelName={name} />
                <NumericMetadata label="Priority" name="priority" model={model} onChange={onChange} modelName={name} />
              </div>
              <dl className="routing-inherited-metadata">
                {([["size_b", "Size (B)"], ["context", "Context"], ["input_price", "Input price"], ["output_price", "Output price"]] as const).map(([field, title]) =>
                  <div key={field}><dt>{title}</dt><dd>{typeof effective[field] === "number" ? (effective[field] as number).toLocaleString() : "Unknown"}</dd></div>)}
                <div><dt>Tags</dt><dd>{stringArray(effective.tags).join(", ") || "None reported"}</dd></div>
              </dl>
              {Object.keys(inherited).length > 0 && <p className="metadata-provenance">From model deployments · prices per million tokens</p>}
              {discovered ? (
                <label className="routing-checkbox metadata-discovery-toggle">
                  <input
                    checked={model.discovery_disabled === true}
                    onChange={(event) => onChange(name, (current) => setOptionalBoolean(current, "discovery_disabled", event.target.checked))}
                    type="checkbox"
                  /> Ignore discovered strategy metadata
                </label>
              ) : null}
              {sources?.length ? (
                <p className="metadata-provenance">
                  {sources.map((source) => `${source.source} · ${new Date(source.observed_at).toLocaleString()}`).join("; ")}
                </p>
              ) : null}
              {MM_PROJECTION_CONFIGURATION_VISIBLE ? (
                <MMProjectionModelOverride
                  model={model}
                  modelName={name}
                  onChange={onChange}
                />
              ) : null}
            </article>
          );
        })}
      </div>
    </section>
  );
}

function MMProjectionModelOverride({ model, modelName, onChange }: {
  model: JSONObject;
  modelName: string;
  onChange: (name: string, transform: (current: JSONObject) => JSONObject) => void;
}) {
  const mode = projectionMode(model);
  return (
    <details className="routing-projection-model" open={mode === "enabled"}>
      <summary>
        <span>Multimedia projection</span>
        <small>{mode === "inherit" ? "Inherit selector policy" : label(mode)}</small>
      </summary>
      <label>
        Projection behavior
        <select
          onChange={(event) => onChange(modelName, (current) => setProjectionMode(current, event.target.value))}
          value={mode}
        >
          <option value="inherit">Inherit selector policy</option>
          <option value="enabled">Enabled override</option>
          <option value="disabled">Explicitly disabled</option>
        </select>
      </label>
      {mode === "enabled" ? (
        <MMProjectionFields
          onChange={(transform) => onChange(modelName, (current) => updateProjection(current, transform))}
          policy={objectValue(model.mm_projection)}
        />
      ) : null}
    </details>
  );
}

function MMProjectionPolicyEditor({ enabled, onEnabledChange, onPolicyChange, policy, scope }: {
  enabled: boolean;
  onEnabledChange: (enabled: boolean) => void;
  onPolicyChange: (transform: (current: JSONObject) => JSONObject) => void;
  policy: JSONObject;
  scope: "selector";
}) {
  return (
    <section className="routing-projection-section">
      <div className="section-title">
        <div>
          <strong>Multimedia projection</strong>
          <small>Transform image, audio, or video input after {scope} selection and before provider delivery.</small>
        </div>
        <label className="routing-checkbox">
          <input checked={enabled} onChange={(event) => onEnabledChange(event.target.checked)} type="checkbox" />
          Enable by default
        </label>
      </div>
      {enabled ? <MMProjectionFields onChange={onPolicyChange} policy={policy} /> : (
        <p className="routing-projection-help">Off unless the selected model supplies an enabled override.</p>
      )}
    </section>
  );
}

function MMProjectionFields({ policy, onChange }: {
  policy: JSONObject;
  onChange: (transform: (current: JSONObject) => JSONObject) => void;
}) {
  return (
    <div className="routing-projection-fields">
      <label>
        Analyzer model
        <input
          onChange={(event) => onChange((current) => setOptionalString(current, "analyzer_model", event.target.value))}
          placeholder="Use connection default"
          value={stringValue(policy.analyzer_model)}
        />
      </label>
      <label>
        Failure behavior
        <select
          onChange={(event) => onChange((current) => setOptionalString(current, "failure_mode", event.target.value))}
          value={stringValue(policy.failure_mode) || "fallback"}
        >
          <option value="fallback">Fall back to original request</option>
          <option value="fail_closed">Reject when projection fails</option>
        </select>
      </label>
      <label>
        Timeout (ms)
        <input
          max="900000"
          min="1000"
          onChange={(event) => onChange((current) => setOptionalNumber(current, "timeout_ms", event.target.value))}
          placeholder="Connection default"
          step="1000"
          type="number"
          value={numberOrBlank(policy.timeout_ms)}
        />
      </label>
    </div>
  );
}

function NumericMetadata({ discoveredValue, label: text, modelName, name, model, onChange }: {
  discoveredValue?: number;
  label: string;
  modelName: string;
  name: string;
  model: JSONObject;
  onChange: (name: string, transform: (current: JSONObject) => JSONObject) => void;
}) {
  return (
    <label>
      {text}
      <input
        min="0"
        onChange={(event) => onChange(modelName, (current) => ({
          ...current,
          [name]: numberInput(event.target.value),
        }))}
        step={name.includes("price") || name === "size_b" ? "0.000001" : "1"}
        type="number"
        value={numberOrBlank(model[name])}
      />
      {discoveredValue !== undefined ? <small>Discovered: {discoveredValue.toLocaleString()}</small> : null}
    </label>
  );
}

function KeywordRuleEditor({ rules, selectors, onChange }: {
  rules: JSONObject[];
  selectors: string[];
  onChange: (rules: JSONObject[]) => void;
}) {
  const update = (index: number, transform: (rule: JSONObject) => JSONObject) => {
    const next = [...rules];
    next[index] = transform({ ...next[index] });
    onChange(next);
  };
  return (
    <section className="routing-rules-section">
      <div className="section-title">
        <div><strong>Ordered keyword rules</strong><small>The first matching rule overrides the requested selector plan.</small></div>
        <button
          className="text-button"
          onClick={() => onChange([...rules, {
            name: uniqueName("rule", new Set(rules.map((rule) => stringValue(rule.name)))),
            keywords: ["keyword"],
            virtual_model: selectors[0] ?? "auto",
          }])}
          type="button"
        >Add rule</button>
      </div>
      <div className="routing-rules-list">
        {rules.map((rule, index) => (
          <article key={`${stringValue(rule.name)}-${index}`}>
            <label>Name<input onChange={(event) => update(index, (current) => ({ ...current, name: event.target.value }))} value={stringValue(rule.name)} /></label>
            <label className="wide">Keywords<input onChange={(event) => update(index, (current) => ({ ...current, keywords: splitList(event.target.value) }))} value={stringArray(rule.keywords).join(", ")} /></label>
            <label>
              Selector
              <select
                onChange={(event) => update(index, (current) => {
                  const next: JSONObject = { ...current, virtual_model: event.target.value };
                  delete next.strategy;
                  delete next.models;
                  return next;
                })}
                value={stringValue(rule.virtual_model)}
              >
                {!stringValue(rule.virtual_model) ? <option value="">Inline strategy (JSON)</option> : null}
                {selectors.map((name) => <option key={name} value={name}>{name}</option>)}
              </select>
            </label>
            <label className="routing-checkbox"><input checked={rule.match_all === true} onChange={(event) => update(index, (current) => ({ ...current, match_all: event.target.checked }))} type="checkbox" /> Match every keyword</label>
            <button className="icon-button remove" aria-label={`Remove rule ${stringValue(rule.name) || index + 1}`} onClick={() => onChange(rules.filter((_, ruleIndex) => ruleIndex !== index))} type="button">−</button>
          </article>
        ))}
        {!rules.length ? <p>No keyword overrides configured.</p> : null}
      </div>
    </section>
  );
}

function RoutingSimulationPanel({ document, endpoints, simulate }: {
  document: ConfigurationDocument;
  endpoints: string[];
  simulate?: RoutingSimulator;
}) {
  const [requested, setRequested] = useState(endpoints[0] ?? "auto");
  const [text, setText] = useState("");
  const [capabilities, setCapabilities] = useState<string[]>([]);
  const [result, setResult] = useState<RoutingSimulationResult>();
  const [error, setError] = useState("");
  const [busy, setBusy] = useState(false);
  useEffect(() => {
    if (!endpoints.includes(requested)) setRequested(endpoints[0] ?? "auto");
  }, [endpoints, requested]);
  if (!simulate) return null;
  const run = async () => {
    setBusy(true);
    setError("");
    try {
      setResult(await simulate(document, requested, text, capabilities));
    } catch (value) {
      setResult(undefined);
      setError(value instanceof Error ? value.message : "Routing simulation failed");
    } finally {
      setBusy(false);
    }
  };
  return (
    <section className="routing-simulation-panel">
      <div className="section-title">
        <div><p className="eyebrow">Stateless policy preview</p><h3>Simulate routing</h3></div>
        <button className="secondary-button" disabled={busy} onClick={() => void run()} type="button">{busy ? "Simulating…" : "Simulate"}</button>
      </div>
      <div className="routing-simulation-inputs">
        <label>Requested selector<select onChange={(event) => setRequested(event.target.value)} value={requested}>{endpoints.map((endpoint) => <option key={endpoint} value={endpoint}>{endpoint}</option>)}</select></label>
        <label className="wide">Latest user routing text<textarea maxLength={16 * 1024} onChange={(event) => setText(event.target.value)} placeholder="Optional text for ordered keyword rules" value={text} /></label>
      </div>
      <details className="routing-simulation-capabilities">
        <summary>Required capabilities <small>{capabilities.length} selected</small></summary>
        <div className="capability-grid">
          {capabilityOptions.map(({ value: capability, label }) => <label key={capability}><input checked={capabilities.includes(capability)} onChange={(event) => setCapabilities((current) => event.target.checked ? [...current, capability] : current.filter((item) => item !== capability))} type="checkbox" /><span>{label}</span></label>)}
        </div>
      </details>
      {error ? <div className="notice error">{error}</div> : null}
      {result ? (
        <div className="routing-simulation-result" aria-label="Routing simulation result" role="region">
          <div><span>Selected logical model</span><strong>{result.decision.resolved_model}</strong></div>
          <div><span>Strategy</span><strong>{label(result.decision.strategy)}</strong></div>
          <div><span>Reason</span><strong>{result.decision.reason}</strong></div>
          <div><span>Matched rule</span><strong>{result.decision.matched_rule || "None"}</strong></div>
          {result.decision.provider_priority?.length ? (
            <div className="wide"><span>Provider preference</span><strong>{result.decision.provider_priority.join(" → ")}</strong></div>
          ) : null}
          <p>{result.available_models.length} capability-compatible model{result.available_models.length === 1 ? "" : "s"}; live adaptive observations are excluded.</p>
          <ul>{result.decision.candidates.map((candidate) => <li key={candidate.model} className={candidate.eligible ? "eligible" : "ineligible"}><span>{candidate.model}</span><small>{candidate.eligible ? candidate.score === undefined ? "Eligible" : `Score ${candidate.score}` : candidate.reason || "Ineligible"}</small></li>)}</ul>
        </div>
      ) : null}
    </section>
  );
}

function newPolicy(models: string[]): JSONObject {
  const metadata: JSONObject = {};
  models.forEach((name) => { metadata[name] = { enabled: true, weight: 100 }; });
  return {
    version: 2,
    revision: 1,
    default_virtual_model: "auto",
    virtual_models: { auto: { strategy: "balanced", models } },
    models: metadata,
  };
}

function routingEndpoints(selectors: JSONObject): string[] {
  const endpoints = new Set<string>();
  Object.entries(selectors).forEach(([name, raw]) => {
    endpoints.add(name);
    const selector = objectValue(raw);
    stringArray(selector.aliases).forEach((alias) => endpoints.add(alias));
    Object.keys(objectValue(selector.kwargs)).forEach((preset) => endpoints.add(`${name}:${preset}`));
  });
  return [...endpoints].sort();
}

function arrayValue(value: unknown): JSONObject[] {
  return Array.isArray(value) ? value.filter(isObject) : [];
}
function objectValue(value: unknown): JSONObject {
  return isObject(value) ? value : {};
}
function isObject(value: unknown): value is JSONObject {
  return Boolean(value) && typeof value === "object" && !Array.isArray(value);
}
function stringValue(value: unknown): string {
  return typeof value === "string" ? value : "";
}
function stringArray(value: unknown): string[] {
  return Array.isArray(value) ? value.filter((item): item is string => typeof item === "string") : [];
}
function numberValue(value: unknown): number {
  return typeof value === "number" && Number.isFinite(value) ? value : 0;
}
function numberOrBlank(value: unknown): number | "" {
  return typeof value === "number" && Number.isFinite(value) ? value : "";
}
function numberInput(value: string): number | undefined {
  return value === "" ? undefined : Number(value);
}
function projectionMode(model: JSONObject): "inherit" | "enabled" | "disabled" {
  if (model.mm_projection_disabled === true) return "disabled";
  return isObject(model.mm_projection) ? "enabled" : "inherit";
}
function setProjectionMode(value: JSONObject, mode: string): JSONObject {
  const next = { ...value };
  delete next.mm_projection;
  delete next.mm_projection_disabled;
  if (mode === "enabled") next.mm_projection = { failure_mode: "fallback" };
  if (mode === "disabled") next.mm_projection_disabled = true;
  return next;
}
function updateProjection(value: JSONObject, transform: (current: JSONObject) => JSONObject): JSONObject {
  const next = { ...value };
  next.mm_projection = transform({ ...objectValue(value.mm_projection) });
  delete next.mm_projection_disabled;
  return next;
}
function setOptionalString(value: JSONObject, field: string, input: string): JSONObject {
  const next = { ...value };
  const trimmed = input.trim();
  if (trimmed) next[field] = trimmed;
  else delete next[field];
  return next;
}
function setOptionalNumber(value: JSONObject, field: string, input: string): JSONObject {
  const next = { ...value };
  if (input === "") delete next[field];
  else next[field] = Number(input);
  return next;
}
function setOptionalBoolean(value: JSONObject, field: string, enabled: boolean): JSONObject {
  const next = { ...value };
  if (enabled) next[field] = true;
  else delete next[field];
  return next;
}
function setRoutingStrategy(value: JSONObject, strategy: string, canonicalModels: string[]): JSONObject {
  const next: JSONObject = { ...value, strategy };
  if (strategy !== "stage_router") {
    delete next.stage_router;
    next.models = [...new Set(stringArray(value.models).filter((model) => canonicalModels.includes(model)))];
    return next;
  }
  const existing = stringArray(value.models).filter((model) => canonicalModels.includes(model));
  const candidates = [...new Set([...existing, ...canonicalModels])].slice(0, 2);
  next.models = candidates;
  next.stage_router = {
    capable_model: candidates[0] ?? "",
    efficient_model: candidates[1] ?? "",
    picker: "efficient_first",
  };
  return next;
}
function updateStageRouter(value: JSONObject, transform: (current: JSONObject) => JSONObject): JSONObject {
  const next = { ...value };
  const stage = transform({ ...objectValue(value.stage_router) });
  next.stage_router = stage;
  next.models = [stringValue(stage.capable_model), stringValue(stage.efficient_model)].filter(Boolean);
  return next;
}
function integerValue(value: string, fallback: number): number {
  const parsed = Number.parseInt(value, 10);
  return Number.isFinite(parsed) ? parsed : fallback;
}
function splitList(value: string): string[] {
  return [...new Set(value.split(/[\n,]/).map((item) => item.trim()).filter(Boolean))];
}
function uniqueName(base: string, names: Set<string>): string {
  if (!names.has(base)) return base;
  let suffix = 2;
  while (names.has(`${base}-${suffix}`)) suffix += 1;
  return `${base}-${suffix}`;
}
function label(value: string): string {
  return value.split("_").map((part) => `${part.charAt(0).toUpperCase()}${part.slice(1)}`).join(" ");
}
