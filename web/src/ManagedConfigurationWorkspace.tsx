import { useCallback, useEffect, useMemo, useRef, useState } from "react";
import {
  AdminAPIError,
  fetchActiveConfiguration,
  fetchDiscoveredModelMetadata,
  fetchManagedConfigurationSet,
  fetchManagedConfigurationSets,
  replaceManagedConfigurationSet,
  simulateManagedModelRouting,
  validateManagedConfigurationSet,
} from "./api";
import type {
  AdminBootstrap,
  ConfigurationDocument,
  DiscoveredMetadataState,
  GatewayStatus,
  ManagedConfigurationOwner,
  ManagedConfigurationSetMetadata,
} from "./types";
import { MMProjectionSettingsEditor } from "./MMProjectionSettingsEditor";
import { TraceSettingsEditor } from "./TraceSettingsEditor";
import { ProviderDeploymentEditor } from "./ProviderDeploymentEditor";
import { PolicyProfilesEditor } from "./PolicyProfilesEditor";
import { VirtualModelEditor } from "./VirtualModelEditor";
import { ModelRoutingEditor } from "./ModelRoutingEditor";
import type { VirtualModelEditorExtension } from "./extensions";
import { configurationSections, type ConfigurationSection } from "./configurationSections";

const emptyDocument = `{
  "providers": [],
  "deployments": [],
  "virtual_models": []
}`;

interface Notice {
  kind: "success" | "error" | "info";
  text: string;
}

export function ManagedConfigurationWorkspace({
  bootstrap,
  token,
  virtualModelExtensions,
  section = "providers",
  runtimeTargets,
}: {
  bootstrap: AdminBootstrap;
  token: string;
  virtualModelExtensions?: VirtualModelEditorExtension[];
  section?: ConfigurationSection;
  runtimeTargets?: GatewayStatus["targets"];
}) {
  const canRead = Boolean(bootstrap.features.config_read);
  const canWriteOperator = Boolean(bootstrap.features.config_write);
  const hasDiscoveredMetadata = Boolean(bootstrap.features.model_routing_discovered_metadata);
  const selectedOwner: ManagedConfigurationOwner = "operator";
  const [sets, setSets] = useState<ManagedConfigurationSetMetadata[]>([]);
  const [drafts, setDrafts] = useState<Partial<Record<ManagedConfigurationOwner, string>>>({});
  const [storedDocuments, setStoredDocuments] = useState<Partial<Record<ManagedConfigurationOwner, ConfigurationDocument>>>({});
  const [storedRevision, setStoredRevision] = useState(bootstrap.config_revision);
  const [runtimeRevision, setRuntimeRevision] = useState(bootstrap.config_revision);
  const [activeDocument, setActiveDocument] = useState<ConfigurationDocument>();
  const [discoveredMetadata, setDiscoveredMetadata] = useState<DiscoveredMetadataState>();
  const [validatedCandidate, setValidatedCandidate] = useState<string>();
  const editorRef = useRef<HTMLDivElement>(null);
  const [notice, setNotice] = useState<Notice>();
  const [busy, setBusy] = useState("");
  const [recipeWizard, setRecipeWizard] = useState(false);
  const [editorMode, setEditorMode] = useState<"structured" | "json">("structured");
  const infrastructureSection = useRef<"providers" | "deployments">("providers");
  if (section === "providers" || section === "deployments") infrastructureSection.current = section;

  const load = useCallback(async () => {
    setBusy("load");
    try {
      const setPage = await fetchManagedConfigurationSets(token);
      setSets(setPage.managed_sets);
      setStoredRevision(setPage.active_revision);
      const visibleOwners = setPage.managed_sets.map((set) => set.owner);
      const loadedSets = await Promise.all(
        visibleOwners.map((owner) => fetchManagedConfigurationSet(token, owner)),
      );
      setDrafts(
        Object.fromEntries(
          loadedSets.map((set) => [set.owner, JSON.stringify(set.document, null, 2)]),
        ),
      );
      setStoredDocuments(Object.fromEntries(loadedSets.map((set) => [set.owner, set.document])));
      if (canRead) {
        const [active, discovered] = await Promise.all([
          fetchActiveConfiguration(token),
          hasDiscoveredMetadata ? fetchDiscoveredModelMetadata(token) : Promise.resolve(undefined),
        ]);
        setRuntimeRevision(active.revision);
        setActiveDocument(active.document);
        setDiscoveredMetadata(discovered);
      }
    } catch (error) {
      setValidatedCandidate(undefined);
      setNotice(errorNotice(error));
    } finally {
      setBusy("");
    }
  }, [canRead, hasDiscoveredMetadata, token]);

  useEffect(() => {
    void load();
  }, [load]);

  const draft = drafts[selectedOwner] ?? emptyDocument;
  const parsed = useMemo(() => parseDocument(draft), [draft]);
  const operatorDraft = drafts.operator ?? emptyDocument;
  const operatorParsed = useMemo(() => parseDocument(operatorDraft), [operatorDraft]);
  const generatedDocument = useMemo(() => {
    const stored = storedDocuments.sparkrun;
    if (!stored || !Array.isArray(stored.deployments)) return stored;
    const titles = new Map(runtimeTargets?.filter((target) => target.title).map((target) => [target.deployment, target.title]));
    return { ...stored, deployments: stored.deployments.map((deployment) => titles.has(deployment.name)
      ? { ...deployment, title: titles.get(deployment.name) } : deployment) };
  }, [storedDocuments.sparkrun, runtimeTargets]);
  const metadata = sets.find((set) => set.owner === selectedOwner);
  const canEdit = canWriteOperator && Boolean(storedDocuments.operator);
  const candidateKey = `${storedRevision}\0${draft}`;
  const validated = validatedCandidate === candidateKey;
  const mergedCandidateModelNames = useMemo(() => {
    const names = new Set(documentEntityNames(operatorParsed.document, "virtual_models"));
    documentEntityNames(generatedDocument, "virtual_models").forEach((name) => names.add(name));
    return [...names].sort();
  }, [operatorParsed.document, generatedDocument]);
  const reservedModelNames = useMemo(() => [
    ...documentEntityNames(generatedDocument, "virtual_models"),
    ...(Array.isArray(generatedDocument?.virtual_models) ? generatedDocument.virtual_models : []).flatMap((model) => Array.isArray(model.aliases)
      ? model.aliases.filter((alias: unknown): alias is string => typeof alias === "string") : []),
  ], [generatedDocument]);
  const dirty = drafts.operator !== undefined && (
    !operatorParsed.document || JSON.stringify(operatorParsed.document) !== JSON.stringify(storedDocuments.operator)
  );

  // Observe application without reloading or replacing the operator's draft.
  useEffect(() => {
    if (!canRead || storedRevision === runtimeRevision) return;
    let cancelled = false;
    const timer = window.setInterval(() => {
      void fetchActiveConfiguration(token).then((active) => {
        if (cancelled) return;
        setRuntimeRevision(active.revision);
        setActiveDocument(active.document);
      }).catch(() => { /* Keep the last confirmed serving revision and retry. */ });
    }, 1000);
    return () => { cancelled = true; window.clearInterval(timer); };
  }, [canRead, storedRevision, runtimeRevision, token]);

  const changeDraft = (value: string) => {
    setValidatedCandidate(undefined);
    setDrafts((current) => ({ ...current, [selectedOwner]: value }));
    setNotice(undefined);
  };

  const fieldsValid = () => {
    const invalid = [...(editorRef.current?.querySelectorAll<HTMLInputElement | HTMLTextAreaElement | HTMLSelectElement>("input,textarea,select") ?? [])]
      .find((field) => !field.checkValidity());
    if (invalid) {
      setValidatedCandidate(undefined);
      setNotice({ kind: "error", text: "Correct the invalid field before validating or saving. Check JSON defaults in other sections too." });
      invalid.reportValidity();
      return false;
    }
    return true;
  };

  const validate = async () => {
    setValidatedCandidate(undefined);
    if (!fieldsValid()) return;
    if (!parsed.document) {
      setNotice({ kind: "error", text: parsed.error ?? "Configuration is invalid JSON." });
      return;
    }
    setBusy("validate");
    try {
      const result = await validateManagedConfigurationSet(
        token,
        selectedOwner,
        parsed.document,
        storedRevision,
      );
      setValidatedCandidate(candidateKey);
      setNotice({
        kind: "success",
        text: `Configuration is valid. Save to apply these changes (${compactRevision(result.candidate_revision)}).`,
      });
    } catch (error) {
      setValidatedCandidate(undefined);
      setNotice(errorNotice(error));
    } finally {
      setBusy("");
    }
  };

  const save = async () => {
    if (!validated || !fieldsValid()) return;
    if (!parsed.document || !canEdit) {
      setNotice({ kind: "error", text: parsed.error ?? "This managed set is read-only." });
      return;
    }
    setBusy("save");
    try {
      const result = await replaceManagedConfigurationSet(
        token,
        selectedOwner,
        parsed.document,
        storedRevision,
        "",
      );
      setStoredRevision(result.current.revision);
      setSets((current) => current.map((set) => (
        set.owner === selectedOwner ? result.managed_set : set
      )));
      setStoredDocuments((current) => ({ ...current, [selectedOwner]: parsed.document }));
      setNotice({
        kind: "success",
        text: result.changed
          ? "Stored the current configuration. Runtime application follows automatically."
          : "The operator set already matched the submitted document.",
      });
      setValidatedCandidate(undefined);
    } catch (error) {
      setValidatedCandidate(undefined);
      setNotice(errorNotice(error));
    } finally {
      setBusy("");
    }
  };

  const structuredChange = (document: ConfigurationDocument) => {
    setValidatedCandidate(undefined);
    setDrafts((current) => ({ ...current, operator: JSON.stringify(document, null, 2) }));
    setNotice({ kind: "info", text: "Draft changed. Validate before saving." });
  };

  return (
    <div className="config-workspace" ref={editorRef} onChangeCapture={() => setValidatedCandidate(undefined)}>
      <section className="managed-config-overview" aria-label="Managed configuration ownership">
        <div className="managed-owner-card">
          <span>Configuration</span>
          <strong>{dirty ? "Unsaved changes" : "Saved configuration"}</strong>
          <small>All sections share one draft; your changes are saved together.{bootstrap.features.sparkrun ? " sparkrun entries are read-only." : ""}</small>
        </div>
        <div className="managed-runtime-card">
          <span>Stored / serving</span>
          <strong>{!canRead ? "Serving revision unavailable" : storedRevision === runtimeRevision ? "In sync" : "Applying"}</strong>
          <code>{compactRevision(storedRevision)} / {compactRevision(runtimeRevision)}</code>
          <small>{documentSummary(activeDocument)}</small>
        </div>
      </section>

      <section className="panel editor-panel">
        <div className="panel-heading">
          <div>
            <p className="eyebrow">Gateway configuration</p>
            <h2>{configurationSections.find((entry) => entry.id === section)?.label}</h2>
          </div>
          <span className="activity-summary">
            {canEdit ? "Editable" : "Read only"} · {compactRevision(metadata?.revision ?? "")}
          </span>
        </div>
        <div className="editor-toolbar">
          <div className="editor-mode-switch" aria-label="Configuration editor mode" role="group">
            <button
              disabled={recipeWizard}
              aria-pressed={editorMode === "structured"}
              className={editorMode === "structured" ? "active" : ""}
              onClick={() => setEditorMode("structured")}
              type="button"
            >
              Structured
            </button>
            <button
              disabled={recipeWizard}
              aria-pressed={editorMode === "json"}
              className={editorMode === "json" ? "active" : ""}
              onClick={() => setEditorMode("json")}
              type="button"
            >
              JSON
            </button>
          </div>
          <button
            className="text-button"
            disabled={Boolean(busy) || recipeWizard || !parsed.document || !canEdit}
            onClick={() => parsed.document && changeDraft(JSON.stringify(parsed.document, null, 2))}
            type="button"
          >
            Format
          </button>
          {canEdit ? (
            <button
              className={validated ? "primary-button editor-action" : "secondary-button editor-action"}
              disabled={Boolean(busy) || recipeWizard}
              onClick={() => void (validated ? save() : validate())}
              type="button"
            >
              {busy === "save" ? "Saving…" : busy === "validate" ? "Validating…" : validated ? "Save" : "Validate"}
            </button>
          ) : null}
        </div>
        {notice ? <div className={`notice ${notice.kind}`} role="status">{notice.text}</div> : null}
        <div hidden={editorMode !== "structured"}>
          {operatorParsed.document ? (
            <>
              <div hidden={section !== "advanced"}>
                <TraceSettingsEditor document={operatorParsed.document} disabled={!canEdit || Boolean(busy) || recipeWizard} onChange={structuredChange}/>
                <MMProjectionSettingsEditor document={operatorParsed.document} disabled={!canEdit || Boolean(busy) || recipeWizard} onChange={structuredChange} token={token} canReadStatus={Boolean(bootstrap.features.status)} runtimeRevision={runtimeRevision}/>
              </div>
              {(["privacy", "guardrails"] as const).map(page => <div key={page} hidden={section !== page}><PolicyProfilesEditor kind={page === "privacy" ? "pii" : "guardrails"} document={operatorParsed.document!} readOnlyDocument={generatedDocument} disabled={!canEdit || Boolean(busy) || recipeWizard} onChange={structuredChange} /></div>)}
              <div hidden={section !== "models"}>
                <VirtualModelEditor
                  disabled={!canEdit || Boolean(busy) || recipeWizard}
                  document={operatorParsed.document}
                  readOnlyDocument={generatedDocument}
                  reservedModelNames={reservedModelNames}
                  extensions={virtualModelExtensions}
                  onChange={structuredChange}
                />
              </div>
              <div hidden={section !== "routing"}>
                <p className="section-help configuration-context">Route to configured virtual models. To use a deployment directly, first add it to a virtual model.</p>
                {generatedDocument?.model_routing ? <p className="read-only-note">sparkrun generated · Read only</p> : null}
                <ModelRoutingEditor
                  additionalDocument={generatedDocument?.model_routing ? operatorParsed.document : generatedDocument}
                  canonicalModelNames={mergedCandidateModelNames}
                  disabled={!canEdit || Boolean(busy) || recipeWizard || Boolean(generatedDocument?.model_routing)}
                  discoveredMetadata={discoveredMetadata}
                  document={generatedDocument?.model_routing ? generatedDocument : operatorParsed.document}
                  onChange={generatedDocument?.model_routing ? () => {} : structuredChange}
                  simulate={bootstrap.features.config_routing_simulation && !generatedDocument?.model_routing
                    ? (document, requestedModel, routingText, requiredCapabilities) => simulateManagedModelRouting(
                      token,
                      "operator",
                      document,
                      storedRevision,
                      requestedModel,
                      routingText,
                      requiredCapabilities,
                    )
                    : undefined}
                />
              </div>
              <div hidden={section !== "providers" && section !== "deployments"}>
                <ProviderDeploymentEditor sparkrun={{enabled: Boolean(bootstrap.features.sparkrun_catalog), token, revision: storedRevision, onEditingChange: setRecipeWizard, onPrepared: (document, message) => { structuredChange(document); setNotice({kind: "info", text: message + " Validate, then Save to make it available. Saving does not launch it."}); }}} section={infrastructureSection.current} simplifiedCapabilities disabled={!canEdit || Boolean(busy)} document={operatorParsed.document} readOnlyDocument={generatedDocument} onChange={structuredChange} subscriptionAuth={{ token, enabled: Boolean(bootstrap.features.provider_auth) }} />
              </div>
            </>
          ) : (
            <div className="structured-unavailable">
              <strong>Structured editor unavailable</strong>
              <p>{operatorParsed.error} Switch to JSON to inspect or repair the document.</p>
            </div>
          )}
        </div>
        {editorMode === "json" ? (
          <>
          <p className="section-help configuration-context">Edit your configuration here. Generated entries are included when validating and saving.</p>
          <textarea
            aria-invalid={Boolean(parsed.error)}
            aria-label={`${selectedOwner} configuration JSON`}
            className="config-editor"
            onChange={(event) => changeDraft(event.target.value)}
            readOnly={!canEdit || Boolean(busy)}
            spellCheck={false}
            value={draft}
          />
          {generatedDocument ? <details className="generated-json"><summary>sparkrun entries · Read only</summary><textarea aria-label="sparkrun configuration JSON" className="config-editor" readOnly value={JSON.stringify(storedDocuments.sparkrun, null, 2)} /></details> : null}
          </>
        ) : null}
      </section>

    </div>
  );
}

function parseDocument(raw: string): { document?: ConfigurationDocument; error?: string } {
  try {
    const value = JSON.parse(raw) as unknown;
    if (!value || Array.isArray(value) || typeof value !== "object") {
      return { error: "Configuration must be a JSON object." };
    }
    return { document: value as ConfigurationDocument };
  } catch (error) {
    return { error: error instanceof Error ? error.message : "Configuration is invalid JSON." };
  }
}

function documentEntityNames(document: ConfigurationDocument | undefined, field: "deployments" | "virtual_models"): string[] {
  if (!document || !Array.isArray(document[field])) return [];
  return document[field]
    .map((entity) => typeof entity.name === "string" ? entity.name : "")
    .filter(Boolean);
}

function compactRevision(revision: string) {
  return revision ? revision.slice(0, 12) : "unavailable";
}

function documentSummary(document?: ConfigurationDocument) {
  if (!document) return "Merged document unavailable";
  const count = (name: string) => Array.isArray(document[name]) ? document[name].length : 0;
  return `${count("providers")} providers · ${count("deployments")} deployments · ${count("virtual_models")} models`;
}

function errorNotice(error: unknown): Notice {
  if (error instanceof AdminAPIError && error.status === 409) {
    return { kind: "error", text: "The active revision changed. Refresh and reapply your edit to the latest owner set." };
  }
  return {
    kind: "error",
    text: error instanceof Error ? error.message : "Configuration request failed.",
  };
}
