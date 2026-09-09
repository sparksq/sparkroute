import { useState } from "react";
import type { ConfigurationDocument } from "./types";
import { deploymentChoices } from "./deploymentTitles";
import { excludedSparkrunDeployments, setSparkrunExclusions, sparkrunRemovalPreview } from "./sparkrunExclusions";

export function SparkrunRemoval({document, generated, deployments, disabled, onChange}: {
  document: ConfigurationDocument; generated: ConfigurationDocument; deployments: string[];
  disabled: boolean; onChange: (document: ConfigurationDocument) => void;
}) {
  const [confirm, setConfirm] = useState(false);
  const preview = sparkrunRemovalPreview(document, generated, deployments);
  const titles = deploymentChoices(document, generated);
  return <section className="sparkrun-removal" aria-label="Remove generated sparkrun entry">
    <button type="button" className="danger-button" disabled={disabled || !deployments.length}
      onClick={() => setConfirm(!confirm)}>{confirm ? "Cancel removal" : "Remove from sparkroute"}</button>
    {confirm && <div className="notice info">
      <p>Exclude {deployments.map(id => titles[id] || id).join(", ")} from sparkroute. Future syncs will keep it excluded. Running workloads are left alone.</p>
      {preview.removed.length > 0 && <p>Generated names and their aliases removed: {preview.removed.join(", ")}.</p>}
      {preview.affected.some(model => !preview.removed.includes(model)) && <p>Models with other deployments keep their remaining targets.</p>}
      {preview.blockers.length > 0 ? <><strong>Update these references first:</strong><ul>{preview.blockers.map(blocker => <li key={blocker}>{blocker}</li>)}</ul></> : null}
      <p>Validate, then Save to apply. Restore is available under Model Deployments.</p>
      <button type="button" className="danger-button confirm" disabled={disabled || preview.blockers.length > 0}
        onClick={() => {onChange(preview.document); setConfirm(false);}}>Confirm removal</button>
    </div>}
  </section>;
}

export function ExcludedSparkrunDeployments({document, generated, disabled, onChange}: {
  document: ConfigurationDocument; generated?: ConfigurationDocument; disabled: boolean;
  onChange: (document: ConfigurationDocument) => void;
}) {
  const excluded = excludedSparkrunDeployments(document);
  const titles = deploymentChoices(document, generated);
  if (!excluded.length) return null;
  return <details className="sparkrun-exclusions"><summary>Excluded sparkrun deployments ({excluded.length})</summary>
    <p>These entries stay out of sparkroute even if sparkrun reports them again. Restoring an entry allows its saved binding or discovery settings to apply after Validate and Save.</p>
    <ul>{excluded.map(id => <li key={id}><span>{titles[id] || id}</span>
      <button type="button" className="secondary-button" disabled={disabled} aria-label={`Restore ${titles[id] || id}`}
        onClick={() => onChange(setSparkrunExclusions(document, excluded.filter(name => name !== id)))}>Restore</button></li>)}</ul>
  </details>;
}
