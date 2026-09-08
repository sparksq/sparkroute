import type { ConfigurationDocument } from "./types";
type ObjectValue = Record<string, unknown>;
const object = (value: unknown): ObjectValue => value && typeof value === "object" && !Array.isArray(value) ? value as ObjectValue : {};
export function TraceSettingsEditor({document, disabled, onChange}: {document: ConfigurationDocument; disabled: boolean; onChange: (document: ConfigurationDocument) => void}) {
 const observability = object(document.observability);
 function update(key: string, value: unknown) { const next = {...observability, [key]: value}; if (value === undefined) delete next[key]; onChange({...document, observability: Object.keys(next).length ? next : undefined}); }
 function mode(key: string) { return observability[key] === undefined ? "inherit" : object(observability[key]).enabled ? "enabled" : "disabled"; }
 const saved = object(observability.saved_traces); const otlp = object(observability.otlp_traces);
 const savedChange = (key: string, value: unknown) => update("saved_traces", {...saved, [key]:value});
 const otlpChange = (key: string, value: unknown) => update("otlp_traces", {...otlp, [key]:value});
 return <div className="trace-settings"><p className="section-help">Validate, then Save to apply tracing changes. Requests already in progress finish with their original settings. “Use startup settings” follows command-line flags or environment variables.</p>
  <fieldset disabled={disabled} className="recipe-fieldset">
   <section className="model-section"><h3>Saved request and response traces</h3><p className="section-help">Capture caller payloads for JSONL or dataset export. These files contain prompt and response content; storage is private to the gateway user.</p>
    <label>Payload capture<select aria-label="Payload capture" value={mode("saved_traces")} onChange={(e)=>update("saved_traces", e.target.value === "inherit" ? undefined : {...saved, storage:saved.storage || "filesystem", enabled:e.target.value === "enabled"})}><option value="inherit">Use startup settings</option><option value="disabled">Disabled</option><option value="enabled">Enabled</option></select></label>
    {mode("saved_traces") !== "inherit" && <div className="model-field-grid">
     <label>Trace storage<select value={String(saved.storage || "filesystem")} onChange={(e)=>savedChange("storage",e.target.value)}><option value="filesystem">Filesystem directory</option><option value="database">SQLite database</option></select></label>
     <label>{saved.storage === "database" ? "Database file" : "Trace directory"}<input aria-label="Trace storage path" value={String(saved.path || "")} onChange={(e)=>savedChange("path",e.target.value)} placeholder={saved.storage === "database" ? "/absolute/path/traces.sqlite" : "/absolute/path/traces"}/><small>An absolute path on the gateway host, using that host’s path format.</small></label>
     <label>Maximum body size (MiB)<input aria-label="Maximum trace body size (MiB)" type="number" min={1} max={256} value={Number(saved.max_body_bytes || 64*1024*1024)/1024/1024} onChange={(e)=>savedChange("max_body_bytes",Number(e.target.value)*1024*1024)}/><small>Applied separately to request and response; larger bodies are marked truncated.</small></label>
     <label>Queue capacity<input type="number" min={1} max={65536} value={Number(saved.queue_capacity || 4096)} onChange={(e)=>savedChange("queue_capacity",Number(e.target.value))}/></label>
     <label>When the queue is full<select value={String(saved.overflow || "block")} onChange={(e)=>savedChange("overflow",e.target.value)}><option value="block">Wait for storage</option><option value="drop">Drop the trace</option></select><small>Waiting favors capture completeness; dropping favors request throughput. Losses are counted and logged.</small></label>
    </div>}
   </section>
   <section className="model-section"><h3>OpenTelemetry trace export</h3><p className="section-help">Export operational spans over OTLP/HTTP to a collector. This is separate from saved request and response payloads.</p>
    <label>OTLP trace export<select aria-label="OTLP trace export" value={mode("otlp_traces")} onChange={(e)=>update("otlp_traces", e.target.value === "inherit" ? undefined : {...otlp, enabled:e.target.value === "enabled"})}><option value="inherit">Use startup settings</option><option value="disabled">Disabled</option><option value="enabled">Enabled</option></select></label>
    {mode("otlp_traces") !== "inherit" && <><div className="model-field-grid">
     <label>Collector trace URL<input aria-label="Collector trace URL" value={String(otlp.endpoint || "")} onChange={(e)=>otlpChange("endpoint",e.target.value)} placeholder="http://localhost:4318/v1/traces"/><small>Full HTTP trace endpoint, including /v1/traces when required by your collector.</small></label>
     <label>Service name<input value={String(otlp.service_name || "sparkroute")} onChange={(e)=>otlpChange("service_name",e.target.value)}/></label>
     <label>Root trace sampling (%)<input aria-label="Root trace sampling (%)" type="number" min={0} max={100} value={Number(otlp.sample_ratio ?? 1)*100} onChange={(e)=>otlpChange("sample_ratio",Number(e.target.value)/100)}/><small>Child spans follow their parent’s sampling decision. This does not sample saved payloads.</small></label>
    </div>
    <h4>Collector authentication headers</h4><p className="section-help">Map a header to an environment variable available to the gateway process. For Authorization, the variable must contain the complete value, such as “Bearer …”. Secret values are never saved here.</p>
    {Object.entries(object(otlp.header_env)).map(([header, env],index) => <div className="trace-header-row" key={index}><label>HTTP header<input aria-label={`Trace header ${index+1}`} value={header} onChange={(e)=>{const entries=Object.entries(object(otlp.header_env)); entries[index]=[e.target.value,env]; otlpChange("header_env",Object.fromEntries(entries));}}/></label><label>Environment variable<input aria-label={`Trace header environment ${index+1}`} value={String(env)} onChange={(e)=>otlpChange("header_env",{...object(otlp.header_env),[header]:e.target.value})}/></label><button type="button" onClick={()=>{const headers={...object(otlp.header_env)};delete headers[header];otlpChange("header_env",headers);}}>Remove header</button></div>)}
    <button type="button" onClick={()=>otlpChange("header_env",{...object(otlp.header_env),[Object.keys(object(otlp.header_env)).length ? `X-Header-${Object.keys(object(otlp.header_env)).length+1}` : "Authorization"]:""})}>Add export header</button></>}
   </section>
  </fieldset>
 </div>;
}
