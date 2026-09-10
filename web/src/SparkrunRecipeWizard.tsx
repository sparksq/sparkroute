// SPDX-FileCopyrightText: 2026 Scitrera LLC
// SPDX-FileCopyrightText: 2026 Fox Engine Ltd.
// SPDX-License-Identifier: AGPL-3.0-only

import { RecoveryFields } from "./RecoveryFields";
import { capabilityOptions } from "./capabilities";
import { RecipePluginStatus } from "./RecipePluginStatus";
import { RegistryManager } from "./RegistryManager";
import { ClusterCapacity } from "./ClusterCapacity";
import { useEffect, useRef, useState } from "react";
import { AdminAPIError, prepareSparkrunRecipe, sparkRunCatalog, type SparkrunOperation, type SparkrunRecipe, type SparkrunRecipeDetails } from "./api";
import type { ConfigurationDocument } from "./types";

type Cluster = { name: string; description: string; host_count: number; default: boolean };
type Registry = { name: string; enabled: boolean; cached: boolean };
type Page = { facets?: Record<string, string[]>; recipes: SparkrunRecipe[]; total: number; next_offset: number | null; unavailable_registries: string[] };
const emptyPage: Page = { recipes: [], total: 0, next_offset: null, unavailable_registries: [] };

export function SparkrunRecipeWizard({ token, document, revision, onChange, onClose, embedded = false, initialDeployment }: {
  token: string; document: ConfigurationDocument; revision: string; embedded?: boolean; initialDeployment?: Record<string, unknown>;
  onChange: (document: ConfigurationDocument, reused: boolean, deployment: string) => void; onClose: () => void;
}) {
  const initialSource = (initialDeployment?.endpoint_source ?? {}) as Record<string, unknown>;
  const [step, setStep] = useState<"recipe" | "configure">("recipe");
  const [source, setSource] = useState("registry");
  const [query, setQuery] = useState("");
  const [registry, setRegistry] = useState("");
  const [runtime, setRuntime] = useState("");
  const [filters, setFilters] = useState<Record<string, string>>({});
  const [page, setPage] = useState<Page>(emptyPage);
  const [offset, setOffset] = useState(0);
  const [registries, setRegistries] = useState<Registry[]>([]);
  const [clusters, setClusters] = useState<Cluster[]>([]);
  const [cluster, setCluster] = useState(String((initialSource.cluster_candidates as string[] | undefined)?.[0] ?? ""));
  const [fallbackClusters, setFallbackClusters] = useState<string[]>((initialSource.cluster_candidates as string[] | undefined)?.slice(1) ?? []);
  const [path, setPath] = useState("");
  const [yaml, setYaml] = useState("");
  const [name, setName] = useState("");
  const customName = useRef(false);
  const [aliases, setAliases] = useState("");
  const [waitMinutes, setWaitMinutes] = useState(() => durationMinutes(initialSource.activation_timeout, 30));
  const [recovery, setRecovery] = useState<Record<string, unknown>>(() => (initialSource.recovery ?? {}) as Record<string, unknown>);
  const [idleAction, setIdleAction] = useState(String(initialSource.idle_action || "stop"));
  const [idle, setIdle] = useState(() => durationMinutes(initialSource.idle_ttl, 0) > 0);
  const [idleMinutes, setIdleMinutes] = useState(() => durationMinutes(initialSource.idle_ttl, 30) || 30);
  const [overrides, setOverrides] = useState(() => JSON.stringify(initialSource.overrides ?? {}, null, 2));
  const [waiters, setWaiters] = useState(Number(initialSource.max_queued_waiters ?? 0));
  const [bodyMiB, setBodyMiB] = useState(Number(initialSource.max_queued_body_bytes ?? 0) / (1024 * 1024));
  const [nativeAPIs, setNativeAPIs] = useState<string[]>([]);
  const [preview, setPreview] = useState<SparkrunRecipeDetails>();
  const [previewOverrides, setPreviewOverrides] = useState("{}");
  const [busy, setBusy] = useState("");
  const [error, setError] = useState("");
  const [refresh, setRefresh] = useState<SparkrunOperation>();
  const [refreshMessage, setRefreshMessage] = useState("");
  const serial = useRef(0);
  const mounted = useRef(true);

  useEffect(() => {
    mounted.current = true;
    const abort = new AbortController();
    setBusy("Loading catalog");
    void Promise.all([
      sparkRunCatalog<{ clusters: Cluster[] }>(token, "catalog_clusters", {}, abort.signal),
      sparkRunCatalog<{ registries: Registry[] }>(token, "catalog_registries", {}, abort.signal),
      sparkRunCatalog<Page>(token, "catalog_search", { limit: 20 }, abort.signal),
    ]).then(async ([c, r, p]) => {
      setClusters(c.clusters); if (!initialDeployment) setCluster(c.clusters.find((entry) => entry.default)?.name ?? "");
      setRegistries(r.registries); setPage(p);
      if (initialDeployment) {
        const result = await sparkRunCatalog<SparkrunRecipeDetails>(token, "catalog_resolve", {reference: initialSource.recipe, overrides: initialSource.overrides ?? {}}, abort.signal);
        acceptPreview(result); setPreviewOverrides(overrides);
        if (result.recipe_revision !== initialSource.recipe_revision) setError("This recipe has changed since it was saved. Review this preview before applying the updated recipe.");
      }
    }).catch((e) => { if (!abort.signal.aborted) setError(message(e)); })
      .finally(() => { if (!abort.signal.aborted) setBusy(""); });
    return () => { mounted.current = false; abort.abort(); serial.current++; };
  }, [token]);

  useEffect(() => {
    if (refresh?.state !== "running") return;
    const abort = new AbortController();
    const timer = setTimeout(() => {
      void sparkRunCatalog<SparkrunOperation>(token, "operation_status", { operation_id: refresh.operation_id }, abort.signal)
        .then((status) => {
          setRefresh(status);
          if (status.state === "failed") setError(status.error?.message ?? "Registry refresh failed");
          if (status.state === "succeeded") {
            setRefreshMessage(status.result?.failed?.length ? `Could not refresh: ${status.result.failed.join(", ")}. Cached recipes are still available.` : "Registries refreshed. Search again to see updated recipes.");
          }
        }).catch((e) => { if (!abort.signal.aborted) { setError(message(e)); setRefresh(undefined); } });
    }, 1000);
    return () => { clearTimeout(timer); abort.abort(); };
  }, [refresh, token]);

  async function action(label: string, work: () => Promise<void>) {
    const current = ++serial.current;
    setBusy(label); setError("");
    try { await work(); } catch (e) { if (current === serial.current) setError(message(e)); }
    finally { if (current === serial.current) setBusy(""); }
  }
  async function search(next = 0) {
    await action("Searching recipes", async () => {
      const result = await sparkRunCatalog<Page>(token, "catalog_search", {
        ...(query.trim() ? { query: query.trim() } : {}), ...(registry ? { registry } : {}),
        ...(runtime ? { runtime } : {}), local_only: source === "local", offset: next, limit: 20, filters,
      });
      setPage(result); setOffset(next);
    });
  }
  function launchOverrides(): Record<string, string> {
    const value: unknown = JSON.parse(overrides);
    if (!value || Array.isArray(value) || typeof value !== "object" || Object.values(value).some((v) => typeof v !== "string")) {
      throw new Error('Overrides must be a JSON object with string values, such as {"tensor_parallel":"2"}.');
    }
    return value as Record<string, string>;
  }
  function acceptPreview(result: SparkrunRecipeDetails) {
    if (!initialDeployment && !customName.current) {
      const served = result.defaults?.served_model_name;
      setName(typeof served === "string" && served.trim() ? served.trim() : result.hf_model || result.model);
    }
    const protocols = (initialDeployment?.native_protocols ?? result.native_protocols) as string[];
    const capabilities = (initialDeployment?.capabilities ?? result.capabilities ?? []) as string[];
    setNativeAPIs([...(protocols.includes("openai") ? ["chat_completions"] : []), ...(capabilities.includes("responses") ? ["responses"] : []), ...(protocols.includes("anthropic") ? ["messages"] : [])]);
    setPreview(result);
    setStep("configure");
  }
  async function select(reference: string) {
    await action("Resolving recipe", async () => {
      const result = await sparkRunCatalog<SparkrunRecipeDetails>(token, "catalog_resolve", { reference, overrides: launchOverrides() });
      acceptPreview(result); setPreviewOverrides(overrides);
    });
  }
  const invalidPreview = !preview || previewOverrides !== overrides || preview.issues.some((issue) => issue.severity === "error");

  return <section className={embedded ? "recipe-wizard recipe-embedded" : "recipe-wizard"} aria-label="sparkrun recipe settings">
    {!embedded && <div className="panel-heading"><div><p className="eyebrow">On-demand model</p><h3>Add sparkrun recipe</h3></div>
      <button type="button" className="text-button" onClick={onClose}>Cancel</button></div>}
    <p className="section-help">{initialDeployment ? "Update the recipe or lifecycle settings. All names targeting this deployment keep their routing reference. Applying settings does not restart a running workload." : "Choose a recipe and a cluster. Saving makes the public model name available; its first request can start the workload."}</p>
    <div className="recipe-steps" aria-label="Setup steps"><strong>{step === "recipe" ? "1. Choose recipe" : "2. Configure model"}</strong></div>
    {error && <div className="notice error" role="alert">{error}</div>}
    {busy && <p role="status">{busy}…</p>}
    {step === "recipe" ? <>
      <fieldset disabled={Boolean(busy)} className="recipe-fieldset">
        <label>Recipe source<select aria-label="Recipe source" value={source} onChange={(e) => { setSource(e.target.value); setRegistry(""); setFilters({}); setPage(emptyPage); setOffset(0); }}>
          <option value="registry">Configured registries</option><option value="local">Files on the control node</option><option value="upload">Upload recipe YAML</option>
        </select></label>
        {source === "upload" ? <>
          <p className="section-help">Upload one self-contained YAML file (up to 256 KiB). Auxiliary files must already be available on the control node. Uploading does not grant permission to run untrusted hooks.</p>
          <label>Recipe file<input type="file" accept=".yaml,.yml" onChange={(e) => {
            const file = e.target.files?.[0]; if (!file) return;
            if (file.size > 256 * 1024) { setError("Recipe exceeds the 256 KiB limit."); return; }
            void file.text().then(setYaml).catch((e) => setError(message(e)));
          }} /></label>
          <label>Recipe YAML<textarea rows={9} spellCheck={false} value={yaml} onChange={(e) => setYaml(e.target.value)} /></label>
          <button type="button" className="secondary-button" disabled={!yaml.trim()} onClick={() => void action("Importing recipe", async () => {
            const result = await sparkRunCatalog<SparkrunRecipeDetails>(token, "catalog_import", { content: yaml });
            acceptPreview(result); setPreviewOverrides("{}"); setOverrides("{}");
          })}>Preview upload</button>
        </> : <>
          {source === "local" && <><p className="section-help">Paths refer to the machine running sparkrun, which may be different from this browser. The list includes its configured recipe folder and prior imports.</p>
            <label>Absolute recipe path<input placeholder="/home/user/recipes/coding.yaml" value={path} onChange={(e) => setPath(e.target.value)} /></label>
            <button type="button" className="secondary-button" disabled={!path.trim()} onClick={() => void select(path.trim())}>Preview local file</button></>}
          <div className="model-field-grid">
            <label>Search recipes<input value={query} onChange={(e) => setQuery(e.target.value)} onKeyDown={(e) => { if (e.key === "Enter") { e.preventDefault(); void search(); } }} placeholder="Model, recipe name, or description" /></label>
            {source === "registry" && <label>Registry<select aria-label="Registry" value={registry} onChange={(e) => setRegistry(e.target.value)}><option value="">All configured registries</option>
              {registries.filter((r) => r.enabled).map((r) => <option key={r.name} value={r.name}>{r.name}{!r.cached ? " · not cached" : ""}</option>)}</select></label>}
            <label>Runtime<input value={runtime} onChange={(e) => setRuntime(e.target.value)} placeholder="Any runtime" /></label>
          </div>
          <div className="model-section"><h4>Native APIs</h4><p className="section-help">Options follow the runtime family. Defaults use known image versions or recipe declarations; enable additional APIs only when the selected image serves them.</p>
          <div className="capability-grid">{(preview?.native_api_options ?? ["chat_completions"]).map((api) => <label key={api}><input type="checkbox" checked={nativeAPIs.includes(api)} disabled={nativeAPIs.includes(api) && nativeAPIs.length === 1 || api === "chat_completions" && nativeAPIs.includes("responses")} onChange={(e) => setNativeAPIs(e.target.checked ? [...new Set([...nativeAPIs, ...(api === "responses" ? ["chat_completions"] : []), api])] : nativeAPIs.filter((value) => value !== api))} /><span>{({chat_completions: "OpenAI (Chat Completions)", responses: "OpenAI (Responses)", messages: "Anthropic Messages"} as Record<string,string>)[api] ?? api}</span></label>)}</div>
        </div>
        <details className="catalog-filters"><summary>More recipe filters{Object.keys(filters).length ? ` (${Object.keys(filters).length})` : ""}</summary>
            <p className="section-help">Values come from recipe metadata; unknown values are not inferred from model names.</p>
            <div className="model-field-grid">{Object.entries({min_nodes:"Minimum nodes", tp:"Tensor parallel", pp:"Pipeline parallel", quantization:"Quantization", context_length:"Context tokens", parameters_b:"Parameters (billions)"}).map(([key, label]) => <label key={key}>{label}<select aria-label={label} value={filters[key] || ""} onChange={(e) => setFilters((current) => {const next = {...current}; if (e.target.value) next[key] = e.target.value; else delete next[key]; return next;})}><option value="">Any</option>{(page.facets?.[key] ?? []).map((value) => <option key={value} value={value}>{value}</option>)}</select></label>)}</div>
            <button type="button" onClick={() => {setFilters({}); setQuery(""); setRegistry(""); setRuntime("");}}>Clear filters</button>
          </details>
          <div className="recipe-actions"><button type="button" className="secondary-button" onClick={() => void search()}>Search</button>
            {source === "registry" && <button type="button" className="text-button" disabled={refresh?.state === "running"} onClick={() => void action("Starting refresh", async () => {
              setRefresh(await sparkRunCatalog<SparkrunOperation>(token, "catalog_refresh")); setRefreshMessage("");
            })}>Refresh registries</button>}</div>
          {source === "registry" && <RegistryManager token={token} registries={registries} onChange={(values) => {setRegistries(values); setPage(emptyPage); setOffset(0);}} />}
          {refresh?.state === "running" && <p role="status">{refresh.phase}… You can keep browsing cached recipes.</p>}
          {refreshMessage && <p role="status">{refreshMessage}</p>}
          {!!page.unavailable_registries.length && <p className="notice info">Not cached: {page.unavailable_registries.join(", ")}. Refresh registries to download their recipes.</p>}
          <ul className="recipe-results" aria-label="Recipes">{page.recipes.map((recipe) => <li key={recipe.reference}>
            <div><strong>{recipe.name}</strong><span>{recipe.model} · {recipe.runtime} · {recipe.min_nodes} node{recipe.min_nodes === 1 ? "" : "s"} minimum</span>
              <small>{recipe.description}</small><small>{[recipe.quantization, recipe.context_length ? `${recipe.context_length} context tokens` : "", recipe.parameters_b ? `${recipe.parameters_b}B parameters` : "", recipe.tp ? `TP ${recipe.tp}` : ""].filter(Boolean).join(" · ")}</small><small className="recipe-source">{recipe.registry ? `@${recipe.registry} · ` : "Local · "}{recipe.source_path}</small></div>
            <button type="button" className="secondary-button" onClick={() => void select(recipe.reference)} aria-label={`Choose ${recipe.name}`}>Choose</button>
          </li>)}</ul>
          {!page.recipes.length && !busy && <p>No recipes shown. Search the cache, refresh registries, or select a local file.</p>}
          <div className="recipe-actions"><span>{page.total} recipes{page.total > 0 ? ` · ${offset + 1}–${offset + page.recipes.length}` : ""}</span>
            <button type="button" disabled={offset === 0} onClick={() => void search(Math.max(0, offset - 20))}>Previous</button>
            <button type="button" disabled={page.next_offset === null} onClick={() => void search(page.next_offset ?? 0)}>Next</button></div>
        </>}
      </fieldset>
    </> : <>
      <button type="button" className="text-button" disabled={Boolean(busy)} onClick={() => setStep("recipe")}>← Choose another recipe</button>
      {preview && <div className="recipe-preview"><strong>{preview.name}</strong><p>{preview.model} · {preview.runtime} · {preview.min_nodes} node{preview.min_nodes === 1 ? "" : "s"} minimum</p>
        <small className="recipe-source">{preview.source_path}</small>
        <p>HF model: {preview.hf_model || preview.model}</p><p>Recipe defaults: {JSON.stringify(preview.defaults)}</p>
        {capabilityOptions.some(({value}) => (preview.capabilities ?? []).includes(value)) && <p>Model capabilities: {capabilityOptions.filter(({value}) => (preview.capabilities ?? []).includes(value)).map(({label}) => label).join(", ")}.</p>}
        {Object.keys(preview.sparkroute?.request_profiles ?? {}).length > 0 && <p>Recipe request profiles: {Object.keys(preview.sparkroute?.request_profiles ?? {}).sort().join(", ")}. {initialDeployment ? "Existing profiles remain unchanged." : "Added under the public model name in Virtual Models / Aliases, where you can edit their parameters."}</p>}
        <p>Recipe extensions: {preview.required_plugins.join(", ") || "none"}.</p>
        <details><summary>Benchmark context</summary><p className="section-help">Declared recipe results depend on hardware and request shape; browsing does not run a benchmark.</p>
          {preview.metadata?.benchmarks?.length ? preview.metadata.benchmarks.map((benchmark, i) => <dl className="recipe-benchmark" key={i}>{Object.entries(benchmark).map(([key, value]) => <div key={key}><dt>{key.replaceAll("_", " ")}</dt><dd>{String(value)}</dd></div>)}</dl>) : <p>No benchmark results declared for this recipe.</p>}
        </details>
        <RecipePluginStatus token={token} required={preview.required_plugins} />
        {preview.issues.map((issue, i) => <p key={`${issue.code}-${i}`} className={`notice ${issue.severity === "error" ? "error" : "info"}`}>{issue.message}</p>)}
      </div>}
      <fieldset className="recipe-fieldset" disabled={Boolean(busy)}>
        <div className="model-field-grid">
          {!initialDeployment && <><label>Public model name<input required value={name} onChange={(e) => { customName.current = true; setName(e.target.value); }} placeholder="coding" /></label>
          <label>Aliases (comma separated)<input value={aliases} onChange={(e) => setAliases(e.target.value)} placeholder="code, assistant" /></label></>}
          <label>Cluster<select aria-label="Cluster" required value={cluster} onChange={(e) => setCluster(e.target.value)}><option value="">Choose a cluster</option>
            {clusters.map((c) => <option key={c.name} value={c.name}>{c.name} · {c.host_count} hosts{c.default ? " · default" : ""}</option>)}</select></label>
          <label>Cold-start wait (minutes)<input aria-label="Cold-start wait (minutes)" aria-describedby="cold-start-wait-help" type="number" min={1} max={60} required value={waitMinutes} onChange={(e) => setWaitMinutes(Number(e.target.value))} /><small id="cold-start-wait-help">How long a request can wait for the model to start and become ready.</small></label>
        </div>
        {!clusters.length && <p className="notice info">No named clusters are configured. Add a cluster with sparkrun on the control node, then reopen this form.</p>}
        <p className="section-help">The selected cluster is saved by name. A later change to sparkrun’s default cluster will not move this model.</p>
        <ClusterCapacity token={token} cluster={cluster} />
        <label className="checkbox-field"><input type="checkbox" checked={idle} onChange={(e) => setIdle(e.target.checked)} />Manage the model when idle</label>
        {idle && <label>Idle action<select aria-label="Idle action" value={idleAction} onChange={(e) => setIdleAction(e.target.value)}><option value="stop">Stop workload</option><option value="sleep" disabled={!preview?.required_plugins.some((p) => p === "coldsnap" || p.endsWith(".coldsnap"))}>Sleep with ColdSnap</option></select><small>Sleep releases GPU memory and wakes on the next request; it requires a job actually started with ColdSnap.</small></label>}
        {idle && <label>Idle time (minutes)<input type="number" min={1} required value={idleMinutes} onChange={(e) => setIdleMinutes(Number(e.target.value))} /></label>}
        <p className="section-help">Idle time begins after the last active request finishes. SparkRoute only stops workloads it started; an adopted workload stays running.</p>
        <RecoveryFields value={recovery} onChange={setRecovery} />
        <details><summary>Advanced launch settings</summary><div className="recipe-fieldset">
          <fieldset className="recipe-fieldset"><legend>Fallback clusters</legend>
            <p className="section-help">Try these clusters in order if the preferred cluster has insufficient capacity; a failed or uncertain launch does not start a second workload.</p>
            {fallbackClusters.map((candidate, index) => <div className="recipe-actions" key={index}>
              <label>Fallback {index + 1}<select aria-label={`Fallback cluster ${index + 1}`} value={candidate} onChange={(e) => setFallbackClusters((values) => values.map((value, i) => i === index ? e.target.value : value))}>
                <option value="">Choose a cluster</option>{clusters.filter((c) => c.name !== cluster && (c.name === candidate || !fallbackClusters.includes(c.name))).map((c) => <option key={c.name} value={c.name}>{c.name} · {c.host_count} hosts</option>)}
              </select></label>
              <button type="button" disabled={index === 0} onClick={() => setFallbackClusters((values) => {const next = [...values]; [next[index-1], next[index]] = [next[index]!, next[index-1]!]; return next;})} aria-label={`Move fallback ${index + 1} up`}>↑</button>
              <button type="button" onClick={() => setFallbackClusters((values) => values.filter((_, i) => i !== index))} aria-label={`Remove fallback ${index + 1}`}>Remove</button>
            </div>)}
            <button type="button" disabled={fallbackClusters.length >= clusters.length - 1 || fallbackClusters.includes("")} onClick={() => setFallbackClusters((values) => [...values, ""])}>Add fallback cluster</button>
          </fieldset>
          <label>Recipe overrides (JSON string values)<textarea rows={4} spellCheck={false} value={overrides} onChange={(e) => setOverrides(e.target.value)} /></label>
          <p className="section-help">For example: {`{"tensor_parallel":"2","max_model_len":"32768"}`}. The serving port increments automatically if occupied. Refresh the preview after changing overrides.</p>
          <button type="button" className="secondary-button" onClick={() => preview && void select(preview.reference)}>Refresh recipe preview</button>
          <div className="model-field-grid"><label>Queued requests (0 uses default)<input type="number" min={0} value={waiters} onChange={(e) => setWaiters(Number(e.target.value))} /></label>
            <label>Queued request data in MiB (0 uses default)<input type="number" min={0} value={bodyMiB} onChange={(e) => setBodyMiB(Number(e.target.value))} /></label></div>
        </div></details>
        {previewOverrides !== overrides && <p role="status">Refresh the recipe preview to validate these overrides.</p>}
        {!initialDeployment && <p className="section-help">If this recipe already has a deployment on the selected cluster, the model will share that deployment and its existing idle and cold-start settings.</p>}
        <button type="button" className="primary-button" disabled={invalidPreview || (!initialDeployment && !name.trim()) || !cluster || fallbackClusters.some((c) => !c || c === cluster) || waitMinutes < 1 || waitMinutes > 60 || (idle && idleMinutes < 1)} onClick={() => void action("Preparing model draft", async () => {
          const result = await prepareSparkrunRecipe(token, document, revision, {
            ...(initialDeployment ? {deployment: initialDeployment.name} : {}),
            native_apis: nativeAPIs, reference: preview!.reference, recipe_revision: preview!.recipe_revision, name: name.trim(),
            aliases: aliases.split(",").map((v) => v.trim()).filter(Boolean), cluster, fallback_clusters: fallbackClusters, overrides: launchOverrides(),
            activation_timeout: `${waitMinutes}m`, idle_action: idleAction, idle_ttl: idle ? `${idleMinutes}m` : "0s",
            recovery: recovery.action ? recovery : undefined,
            max_queued_waiters: waiters, max_queued_body_bytes: bodyMiB * 1024 * 1024,
          });
          if (mounted.current) onChange(result.document, result.reused, result.deployment);
        })}>{initialDeployment ? "Apply to draft" : "Add to draft"}</button>
      </fieldset>
    </>}
  </section>;
}

function message(error: unknown) {
  if (error instanceof AdminAPIError && error.status === 404) return "The sparkrun catalog is unavailable on this gateway. Enable the sparkrun integration and use the matching gateway and plugin versions.";
  return error instanceof Error ? error.message : "sparkrun catalog is unavailable. Check that sparkrun and its SparkRoute plugin are installed on the control node.";
}

export function durationMinutes(value: unknown, fallback: number): number {
  if (typeof value !== "string" || !value) return fallback;
  const parts = [...value.matchAll(/(\d+(?:\.\d+)?)(h|ms|us|µs|ns|m|s)/g)];
  if (!parts.length) return fallback;
  const scale: Record<string, number> = {h: 60, m: 1, s: 1/60, ms: 1/60000, us: 1/60000000, "µs": 1/60000000, ns: 1/60000000000};
  return parts.reduce((total, part) => total + Number(part[1]) * scale[part[2]!]!, 0);
}
