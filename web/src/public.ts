import { createElement } from "react";
import { createRoot } from "react-dom/client";
import { AdminApp } from "./App";
import type { AdminAppOptions } from "./types";

export type {
  AdminAppOptions,
  AdminBootstrap,
  AdminEdition,
  AttemptFilters,
  AttemptPage,
  AttemptRecord,
  ConfigurationDocument,
  ConfigurationValidation,
  DiscoveredMetadataState,
  RoutingSimulationResult,
  CredentialResolutionStatus,
  CredentialSourceStatus,
  CredentialStatus,
  ClientCredential,
  ClientCredentialAuditEvent,
  ClientCredentialCreateInput,
  ClientCredentialFilters,
  ClientCredentialState,
  IssuedClientCredential,
  GatewayStatus,
  MMProjectionStatus,
  ManagedConfigurationCurrent,
  ManagedConfigurationOwner,
  ManagedConfigurationReplaceResult,
  ManagedConfigurationSet,
  ManagedConfigurationSetMetadata,
  ManagedConfigurationValidation,
  RequestFilters,
  RequestPage,
  RequestRecord,
  PrivacyStatus,
  SavedTraceExportFilters,
  TargetStatus,
  TokenUsage,
} from "./types";
export type {
  AdminConsoleExtensions,
  AdminConfigurationWorkspaceProps,
  AdminExtensionContext,
  AdminOverviewExtension,
  AdminPageExtension,
  JSONObject,
  VirtualModelEditorContext,
  VirtualModelEditorExtension,
} from "./extensions";
export { AdminApp } from "./App";
export { ModelRoutingEditor } from "./ModelRoutingEditor";
export { ProviderDeploymentEditor } from "./ProviderDeploymentEditor";
export { PolicyProfilesEditor } from "./PolicyProfilesEditor";
export { VirtualModelEditor } from "./VirtualModelEditor";

export function mountAdminApp(element: HTMLElement, options: AdminAppOptions) {
  createRoot(element).render(createElement(AdminApp, options));
}
