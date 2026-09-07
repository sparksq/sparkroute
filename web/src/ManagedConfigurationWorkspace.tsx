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
  ManagedConfigurationOwner,
  ManagedConfigurationSetMetadata,
} from "./types";
import { ProviderDeploymentEditor } from "./ProviderDeploymentEditor";
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
}: {
  bootstrap: AdminBootstrap;
  token: string;
  virtualModelExtensions?: VirtualModelEditorExtension[];
  section?: ConfigurationSection;
}) {
  const canRead = Boolean(bootstrap.features.config_read);
  const canWriteOperator = Boolean(bootstrap.features.config_write);
  const hasDiscoveredMetadata = Boolean(bootstrap.features.model_routing_discovered_metadata);
  const selectedOwner: ManagedConfigurationOwner = section === "sparkrun" ? "sparkrun" : "operator";
  const [sets, setSets] = useState<ManagedConfigurationSetMetadata[]>([]);
  const [drafts, setDrafts] = useState<Partial<Record<ManagedConfigurationOwner, string>>>({});
  const [storedDocuments, setStoredDocuments] = useState<Partial<Record<ManagedConfigurationOwner, ConfigurationDocument>>>({});
  const [storedRevision, setStoredRevision] = useState(bootstrap.config_revision);
  const [runtimeRevision, setRuntimeRevision] = useState(bootstrap.config_revision);
  const [activeDocument, setActiveDocument] = useState<ConfigurationDocument>();
  const [discoveredMetadata, setDiscoveredMetadata] = useState<DiscoveredMetadataState>();
  const [reason, setReason] = useState("");
  const [notice, setNotice] = useState<Notice>();
  const [busy, setBusy] = useState("");
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
  const generatedDocument = storedDocuments.sparkrun;
  const generatedDeployments = useMemo(() => documentEntityNames(generatedDocument, "deployments")
    .map((name) => ({ name, source: "SparkRun generated" })), [generatedDocument]);
  const metadata = sets.find((set) => set.owner === selectedOwner);
  const canEdit = selectedOwner === "operator" && canWriteOperator;
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
    setDrafts((current) => ({ ...current, [selectedOwner]: value }));
    setNotice(undefined);
  };

  const validate = async () => {
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
      setNotice({
        kind: "success",
        text: `The merged candidate is valid at ${compactRevision(result.candidate_revision)}.`,
      });
    } catch (error) {
      setNotice(errorNotice(error));
    } finally {
      setBusy("");
    }
  };

  const save = async () => {
    if (!parsed.document || !canEdit) {
      setNotice({ kind: "error", text: parsed.error ?? "This managed set is read-only." });
      return;
    }
    setBusy("save");
    try {
      await validateManagedConfigurationSet(
        token,
        selectedOwner,
        parsed.document,
        storedRevision,
      );
      const result = await replaceManagedConfigurationSet(
        token,
        selectedOwner,
        parsed.document,
        storedRevision,
        reason.trim(),
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
      setReason("");
    } catch (error) {
      setNotice(errorNotice(error));
    } finally {
      setBusy("");
    }
  };

  const structuredChange = (document: ConfigurationDocument) => {
    setDrafts((current) => ({ ...current, operator: JSON.stringify(document, null, 2) }));
    setNotice({ kind: "info", text: "Draft changed. Validate the merged candidate before saving." });
  };

  return (
    <div className="config-workspace">
      <section className="managed-config-overview" aria-label="Managed configuration ownership">
        <div className="managed-owner-card">
          <span>{selectedOwner === "operator" ? "Operator managed" : "SparkRun generated · Read only"}</span>
          <strong>{selectedOwner === "operator" ? (dirty ? "Unsaved changes" : "Saved configuration") : "Managed by SparkRun"}</strong>
          <small>{selectedOwner === "operator"
            ? "Providers, deployments, aliases and routing share one draft. Save applies all four sections."
            : "Use these deployments and models as targets in operator-managed aliases and routing."}</small>
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
            <p className="eyebrow">{selectedOwner} managed set</p>
            <h2>{configurationSections.find((entry) => entry.id === section)?.label}</h2>
          </div>
          <span className="activity-summary">
            {canEdit ? "Editable" : "Read only"} · {compactRevision(metadata?.revision ?? "")}
          </span>
        </div>
        <div className="editor-toolbar">
          <div className="editor-mode-switch" aria-label="Configuration editor mode" role="group">
            <button
              aria-pressed={editorMode === "structured"}
              className={editorMode === "structured" ? "active" : ""}
              onClick={() => setEditorMode("structured")}
              type="button"
            >
              Structured
            </button>
            <button
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
            disabled={Boolean(busy) || !parsed.document || !canEdit}
            onClick={() => parsed.document && changeDraft(JSON.stringify(parsed.document, null, 2))}
            type="button"
          >
            Format
          </button>
          {canEdit ? (
            <button
              className="secondary-button editor-action"
              disabled={Boolean(busy)}
              onClick={() => void validate()}
              type="button"
            >
              {busy === "validate" ? "Validating…" : "Validate merged candidate"}
            </button>
          ) : null}
        </div>
        <div hidden={editorMode !== "structured" || selectedOwner !== "operator"}>
          {operatorParsed.document ? (
            <>
              <div hidden={section !== "models"}>
                {generatedDeployments.length ? <p className="section-help configuration-context">Targets include SparkRun-generated deployments. Saving a virtual model does not change their generated configuration.</p> : null}
                <VirtualModelEditor
                  disabled={!canWriteOperator || Boolean(busy)}
                  document={operatorParsed.document}
                  referencedDeployments={generatedDeployments}
                  reservedModelNames={reservedModelNames}
                  extensions={virtualModelExtensions}
                  onChange={structuredChange}
                />
              </div>
              <div hidden={section !== "routing"}>
                <p className="section-help configuration-context">Route to operator-managed or SparkRun-generated virtual models. To use a deployment directly, first add it to a virtual model.</p>
                <ModelRoutingEditor
                  canonicalModelNames={mergedCandidateModelNames}
                  disabled={!canWriteOperator || Boolean(busy)}
                  discoveredMetadata={discoveredMetadata}
                  document={operatorParsed.document}
                  onChange={structuredChange}
                  simulate={bootstrap.features.config_routing_simulation
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
                <ProviderDeploymentEditor section={infrastructureSection.current} simplifiedCapabilities disabled={!canWriteOperator || Boolean(busy)} document={operatorParsed.document} onChange={structuredChange} subscriptionAuth={{ token, enabled: Boolean(bootstrap.features.provider_auth) }} />
              </div>
            </>
          ) : (
            <div className="structured-unavailable">
              <strong>Structured editor unavailable</strong>
              <p>{operatorParsed.error} Switch to JSON to inspect or repair the document.</p>
            </div>
          )}
        </div>
        {editorMode === "structured" && selectedOwner === "sparkrun" ? (
          generatedDocument ? <>
            <ProviderDeploymentEditor simplifiedCapabilities disabled document={generatedDocument} onChange={() => {}} />
            <VirtualModelEditor disabled document={generatedDocument} onChange={() => {}} />
          </> : <p className="read-only-note">No SparkRun-generated configuration is available to this identity.</p>
        ) : null}
        {editorMode === "json" ? (
          <textarea
            aria-invalid={Boolean(parsed.error)}
            aria-label={`${selectedOwner} configuration JSON`}
            className="config-editor"
            onChange={(event) => changeDraft(event.target.value)}
            readOnly={!canEdit || Boolean(busy)}
            spellCheck={false}
            value={draft}
          />
        ) : null}
        {notice ? <div className={`notice ${notice.kind}`} role="status">{notice.text}</div> : null}
        {canEdit ? (
          <div className="publish-bar">
            <label>
              Change reason
              <input
                maxLength={4096}
                onChange={(event) => setReason(event.target.value)}
                placeholder="Why is the operator-owned set changing?"
                value={reason}
              />
            </label>
            <button className="primary-button" disabled={Boolean(busy)} onClick={() => void save()} type="button">
              {busy === "save" ? "Saving…" : "Validate and replace operator set"}
            </button>
          </div>
        ) : (
          <p className="read-only-note">
            {selectedOwner === "sparkrun"
              ? "This generated set is owned by SparkRun and cannot be edited in the console."
              : "This identity can inspect the operator set but cannot replace it."}
          </p>
        )}
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
