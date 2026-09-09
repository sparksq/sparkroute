import { useCallback, useEffect, useRef, useState } from "react";
import { fetchConfigurationPresets, mutateConfigurationPreset } from "./api";
import type { ConfigurationPresets } from "./types";

export function ConfigurationPresetSelector({ token, canWrite, dirty, locked, refreshKey, onApplied, onBusyChange }: {
  token: string;
  canWrite: boolean;
  dirty: boolean;
  locked: boolean;
  refreshKey: string;
  onApplied: () => void;
  onBusyChange: (busy: boolean) => void;
}) {
  const [catalog, setCatalog] = useState<ConfigurationPresets>();
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState("");
  const [notice, setNotice] = useState("");
  const [mode, setMode] = useState<"save" | "manage" | "switch">();
  const [target, setTarget] = useState("");
  const [name, setName] = useState("");
  const [confirmDelete, setConfirmDelete] = useState(false);
  const dialog = useRef<HTMLDialogElement>(null);
  const select = useRef<HTMLSelectElement>(null);
  const reload = useCallback(async () => {
    const next = await fetchConfigurationPresets(token);
    setCatalog(next);
    return next;
  }, [token]);
  useEffect(() => {
    let cancelled = false;
    void fetchConfigurationPresets(token).then(next => {
      if (!cancelled) { setCatalog(next); setError(""); }
    }).catch(error => { if (!cancelled) setError(message(error)); });
    return () => { cancelled = true; };
  }, [token, refreshKey]);
  useEffect(() => { if (mode && dialog.current && !dialog.current.open) dialog.current.showModal(); }, [mode]);
  const close = () => { dialog.current?.close(); setMode(undefined); setConfirmDelete(false); select.current?.focus(); };
  const mutate = async (operation: "save" | "activate" | "rename" | "delete", input: { id?: string; name?: string }) => {
    if (!catalog || busy || locked || !canWrite) return;
    setBusy(true); onBusyChange(true); setError(""); setNotice("");
    try {
      await mutateConfigurationPreset(token, operation, catalog, input);
      // Refresh the editor even if fetching the updated catalog subsequently fails.
      onApplied();
      close();
      const next = await reload();
      const active = next.presets.find(preset => preset.id === next.active_preset)?.name ?? "Default";
      setNotice(operation === "activate" ? `Loaded ${active}.` : operation === "save" ? `Saved as ${active}.` : "Presets updated.");
    } catch (error) {
      setError(message(error));
      // A conflict refreshes choices but keeps the operator's draft intact.
      try { await reload(); } catch { /* Keep the original action error. */ }
    } finally { setBusy(false); onBusyChange(false); }
  };
  const choose = (value: string) => {
    setError(""); setNotice(""); setConfirmDelete(false);
    if (value === "__save") { setName(""); setMode("save"); }
    else if (value === "__manage") {
      const preset = catalog?.presets.find(preset => preset.id === catalog.active_preset && preset.id !== "default")
        ?? catalog?.presets.find(preset => preset.id !== "default");
      setTarget(preset?.id ?? ""); setName(preset?.name ?? ""); setMode("manage");
    } else if (value !== catalog?.active_preset) {
      if (dirty) { setTarget(value); setMode("switch"); }
      else void mutate("activate", { id: value });
    }
  };
  return <div className="preset-selector">
    <label htmlFor="configuration-preset">Configuration preset</label>
    <select id="configuration-preset" ref={select} value={catalog?.active_preset ?? ""}
      disabled={!catalog || !canWrite || busy || locked} onChange={event => choose(event.target.value)}>
      {!catalog && <option value="">Loading presets…</option>}
      {catalog?.presets.map(preset => <option key={preset.id} value={preset.id}>{preset.name}</option>)}
      {canWrite && <optgroup label="Presets">
        <option value="__save" disabled={dirty}>Save as preset…</option>
        <option value="__manage" disabled={dirty}>Manage presets…</option>
      </optgroup>}
    </select>
    {dirty && <small>Save your draft before saving a preset.</small>}
    {busy && <small role="status">Applying preset changes…</small>}
    {!mode && error && <div role="alert">{error} <button className="secondary-button" onClick={() => { void reload().then(() => setError("")).catch(error => setError(message(error))); }}>Reload presets</button></div>}
    {notice && <small role="status">{notice}</small>}
    {mode && <dialog ref={dialog} className="preset-dialog" aria-labelledby="preset-dialog-title" onCancel={event => { event.preventDefault(); if (!busy) close(); }}>
      <h2 id="preset-dialog-title">{mode === "save" ? "Save configuration as preset" : mode === "manage" ? "Manage presets" : "Discard draft and switch presets?"}</h2>
      {mode === "switch" ? <p>Load {catalog?.presets.find(preset => preset.id === target)?.name} and discard the unsaved operator configuration draft?</p>
        : <p>Presets contain operator configuration. Configuration saves update the selected preset, which is remembered after restart.</p>}
      {mode === "manage" && <label>Preset to manage
        <select value={target} disabled={busy} onChange={event => {
          setTarget(event.target.value); setName(catalog?.presets.find(preset => preset.id === event.target.value)?.name ?? ""); setConfirmDelete(false);
        }}>
          {!target && <option value="">No named presets</option>}
          {catalog?.presets.filter(preset => preset.id !== "default").map(preset => <option key={preset.id} value={preset.id}>{preset.name}</option>)}
        </select>
      </label>}
      {(mode === "save" || (mode === "manage" && target)) && <form onSubmit={event => { event.preventDefault(); void mutate(mode === "save" ? "save" : "rename", { id: mode === "manage" ? target : undefined, name: name.trim() }); }}>
        <label>Preset name<input autoFocus required maxLength={64} value={name} disabled={busy} onChange={event => setName(event.target.value)} /></label>
        <button className="primary-button" disabled={busy || locked || !name.trim()} type="submit">{mode === "save" ? "Save preset" : "Rename preset"}</button>
      </form>}
      {mode === "manage" && <>
        <p>Default is always available. Switch away from a preset before deleting it.</p>
        {target && <button className="secondary-button" disabled={busy || locked || target === catalog?.active_preset}
          onClick={() => { if (confirmDelete) void mutate("delete", { id: target }); else setConfirmDelete(true); }}>
          {confirmDelete ? `Confirm deletion of ${catalog?.presets.find(preset => preset.id === target)?.name}` : "Delete preset"}
        </button>}
      </>}
      {error && <p role="alert">{error}</p>}
      <div className="preset-dialog-actions">
        <button className="secondary-button" disabled={busy} onClick={close}>Cancel</button>
        {mode === "switch" && <button className="primary-button" disabled={busy || locked} onClick={() => void mutate("activate", { id: target })}>Discard draft and switch</button>}
      </div>
    </dialog>}
  </div>;
}

function message(error: unknown) { return error instanceof Error ? error.message : "Unable to update configuration presets."; }
