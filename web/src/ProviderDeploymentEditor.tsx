import { DeploymentMetadataEditor } from "./DeploymentMetadataEditor";
import { SparkrunRemoval } from "./SparkrunRemoval";
import { useEffect, useMemo, useRef, useState } from "react";
import type { ConfigurationDocument } from "./types";
import { capabilityOptions } from "./capabilities";
import { deploymentChoices, deploymentTitle, sparkrunDeploymentClusters } from "./deploymentTitles";
import { SparkrunRecipeWizard, durationMinutes } from "./SparkrunRecipeWizard";
import { sparkRunCatalog } from "./api";
import { SubscriptionSignIn } from "./SubscriptionSignIn";

type JSONObject = Record<string, unknown>;
type AuthDraft = { regular?: JSONObject; profile?: string };
type Selection = { kind: "provider" | "deployment"; index: number };

const providerTypes: Record<string, { label: string; url: string }> = {
  sparkrun: { label: "sparkrun", url: "" },
  openai: { label: "OpenAI (Chat)", url: "https://api.openai.com/v1" },
  openai_responses: { label: "OpenAI (Responses)", url: "https://api.openai.com/v1" },
  anthropic: { label: "Anthropic", url: "https://api.anthropic.com/v1" },
  openai_compatible: { label: "OpenAI compatible (custom)", url: "http://127.0.0.1:8000/v1" },
  gemini: { label: "Google Gemini", url: "https://generativelanguage.googleapis.com/v1beta" },
  bedrock: { label: "Amazon Bedrock", url: "" },
};
const responsesProvider = (type: unknown) => type === "openai_responses" || type === "openai_subscription";
const subscriptionDeploymentCompatible = (deployment: JSONObject) => !deployment.credential
  && ["", "static"].includes(stringValue(objectValue(deployment.endpoint_source).type))
  && stringArray(deployment.native_protocols).every((protocol) => protocol === "openai");
const providerTypeLabel = (type: unknown) => type === "openai_subscription"
  ? "OpenAI (Responses) · Codex Subscription" : providerTypes[stringValue(type)]?.label ?? stringValue(type);

function providerDeploymentDefaults(deployment: JSONObject, provider: JSONObject, previousType?: string): JSONObject {
  let next = { ...deployment };
  const protocol = defaultProtocolForProviderType(stringValue(provider.type));
  const previous = defaultProtocolForProviderType(previousType ?? "");
  const protocols = stringArray(next.native_protocols);
  if (protocols.length && protocol && (previous !== protocol || !protocols.includes(protocol))) {
    next.native_protocols = [...new Set([...protocols.filter((value) => value !== previous), protocol])];
  }
  if (responsesProvider(provider.type)) {
    next = toggleCapability(next, "responses", true);
    const policy = objectValue(next.capability_policy);
    if (stringArray(policy.unsupported).includes("responses")) {
      next = setObject(next, "capability_policy", setStringArray(policy, "unsupported", stringArray(policy.unsupported).filter((value) => value !== "responses")));
    }
  }
  return next;
}

export function ProviderDeploymentEditor({
  document,
  disabled,
  onChange,
  subscriptionAuth,
  section = "all",
  simplifiedCapabilities = false,
  readOnlyDocument,
  sparkrun,
  allowGeneratedRemoval = false,
}: {
  document: ConfigurationDocument;
  disabled: boolean;
  onChange: (document: ConfigurationDocument) => void;
  subscriptionAuth?: { token: string; enabled: boolean };
  section?: "all" | "providers" | "deployments";
  simplifiedCapabilities?: boolean;
  readOnlyDocument?: ConfigurationDocument;
  allowGeneratedRemoval?: boolean;
  sparkrun?: { enabled: boolean; token: string; revision: string;
    onEditingChange: (editing: boolean) => void;
    onPrepared: (document: ConfigurationDocument, message: string) => void };

}) {
  const shape = useMemo(() => inspectDocument(document), [document]);
  const generatedShape = useMemo(() => inspectDocument(readOnlyDocument ?? { providers: [], deployments: [], virtual_models: [] }), [readOnlyDocument]);
  const editableProviders = shape.providers ?? [];
  const editableDeployments = shape.deployments ?? [];
  const editableModels = shape.virtualModels ?? [];
  const providers = [...editableProviders, ...(generatedShape.providers ?? [])];
  const deployments = [...editableDeployments, ...(generatedShape.deployments ?? [])];
  const virtualModels = [...editableModels, ...(generatedShape.virtualModels ?? [])];
  const titles = deploymentChoices(document, readOnlyDocument);
  const [selectionState, setSelectionState] = useState<{ kind: Selection["kind"]; provider: number; deployment: number }>(() => ({
    kind: section === "deployments" ? "deployment" : section === "providers" || providers.length ? "provider" : "deployment",
    provider: 0,
    deployment: 0,
  }));
  const selectedKind = section === "all" ? selectionState.kind : section === "providers" ? "provider" : "deployment";
  const selection: Selection = { kind: selectedKind, index: selectionState[selectedKind] };
  const setSelection = (next: Selection) => setSelectionState((current) => ({
    ...current, kind: next.kind, [next.kind]: next.index,
  }));
  const readOnlySelected = selection.index >= (selection.kind === "provider" ? editableProviders.length : editableDeployments.length);
  const formDisabled = disabled || readOnlySelected;
  const [confirmRemove, setConfirmRemove] = useState(false);
  const [newDeployment, setNewDeployment] = useState<JSONObject>();
  const [deploymentType, setDeploymentType] = useState("local");
  const [recipeVisited, setRecipeVisited] = useState(false);
  const [editingRecipe, setEditingRecipe] = useState(false);
  const settingUp = Boolean(newDeployment) || editingRecipe;
  const onEditingChange = sparkrun?.onEditingChange;
  useEffect(() => { onEditingChange?.(settingUp); return () => onEditingChange?.(false); }, [settingUp, onEditingChange]);
  const cancelSetup = () => { setNewDeployment(undefined); setEditingRecipe(false); setRecipeVisited(false); setDeploymentType("local"); };
  const authDrafts = useRef(new Map<string, AuthDraft>());

  useEffect(() => {
    setConfirmRemove(false);
    if (selection.kind === "provider") {
      if (section === "all" && !providers.length && deployments.length) {
        setSelection({ kind: "deployment", index: 0 });
      } else if (selection.index > Math.max(0, providers.length - 1)) {
        setSelection({ kind: "provider", index: Math.max(0, providers.length - 1) });
      }
    } else if (section === "all" && !deployments.length && providers.length) {
      setSelection({ kind: "provider", index: 0 });
    } else if (selection.index > Math.max(0, deployments.length - 1)) {
      setSelection({ kind: "deployment", index: Math.max(0, deployments.length - 1) });
    }
  }, [deployments.length, providers.length, selection.index, selection.kind, section]);

  useEffect(() => setConfirmRemove(false), [selection.index, selection.kind]);

  if (shape.error) {
    return (
      <div className="structured-unavailable">
        <strong>Provider editor unavailable</strong>
        <p>{shape.error} Switch to JSON to repair the document without losing data.</p>
      </div>
    );
  }

  const providerNames = providers.map((provider) => stringValue(provider.name));
  const selectedProvider = selection.kind === "provider" ? providers[selection.index] : undefined;
  const selectedDeployment = selection.kind === "deployment" ? deployments[selection.index] : undefined;

  const updateProvider = (transform: (provider: JSONObject) => JSONObject) => {
    if (!selectedProvider || formDisabled) return;
    const nextProviders = [...editableProviders];
    const nextProvider = transform({ ...selectedProvider });
    nextProviders[selection.index] = nextProvider;
    const oldName = stringValue(selectedProvider.name);
    const nextName = stringValue(nextProvider.name);
    if (oldName !== nextName && authDrafts.current.has(oldName)) {
      authDrafts.current.set(nextName, authDrafts.current.get(oldName)!);
      authDrafts.current.delete(oldName);
    }
    const nextDeployments = editableDeployments.map((deployment) => {
      if (stringValue(deployment.provider) !== oldName) return deployment;
      const renamed = { ...deployment, provider: nextName };
      return selectedProvider.type !== nextProvider.type
        ? providerDeploymentDefaults(renamed, nextProvider, stringValue(selectedProvider.type)) : renamed;
    });
    onChange({ ...document, providers: nextProviders, deployments: nextDeployments });
  };

  const updateDeployment = (transform: (deployment: JSONObject) => JSONObject) => {
    if (!selectedDeployment || formDisabled) return;
    const nextDeployments = [...editableDeployments];
    let nextDeployment = transform({ ...selectedDeployment });
    if (nextDeployment.provider !== selectedDeployment.provider) {
      const provider = providers.find((value) => value.name === nextDeployment.provider);
      const previous = providers.find((value) => value.name === selectedDeployment.provider);
      if (provider) nextDeployment = providerDeploymentDefaults(nextDeployment, provider, stringValue(previous?.type));
    }
    nextDeployments[selection.index] = nextDeployment;
    const oldName = stringValue(selectedDeployment.name);
    const nextName = stringValue(nextDeployment.name);
    onChange({
      ...document,
      deployments: nextDeployments,
      virtual_models: oldName === nextName
        ? editableModels
        : renameDeploymentTargets(editableModels, oldName, nextName),
    });
  };

  const addProvider = () => {
    const name = uniqueName("new-provider", new Set(providerNames));
    const next: JSONObject = {
      name,
      type: "openai",
      base_url: providerTypes.openai!.url,
    };
    onChange({ ...document, providers: [...editableProviders, next] });
    setSelection({ kind: "provider", index: editableProviders.length });
  };

  const addDeployment = () => {
    if (!providers.length && !sparkrun?.enabled) return;
    const names = new Set(deployments.map((deployment) => stringValue(deployment.name)));
    const provider = selectedProvider
      ?? (selectionState.kind === "provider" ? providers[selectionState.provider] : providers.find((value) => value.name === selectedDeployment?.provider))
      ?? providers[0] ?? { name: "", type: "openai" };
    const next = providerDeploymentDefaults({
      name: uniqueName("new-deployment", names),
      provider: provider.name,
      model: "upstream-model",
    }, provider);
    if (sparkrun?.enabled) {
      setNewDeployment(next); setDeploymentType("local"); setSelection({kind: "deployment", index: selectionState.deployment});
    } else {
      onChange({ ...document, deployments: [...editableDeployments, next] });
      setSelection({ kind: "deployment", index: editableDeployments.length });
    }
  };

  const removeSelected = () => {
    if (formDisabled) return;
    if (!confirmRemove) {
      setConfirmRemove(true);
      return;
    }
    if (selectedProvider) {
      authDrafts.current.delete(stringValue(selectedProvider.name));
      onChange({
        ...document,
        providers: editableProviders.filter((_, index) => index !== selection.index),
      });
    } else if (selectedDeployment) {
      onChange({
        ...document,
        deployments: editableDeployments.filter((_, index) => index !== selection.index),
      });
    }
    setConfirmRemove(false);
  };

  const providerUseCount = selectedProvider
    ? deployments.filter((deployment) => deployment.provider === selectedProvider.name).length
    : 0;
  const deploymentUseCount = selectedDeployment
    ? countDeploymentTargets(virtualModels, stringValue(selectedDeployment.name))
    : 0;
  const removeBlocked = providerUseCount > 0 || deploymentUseCount > 0;

  return (
    <div className="structured-editor infrastructure-editor" aria-disabled={disabled}>
      <aside className="model-list infrastructure-list" aria-label="Providers and deployments">
        {section !== "deployments" ? <EntityList
          active={selection.kind === "provider" ? selection.index : -1}
          addLabel="Add provider"
          count={providers.length}
          disabled={disabled || settingUp}
          heading="Providers"
          items={providers.map((provider, index) => ({
            name: stringValue(provider.name),
            detail: providerTypeLabel(provider.type) || "No type",
            readOnly: index >= editableProviders.length,
          }))}
          onAdd={addProvider}
          onSelect={(index) => setSelection({ kind: "provider", index })}
        /> : null}
        {section !== "providers" ? <EntityList
          active={selection.kind === "deployment" ? selection.index : -1}
          addLabel="Add deployment"
          count={deployments.length}
          disabled={disabled || settingUp || (!providers.length && !sparkrun?.enabled)}
          heading="Deployments"
          items={deployments.map((deployment, index) => ({
            name: titles[stringValue(deployment.name)] ?? stringValue(deployment.name),
            readOnly: index >= editableDeployments.length,
            detail: objectValue(deployment.endpoint_source).controller === "sparkrun" ? (objectValue(deployment.endpoint_source).type === "activatable" ? "On-demand recipe" : "Discovered workload") : stringValue(deployment.provider) || "No provider",
          }))}
          onAdd={addDeployment}
          onSelect={(index) => setSelection({ kind: "deployment", index })}
        /> : null}
      </aside>

      <div className={!settingUp && readOnlySelected ? "model-form infrastructure-form generated-form" : "model-form infrastructure-form"}>
        {settingUp ? <>
          <div className="model-form-heading"><div><p className="eyebrow">Model deployment</p><h3>{editingRecipe ? "Edit sparkrun deployment" : "New deployment"}</h3></div>
            <button className="text-button" type="button" onClick={cancelSetup}>Cancel</button></div>
          <div className="deployment-type-field"><Field label="Deployment type">
            <select aria-label="Deployment type" value={editingRecipe ? "sparkrun" : deploymentType} disabled={editingRecipe || disabled} onChange={(e) => { setDeploymentType(e.target.value); if (e.target.value === "sparkrun") setRecipeVisited(true); }}>
              <option value="local">Standard</option><option value="sparkrun">sparkrun</option>
            </select>
            <small>{editingRecipe || deploymentType === "sparkrun" ? "Choose a recipe and let sparkrun manage its workload." : "Connect to an existing model endpoint through a provider."}</small>
          </Field></div>
          {newDeployment && <div hidden={deploymentType !== "local"}>
            {!providers.length ? <p className="notice info">Add a provider in Configuration → Providers to connect an existing endpoint, or select sparkrun to configure a recipe.</p> : null}
            <DeploymentForm hideHeading simplifiedCapabilities={simplifiedCapabilities} deployment={newDeployment} providers={providerNames}
              subscriptionProviders={providers.filter((p) => p.type === "openai_subscription").map((p) => stringValue(p.name))}
              providerType={stringValue(providers.find((p) => p.name === newDeployment.provider)?.type)} disabled={disabled}
              routeCount={0} removeBlocked={false} confirmRemove={false} onRemove={cancelSetup} onCancelRemove={() => {}}
              onChange={(transform) => setNewDeployment((previous) => { if (!previous) return previous; const next = transform(previous); return providerDeploymentDefaults(next, providers.find((p) => p.name === next.provider) ?? {}, stringValue(providers.find((p) => p.name === previous.provider)?.type)); })} />
            <div className="deployment-draft-actions"><button type="button" className="primary-button" disabled={disabled || !providerNames.includes(stringValue(newDeployment.provider)) || !stringValue(newDeployment.name).trim() || !stringValue(newDeployment.model).trim()}
              onClick={() => { onChange({...document, deployments: [...editableDeployments, newDeployment]}); setSelection({kind: "deployment", index: editableDeployments.length}); cancelSetup(); }}>Add to draft</button></div>
          </div>}
          {sparkrun && (recipeVisited || editingRecipe) && <div hidden={!editingRecipe && deploymentType !== "sparkrun"}>
            <SparkrunRecipeWizard embedded initialDeployment={editingRecipe ? selectedDeployment : undefined} token={sparkrun.token} document={document} revision={sparkrun.revision} onClose={cancelSetup}
              onChange={(next, reused, deployment) => {
                sparkrun.onPrepared(next, editingRecipe ? "Deployment settings updated in the draft." : reused ? "Model added using the existing deployment and its lifecycle settings." : "On-demand model added to the draft.");
                const index = [...(inspectDocument(next).deployments ?? []), ...(generatedShape.deployments ?? [])].findIndex((d) => d.name === deployment);
                setSelection({kind: "deployment", index: Math.max(0, index)}); cancelSetup();
              }} />
          </div>}
        </> : <>
        {readOnlySelected && (selectedProvider || selectedDeployment) ? <p className="read-only-note">sparkrun generated · Read only</p> : null}
        {selectedDeployment && (sparkrun?.enabled || objectValue(selectedDeployment.endpoint_source).controller === "sparkrun") ? <div className="deployment-type-field"><Field label="Deployment type"><select aria-label="Deployment type" disabled value={objectValue(selectedDeployment.endpoint_source).controller === "sparkrun" ? "sparkrun" : "local"}><option value="local">Standard</option><option value="sparkrun">sparkrun</option></select></Field></div> : null}
        {selectedProvider ? (
          <ProviderForm
            key={selection.index}
            authDrafts={authDrafts.current}
            deployments={deployments.filter((deployment) => deployment.provider === selectedProvider.name)}
            confirmRemove={confirmRemove}
            disabled={formDisabled}
            deploymentCount={providerUseCount}
            onCancelRemove={() => setConfirmRemove(false)}
            onChange={updateProvider}
            onRemove={removeSelected}
            provider={selectedProvider}
            removeBlocked={removeBlocked}
            subscriptionAuth={subscriptionAuth}
          />
        ) : selectedDeployment && sparkrun && objectValue(selectedDeployment.endpoint_source).controller === "sparkrun" ? (
          <>
            <FormHeading eyebrow="sparkrun workload" title={deploymentTitle(selectedDeployment)} disabled={formDisabled || removeBlocked} confirmRemove={confirmRemove} removeTitle={removeBlocked ? "Used by virtual models" : "Remove deployment"} onRemove={removeSelected} onBlur={() => setConfirmRemove(false)} hideRemove={readOnlySelected && allowGeneratedRemoval} />
            {readOnlySelected && allowGeneratedRemoval && readOnlyDocument ? <SparkrunRemoval
              key={stringValue(selectedDeployment.name)} document={document} generated={readOnlyDocument}
              deployments={[stringValue(selectedDeployment.name)]} disabled={disabled} onChange={onChange} /> : null}
            <div className="sparkrun-deployment-content"><SparkrunDeploymentSummary deployment={selectedDeployment} catalog={sparkrun} />
            {readOnlySelected && objectValue(selectedDeployment.endpoint_source).type === "activatable" ? <p className="notice info">
              This deployment is retained by a recipe binding in sparkrun’s proxy.yaml, even while its workload is stopped.
              {allowGeneratedRemoval ? " Use Remove from sparkroute above to exclude it and its generated names. No manual configuration-file edit is needed." : " Its generated names are managed with this deployment."}
            </p> : null}
            <DeploymentMetadataEditor deployment={selectedDeployment} disabled={formDisabled} onChange={updateDeployment} />
            {sparkrun?.enabled && !readOnlySelected && objectValue(selectedDeployment.endpoint_source).type === "activatable" ? <button type="button" className="secondary-button" disabled={disabled} onClick={() => setEditingRecipe(true)}>Edit recipe settings</button> : null}
            <p className="section-help">Manage public names and aliases in Virtual Models / Aliases. Deployment settings are shared by all of its names.</p></div>
          </>
        ) : selectedDeployment ? (
          <DeploymentForm
            simplifiedCapabilities={simplifiedCapabilities}
            confirmRemove={confirmRemove}
            deployment={selectedDeployment}
            disabled={formDisabled}
            onCancelRemove={() => setConfirmRemove(false)}
            onChange={updateDeployment}
            onRemove={removeSelected}
            providers={providerNames}
            subscriptionProviders={providers.filter((provider) => provider.type === "openai_subscription").map((provider) => stringValue(provider.name))}
            providerType={stringValue(providers.find(
              (provider) => provider.name === selectedDeployment.provider,
            )?.type)}
            removeBlocked={removeBlocked}
            routeCount={deploymentUseCount}
          />
        ) : (
          <div className="structured-empty">
            <strong>{section === "deployments" ? "No model deployment selected" : section === "providers" ? "No provider selected" : "No provider or deployment selected"}</strong>
            <p>{section === "deployments" && !providers.length
              ? sparkrun?.enabled ? "Add a deployment to choose an existing endpoint or a sparkrun recipe. Public model names are managed in Virtual Models / Aliases." : "Create a provider in Configuration → Providers, then add a deployment here."
              : "Add a provider, then add an independently routable deployment target."}</p>
          </div>
        )}
        </>}
      </div>
    </div>
  );
}

function SparkrunDeploymentSummary({deployment, catalog}: {deployment: JSONObject; catalog: {enabled: boolean; token: string}}) {
  const source = objectValue(deployment.endpoint_source);
  const [preview, setPreview] = useState<{name: string; source_path: string}>();
  const [error, setError] = useState("");
  const recipe = stringValue(source.recipe), overrides = JSON.stringify(source.overrides ?? {});
  useEffect(() => {
    setPreview(undefined); setError("");
    if (!catalog.enabled || !recipe) return;
    const abort = new AbortController();
    void sparkRunCatalog<{name: string; source_path: string}>(catalog.token, "catalog_resolve", {reference: recipe, overrides: JSON.parse(overrides)}, abort.signal)
      .then(setPreview).catch(() => { if (!abort.signal.aborted) setError("Recipe details are unavailable. Edit recipe settings to check the selection on the control node."); });
    return () => abort.abort();
  }, [catalog.enabled, catalog.token, recipe, overrides]);
  return <>
    <dl className="sparkrun-deployment-summary">
      <div><dt>Model</dt><dd>{stringValue(deployment.model)}</dd></div>
      <div><dt>Availability</dt><dd>{source.type === "activatable" ? "On demand · retained while stopped" : "Discovered while running"}</dd></div>
      <div><dt>Native APIs</dt><dd>{[...(stringArray(deployment.native_protocols).includes("openai") ? ["OpenAI (Chat Completions)"] : []), ...(stringArray(deployment.capabilities).includes("responses") ? ["OpenAI (Responses)"] : []), ...(stringArray(deployment.native_protocols).includes("anthropic") ? ["Anthropic Messages"] : [])].join(", ") || "OpenAI (Chat Completions)"}</dd></div>
      {capabilityOptions.some(({value}) => stringArray(deployment.capabilities).includes(value)) && <div><dt>Model capabilities</dt><dd>{capabilityOptions.filter(({value}) => stringArray(deployment.capabilities).includes(value)).map(({label}) => label).join(", ")}</dd></div>}
      <div><dt>Recipe</dt><dd><small className="recipe-source">{preview?.source_path || recipe || "Discovered workload"}</small></dd></div>
      <div><dt>Cluster</dt><dd>{sparkrunDeploymentClusters(deployment).join(", ") || "Not yet reported"}</dd></div>
      {source.type === "activatable" && <>
        <div><dt>Cold-start wait</dt><dd>{durationMinutes(source.activation_timeout, 30)} minutes</dd></div>
        <div><dt>Idle shutdown</dt><dd>{durationMinutes(source.idle_ttl, 0) > 0 ? `${source.idle_action === "sleep" ? "Sleep" : "Stop"} after ${durationMinutes(source.idle_ttl, 0)} minutes` : "Disabled"}</dd></div>
      </>}
    </dl>
    {error && <p className="notice info">{error}</p>}
  </>;
}

function EntityList({
  heading,
  count,
  items,
  active,
  disabled,
  addLabel,
  onAdd,
  onSelect,
}: {
  heading: string;
  count: number;
  items: Array<{ name: string; detail: string; readOnly?: boolean }>;
  active: number;
  disabled: boolean;
  addLabel: string;
  onAdd: () => void;
  onSelect: (index: number) => void;
}) {
  return (
    <section className="entity-list-group">
      <div className="model-list-heading">
        <div><span>{heading}</span><strong>{count}</strong></div>
        <button
          aria-label={addLabel}
          className="icon-button"
          disabled={disabled}
          onClick={onAdd}
          title={addLabel}
          type="button"
        >
          +
        </button>
      </div>
      <div className="model-list-items">
        {items.map((item, index) => (
          <button
            aria-current={index === active ? "true" : undefined}
            className={[index === active ? "active" : "", item.readOnly ? "generated-entry" : ""].filter(Boolean).join(" ")}
            key={`${item.name}-${index}`}
            onClick={() => onSelect(index)}
            type="button"
          >
            <span>{item.name || `${heading.slice(0, -1)} ${index + 1}`}</span>
            <small>{item.detail}</small>
            {item.readOnly ? <small className="ownership-label">sparkrun · Read only</small> : null}
          </button>
        ))}
        {!items.length ? <p>No {heading.toLowerCase()} configured.</p> : null}
      </div>
    </section>
  );
}

function ProviderForm({
  provider,
  deployments,
  authDrafts,
  disabled,
  deploymentCount,
  removeBlocked,
  confirmRemove,
  onChange,
  onRemove,
  onCancelRemove,
  subscriptionAuth,
}: {
  provider: JSONObject;
  deployments: JSONObject[];
  authDrafts: Map<string, AuthDraft>;
  disabled: boolean;
  deploymentCount: number;
  removeBlocked: boolean;
  confirmRemove: boolean;
  onChange: (transform: (provider: JSONObject) => JSONObject) => void;
  onRemove: () => void;
  onCancelRemove: () => void;
  subscriptionAuth?: { token: string; enabled: boolean };
}) {
  const auth = objectValue(provider.auth);
  const subscription = provider.type === "openai_subscription";
  const authType = subscription ? "codex_subscription" : stringValue(auth.type);
  const displayType = subscription ? "openai_responses" : stringValue(provider.type);
  const providerName = stringValue(provider.name);
  const authDraft = authDrafts.get(providerName) ?? {};
  authDrafts.set(providerName, authDraft);
  const subscriptionCompatible = deployments.every(subscriptionDeploymentCompatible);
  const leaveSubscription = (value: JSONObject): JSONObject => {
    if (value.type !== "openai_subscription") return value;
    authDraft.profile = stringValue(value.subscription_profile);
    const next: JSONObject = { ...value, type: "openai_responses", base_url: providerTypes.openai_responses!.url, ...authDraft.regular };
    delete next.subscription_profile;
    return next;
  };
  const changeAuth = (type: string) => onChange((value) => {
    if (type === "codex_subscription") {
      authDraft.regular = Object.fromEntries(["type", "base_url", "auth", "region"].filter((key) => value[key] !== undefined).map((key) => [key, value[key]]));
      const next = setProviderType(value, "openai_subscription");
      next.subscription_profile = authDraft.profile || "codex";
      return next;
    }
    return setAuthType(leaveSubscription(value), type);
  });
  return (
    <>
      <FormHeading
        confirmRemove={confirmRemove}
        disabled={disabled || removeBlocked}
        eyebrow="Endpoint and authentication boundary"
        onBlur={onCancelRemove}
        onRemove={onRemove}
        removeTitle={removeBlocked ? `Used by ${deploymentCount} deployment${deploymentCount === 1 ? "" : "s"}` : "Remove provider"}
        title={stringValue(provider.name) || "Unnamed provider"}
      />
      <fieldset disabled={disabled}>
        <div className="model-field-grid infrastructure-field-grid provider-field-grid">
          <Field label="Provider name">
            <input value={stringValue(provider.name)} onChange={(event) => onChange((value) => setString(value, "name", event.target.value, true))} />
          </Field>
          <Field label="Provider type">
            <select aria-label="Provider type" value={displayType} onChange={(event) => onChange((value) => setProviderType(leaveSubscription(value), event.target.value))}>
              {Object.entries(providerTypes).map(([type, preset]) => <option key={type} value={type}>{preset.label}</option>)}
              {displayType && !providerTypes[displayType] ? <option value={displayType}>Existing: {displayType}</option> : null}
            </select>
            <small>{responsesProvider(provider.type) ? "Responses is enabled for every deployment using this provider. Chat Completions requires a Chat provider."
              : displayType === "anthropic" ? "Uses the native Anthropic Messages API."
              : displayType === "sparkrun" ? "Native APIs follow each deployment’s runtime and recipe."
              : "Sets the default upstream API for this provider’s deployments."}</small>
          </Field>
          {!subscription ? <>
            {!["bedrock", "sparkrun"].includes(displayType) ? <Field label="Base URL" wide>
              <input placeholder={providerTypes[displayType]?.url || "https://api.example/v1"} spellCheck={false} value={stringValue(provider.base_url)} onChange={(event) => onChange((value) => setString(value, "base_url", event.target.value))} />
              <small>API root for your hosted or local provider.</small>
            </Field> : null}
            {displayType === "bedrock" ? <Field label="AWS region">
              <input aria-label="AWS region" placeholder="us-east-1" spellCheck={false} value={stringValue(provider.region)} onChange={(event) => onChange((value) => setString(value, "region", event.target.value))} />
              <small>The Bedrock runtime endpoint is derived from this region.</small>
            </Field> : null}
          </> : null}
        </div>

        {displayType !== "sparkrun" ? <details className="model-section policy-section" open>
          <summary><span>Provider authentication</span><small>{subscription ? "Codex subscription" : "Resolved only by configured credential sources"}</small></summary>
          <div className="policy-fields provider-auth-fields">
            <Field label="Authentication type">
              <select aria-label="Authentication type" value={authType} onChange={(event) => changeAuth(event.target.value)}>
                <option value="" disabled={displayType === "bedrock"}>None</option>
                <option value="bearer" disabled={displayType === "bedrock"}>Bearer</option>
                <option value="header" disabled={displayType === "bedrock"}>Custom header</option>
                <option value="aws_sigv4" disabled={displayType !== "bedrock"}>AWS SigV4</option>
                <option value="codex_subscription" disabled={!displayType.startsWith("openai") || !subscriptionCompatible}>Codex Subscription</option>
                {authType && !["bearer", "header", "aws_sigv4", "codex_subscription"].includes(authType) ? (
                  <option value={authType}>Existing: {authType}</option>
                ) : null}
              </select>
            </Field>
            {!subscriptionCompatible ? <p className="section-help wide">Codex Subscription requires static deployments without credential overrides or other native protocols. Use a separate provider for these deployments.</p> : null}
            {subscription ? <Field label="Subscription profile" wide>
              <input aria-label="Subscription profile" autoComplete="off" spellCheck={false} value={stringValue(provider.subscription_profile)} onChange={(event) => onChange((value) => setString(value, "subscription_profile", event.target.value))} />
              <small>A reusable account name, such as personal-codex. Uses the fixed Codex Responses endpoint.</small>
            </Field> : null}
            {authType && !subscription ? (
              <Field label="Credential reference" wide>
                <input
                  autoComplete="off"
                  placeholder={authType === "aws_sigv4" ? "workload://aws" : "env://PROVIDER_API_KEY"}
                  spellCheck={false}
                  value={stringValue(auth.credential)}
                  onChange={(event) => onChange((value) => setNestedString(value, "auth", "credential", event.target.value))}
                />
                <small>Reference only; resolved secret material is never returned to this console.</small>
              </Field>
            ) : null}
            {authType === "header" ? (
              <>
                <Field label="Authentication header">
                  <input placeholder="X-Api-Key" spellCheck={false} value={stringValue(auth.header)} onChange={(event) => onChange((value) => setNestedString(value, "auth", "header", event.target.value))} />
                </Field>
                <Field label="Value prefix">
                  <input placeholder="Optional, for example Token " value={stringValue(auth.prefix)} onChange={(event) => onChange((value) => setNestedString(value, "auth", "prefix", event.target.value))} />
                </Field>
              </>
            ) : null}
          </div>
          {subscription ? <SubscriptionSignIn key={stringValue(provider.subscription_profile)} profile={stringValue(provider.subscription_profile)} token={subscriptionAuth?.token ?? ""} enabled={!disabled && (subscriptionAuth?.enabled ?? false)} /> : null}
        </details> : <p className="section-help">sparkrun supplies workload endpoints. Configure each deployment’s native APIs in its recipe settings.</p>}

        <HeaderSection
          heading="Default upstream headers"
          headers={objectValue(provider.default_headers)}
          identity={`provider-${stringValue(provider.name)}`}
          onChange={(headers) => onChange((value) => setObject(value, "default_headers", headers))}
        />
        <JSONDefaultsSection
          heading="Provider extra body defaults"
          identity={`provider-extra-body-${stringValue(provider.name)}`}
          onChange={(defaults) => onChange((value) => setObject(value, "extra_body", defaults))}
          value={objectValue(provider.extra_body)}
        />
      </fieldset>
    </>
  );
}

function setProviderType(value: JSONObject, type: string): JSONObject {
  const next: JSONObject = { ...value, type };
  if (type === "sparkrun") {
    delete next.base_url; delete next.auth; delete next.region; delete next.subscription_profile;
  } else if (type === "openai_subscription") {
    delete next.base_url;
    delete next.auth;
    delete next.region;
    next.subscription_profile ||= "codex";
  } else {
    delete next.subscription_profile;
    const knownURLs = ["https://provider.example/v1", ...Object.values(providerTypes).map((preset) => preset.url)];
    if (!next.base_url || knownURLs.includes(stringValue(next.base_url))) next.base_url = providerTypes[type]?.url ?? "";
    if (type === "bedrock") {
      next.region ||= "us-east-1";
      next.auth = { type: "aws_sigv4", credential: "workload://aws" };
    } else {
      delete next.region;
      if (objectValue(next.auth).type === "aws_sigv4") delete next.auth;
      if ((type === "anthropic" || type === "gemini") && !next.auth) {
        next.auth = { type: "header", header: type === "anthropic" ? "x-api-key" : "x-goog-api-key" };
      }
    }
  }
  return next;
}

function DeploymentForm({
  hideHeading = false,
  simplifiedCapabilities,
  deployment,
  providers,
  subscriptionProviders,
  providerType,
  disabled,
  routeCount,
  removeBlocked,
  confirmRemove,
  onChange,
  onRemove,
  onCancelRemove,
}: {
  hideHeading?: boolean;
  simplifiedCapabilities: boolean;
  deployment: JSONObject;
  providers: string[];
  subscriptionProviders: string[];
  providerType: string;
  disabled: boolean;
  routeCount: number;
  removeBlocked: boolean;
  confirmRemove: boolean;
  onChange: (transform: (deployment: JSONObject) => JSONObject) => void;
  onRemove: () => void;
  onCancelRemove: () => void;
}) {
  const explicitProtocols = stringArray(deployment.native_protocols);
  const defaultProtocol = defaultProtocolForProviderType(providerType);
  const nativeProtocols = explicitProtocols.length
    ? explicitProtocols
    : defaultProtocol ? [defaultProtocol] : [];

  return (
    <>
      {!hideHeading && <FormHeading
        confirmRemove={confirmRemove}
        disabled={disabled || removeBlocked}
        eyebrow="Independently attributable routing target"
        onBlur={onCancelRemove}
        onRemove={onRemove}
        removeTitle={removeBlocked ? `Used by ${routeCount} virtual-model target${routeCount === 1 ? "" : "s"}` : "Remove deployment"}
        title={deploymentTitle(deployment) || "Unnamed deployment"}
      />}
      <fieldset disabled={disabled}>
        <div className="model-field-grid infrastructure-field-grid">
          <Field label={deployment.title ? "Deployment ID" : "Deployment name"}>
            <input value={stringValue(deployment.name)} onChange={(event) => onChange((value) => setString(value, "name", event.target.value, true))} />
          </Field>
          <Field label="Display title">
            <input placeholder="Optional; defaults to the deployment ID" value={stringValue(deployment.title)} onChange={(event) => onChange((value) => setString(value, "title", event.target.value))} />
          </Field>
          <Field label="Provider">
            <select aria-label="Provider" value={stringValue(deployment.provider)} onChange={(event) => onChange((value) => setString(value, "provider", event.target.value, true))}>
              {stringValue(deployment.provider) && !providers.includes(stringValue(deployment.provider)) ? (
                <option value={stringValue(deployment.provider)}>Unknown: {stringValue(deployment.provider)}</option>
              ) : null}
              {providers.map((provider) => <option key={provider} value={provider} disabled={subscriptionProviders.includes(provider) && !subscriptionDeploymentCompatible(deployment)}>{provider}</option>)}
            </select>
          </Field>
          <Field label="Upstream model" wide>
            <input spellCheck={false} value={stringValue(deployment.model)} onChange={(event) => onChange((value) => setString(value, "model", event.target.value, true))} />
          </Field>
          {providerType !== "openai_subscription" ? <Field label="Credential override" wide>
            <input
              autoComplete="off"
              placeholder="Optional; inherits provider authentication"
              spellCheck={false}
              value={stringValue(deployment.credential)}
              onChange={(event) => onChange((value) => setString(value, "credential", event.target.value))}
            />
            <small>Overrides the provider credential while retaining its authentication method.</small>
          </Field> : <p className="section-help wide">Authentication is managed by the provider’s Codex subscription profile.</p>}
        </div>

        {!simplifiedCapabilities ? <details className="model-section capability-section" open>
          <summary><span>Native upstream protocols</span><small>{nativeProtocols.length} enabled</small></summary>
          <p className="section-help">
            Requests prefer a native dialect before an available translation. With no explicit list, the gateway infers one protocol from the provider type.
          </p>
          <div className="capability-grid">
            {["openai", "anthropic", "gemini", "bedrock"].map((protocol) => {
              const checked = nativeProtocols.includes(protocol);
              return (
                <label key={protocol}>
                  <input
                    checked={checked}
                    disabled={providerType === "openai_subscription" || checked && nativeProtocols.length === 1}
                    onChange={(event) => onChange((value) => toggleNativeProtocol(
                      value,
                      providerType,
                      protocol,
                      event.target.checked,
                    ))}
                    type="checkbox"
                  />
                  <span>{humanize(protocol)}</span>
                </label>
              );
            })}
          </div>
        </details> : null}

        <details className="model-section capability-section" open>
          <summary><span>Deployment capabilities</span><small>{simplifiedCapabilities ? "Optional" : `${stringArray(deployment.capabilities).length} declared`}</small></summary>
          <p className="section-help">{simplifiedCapabilities
            ? "Vision and file inputs are optional declarations. Unchecked leaves support unspecified; requests are tried unless a configured policy says otherwise. The native API follows the provider type. Advanced declarations are preserved and available in JSON."
            : "Declarations are authoritative; only enable semantics this target actually supports. Other configured declarations are preserved and available in JSON."}
            {responsesProvider(providerType) ? " Responses is required by the selected provider type." : null}
          </p>
          <div className="capability-grid">
            {capabilityOptions.map(({ value: capability, label }) => (
              <label key={capability}>
                <input
                  checked={stringArray(deployment.capabilities).includes(capability)}
                  onChange={(event) => onChange((value) => toggleCapability(value, capability, event.target.checked))}
                  type="checkbox"
                />
                <span>{label}</span>
              </label>
            ))}
          </div>
        </details>

        <DeploymentMetadataEditor deployment={deployment} disabled={disabled} onChange={onChange} />

        <details className="model-section policy-section">
          <summary><span>Concurrency and circuit policy</span><small>Optional limits and failure handling</small></summary>
          <CircuitFields deployment={deployment} onChange={onChange} />
        </details>

        <details className="model-section policy-section">
          <summary>
            <span>Endpoint source &amp; lifecycle</span>
            <small>{humanize(stringValue(objectValue(deployment.endpoint_source).type) || "static")}</small>
          </summary>
          {providerType === "openai_subscription" ? <p className="section-help">Codex subscriptions use a fixed, static endpoint. sparkrun start/stop controls apply to local model providers.</p>
            : <EndpointSourceFields deployment={deployment} onChange={onChange} />}
        </details>

        <HeaderSection
          heading="Deployment upstream headers"
          headers={objectValue(deployment.upstream_headers)}
          identity={`deployment-${stringValue(deployment.name)}`}
          onChange={(headers) => onChange((value) => setObject(value, "upstream_headers", headers))}
        />
        <JSONDefaultsSection
          heading="Deployment extra body defaults"
          identity={`deployment-extra-body-${stringValue(deployment.name)}`}
          onChange={(defaults) => onChange((value) => setObject(value, "extra_body", defaults))}
          value={objectValue(deployment.extra_body)}
        />
      </fieldset>
    </>
  );
}

function EndpointSourceFields({
  deployment,
  onChange,
}: {
  deployment: JSONObject;
  onChange: (transform: (deployment: JSONObject) => JSONObject) => void;
}) {
  const source = objectValue(deployment.endpoint_source);
  const sourceType = stringValue(source.type) || "static";
  const [clusters, setClusters] = useState(stringArray(source.cluster_candidates).join(", "));
  const [overrides, setOverrides] = useState(formatOverrides(objectValue(source.overrides)));
  useEffect(() => {
    setClusters(stringArray(source.cluster_candidates).join(", "));
    setOverrides(formatOverrides(objectValue(source.overrides)));
  }, [deployment.name, source.cluster_candidates, source.overrides]);

  return (
    <div className="policy-fields circuit-fields">
      <Field label="Endpoint source">
        <select value={sourceType} onChange={(event) => onChange((value) => setEndpointSourceType(value, event.target.value))}>
          <option value="static">Static provider URL</option>
          <option value="discovered">Discovered endpoints</option>
          <option value="activatable">On-demand activation</option>
          {!['static', 'discovered', 'activatable'].includes(sourceType) ? (
            <option value={sourceType}>Existing: {sourceType}</option>
          ) : null}
        </select>
      </Field>
      {sourceType !== "static" ? (
        <Field label="Runtime controller">
          <input value={stringValue(source.controller)} onChange={(event) => onChange((value) => setNestedString(value, "endpoint_source", "controller", event.target.value))} />
        </Field>
      ) : null}
      {sourceType === "discovered" ? (
        <p className="section-help wide">Only ready, unexpired, authorized registrations for this deployment are routable.</p>
      ) : null}
      {sourceType === "activatable" ? (
        <>
          <Field label="Binding revision">
            <input spellCheck={false} value={stringValue(source.revision)} onChange={(event) => onChange((value) => setNestedString(value, "endpoint_source", "revision", event.target.value))} />
          </Field>
          <Field label="Recipe reference">
            <input spellCheck={false} value={stringValue(source.recipe)} onChange={(event) => onChange((value) => setNestedString(value, "endpoint_source", "recipe", event.target.value))} />
          </Field>
          <Field label="Recipe revision">
            <input placeholder="Optional immutable digest" spellCheck={false} value={stringValue(source.recipe_revision)} onChange={(event) => onChange((value) => setNestedString(value, "endpoint_source", "recipe_revision", event.target.value))} />
          </Field>
          <Field label="Cold-start behavior">
            <select value={stringValue(source.cold_start) || "wait"} onChange={(event) => onChange((value) => setNestedString(value, "endpoint_source", "cold_start", event.target.value))}>
              <option value="wait">Wait within limits</option>
              <option value="reject">Reject while cold</option>
            </select>
          </Field>
          <Field label="Activation timeout">
            <input placeholder="30m" value={stringValue(source.activation_timeout)} onChange={(event) => onChange((value) => setNestedString(value, "endpoint_source", "activation_timeout", event.target.value))} />
          </Field>
          <Field label="Idle TTL">
            <input placeholder="30m" value={stringValue(source.idle_ttl)} onChange={(event) => onChange((value) => setNestedString(value, "endpoint_source", "idle_ttl", event.target.value))} />
          </Field>
          <Field label="Maximum queued waiters">
            <input min="0" max="100000" step="1" type="number" value={numberInput(source.max_queued_waiters)} onChange={(event) => onChange((value) => setNestedNumber(value, "endpoint_source", "max_queued_waiters", event.target.value))} />
            <small>Blank uses the gateway default of 100.</small>
          </Field>
          <Field label="Maximum queued body bytes">
            <input min="0" max="1099511627776" step="1" type="number" value={numberInput(source.max_queued_body_bytes)} onChange={(event) => onChange((value) => setNestedNumber(value, "endpoint_source", "max_queued_body_bytes", event.target.value))} />
            <small>Blank uses 256 MiB.</small>
          </Field>
          <Field label="Cluster candidates" wide>
            <textarea
              onBlur={() => onChange((value) => setNested(value, "endpoint_source", (nested) => setStringArray(nested, "cluster_candidates", splitList(clusters))))}
              onChange={(event) => setClusters(event.target.value)}
              placeholder="spark-a, spark-b"
              value={clusters}
            />
          </Field>
          <Field label="Approved recipe overrides" wide>
            <textarea
              onBlur={() => onChange((value) => setNested(value, "endpoint_source", (nested) => setObject(nested, "overrides", parseOverrides(overrides))))}
              onChange={(event) => setOverrides(event.target.value)}
              placeholder={'tensor_parallel=2\nprofile=throughput'}
              value={overrides}
            />
            <small>One key=value pair per line. Inference requests cannot change these values.</small>
          </Field>
          <p className="section-help wide">Dynamic endpoint URLs are accepted only through the configured runtime registry and its injected endpoint authorization policy.</p>
        </>
      ) : null}
    </div>
  );
}

function FormHeading({
  eyebrow,
  title,
  disabled,
  confirmRemove,
  removeTitle,
  onRemove,
  onBlur,
  hideRemove = false,
}: {
  eyebrow: string;
  title: string;
  disabled: boolean;
  confirmRemove: boolean;
  removeTitle: string;
  onRemove: () => void;
  onBlur: () => void;
  hideRemove?: boolean;
}) {
  return (
    <div className="model-form-heading">
      <div><p className="eyebrow">{eyebrow}</p><h3>{title}</h3></div>
      {!hideRemove ? <button
        className={confirmRemove ? "danger-button confirm" : "danger-button"}
        disabled={disabled}
        onBlur={onBlur}
        onClick={onRemove}
        title={removeTitle}
        type="button"
      >
        {confirmRemove ? "Confirm remove" : "Remove"}
      </button> : null}
    </div>
  );
}

function CircuitFields({
  deployment,
  onChange,
}: {
  deployment: JSONObject;
  onChange: (transform: (deployment: JSONObject) => JSONObject) => void;
}) {
  const circuit = objectValue(deployment.circuit);
  return (
    <div className="policy-fields circuit-fields">
      <Field label="Maximum concurrency">
        <input aria-label="Maximum concurrency" min="0" max="1000000" step="1" type="number" value={numberInput(deployment.max_concurrency)} onChange={(event) => onChange((value) => setNumber(value, "max_concurrency", event.target.value))} />
        <small>Zero or blank means unlimited per gateway replica.</small>
      </Field>
      <label className="checkbox-field wide">
        <input checked={booleanValue(circuit.disabled)} onChange={(event) => onChange((value) => setNestedBoolean(value, "circuit", "disabled", event.target.checked))} type="checkbox" />
        <span>Disable passive circuit health</span>
      </label>
      <Field label="Consecutive failures">
        <input min="0" max="10000" step="1" type="number" value={numberInput(circuit.consecutive_failures)} onChange={(event) => onChange((value) => setNestedNumber(value, "circuit", "consecutive_failures", event.target.value))} />
      </Field>
      <Field label="Minimum samples">
        <input min="0" max="10000" step="1" type="number" value={numberInput(circuit.minimum_samples)} onChange={(event) => onChange((value) => setNestedNumber(value, "circuit", "minimum_samples", event.target.value))} />
      </Field>
      <Field label="Sample window">
        <input min="0" max="10000" step="1" type="number" value={numberInput(circuit.sample_window)} onChange={(event) => onChange((value) => setNestedNumber(value, "circuit", "sample_window", event.target.value))} />
      </Field>
      <Field label="Failure rate">
        <input min="0" max="1" step="0.01" type="number" value={numberInput(circuit.failure_rate)} onChange={(event) => onChange((value) => setNestedNumber(value, "circuit", "failure_rate", event.target.value))} />
      </Field>
      <Field label="Base ejection time">
        <input placeholder="30s" value={stringValue(circuit.base_ejection_time)} onChange={(event) => onChange((value) => setNestedString(value, "circuit", "base_ejection_time", event.target.value))} />
      </Field>
      <Field label="Maximum ejection time">
        <input placeholder="10m" value={stringValue(circuit.max_ejection_time)} onChange={(event) => onChange((value) => setNestedString(value, "circuit", "max_ejection_time", event.target.value))} />
      </Field>
    </div>
  );
}

function HeaderSection({
  heading,
  headers,
  identity,
  onChange,
}: {
  heading: string;
  headers: JSONObject;
  identity: string;
  onChange: (headers: JSONObject) => void;
}) {
  const entries = Object.entries(headers) as Array<[string, JSONObject]>;
  const addHeader = () => {
    const name = uniqueHeaderName("X-New-Header", Object.keys(headers));
    onChange({ ...headers, [name]: { value: "", scope: "inference" } });
  };
  return (
    <details className="model-section header-section">
      <summary><span>{heading}</span><small>{entries.length} configured</small></summary>
      <p className="section-help">
        Protected transport and adapter authentication headers are rejected by validation. Existing literal values are preserved but never rendered in this view.
      </p>
      <div className="header-list">
        {entries.map(([name, value]) => (
          <HeaderRow
            headers={headers}
            identity={`${identity}-${name}`}
            key={name}
            name={name}
            onChange={onChange}
            value={value}
          />
        ))}
        {!entries.length ? <p className="header-empty">No custom headers configured.</p> : null}
      </div>
      <button className="secondary-button add-header" onClick={addHeader} type="button">Add header</button>
    </details>
  );
}

function JSONDefaultsSection({ heading, identity, value, onChange }: {
  heading: string;
  identity: string;
  value: JSONObject;
  onChange: (value: JSONObject) => void;
}) {
  const serialized = JSON.stringify(value, null, 2);
  const inputRef = useRef<HTMLTextAreaElement>(null);
  const [draft, setDraft] = useState(serialized);
  const [error, setError] = useState("");
  useEffect(() => {
    setDraft(serialized);
    setError("");
    inputRef.current?.setCustomValidity("");
  }, [identity, serialized]);
  const commit = () => {
    try {
      const parsed: unknown = JSON.parse(draft.trim() || "{}");
      if (!isObject(parsed)) throw new Error("Value must be a JSON object.");
      setError("");
      onChange(parsed);
    } catch (value) {
      setError(value instanceof Error ? value.message : "Invalid JSON object.");
    }
  };
  return (
    <details className="model-section policy-section extra-body-section">
      <summary><span>{heading}</span><small>{Object.keys(value).length} configured</small></summary>
      <p className="section-help">
        Static JSON defaults are merged provider first, then deployment; caller fields win. Protocol-owned request fields and oversized values are rejected by validation.
      </p>
      <div className="extra-body-editor">
        <textarea
          aria-label={`${heading} JSON`}
          aria-invalid={Boolean(error)}
          ref={inputRef}
          maxLength={4 * 1024 * 1024}
          onBlur={commit}
          onChange={(event) => {
            const text = event.target.value;
            setDraft(text);
            try {
              if (!isObject(JSON.parse(text.trim() || "{}"))) throw new Error("Value must be a JSON object.");
              event.currentTarget.setCustomValidity("");
              setError("");
            } catch (error) {
              const message = error instanceof Error ? error.message : "Invalid JSON object.";
              event.currentTarget.setCustomValidity(message);
              setError(message);
            }
          }}
          spellCheck={false}
          value={draft}
        />
        {error ? <small className="field-error" role="alert">{error}</small> : null}
      </div>
    </details>
  );
}

function HeaderRow({
  headers,
  name,
  value,
  identity,
  onChange,
}: {
  headers: JSONObject;
  name: string;
  value: JSONObject;
  identity: string;
  onChange: (headers: JSONObject) => void;
}) {
  const source = Object.prototype.hasOwnProperty.call(value, "value_from") ? "reference" : "literal";
  const [nameDraft, setNameDraft] = useState(name);
  useEffect(() => setNameDraft(name), [name]);
  const update = (transform: (current: JSONObject) => JSONObject) => {
    onChange({ ...headers, [name]: transform({ ...value }) });
  };
  const commitName = () => {
    const nextName = nameDraft.trim();
    if (!nextName || nextName === name) return setNameDraft(name);
    const duplicate = Object.keys(headers).some((current) => current !== name && current.toLowerCase() === nextName.toLowerCase());
    if (duplicate) return setNameDraft(name);
    const renamed: JSONObject = {};
    for (const [current, header] of Object.entries(headers)) {
      renamed[current === name ? nextName : current] = header;
    }
    onChange(renamed);
  };
  const changeSource = (nextSource: string) => update((current) => {
    const next = { ...current };
    if (nextSource === "reference") {
      delete next.value;
      next.value_from = "";
    } else {
      delete next.value_from;
      next.value = "";
    }
    return next;
  });
  return (
    <article className="header-row">
      <Field label="Header name">
        <input spellCheck={false} value={nameDraft} onBlur={commitName} onChange={(event) => setNameDraft(event.target.value)} />
      </Field>
      <Field label="Value source">
        <select value={source} onChange={(event) => changeSource(event.target.value)}>
          <option value="literal">Literal</option>
          <option value="reference">Credential reference</option>
        </select>
      </Field>
      {source === "reference" ? (
        <Field label="Header credential reference" wide>
          <input autoComplete="off" placeholder="k8s://namespace/secret#field" spellCheck={false} value={stringValue(value.value_from)} onChange={(event) => update((current) => setString(current, "value_from", event.target.value, true))} />
        </Field>
      ) : (
        <Field label="Literal header value" wide>
          <RedactedLiteralInput
            configured={stringValue(value.value) !== ""}
            identity={identity}
            onCommit={(replacement) => update((current) => setString(current, "value", replacement, true))}
          />
        </Field>
      )}
      <Field label="Scope">
        <select value={stringValue(value.scope)} onChange={(event) => update((current) => setString(current, "scope", event.target.value))}>
          <option value="">Default: inference and health</option>
          <option value="both">Inference and health</option>
          <option value="inference">Inference only</option>
          <option value="health">Health only</option>
          {stringValue(value.scope) && !["both", "inference", "health"].includes(stringValue(value.scope)) ? (
            <option value={stringValue(value.scope)}>Existing: {stringValue(value.scope)}</option>
          ) : null}
        </select>
      </Field>
      <button className="icon-button remove" aria-label={`Remove ${name}`} onClick={() => {
        const next = { ...headers };
        delete next[name];
        onChange(next);
      }} type="button">×</button>
    </article>
  );
}

function RedactedLiteralInput({
  configured,
  identity,
  onCommit,
}: {
  configured: boolean;
  identity: string;
  onCommit: (value: string) => void;
}) {
  const [draft, setDraft] = useState("");
  useEffect(() => setDraft(""), [identity]);
  const commit = () => {
    if (!draft) return;
    onCommit(draft);
    setDraft("");
  };
  return (
    <>
      <input
        autoComplete="new-password"
        onBlur={commit}
        onChange={(event) => setDraft(event.target.value)}
        placeholder={configured ? "Configured value preserved; enter replacement" : "Enter literal value"}
        type="password"
        value={draft}
      />
      <small>{configured ? "The current literal is intentionally not rendered." : "A non-empty value is required before validation."}</small>
    </>
  );
}

function Field({ label, wide = false, children }: { label: string; wide?: boolean; children: React.ReactNode }) {
  return <label className={wide ? "wide" : ""}><span>{label}</span>{children}</label>;
}

function inspectDocument(document: ConfigurationDocument): {
  providers?: JSONObject[];
  deployments?: JSONObject[];
  virtualModels?: JSONObject[];
  error?: string;
} {
  if (!Array.isArray(document.providers)) return { error: "providers must be an array." };
  if (!Array.isArray(document.deployments)) return { error: "deployments must be an array." };
  if (!Array.isArray(document.virtual_models)) return { error: "virtual_models must be an array." };
  const providers: JSONObject[] = [];
  for (const [index, provider] of document.providers.entries()) {
    if (!isObject(provider)) return { error: `providers[${index}] must be an object.` };
    for (const field of ["name", "type", "base_url", "region"] as const) {
      if (!validOptionalString(provider[field])) return { error: `providers[${index}].${field} must be a string.` };
    }
    if (provider.auth !== undefined && !isObject(provider.auth)) return { error: `providers[${index}].auth must be an object.` };
    if (isObject(provider.auth)) {
      for (const field of ["type", "header", "prefix", "credential"] as const) {
        if (!validOptionalString(provider.auth[field])) return { error: `providers[${index}].auth.${field} must be a string.` };
      }
    }
    const headerError = inspectHeaders(provider.default_headers, `providers[${index}].default_headers`);
    if (headerError) return { error: headerError };
    if (provider.extra_body !== undefined && !isObject(provider.extra_body)) return { error: `providers[${index}].extra_body must be an object.` };
    providers.push(provider);
  }
  const deployments: JSONObject[] = [];
  for (const [index, deployment] of document.deployments.entries()) {
    if (!isObject(deployment)) return { error: `deployments[${index}] must be an object.` };
    for (const field of ["name", "provider", "model", "credential"] as const) {
      if (!validOptionalString(deployment[field])) return { error: `deployments[${index}].${field} must be a string.` };
    }
    if (!validOptionalNumber(deployment.max_concurrency)) return { error: `deployments[${index}].max_concurrency must be a number.` };
    if (!validOptionalStringArray(deployment.native_protocols)) return { error: `deployments[${index}].native_protocols must be a string array.` };
    if (stringArray(deployment.native_protocols).some(
      (protocol) => !["openai", "anthropic", "gemini", "bedrock"].includes(protocol),
    )) return { error: `deployments[${index}].native_protocols contains an unsupported protocol.` };
    if (!validOptionalStringArray(deployment.capabilities)) return { error: `deployments[${index}].capabilities must be a string array.` };
		if (deployment.endpoint_source !== undefined && !isObject(deployment.endpoint_source)) return { error: `deployments[${index}].endpoint_source must be an object.` };
		if (isObject(deployment.endpoint_source)) {
			for (const field of ["type", "controller", "revision", "recipe", "recipe_revision", "activation_timeout", "idle_ttl", "idle_action", "cold_start"] as const) {
				if (!validOptionalString(deployment.endpoint_source[field])) return { error: `deployments[${index}].endpoint_source.${field} must be a string.` };
			}
			for (const field of ["max_queued_waiters", "max_queued_body_bytes"] as const) {
				if (!validOptionalNumber(deployment.endpoint_source[field])) return { error: `deployments[${index}].endpoint_source.${field} must be a number.` };
			}
			if (!validOptionalStringArray(deployment.endpoint_source.cluster_candidates)) return { error: `deployments[${index}].endpoint_source.cluster_candidates must be a string array.` };
			if (deployment.endpoint_source.overrides !== undefined && !isStringMap(deployment.endpoint_source.overrides)) return { error: `deployments[${index}].endpoint_source.overrides must be a string map.` };
		}
    if (deployment.circuit !== undefined && !isObject(deployment.circuit)) return { error: `deployments[${index}].circuit must be an object.` };
    if (isObject(deployment.circuit)) {
      if (!validOptionalBoolean(deployment.circuit.disabled)) return { error: `deployments[${index}].circuit.disabled must be a boolean.` };
      for (const field of ["consecutive_failures", "minimum_samples", "sample_window", "failure_rate"] as const) {
        if (!validOptionalNumber(deployment.circuit[field])) return { error: `deployments[${index}].circuit.${field} must be a number.` };
      }
      for (const field of ["base_ejection_time", "max_ejection_time"] as const) {
        if (!validOptionalString(deployment.circuit[field])) return { error: `deployments[${index}].circuit.${field} must be a string.` };
      }
    }
    const headerError = inspectHeaders(deployment.upstream_headers, `deployments[${index}].upstream_headers`);
    if (headerError) return { error: headerError };
    if (deployment.extra_body !== undefined && !isObject(deployment.extra_body)) return { error: `deployments[${index}].extra_body must be an object.` };
    deployments.push(deployment);
  }
  const virtualModels: JSONObject[] = [];
  for (const [modelIndex, model] of document.virtual_models.entries()) {
    if (!isObject(model)) return { error: `virtual_models[${modelIndex}] must be an object.` };
    if (!Array.isArray(model.pools)) return { error: `virtual_models[${modelIndex}].pools must be an array.` };
    for (const [poolIndex, pool] of model.pools.entries()) {
      if (!isObject(pool) || !Array.isArray(pool.targets)) return { error: `virtual_models[${modelIndex}].pools[${poolIndex}] must contain a targets array.` };
      for (const [targetIndex, target] of pool.targets.entries()) {
        if (!isObject(target) || !validOptionalString(target.deployment)) return { error: `virtual_models[${modelIndex}].pools[${poolIndex}].targets[${targetIndex}] must be an object with a string deployment.` };
      }
    }
    virtualModels.push(model);
  }
  return { providers, deployments, virtualModels };
}

function inspectHeaders(value: unknown, path: string) {
  if (value === undefined) return "";
  if (!isObject(value)) return `${path} must be an object.`;
  for (const [name, header] of Object.entries(value)) {
    if (!isObject(header)) return `${path}.${name} must be an object.`;
    for (const field of ["value", "value_from", "scope"] as const) {
      if (!validOptionalString(header[field])) return `${path}.${name}.${field} must be a string.`;
    }
  }
  return "";
}

function renameDeploymentTargets(models: JSONObject[], oldName: string, nextName: string) {
  return models.map((model) => ({
    ...model,
    pools: objectArray(model.pools).map((pool) => ({
      ...pool,
      targets: objectArray(pool.targets).map((target) => stringValue(target.deployment) === oldName
        ? { ...target, deployment: nextName }
        : target),
    })),
  }));
}

function countDeploymentTargets(models: JSONObject[], deployment: string) {
  let count = 0;
  for (const model of models) {
    for (const pool of objectArray(model.pools)) {
      for (const target of objectArray(pool.targets)) {
        if (target.deployment === deployment) count += 1;
      }
    }
  }
  return count;
}

function setAuthType(provider: JSONObject, type: string) {
  if (!type) {
    const next = { ...provider };
    delete next.auth;
    return next;
  }
  return setNested(provider, "auth", (auth) => {
    const next: JSONObject = { ...auth, type };
    if (type !== "header") {
      delete next.header;
      delete next.prefix;
    }
    if (type === "aws_sigv4" && !stringValue(next.credential)) next.credential = "workload://aws";
    return next;
  });
}

function toggleCapability(deployment: JSONObject, capability: string, enabled: boolean) {
  const current = stringArray(deployment.capabilities);
  const next = enabled
    ? [...new Set([...current, capability])]
    : current.filter((value) => value !== capability);
  return setStringArray(deployment, "capabilities", next);
}

function defaultProtocolForProviderType(providerType: string) {
  if (["sparkrun", "openai", "openai_compatible", "openai_responses", "openai_subscription"].includes(providerType)) return "openai";
  if (["anthropic", "gemini", "bedrock"].includes(providerType)) return providerType;
  return "";
}

function toggleNativeProtocol(
	deployment: JSONObject,
	providerType: string,
	protocol: string,
	enabled: boolean,
) {
  const explicit = stringArray(deployment.native_protocols);
  const fallback = defaultProtocolForProviderType(providerType);
  const current = explicit.length ? explicit : fallback ? [fallback] : [];
  const next = enabled
    ? [...new Set([...current, protocol])]
    : current.filter((value) => value !== protocol);
  return setStringArray(deployment, "native_protocols", next);
}

function setEndpointSourceType(deployment: JSONObject, sourceType: string) {
	if (sourceType === "static") {
		const next = { ...deployment };
		delete next.endpoint_source;
		return next;
	}
	return setNested(deployment, "endpoint_source", (current) => {
		const next: JSONObject = { ...current, type: sourceType };
		if (sourceType === "discovered") {
			for (const field of [
				"revision", "recipe", "recipe_revision", "cluster_candidates", "overrides",
				"activation_timeout", "idle_ttl", "idle_action", "max_queued_waiters",
				"max_queued_body_bytes", "cold_start",
			]) delete next[field];
		}
		return next;
	});
}

function formatOverrides(overrides: JSONObject) {
	return Object.entries(overrides).map(([key, value]) => `${key}=${stringValue(value)}`).join("\n");
}

function parseOverrides(value: string): JSONObject {
	const result: JSONObject = {};
	for (const line of value.split("\n")) {
		const separator = line.indexOf("=");
		if (separator <= 0) continue;
		const key = line.slice(0, separator).trim();
		if (!key) continue;
		result[key] = line.slice(separator + 1).trim();
	}
	return result;
}

function setString(value: JSONObject, field: string, nextValue: string, required = false) {
  const next = { ...value };
  if (nextValue || required) next[field] = nextValue;
  else delete next[field];
  return next;
}

function setNumber(value: JSONObject, field: string, nextValue: string) {
  const next = { ...value };
  if (nextValue !== "") next[field] = Number(nextValue);
  else delete next[field];
  return next;
}

function setBoolean(value: JSONObject, field: string, nextValue: boolean) {
  const next = { ...value };
  if (nextValue) next[field] = true;
  else delete next[field];
  return next;
}

function setStringArray(value: JSONObject, field: string, values: string[]) {
  const next = { ...value };
  if (values.length) next[field] = values;
  else delete next[field];
  return next;
}

function setObject(value: JSONObject, field: string, nested: JSONObject) {
  const next = { ...value };
  if (Object.keys(nested).length) next[field] = nested;
  else delete next[field];
  return next;
}

function setNestedString(value: JSONObject, parent: string, field: string, nextValue: string) {
  return setNested(value, parent, (nested) => setString(nested, field, nextValue));
}

function setNestedNumber(value: JSONObject, parent: string, field: string, nextValue: string) {
  return setNested(value, parent, (nested) => setNumber(nested, field, nextValue));
}

function setNestedBoolean(value: JSONObject, parent: string, field: string, nextValue: boolean) {
  return setNested(value, parent, (nested) => setBoolean(nested, field, nextValue));
}

function setNested(value: JSONObject, parent: string, transform: (nested: JSONObject) => JSONObject) {
  const next = { ...value };
  const nested = transform({ ...objectValue(value[parent]) });
  if (Object.keys(nested).length) next[parent] = nested;
  else delete next[parent];
  return next;
}

function objectValue(value: unknown): JSONObject {
  return isObject(value) ? value : {};
}

function objectArray(value: unknown): JSONObject[] {
  return Array.isArray(value) ? value.filter(isObject) : [];
}

function stringValue(value: unknown) {
  return typeof value === "string" ? value : "";
}

function stringArray(value: unknown): string[] {
  return Array.isArray(value) ? value.filter((current): current is string => typeof current === "string") : [];
}

function numberInput(value: unknown) {
  return typeof value === "number" && Number.isFinite(value) ? value : "";
}

function booleanValue(value: unknown) {
  return value === true;
}

function validOptionalString(value: unknown) {
  return value === undefined || typeof value === "string";
}

function validOptionalNumber(value: unknown) {
  return value === undefined || typeof value === "number" && Number.isFinite(value);
}

function validOptionalBoolean(value: unknown) {
  return value === undefined || typeof value === "boolean";
}

function validOptionalStringArray(value: unknown) {
  return value === undefined || Array.isArray(value) && value.every((current) => typeof current === "string");
}

function isStringMap(value: unknown): value is Record<string, string> {
	return isObject(value) && Object.values(value).every((current) => typeof current === "string");
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

function uniqueHeaderName(prefix: string, names: string[]) {
  const lowerNames = new Set(names.map((name) => name.toLowerCase()));
  if (!lowerNames.has(prefix.toLowerCase())) return prefix;
  let suffix = 2;
  while (lowerNames.has(`${prefix}-${suffix}`.toLowerCase())) suffix += 1;
  return `${prefix}-${suffix}`;
}

function humanize(value: string) {
  return value.split("_").map((part) => part[0]?.toUpperCase() + part.slice(1)).join(" ");
}
