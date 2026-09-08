import { useEffect, useState } from "react";
import type { ConfigurationDocument } from "./types";
import { GuardrailEditor } from "./GuardrailEditor";
import { PIIVirtualModelSection } from "./PIIEditor";
import { assignment, isObject, object, policyInfo, policyModels, profileUses, profiles, renameProfile, rows, setAssignment, uniqueProfileName, validProfileName, type PolicyKind } from "./policyProfiles";

export function PolicyProfilesEditor({ document, readOnlyDocument, kind, disabled, onChange }: {
  document: ConfigurationDocument; readOnlyDocument?: ConfigurationDocument; kind: PolicyKind; disabled: boolean; onChange: (document: ConfigurationDocument) => void;
}) {
  const info = policyInfo[kind], values = profiles(document, kind), names = Object.keys(values).sort();
  const [requested, setRequested] = useState("");
  const selected = names.includes(requested) ? requested : names[0] ?? "";
  const [nameDraft, setNameDraft] = useState(selected);
  const [modelDraft, setModelDraft] = useState("");
  const [confirmRemove, setConfirmRemove] = useState(false);
  useEffect(() => { setNameDraft(selected); setConfirmRemove(false); }, [selected]);
  const models = policyModels(document, readOnlyDocument);
  const allModels = [...rows(document.virtual_models), ...rows(readOnlyDocument?.virtual_models)];
  const uses = selected ? profileUses(document, kind, selected) : [];
  const eligible = models.filter(({model}) => assignment(document, String(model.name))[info.reference] !== selected);
  const modelName = eligible.some(({model}) => model.name === modelDraft) ? modelDraft : String(eligible[0]?.model.name ?? "");
  const invalid = document[info.field] !== undefined && (!isObject(document[info.field]) || Object.values(values).some(value => !isObject(value)));
  if (invalid) return <p className="notice error" role="alert">{info.page} profiles must be objects. Switch to JSON to repair them; existing values are preserved.</p>;

  function change(policy: Record<string, unknown>) {
    if (disabled) return;
    onChange({ ...document, [info.field]: { ...values, [selected]: policy } });
  }
  function add() {
    const name = uniqueProfileName(document, kind);
    onChange({ ...document, [info.field]: { ...values, [name]: kind === "pii" ? { mode: "substitute" } : {} } });
    setRequested(name);
  }
  function remove() {
    if (uses.length || disabled) return;
    if (!confirmRemove) { setConfirmRemove(true); return; }
    const next = { ...values }; delete next[selected];
    onChange({ ...document, [info.field]: next }); setConfirmRemove(false);
  }
  return <div className="structured-editor policy-profiles-editor">
    <aside className="model-list" aria-label={`${info.page} profiles`}>
      <div className="model-list-heading"><div><span>Profiles</span><strong>{names.length}</strong></div><button type="button" className="icon-button" aria-label={`Add ${info.label.toLowerCase()} profile`} title="Add profile" disabled={disabled || names.length >= 256} onClick={add}>+</button></div>
      <div className="model-list-items">{names.map(name => <button key={name} type="button" className={selected === name ? "active" : ""} aria-current={selected === name ? "true" : undefined} onClick={() => setRequested(name)}><span>{name}</span><small>{profileUses(document, kind, name).length} model assignments</small></button>)}
        {!names.length && <p>No profiles yet. Add a profile, configure its policy, then assign it to virtual models.</p>}
      </div>
    </aside>
    <div className="model-form">
      <p className="section-help">Reusable {kind === "pii" ? "privacy policies" : "request and response checks"} for operator and sparkrun virtual models. Saving changes updates every model assigned this profile.</p>
      <p className="notice info" role="note">
        {kind === "pii"
          ? "The built-in PII detector is best effort and may miss or misidentify personal information. It comes with no guarantees; you are responsible for protecting your data. Test it for your use case to ensure it meets your goals."
          : "Guardrails are best effort and may miss harmful content or block legitimate content. They come with no guarantees; you are responsible for their use and results. Test them for your use case to ensure they meet your goals."}
      </p>
      {selected ? <>
        <div className="model-form-heading"><h3>{selected}</h3><button type="button" className="danger-button" disabled={disabled || uses.length > 0} title={uses.length ? "Remove assignments before deleting this profile" : undefined} onClick={remove} onBlur={() => setConfirmRemove(false)}>{confirmRemove ? "Confirm delete profile" : "Delete profile"}</button></div>
        <div className="model-field-grid policy-profile-name"><label>Profile name<input disabled={disabled} value={nameDraft} onChange={e => setNameDraft(e.target.value)} /></label><button type="button" className="secondary-button" disabled={disabled || !validProfileName(nameDraft) || selected === nameDraft || Object.hasOwn(values, nameDraft)} onClick={() => { onChange(renameProfile(document, kind, selected, nameDraft)); setRequested(nameDraft); }}>Rename</button></div>
        {nameDraft !== selected && (!validProfileName(nameDraft) || Object.hasOwn(values, nameDraft)) && <p className="notice error">Use a unique name without spaces or control characters (up to 256 bytes).</p>}
        <p className="section-help">Renaming updates all assignments.</p>
        {kind === "pii" ? <PIIVirtualModelSection profile model={{privacy: {pii: values[selected]}}} disabled={disabled} updateModel={transform => change(object(object(transform({privacy: {pii: values[selected]}}).privacy).pii))} />
          : <GuardrailEditor profile model={{guardrails: values[selected]}} modelNames={allModels.flatMap(model => [String(model.name), ...(Array.isArray(model.aliases) ? model.aliases as string[] : [])])} disabled={disabled} updateModel={transform => change(object(transform({guardrails: values[selected]}).guardrails))} />}
        <section className="model-section" aria-label="Profile assignments">
          <div className="model-section-heading"><h4>Assigned virtual models</h4></div>
          <p className="section-help">Assignments include aliases and request profiles of each model. Assignments to unavailable models are retained for when they return. Remove assignments before deleting this profile.</p>
          <div className="model-field-grid policy-profile-name"><label>Assign to virtual model<select disabled={disabled || !eligible.length} value={modelName} onChange={e => setModelDraft(e.target.value)}>{!eligible.length && <option value="">No unassigned models</option>}{eligible.map(({model, generated}) => <option key={String(model.name)} value={String(model.name)}>{String(model.name)}{generated ? " · sparkrun" : ""}</option>)}</select></label><button type="button" className="secondary-button" disabled={disabled || !modelName} onClick={() => onChange(setAssignment(document, modelName, kind, selected))}>Apply profile</button></div>
          {modelName && typeof assignment(document, modelName)[info.reference] === "string" && <p className="section-help">Applying replaces this model’s current {info.label.toLowerCase()} assignment.</p>}
          {uses.length ? <ul className="policy-assignment-list">{uses.map(name => <li key={name}><span><strong>{name}</strong><small>{!allModels.some(model => model.name === name) ? "Not currently available · assignment retained" : rows(readOnlyDocument?.virtual_models).some(model => model.name === name) ? "sparkrun · operator policy assignment" : "Operator model"}</small></span><button type="button" className="text-button" disabled={disabled} aria-label={`Remove assignment for ${name}`} onClick={() => onChange(setAssignment(document, name, kind, undefined))}>Remove assignment</button></li>)}</ul> : <p className="section-help">No models assigned yet.</p>}
          {uses.length > 0 && <p className="section-help">Removing an assignment restores the model’s existing settings or inherited profile.</p>}
        </section>
      </> : <p className="empty-state">Add a profile to begin.</p>}
    </div>
  </div>;
}
