// SPDX-FileCopyrightText: 2026 Scitrera LLC
// SPDX-FileCopyrightText: 2026 Fox Engine Ltd.
// SPDX-License-Identifier: AGPL-3.0-only

import { useRef, useState } from "react";
import { controlSparkrunWorkload } from "./api";
import { bindingState } from "./deploymentInventory";
import type { LifecycleBindingStatus } from "./types";

const labels: Record<string, string> = { start: "Start", stop: "Stop", status: "Check status", sleep: "Sleep", wake: "Wake" };
const progress: Record<string, string> = { start: "Starting workload…", stop: "Stopping workload…", status: "Checking workload…", sleep: "Putting workload to sleep…", wake: "Waking workload…" };

export function useWorkloadControls(token: string, refresh: () => Promise<unknown>) {
  const [operations, setOperations] = useState<Record<string, string | undefined>>({});
  const [errors, setErrors] = useState<Record<string, string | undefined>>({});
  const pending = useRef(new Set<string>());
  async function act(binding: LifecycleBindingStatus, operation: string) {
    const id = binding.deployment;
    if (pending.current.has(id)) return;
    pending.current.add(id);
    setOperations(current => ({ ...current, [id]: operation }));
    setErrors(current => ({ ...current, [id]: undefined }));
    try {
      await controlSparkrunWorkload(token, id, operation === "start" ? "" : binding.job_id!, operation);
    } catch (caught) {
      setErrors(current => ({ ...current, [id]: caught instanceof Error ? caught.message : "Workload control failed." }));
    } finally {
      // Also reconcile a failed operation: a remote transition may have
      // completed before its response was lost.
      await refresh();
      pending.current.delete(id);
      setOperations(current => ({ ...current, [id]: undefined }));
    }
  }
  return { operations, errors, act };
}

export function WorkloadControls({ binding, canControl, stale, activeRequests, operation, error, onAction }: {
  binding: LifecycleBindingStatus;
  canControl: boolean;
  stale: boolean;
  activeRequests: number;
  operation?: string;
  error?: string;
  onAction: (operation: string) => void;
}) {
  const state = bindingState(binding);
  const transitioning = ["starting", "stopping", "draining"].includes(state);
  const supported = binding.job_id ? binding.lifecycle_actions ?? [] : [];
  const actions: string[] = [];
  if (binding.controller === "sparkrun" && !transitioning) {
    if (state === "sleeping") {
      if (binding.owned === true && supported.includes("wake")) actions.push("wake");
    } else if (["stopped", "failed", "unknown"].includes(state)) actions.push("start");
    if (binding.job_id && binding.owned === true && state !== "stopped") actions.push("stop");
    if (state === "running" && binding.owned === true && supported.includes("sleep")) actions.push("sleep");
  }
  if (binding.controller === "sparkrun" && supported.includes("status")) actions.push("status");
  const active = activeRequests > 0 || binding.active_leases > 0;
  const reason = stale ? "Status is stale. Refresh before controlling this workload."
    : active ? "Lifecycle changes wait for active requests and leases to finish."
    : transitioning ? "A lifecycle operation is in progress." : "";
  return <div className="workload-controls">
    {canControl ? <div className="runtime-workload-actions">
      {actions.map(action => <button key={action} className={action === "start" || action === "wake" ? "primary-button" : "secondary-button"}
        type="button" disabled={Boolean(operation) || (action !== "status" && (stale || active || transitioning))}
        title={action === "status" ? undefined : reason || (action === "stop" ? "Requests may start this workload again, according to its activation policy." : undefined)}
        onClick={() => onAction(action)}>{labels[action]}</button>)}
    </div> : <small>View only</small>}
    {binding.owned === false ? <small>Lifecycle changes are managed externally.</small> : null}
    {binding.controller !== "sparkrun" ? <small>Controls unavailable for {binding.controller}.</small> : null}
    {canControl && reason ? <small>{reason}</small> : null}
    {operation ? <small role="status">{progress[operation]}</small> : null}
    {error ? <p className="form-error" role="alert">{error}</p> : null}
  </div>;
}
