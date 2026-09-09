// SPDX-FileCopyrightText: 2026 Scitrera LLC
// SPDX-FileCopyrightText: 2026 Fox Engine Ltd.
// SPDX-License-Identifier: AGPL-3.0-only

import type { JSONObject } from "./extensions";
import type { ConfigurationDocument } from "./types";
import { assignment, copyInlineProfile, inlinePolicy, policyInfo, policyKinds, profiles, setAssignment } from "./policyProfiles";

export function ModelPolicyAssignments({ document, model, generated, disabled, onChange }: {
  document: ConfigurationDocument; model: JSONObject; generated: boolean; disabled: boolean; onChange: (document: ConfigurationDocument) => void;
}) {
  const values = assignment(document, String(model.name));
  return <section className="model-section model-policy-assignments" aria-label="Policy profiles">
    <div className="model-section-heading"><h4>Policy profiles</h4></div>
    <p className="section-help">Choose reusable policies for this model and its request profiles. Edit shared policies under PII Privacy and Guardrails in Configuration.</p>
    {generated && <p className="section-help">These assignments are operator managed and stay in place when sparkrun refreshes or restarts this model.</p>}
    <div className="model-field-grid">
      {policyKinds.map(kind => {
        const info = policyInfo[kind], ref = values[info.reference], names = Object.keys(profiles(document, kind)).sort();
        const inline = inlinePolicy(model, kind);
        return <div className="policy-assignment-field" key={kind}>
          <label>{info.label} profile<select aria-label={`${info.label} profile`} disabled={disabled} value={ref === undefined ? "inherit" : ref === "" ? "disabled" : `profile:${ref}`} onChange={e => onChange(setAssignment(document, String(model.name), kind, e.target.value === "inherit" ? undefined : e.target.value === "disabled" ? "" : e.target.value.slice(8)))}>
            <option value="inherit">{inline ? "Use existing model settings" : "Not assigned"}</option>
            <option value="disabled">Disabled</option>
            {typeof ref === "string" && ref && !names.includes(ref) && <option value={`profile:${ref}`}>{ref} (missing)</option>}
            {names.map(name => <option value={`profile:${name}`} key={name}>{name}</option>)}
          </select></label>
          {inline && <><small>Existing inline settings are preserved. A selected profile overrides them.</small><button type="button" className="text-button" disabled={disabled} onClick={() => onChange(copyInlineProfile(document, model, kind))}>Create {info.label.toLowerCase()} profile from existing settings</button></>}
        </div>;
      })}
    </div>
  </section>;
}
