import { useState } from "react";
import { sparkRunCatalog } from "./api";
type Plugin = {name:string; installed:boolean; enabled:boolean; lifecycle:boolean};
export function RecipePluginStatus({token, required}: {token:string; required:string[]}) {
 const [plugins,setPlugins]=useState<Plugin[]>(); const [error,setError]=useState(""); const [busy,setBusy]=useState(false);
 return <details onToggle={(e) => {if (!e.currentTarget.open || plugins || busy) return; setBusy(true); void sparkRunCatalog<{plugins:Plugin[]}>(token,"catalog_plugins").then((value)=>setPlugins(value.plugins)).catch((e)=>setError(String(e))).finally(()=>setBusy(false));}}><summary>Plugin availability</summary>
 <p className="section-help">Required by this recipe: {required.join(", ") || "None"}. Actual workload usage is reported on the Runtime page after launch.</p>
 {plugins?.map((plugin)=><p key={plugin.name}>{plugin.name}: {plugin.installed?"Installed":"Not installed"} · {plugin.enabled?"Enabled":"Disabled"} · {plugin.lifecycle?"Sleep/wake API available":"Sleep/wake API unavailable"}</p>)}
 {busy && <p role="status">Checking plugins…</p>}{error && <p className="notice error" role="alert">{error}</p>}
 </details>;
}
