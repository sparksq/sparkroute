import { type ChangeEvent, useEffect, useMemo, useState } from "react";
import {
  fetchActiveConfiguration,
  fetchDiscoveredModelMetadata,
  simulateModelRouting,
  validateConfiguration,
} from "./api";
import type { ConfigurationDocument, DiscoveredMetadataState } from "./types";
import { ModelRoutingEditor } from "./ModelRoutingEditor";
import { ProviderDeploymentEditor } from "./ProviderDeploymentEditor";
import { VirtualModelEditor } from "./VirtualModelEditor";
import type { AdminConfigurationWorkspaceProps } from "./extensions";

const emptyDocument = `{
  "providers": [],
  "deployments": [],
  "virtual_models": []
}`;

interface Notice {
  kind: "success" | "error" | "info";
  text: string;
}

export function ConfigurationWorkspace({
  bootstrap,
  token,
  virtualModelExtensions,
}: AdminConfigurationWorkspaceProps) {
  const canRead = Boolean(bootstrap.features.config_read);
  const canValidate = Boolean(bootstrap.features.config_validate);
  const hasDiscoveredMetadata = Boolean(
    bootstrap.features.model_routing_discovered_metadata,
  );
  const [draft, setDraft] = useState(emptyDocument);
  const [currentFingerprint, setCurrentFingerprint] = useState(
    bootstrap.config_revision,
  );
  const [discoveredMetadata, setDiscoveredMetadata] =
    useState<DiscoveredMetadataState>();
  const [notice, setNotice] = useState<Notice>();
  const [busy, setBusy] = useState("");
  const [editorMode, setEditorMode] = useState<"structured" | "json">(
    "structured",
  );
  const [structuredSection, setStructuredSection] = useState<
    "models" | "routing" | "infrastructure"
  >("models");

  useEffect(() => {
    if (!canRead) return;
    let cancelled = false;
    const load = async () => {
      setBusy("load");
      try {
        const [current, discovered] = await Promise.all([
          fetchActiveConfiguration(token),
          hasDiscoveredMetadata
            ? fetchDiscoveredModelMetadata(token)
            : Promise.resolve(undefined),
        ]);
        if (cancelled) return;
        setDraft(JSON.stringify(current.document, null, 2));
        setCurrentFingerprint(current.revision);
        setDiscoveredMetadata(discovered);
      } catch (error) {
        if (!cancelled) setNotice(errorNotice(error));
      } finally {
        if (!cancelled) setBusy("");
      }
    };
    void load();
    return () => {
      cancelled = true;
    };
  }, [canRead, hasDiscoveredMetadata, token]);

  const parsedDocument = useMemo(() => {
    try {
      const value = JSON.parse(draft) as unknown;
      if (!value || Array.isArray(value) || typeof value !== "object") {
        return { error: "Configuration must be a JSON object." };
      }
      return { document: value as ConfigurationDocument };
    } catch (error) {
      return {
        error:
          error instanceof Error
            ? error.message
            : "Configuration is invalid JSON.",
      };
    }
  }, [draft]);

  const runValidation = async () => {
    if (!parsedDocument.document) {
      setNotice({ kind: "error", text: parsedDocument.error ?? "Invalid JSON" });
      return;
    }
    setBusy("validate");
    try {
      const result = await validateConfiguration(token, parsedDocument.document);
      setNotice({
        kind: "success",
        text: `Configuration is valid. Content fingerprint ${compactFingerprint(
          result.revision,
        )}.`,
      });
    } catch (error) {
      setNotice(errorNotice(error));
    } finally {
      setBusy("");
    }
  };

  const importFile = async (event: ChangeEvent<HTMLInputElement>) => {
    const file = event.target.files?.[0];
    if (!file) return;
    if (file.size > 8 * 1024 * 1024) {
      setNotice({ kind: "error", text: "Configuration file exceeds 8 MiB." });
      return;
    }
    setDraft(await file.text());
    setNotice({
      kind: "info",
      text: `Imported ${file.name}; validate before use.`,
    });
    event.target.value = "";
  };

  const formatDraft = () => {
    if (!parsedDocument.document) {
      setNotice({ kind: "error", text: parsedDocument.error ?? "Invalid JSON" });
      return;
    }
    setDraft(JSON.stringify(parsedDocument.document, null, 2));
  };

  const downloadDraft = () => {
    if (!parsedDocument.document) {
      setNotice({ kind: "error", text: parsedDocument.error ?? "Invalid JSON" });
      return;
    }
    const blob = new Blob(
      [JSON.stringify(parsedDocument.document, null, 2) + "\n"],
      { type: "application/json" },
    );
    const url = URL.createObjectURL(blob);
    const anchor = document.createElement("a");
    anchor.href = url;
    anchor.download = "sparkroute-config.json";
    anchor.click();
    URL.revokeObjectURL(url);
  };

  const changeStructuredDocument = (document: ConfigurationDocument) => {
    setDraft(JSON.stringify(document, null, 2));
    setNotice({
      kind: "info",
      text: "Structured draft changed. Validate before using the exported file.",
    });
  };

  return (
    <div className="config-workspace">
      <section className="panel editor-panel">
        <div className="panel-heading">
          <div>
            <p className="eyebrow">Configuration document</p>
            <h2>
              {canRead ? "Current configuration" : "Local validation draft"}
            </h2>
          </div>
          <span className="activity-summary">
            Current {compactFingerprint(currentFingerprint)}
          </span>
        </div>
        <div className="editor-toolbar">
          <div
            className="editor-mode-switch"
            aria-label="Configuration editor mode"
            role="group"
          >
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
          <label className="file-button">
            Import JSON
            <input
              accept="application/json,.json"
              disabled={Boolean(busy)}
              onChange={importFile}
              type="file"
            />
          </label>
          <button
            className="text-button"
            disabled={Boolean(busy)}
            onClick={formatDraft}
            type="button"
          >
            Format
          </button>
          <button
            className="text-button"
            disabled={Boolean(busy)}
            onClick={downloadDraft}
            type="button"
          >
            Export
          </button>
          {canValidate ? (
            <button
              className="secondary-button editor-action"
              disabled={Boolean(busy)}
              onClick={() => void runValidation()}
              type="button"
            >
              {busy === "validate" ? "Validating…" : "Validate"}
            </button>
          ) : null}
        </div>
        {editorMode === "structured" ? (
          parsedDocument.document ? (
            <>
              <div
                className="structured-surface-tabs"
                aria-label="Structured configuration section"
                role="group"
              >
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
                  className={
                    structuredSection === "infrastructure" ? "active" : ""
                  }
                  onClick={() => setStructuredSection("infrastructure")}
                  type="button"
                >
                  Providers &amp; deployments
                </button>
              </div>
              {structuredSection === "models" ? (
                <VirtualModelEditor
                  disabled={Boolean(busy)}
                  document={parsedDocument.document}
                  extensions={virtualModelExtensions}
                  onChange={changeStructuredDocument}
                />
              ) : structuredSection === "routing" ? (
                <ModelRoutingEditor
                  discoveredMetadata={discoveredMetadata}
                  disabled={Boolean(busy)}
                  document={parsedDocument.document}
                  onChange={changeStructuredDocument}
                  simulate={
                    bootstrap.features.config_routing_simulation
                      ? (
                          document,
                          requestedModel,
                          routingText,
                          requiredCapabilities,
                        ) =>
                          simulateModelRouting(
                            token,
                            document,
                            requestedModel,
                            routingText,
                            requiredCapabilities,
                          )
                      : undefined
                  }
                />
              ) : (
                <ProviderDeploymentEditor
                  subscriptionAuth={{ token, enabled: Boolean(bootstrap.features.provider_auth) }}
                  disabled={Boolean(busy)}
                  document={parsedDocument.document}
                  onChange={changeStructuredDocument}
                />
              )}
            </>
          ) : (
            <div className="structured-unavailable">
              <strong>Structured editor unavailable</strong>
              <p>
                {parsedDocument.error} Switch to JSON to repair the document.
              </p>
            </div>
          )
        ) : (
          <>
            <label className="sr-only" htmlFor="configuration-document">
              Gateway configuration JSON
            </label>
            <textarea
              aria-invalid={Boolean(parsedDocument.error)}
              className="config-editor"
              disabled={Boolean(busy)}
              id="configuration-document"
              onChange={(event) => setDraft(event.target.value)}
              spellCheck={false}
              value={draft}
            />
          </>
        )}
        {notice ? <NoticeBanner notice={notice} /> : null}
        <p className="read-only-note">
          This workspace shows the current configuration. Use the managed
          operator and Sparkrun sources when managed configuration is enabled;
          otherwise import, validate, and export a local draft.
        </p>
      </section>
    </div>
  );
}

function NoticeBanner({ notice }: { notice: Notice }) {
  return <div className={`notice ${notice.kind}`}>{notice.text}</div>;
}

function errorNotice(error: unknown): Notice {
  return {
    kind: "error",
    text:
      error instanceof Error ? error.message : "Configuration request failed.",
  };
}

function compactFingerprint(fingerprint: string) {
  if (!fingerprint) return "unknown";
  return fingerprint.length > 16
    ? `${fingerprint.slice(0, 12)}…`
    : fingerprint;
}
