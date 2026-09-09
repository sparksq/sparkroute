// SPDX-FileCopyrightText: 2026 Scitrera LLC
// SPDX-FileCopyrightText: 2026 Fox Engine Ltd.
// SPDX-License-Identifier: AGPL-3.0-only

import { useState } from "react";
import { sparkRunCatalog } from "./api";
export type RegistryEntry = {name: string; enabled: boolean; cached: boolean; trusted?: boolean};
export function RegistryManager({token, registries, onChange}: {token: string; registries: RegistryEntry[]; onChange: (values: RegistryEntry[]) => void}) {
  const [name, setName] = useState(""); const [url, setURL] = useState(""); const [subpath, setSubpath] = useState("");
  const [pending, setPending] = useState<{action: string; name: string}>(); const [acknowledged, setAcknowledged] = useState(false);
  const [busy, setBusy] = useState(false); const [error, setError] = useState("");
  async function change(action: string, registry: string) {
    setBusy(true); setError("");
    try { const result = await sparkRunCatalog<{registries: RegistryEntry[]}>(token, "catalog_registry", {action, name: registry, ...(action === "add" ? {url, subpath} : {}), ...(action === "trust" ? {acknowledge_trust: acknowledged} : {})}); onChange(result.registries); setPending(undefined); setAcknowledged(false); if (action === "add") {setName(""); setURL(""); setSubpath("");} }
    catch (e) {setError(e instanceof Error ? e.message : "Registry update failed.");} finally {setBusy(false);}
  }
  return <details className="registry-manager"><summary>Manage registries</summary><fieldset disabled={busy} className="recipe-fieldset">
    <p className="section-help">Changes apply immediately on the control node. Removing or disabling a registry can make saved recipes unavailable; adding one does not download it or grant trust.</p>
    {registries.map((r) => <div className="registry-row" key={r.name}><strong>{r.name}</strong><span>{r.enabled ? "Enabled" : "Disabled"} · {r.cached ? "Cached" : "Not cached"} · {r.trusted ? "Trusted" : "Untrusted"}</span><div className="recipe-actions">
      <button type="button" onClick={() => {setPending({action:r.enabled ? "disable" : "enable", name:r.name}); setAcknowledged(false);}}>{r.enabled ? "Disable" : "Enable"}</button>
      <button type="button" onClick={() => {setPending({action:r.trusted ? "untrust" : "trust", name:r.name}); setAcknowledged(false);}}>{r.trusted ? "Revoke trust" : "Review trust"}</button>
      <button type="button" onClick={() => {setPending({action:"remove", name:r.name}); setAcknowledged(false);}}>Remove</button>
    </div></div>)}
    {pending && <div className="notice info"><p>{pending.action} registry {pending.name}?</p>{pending.action === "trust" && <label className="checkbox-field"><input type="checkbox" checked={acknowledged} onChange={(e) => setAcknowledged(e.target.checked)} />I reviewed this registry and allow its recipes to execute hooks on my hosts.</label>}
      <button type="button" disabled={pending.action === "trust" && !acknowledged} onClick={() => void change(pending.action, pending.name)}>Confirm {pending.action}</button> <button type="button" onClick={() => setPending(undefined)}>Cancel</button></div>}
    <div className="model-field-grid"><label>New registry name<input value={name} onChange={(e) => setName(e.target.value)} /></label><label>Git URL<input value={url} onChange={(e) => setURL(e.target.value)} /></label><label>Recipe subdirectory (optional)<input value={subpath} onChange={(e) => setSubpath(e.target.value)} /></label></div>
    <button type="button" disabled={!name.trim() || !url.trim()} onClick={() => void change("add", name.trim())}>Add registry</button>
    {error && <p role="alert" className="notice error">{error}</p>}
  </fieldset></details>;
}
