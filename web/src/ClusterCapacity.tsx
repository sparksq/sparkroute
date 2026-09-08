import { useEffect, useRef, useState } from "react";
import { sparkRunCatalog, type SparkrunOperation } from "./api";
type Capacity = {cluster: string; observed_at: number; hosts: {host: string; reachable: boolean; free_slots: number | null; used_slots: number | null; workloads: number | null}[]};
export function ClusterCapacity({token, cluster}: {token: string; cluster: string}) {
  const pending = useRef<AbortController | undefined>(undefined);
  const [operation, setOperation] = useState<SparkrunOperation>(); const [result, setResult] = useState<Capacity>(); const [error, setError] = useState(""); const [busy, setBusy] = useState(false);
  useEffect(() => {pending.current?.abort(); setBusy(false); setOperation(undefined); setResult(undefined); setError(""); return () => pending.current?.abort();}, [cluster, token]);
  useEffect(() => {
    if (operation?.state !== "running") return;
    const abort = new AbortController(); const timer = setTimeout(() => {
      void sparkRunCatalog<SparkrunOperation>(token, "operation_status", {operation_id:operation.operation_id}, abort.signal).then((value) => {
        setOperation(value); if (value.state === "succeeded") setResult(value.result as unknown as Capacity); if (value.state === "failed") setError(value.error?.message || "Capacity check failed.");
      }).catch((e) => {if (!abort.signal.aborted) {setError(String(e)); setOperation(undefined);}});
    }, 1000); return () => {clearTimeout(timer); abort.abort();};
  }, [operation, token]);
  return <details><summary>Current cluster capacity</summary><p className="section-help">An explicit live check of the selected cluster; this is advisory and does not reserve capacity or start a model.</p>
    <button type="button" disabled={!cluster || busy || operation?.state === "running"} onClick={async () => {const abort = new AbortController(); pending.current?.abort(); pending.current = abort; setBusy(true); setError(""); setResult(undefined); try {const value = await sparkRunCatalog<SparkrunOperation>(token, "catalog_capacity", {cluster}, abort.signal); if (!abort.signal.aborted) {setOperation(value); if (value.state === "succeeded") setResult(value.result as unknown as Capacity);}} catch(e) {if (!abort.signal.aborted) setError(String(e));} finally {if (!abort.signal.aborted) setBusy(false);}}}>Check cluster</button>
    {operation?.state === "running" && <p role="status">Checking cluster occupancy…</p>}
    {error && <p role="alert" className="notice error">{error}</p>}
    {result && <div className="capacity-grid"><small>Observed {new Date(result.observed_at * 1000).toLocaleTimeString()}</small>{result.hosts.map((host) => <div key={host.host}><strong>{host.host}</strong>{host.reachable ? <><meter aria-label={`${host.host} used slots`} min={0} max={Math.max(1, (host.used_slots ?? 0) + (host.free_slots ?? 0))} value={host.used_slots ?? 0} /><span>{host.free_slots ?? "Unknown"} free slots · {host.workloads} workloads</span></> : <span>Unreachable · capacity unknown</span>}</div>)}</div>}
  </details>;
}
