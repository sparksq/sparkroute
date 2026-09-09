// SPDX-FileCopyrightText: 2026 Scitrera LLC
// SPDX-FileCopyrightText: 2026 Fox Engine Ltd.
// SPDX-License-Identifier: AGPL-3.0-only

import { piiVirtualModelExtension } from "./PIIEditor";
import {
  type ComponentType,
  type FormEvent,
  type ReactNode,
  useCallback,
  useEffect,
  useRef,
  useState,
} from "react";
import { AdminAPIError, fetchBootstrap, fetchStatus, probeMMProjection } from "./api";
import { ConfigurationWorkspace } from "./ConfigurationWorkspace";
import { ManagedConfigurationWorkspace } from "./ManagedConfigurationWorkspace";
import { ConfigurationPresetSelector } from "./ConfigurationPresetSelector";
import { configurationSection, configurationSections } from "./configurationSections";
import { TraceExportWorkspace } from "./TraceExportWorkspace";
import { TrafficWorkspace } from "./TrafficWorkspace";
import { OverviewWorkspace } from "./OverviewWorkspace";
import { TargetFact } from "./TargetHealthDetail";
import { CredentialsWorkspace } from "./CredentialsWorkspace";
import type {
  AdminConfigurationWorkspaceProps,
  AdminConsoleExtensions,
  AdminExtensionContext,
} from "./extensions";
import type {
  AdminAppOptions,
  AdminBootstrap,
  GatewayStatus,
  MMProjectionStatus,
} from "./types";
import "./styles.css";

type ConnectionState =
  | { phase: "loading" }
  | { phase: "authentication"; message?: string }
  | { phase: "error"; message: string }
  | {
      phase: "ready";
      bootstrap: AdminBootstrap;
      status?: GatewayStatus;
    };

type BuiltInConsolePage = "overview" | "configuration" | "credentials" | "traffic" | "traces";
type ConsolePage = BuiltInConsolePage | string;

export function AdminApp({
  productName,
  extensions,
}: AdminAppOptions) {
  const [connection, setConnection] = useState<ConnectionState>({
    phase: "loading",
  });
  const [token, setToken] = useState("");
  const tokenRef = useRef("");

  const connect = useCallback(
    async (credential: string, signal?: AbortSignal) => {
      tokenRef.current = credential;
      setConnection({ phase: "loading" });
      try {
        const bootstrap = await fetchBootstrap(credential, signal);
        if (bootstrap.schema_version !== 1) {
          throw new Error(
            `Unsupported admin API schema ${bootstrap.schema_version}`,
          );
        }
        const status = bootstrap.features.status
          ? await fetchStatus(credential, signal)
          : undefined;
        setConnection({ phase: "ready", bootstrap, status });
      } catch (error) {
        if (signal?.aborted) return;
        if (error instanceof AdminAPIError && error.status === 401) {
          setConnection({
            phase: "authentication",
            message: credential ? error.message : undefined,
          });
          return;
        }
        setConnection({
          phase: "error",
          message: error instanceof Error ? error.message : "Admin request failed",
        });
      }
    },
    [],
  );

  useEffect(() => {
    const controller = new AbortController();
    void connect("", controller.signal);
    return () => controller.abort();
  }, [connect]);

  const submitToken = (event: FormEvent<HTMLFormElement>) => {
    event.preventDefault();
    void connect(token.trim());
  };

  if (connection.phase === "loading") {
    return <LoadingScreen productName={productName} />;
  }
  if (connection.phase === "authentication") {
    return (
      <AuthenticationScreen
        message={connection.message}
        productName={productName}
        token={token}
        onTokenChange={setToken}
        onSubmit={submitToken}
      />
    );
  }
  if (connection.phase === "error") {
    return (
      <ErrorScreen
        message={connection.message}
        productName={productName}
        onRetry={() => void connect(tokenRef.current)}
      />
    );
  }
  return (
    <ConsoleLayout
      connection={connection}
      extensions={extensions}
      productName={productName}
      token={tokenRef.current}
      onRefresh={() => void connect(tokenRef.current)}
    />
  );
}

function LoadingScreen({ productName }: { productName: string }) {
  return (
    <main className="gate-screen" aria-busy="true">
      <BrandMark />
      <p className="eyebrow">{productName}</p>
      <h1>Opening the control plane</h1>
      <div className="loading-track" aria-label="Loading admin console">
        <span />
      </div>
    </main>
  );
}

interface AuthenticationScreenProps {
  productName: string;
  token: string;
  message?: string;
  onTokenChange: (value: string) => void;
  onSubmit: (event: FormEvent<HTMLFormElement>) => void;
}

function AuthenticationScreen({
  productName,
  token,
  message,
  onTokenChange,
  onSubmit,
}: AuthenticationScreenProps) {
  return (
    <main className="gate-screen">
      <BrandMark />
      <p className="eyebrow">{productName}</p>
      <h1>Connect to admin</h1>
      <p className="gate-copy">
        Enter an admin bearer token. It stays in memory for this page and is
        never written to browser storage.
      </p>
      <form className="auth-form" onSubmit={onSubmit}>
        <label htmlFor="admin-token">Bearer token</label>
        <input
          id="admin-token"
          autoComplete="off"
          autoFocus
          onChange={(event) => onTokenChange(event.target.value)}
          placeholder="Paste token"
          type="password"
          value={token}
        />
        {message ? <p className="form-error">{message}</p> : null}
        <button type="submit">Connect securely</button>
      </form>
      <p className="gate-footnote">
        mTLS administrators connect automatically when the browser presents an
        accepted client certificate.
      </p>
    </main>
  );
}

function ErrorScreen({
  productName,
  message,
  onRetry,
}: {
  productName: string;
  message: string;
  onRetry: () => void;
}) {
  return (
    <main className="gate-screen">
      <BrandMark />
      <p className="eyebrow">{productName}</p>
      <h1>Console unavailable</h1>
      <p className="gate-copy">{message}</p>
      <button className="primary-button" onClick={onRetry} type="button">
        Try again
      </button>
    </main>
  );
}

function ConsoleLayout({
  connection,
  extensions,
  productName,
  token,
  onRefresh,
}: {
  connection: Extract<ConnectionState, { phase: "ready" }>;
  extensions?: AdminConsoleExtensions;
  productName: string;
  token: string;
  onRefresh: () => void;
}) {
  const ConfigurationComponent: ComponentType<AdminConfigurationWorkspaceProps> =
    extensions?.configuration ?? ConfigurationWorkspace;
  const { bootstrap, status } = connection;
  const [operatorDirty, setOperatorDirty] = useState(false);
  const [operatorBusy, setOperatorBusy] = useState(false);
  const [presetBusy, setPresetBusy] = useState(false);
  const [configurationReload, setConfigurationReload] = useState(0);
  const [overviewRefresh, setOverviewRefresh] = useState(0);
  const [presetsRefresh, setPresetsRefresh] = useState(0);
  const extensionContext: AdminExtensionContext = {
    bootstrap,
    status,
    token,
    refresh: onRefresh,
  };
  const pageExtensions = (extensions?.pages ?? []).filter(
    (extension) => !extension.available || extension.available(extensionContext),
  );
  const overviewExtensions = (extensions?.overview ?? []).filter(
    (extension) => !extension.available || extension.available(extensionContext),
  );
  const virtualModelExtensions = [piiVirtualModelExtension, ...(extensions?.virtualModelEditor ?? [])].filter(
    (extension) => !extension.available || extension.available(bootstrap),
  );
  const [page, setPage] = useState<ConsolePage>(
    pageFromPath(window.location.pathname, pageExtensions),
  );
  const section = configurationSection(page);
  const lastConfigurationSection = useRef(section ?? "providers");
  if (section) lastConfigurationSection.current = section;
  const [configurationVisited, setConfigurationVisited] = useState(Boolean(section));
  useEffect(() => {
    if (section) setConfigurationVisited(true);
  }, [section]);
  const configurationAvailable = Boolean(
    bootstrap.features.config_read ||
      bootstrap.features.config_validate ||
      bootstrap.features.config_managed_sets,
  );
  const trafficAvailable = Boolean(bootstrap.features.ledger_query);
  const runtimeAvailable = Boolean(
    bootstrap.features.lifecycle ||
      bootstrap.features.endpoint_inventory ||
      bootstrap.features.lifecycle_status ||
      bootstrap.features.runtime_events,
  );
  const traceExportAvailable = Boolean(bootstrap.features.saved_trace_export);
  const credentialsAvailable = Boolean(bootstrap.features.client_credentials);
  useEffect(() => {
    const update = () => {
      if (window.location.pathname === "/admin/runtime") window.history.replaceState({}, "", "/admin/");
      setPage(pageFromPath(window.location.pathname, pageExtensions));
    };
    if (window.location.pathname === "/admin/runtime") update();
    window.addEventListener("popstate", update);
    return () => window.removeEventListener("popstate", update);
  }, [pageExtensions]);
  const navigate = (next: ConsolePage) => {
    const target = ({
      overview: "/admin/",
      configuration: "/admin/configuration",
      credentials: "/admin/credentials",
      traffic: "/admin/traffic",
      traces: "/admin/traces",
    } satisfies Record<BuiltInConsolePage, string>)[next as BuiltInConsolePage]
      ?? (configurationSection(next) ? `/admin/${next}` : undefined)
      ?? pageExtensions.find((extension) => extension.id === next)?.path
      ?? "/admin/";
    window.history.pushState({}, "", target);
    setPage(next);
  };
  const heading = (section && (bootstrap.features.config_managed_sets || section === "advanced" || section === "privacy" || section === "guardrails")
    ? configurationSections.find((entry) => entry.id === section)?.label : undefined) ?? ({
    overview: "Gateway overview",
    configuration: "Configuration",
    credentials: "Client credentials",
    traffic: "Traffic history",
    traces: "Saved trace export",
  } satisfies Record<BuiltInConsolePage, string>)[page as BuiltInConsolePage]
    ?? pageExtensions.find((extension) => extension.id === page)?.heading
    ?? "Administration";
  const activePageExtension = pageExtensions.find((extension) => extension.id === page);
  return (
    <div className={bootstrap.features.config_managed_sets ? "console-shell managed-console" : "console-shell"}>
      <aside className="sidebar">
        <div className="sidebar-brand">
          <BrandMark />
          <div>
            <strong>{productName}</strong>
          </div>
        </div>
        <nav aria-label="Admin sections">
          <a
            aria-current={page === "overview" ? "page" : undefined}
            className={page === "overview" ? "nav-item active" : "nav-item"}
            href="/admin/"
            onClick={(event) => {
              event.preventDefault();
              navigate("overview");
            }}
          >
            <span className="nav-glyph">O</span>
            Overview
          </a>
          {configurationAvailable ? <div className="configuration-nav">
            <a
              aria-current={section && !bootstrap.features.config_managed_sets ? "page" : undefined}
              className={section ? "nav-item active" : "nav-item"}
              href="/admin/configuration"
              onClick={(event) => {
                event.preventDefault();
                navigate("configuration");
              }}
            >
              <span className="nav-glyph">C</span>
              Configuration
            </a>
            {(
              <ul className="configuration-nav-children" aria-label="Configuration sections">
                {configurationSections.filter((entry) => bootstrap.features.config_managed_sets || entry.id === "advanced" || entry.id === "privacy" || entry.id === "guardrails").map((entry) => (
                  <li key={entry.id}>
                    <a
                      aria-current={section === entry.id ? "page" : undefined}
                      className={section === entry.id ? "nav-item active" : "nav-item"}
                      href={`/admin/configuration/${entry.id}`}
                      onClick={(event) => { event.preventDefault(); navigate(`configuration/${entry.id}`); }}
                    >{entry.label}</a>
                  </li>
                ))}
              </ul>
            )}
          </div> : null}
          {credentialsAvailable ? (
            <a
              aria-current={page === "credentials" ? "page" : undefined}
              className={page === "credentials" ? "nav-item active" : "nav-item"}
              href="/admin/credentials"
              onClick={(event) => { event.preventDefault(); navigate("credentials"); }}
            >
              <span className="nav-glyph">K</span>
              Credentials
            </a>
          ) : null}
          {trafficAvailable ? (
            <a
              aria-current={page === "traffic" ? "page" : undefined}
              className={page === "traffic" ? "nav-item active" : "nav-item"}
              href="/admin/traffic"
              onClick={(event) => {
                event.preventDefault();
                navigate("traffic");
              }}
            >
              <span className="nav-glyph">T</span>
              Traffic
            </a>
          ) : null}
          {traceExportAvailable ? (
            <a
              aria-current={page === "traces" ? "page" : undefined}
              className={page === "traces" ? "nav-item active" : "nav-item"}
              href="/admin/traces"
              onClick={(event) => {
                event.preventDefault();
                navigate("traces");
              }}
            >
              <span className="nav-glyph">E</span>
              Trace export
            </a>
          ) : null}
          {pageExtensions.map((extension) => (
            <a
              aria-current={page === extension.id ? "page" : undefined}
              className={page === extension.id ? "nav-item active" : "nav-item"}
              href={extension.path}
              key={extension.id}
              onClick={(event) => {
                event.preventDefault();
                navigate(extension.id);
              }}
            >
              <span className="nav-glyph">{extension.glyph}</span>
              {extension.label}
            </a>
          ))}
        </nav>
        <div className="sidebar-footer">
          <span>Gateway</span>
          <strong>{bootstrap.gateway_version || "development"}</strong>
          <a href="/admin/legal.html" target="_blank" rel="noreferrer">License &amp; notices</a>
          {bootstrap.build && (
            <a href={bootstrap.build.source} target="_blank" rel="noreferrer">
              {bootstrap.build.license} · Source {bootstrap.build.commit.slice(0, 12)}
            </a>
          )}
        </div>
      </aside>

      <main className="workspace" id={page}>
        <header className="topbar">
          <div>
            <p className="eyebrow">Control plane</p>
            <h1>{heading}</h1>
          </div>
          <div className="topbar-actions">
            {bootstrap.features.config_presets ? <ConfigurationPresetSelector token={token} canWrite={Boolean(bootstrap.features.config_write)}
              dirty={operatorDirty} locked={operatorBusy} refreshKey={`${bootstrap.config_revision}:${presetsRefresh}`}
              onBusyChange={setPresetBusy} onApplied={() => setConfigurationReload(value => value + 1)} /> : <Revision revision={bootstrap.config_revision} />}
            <button className="secondary-button" onClick={page === "overview" ? () => setOverviewRefresh(value => value + 1) : onRefresh} type="button">
              Refresh
            </button>
          </div>
        </header>

        {configurationAvailable && bootstrap.features.config_managed_sets && configurationVisited ? (
          <div hidden={!section}>
            <ManagedConfigurationWorkspace bootstrap={bootstrap} token={token} runtimeTargets={status?.targets} virtualModelExtensions={virtualModelExtensions} section={lastConfigurationSection.current}
              reloadKey={configurationReload} externalBusy={presetBusy} onDirtyChange={setOperatorDirty} onBusyChange={setOperatorBusy} onSaved={() => setPresetsRefresh(value => value + 1)} />
          </div>
        ) : null}
        {section === "advanced" ? (
          configurationAvailable && bootstrap.features.config_managed_sets ? null :
          status ? <MMProjectionStatusPanel
            canProbe={Boolean(bootstrap.features.mm_projection_probe)}
            initialStatus={status.mm_projection ?? { configured: false, state: "disabled", models: 0, consecutive_failures: 0, circuit_open: false }}
            token={token}
          /> : <PermissionNotice />
        ) : section ? (
          configurationAvailable ? (
            bootstrap.features.config_managed_sets ? null : (
              <ConfigurationComponent
                section={section}
                bootstrap={bootstrap}
                token={token}
                virtualModelExtensions={virtualModelExtensions}
              />
            )
          ) : <ModuleUnavailable />
        ) : page === "credentials" ? (
          credentialsAvailable ? <CredentialsWorkspace bootstrap={bootstrap} token={token} /> : <ModuleUnavailable />
        ) : page === "traffic" ? (
          trafficAvailable ? <TrafficWorkspace bootstrap={bootstrap} token={token} /> : <ModuleUnavailable />
        ) : page === "traces" ? (
          traceExportAvailable ? <TraceExportWorkspace bootstrap={bootstrap} token={token} /> : <ModuleUnavailable />
        ) : activePageExtension ? (
          <activePageExtension.Component {...extensionContext} />
        ) : (
          <>
            {status || runtimeAvailable ? (
              <OverviewWorkspace bootstrap={bootstrap} token={token} initialStatus={status} refreshKey={configurationReload + overviewRefresh} />
            ) : <PermissionNotice />}
            {overviewExtensions.map((extension) => (
              <extension.Component {...extensionContext} key={extension.id} />
            ))}
          </>
        )}
      </main>
    </div>
  );
}

function pageFromPath(
  pathname: string,
  extensions: NonNullable<AdminConsoleExtensions["pages"]> = [],
): ConsolePage {
  if (pathname === "/admin/configuration") return "configuration";
  const configurationPage = pathname.replace(/^\/admin\//, "");
  if (configurationSection(configurationPage)) return configurationPage;
  if (pathname === "/admin/credentials") return "credentials";
  if (pathname === "/admin/traffic") return "traffic";
  if (pathname === "/admin/runtime") return "overview";
  if (pathname === "/admin/traces") return "traces";
  const extension = extensions.find((candidate) => candidate.path === pathname);
  if (extension) return extension.id;
  return "overview";
}

function MMProjectionStatusPanel({ canProbe, initialStatus, token }: {
  canProbe: boolean;
  initialStatus: MMProjectionStatus;
  token: string;
}) {
  const [status, setStatus] = useState(initialStatus);
  const [probing, setProbing] = useState(false);
  const [error, setError] = useState("");
  useEffect(() => setStatus(initialStatus), [initialStatus]);
  const probe = async () => {
    setProbing(true);
    setError("");
    try {
      setStatus(await probeMMProjection(token));
    } catch (value) {
      setError(value instanceof Error ? value.message : "Projection probe failed");
    } finally {
      setProbing(false);
    }
  };
  const good = status.state === "healthy" || status.state === "disabled";
  return (
    <section className="panel projection-status-panel">
      <div className="panel-heading">
        <div>
          <p className="eyebrow">Multimedia request transform</p>
          <h2>MMBridge projection</h2>
        </div>
        <div className="panel-actions">
          <StatusPill good={good}>{humanize(status.state)}</StatusPill>
          {canProbe ? (
            <button className="secondary-button" disabled={probing} onClick={() => void probe()} type="button">
              {probing ? "Testing…" : "Test bridge"}
            </button>
          ) : null}
        </div>
      </div>
      {!status.configured ? (
        <p>Projection execution is not configured for this gateway process.</p>
      ) : (
        <div className="projection-status-facts">
          <TargetFact label="Provider" value={status.provider || "Unknown"} />
          <TargetFact label="Analyzer model" value={status.analyzer_model || "Policy selected"} />
          <TargetFact label="Projection API" value={status.projection_api ? `v${status.projection_api}` : "Not probed"} />
          <TargetFact label="Discovered models" value={status.models > 0 ? String(status.models) : "Not probed"} />
          <TargetFact label="Last attempt" value={status.last_attempt_at ? formatStatusTime(status.last_attempt_at) : "Not observed"} />
          <TargetFact label="Last success" value={status.last_success_at ? formatStatusTime(status.last_success_at) : "Not observed"} />
          <TargetFact label="Last HTTP status" value={status.last_http_status ? String(status.last_http_status) : "Not observed"} />
          <TargetFact label="Last latency" value={status.last_latency_ms === undefined ? "Not observed" : `${status.last_latency_ms} ms`} />
          <TargetFact label="Failures" value={String(status.consecutive_failures)} />
          <TargetFact label="Circuit" value={status.circuit_open ? `Open${status.circuit_open_until ? ` until ${formatStatusTime(status.circuit_open_until)}` : ""}` : "Closed"} />
        </div>
      )}
      {status.last_error ? <p className="status-note">Last result: {humanize(status.last_error)}</p> : null}
      {error ? <div className="notice error">{error}</div> : null}
      <p className="status-footnote">
        This projection contains no bridge URL, credentials, discovered model names, payloads, or raw transport errors.
      </p>
    </section>
  );
}


function StatusPill({
  children,
  good,
}: {
  children: ReactNode;
  good: boolean;
}) {
  return (
    <span className={good ? "status-pill good" : "status-pill warning"}>
      <i />
      {children}
    </span>
  );
}

function Revision({ revision }: { revision: string }) {
  const compact = revision.length > 14 ? `${revision.slice(0, 12)}…` : revision;
  return (
    <div className="revision" title={revision}>
      <span>Config</span>
      <code>{compact}</code>
    </div>
  );
}

function PermissionNotice() {
  return (
    <section className="panel permission-notice">
      <h2>Status access is not enabled</h2>
      <p>
        This identity can open the console but cannot read live gateway status.
      </p>
    </section>
  );
}

function ModuleUnavailable() {
  return (
    <section className="panel permission-notice">
      <h2>This module is not enabled</h2>
      <p>The gateway did not advertise this capability for the current identity.</p>
    </section>
  );
}

function BrandMark() {
  return (
    <span aria-hidden="true" className="brand-mark">
      <i />
      <i />
      <i />
    </span>
  );
}

function humanize(value: string) {
  const labels: Record<string, string> = {
    ipv4: "IPv4",
    postgres: "PostgreSQL",
    sqlite: "SQLite",
    ssn: "SSN",
  };
  if (labels[value]) return labels[value];
  return value
    .split("_")
    .filter(Boolean)
    .map((part) => part[0]?.toUpperCase() + part.slice(1))
    .join(" ");
}

function formatStatusTime(value: string) {
  const parsed = new Date(value);
  if (Number.isNaN(parsed.getTime())) return value;
  return new Intl.DateTimeFormat(undefined, {
    dateStyle: "medium",
    timeStyle: "medium",
  }).format(parsed);
}
