// SPDX-FileCopyrightText: 2026 Scitrera LLC
// SPDX-FileCopyrightText: 2026 Fox Engine Ltd.
// SPDX-License-Identifier: AGPL-3.0-only

export function RecoveryFields({ value, onChange }: {
  value: Record<string, unknown>;
  onChange: (value: Record<string, unknown>) => void;
}) {
  const set = (field: string, content: string | number) => {
    const next = { ...value };
    if (content === "") delete next[field]; else next[field] = content;
    onChange(next);
  };
  return <div className="model-section">
    <h4>Automatic recovery</h4>
    <label>When persistently unhealthy<select aria-label="When persistently unhealthy" value={String(value.action || "")} onChange={e => onChange(e.target.value ? { ...value, action: e.target.value } : {})}>
      <option value="">Disabled</option>
      <option value="restart">Stop and restart immediately</option>
      <option value="stop">Stop; start again on demand</option>
    </select></label>
    <p className="section-help">Recover an owned workload after repeated failed circuit probes. Recovery uses a fresh process even when the idle action is sleep. Rate limits and overload responses do not trigger recovery.</p>
    {Boolean(value.action) && <>
      <div className="model-field-grid">
        <label>Failed probes before recovery<input aria-label="Failed probes before recovery" type="number" min={1} max={100} value={Number(value.failed_probes || 3)} onChange={e => set("failed_probes", Number(e.target.value))} /></label>
        <label>Minimum unhealthy duration<input aria-label="Minimum unhealthy duration" placeholder="2m" value={String(value.unhealthy_for || "")} onChange={e => set("unhealthy_for", e.target.value)} /></label>
        <label>Automatic recovery limit<input aria-label="Automatic recovery limit" type="number" min={1} max={100} value={Number(value.max_restarts || 3)} onChange={e => set("max_restarts", Number(e.target.value))} /></label>
      </div>
      <details><summary>Recovery timing</summary><div className="model-field-grid">
        <label>Drain timeout<input aria-label="Drain timeout" placeholder="30s" value={String(value.drain_timeout || "")} onChange={e => set("drain_timeout", e.target.value)} /></label>
        <label>Initial recovery backoff<input aria-label="Initial recovery backoff" placeholder="1m" value={String(value.backoff || "")} onChange={e => set("backoff", e.target.value)} /></label>
        <label>Maximum recovery backoff<input aria-label="Maximum recovery backoff" placeholder="15m" value={String(value.max_backoff || "")} onChange={e => set("max_backoff", e.target.value)} /></label>
      </div></details>
      <p className="section-help">Unfinished requests block shutdown. A drain timeout, uncertain stop, or exhausted recovery limit requires operator intervention. Use Start to retry after inspecting the workload, or Stop to shut it down. The limit follows workload replacements and configuration changes until an explicit Start, Stop, or gateway process restart.</p>
    </>}
  </div>;
}
