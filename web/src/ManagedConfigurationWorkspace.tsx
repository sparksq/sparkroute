import { useCallback, useEffect, useMemo, useState } from "react";
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
}: {
  bootstrap: AdminBootstrap;
  token: string;
  virtualModelExtensions?: VirtualModelEditorExtension[];
}) {
  const canRead = Boolean(bootstrap.features.config_read);
  const canWriteOperator = Boolean(bootstrap.features.config_write);
  const hasDiscoveredMetadata = Boolean(bootstrap.features.model_routing_discovered_metadata);
  const [selectedOwner, setSelectedOwner] = useState<ManagedConfigurationOwner>(
    canRead || canWriteOperator ? "operator" : "sparkrun",
  );
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
  const [structuredSection, setStructuredSection] = useState<"models" | "routing" | "infrastructure">("models");

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
      setSelectedOwner((current) => (
        visibleOwners.includes(current) || visibleOwners.length === 0
          ? current
          : visibleOwners[0]!
      ));
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
  const metadata = sets.find((set) => set.owner === selectedOwner);
  const canEdit = selectedOwner === "operator" && canWriteOperator;
  const mergedCandidateModelNames = useMemo(() => {
    const names = new Set(documentModelNames(parsed.document));
    Object.entries(storedDocuments).forEach(([owner, document]) => {
      if (owner !== selectedOwner) documentModelNames(document).forEach((name) => names.add(name));
    });
    return [...names].sort();
  }, [parsed.document, selectedOwner, storedDocuments]);

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
    changeDraft(JSON.stringify(document, null, 2));
    setNotice({ kind: "info", text: "Draft changed. Validate the merged candidate before saving." });
  };

  return (
    <div className="config-workspace">
      <section className="managed-config-overview" aria-label="Managed configuration ownership">
        {sets.map((set) => (
          <button
            aria-pressed={selectedOwner === set.owner}
            className={selectedOwner === set.owner ? "managed-owner-card active" : "managed-owner-card"}
            key={set.owner}
            onClick={() => setSelectedOwner(set.owner)}
            type="button"
          >
            <span>{set.owner === "operator" ? "Operator managed" : "Sparkrun generated"}</span>
            <strong>{set.owner}</strong>
            <code>{compactRevision(set.revision)}</code>
            <small>Updated by {set.updated_by}</small>
          </button>
        ))}
        <div className="managed-runtime-card">
          <span>Stored / serving</span>
          <strong>{storedRevision === runtimeRevision ? "In sync" : "Applying"}</strong>
          <code>{compactRevision(storedRevision)} / {compactRevision(runtimeRevision)}</code>
          <small>{documentSummary(activeDocument)}</small>
        </div>
      </section>

      <section className="panel editor-panel">
        <div className="panel-heading">
          <div>
            <p className="eyebrow">{selectedOwner} managed set</p>
            <h2>{selectedOwner === "operator" ? "Operator configuration" : "Sparkrun-generated configuration"}</h2>
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
        {editorMode === "structured" ? (
          parsed.document ? (
            <>
              <div className="structured-surface-tabs" aria-label="Structured configuration section" role="group">
                <button
                  aria-pressed={structuredSection === "models"}
                  className={structuredSection === "models" ? "active" : ""}
                  onClick={() => setStructuredSection("models")}
                  type="button"
                >
                  Virtual models
                </button>
                <button
                  aria-pressed={structuredSection === "routing"}
                  className={structuredSection === "routing" ? "active" : ""}
                  onClick={() => setStructuredSection("routing")}
                  type="button"
                >
                  Model routing
                </button>
                <button
                  aria-pressed={structuredSection === "infrastructure"}
                  className={structuredSection === "infrastructure" ? "active" : ""}
                  onClick={() => setStructuredSection("infrastructure")}
                  type="button"
                >
                  Providers &amp; deployments
                </button>
              </div>
              {structuredSection === "models" ? (
                <VirtualModelEditor
                  disabled={!canEdit || Boolean(busy)}
                  document={parsed.document}
                  extensions={virtualModelExtensions}
                  onChange={structuredChange}
                />
              ) : structuredSection === "routing" ? (
                <ModelRoutingEditor
                  canonicalModelNames={mergedCandidateModelNames}
                  disabled={!canEdit || Boolean(busy)}
                  discoveredMetadata={discoveredMetadata}
                  document={parsed.document}
                  onChange={structuredChange}
                  simulate={bootstrap.features.config_routing_simulation
                    ? (document, requestedModel, routingText, requiredCapabilities) => simulateManagedModelRouting(
                      token,
                      selectedOwner,
                      document,
                      storedRevision,
                      requestedModel,
                      routingText,
                      requiredCapabilities,
                    )
                    : undefined}
                />
              ) : (
                <ProviderDeploymentEditor disabled={!canEdit || Boolean(busy)} document={parsed.document} onChange={structuredChange} />
              )}
            </>
          ) : (
            <div className="structured-unavailable">
              <strong>Structured editor unavailable</strong>
              <p>{parsed.error} Switch to JSON to inspect or repair the document.</p>
            </div>
          )
        ) : (
          <textarea
            aria-invalid={Boolean(parsed.error)}
            aria-label={`${selectedOwner} configuration JSON`}
            className="config-editor"
            onChange={(event) => changeDraft(event.target.value)}
            readOnly={!canEdit || Boolean(busy)}
            spellCheck={false}
            value={draft}
          />
        )}
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
              ? "This generated set is owned by Sparkrun and cannot be edited in the console."
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

function documentModelNames(document?: ConfigurationDocument): string[] {
  if (!document || !Array.isArray(document.virtual_models)) return [];
  return document.virtual_models
    .map((model) => typeof model.name === "string" ? model.name : "")
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
