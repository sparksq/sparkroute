import type { ComponentType } from "react";
import type { AdminBootstrap, GatewayStatus } from "./types";

export type JSONObject = Record<string, unknown>;

export interface AdminExtensionContext {
  bootstrap: AdminBootstrap;
  status?: GatewayStatus;
  token: string;
  refresh: () => void;
}

export interface AdminConfigurationWorkspaceProps {
  bootstrap: AdminBootstrap;
  token: string;
  virtualModelExtensions?: VirtualModelEditorExtension[];
  section?: import("./configurationSections").ConfigurationSection;
}

export interface AdminOverviewExtension {
  id: string;
  available?: (context: AdminExtensionContext) => boolean;
  Component: ComponentType<AdminExtensionContext>;
}

export interface AdminPageExtension {
  id: string;
  path: `/admin/${string}`;
  label: string;
  glyph: string;
  heading: string;
  available?: (context: AdminExtensionContext) => boolean;
  Component: ComponentType<AdminExtensionContext>;
}

export interface VirtualModelEditorContext {
  model: JSONObject;
  disabled: boolean;
  updateModel: (transform: (model: JSONObject) => JSONObject) => void;
}

export interface VirtualModelEditorExtension {
  id: string;
  available?: (bootstrap: AdminBootstrap) => boolean;
  Component: ComponentType<VirtualModelEditorContext>;
  validateModel?: (model: JSONObject, index: number) => string | undefined;
}

export interface AdminConsoleExtensions {
	  configuration?: ComponentType<AdminConfigurationWorkspaceProps>;
  overview?: AdminOverviewExtension[];
  pages?: AdminPageExtension[];
  virtualModelEditor?: VirtualModelEditorExtension[];
}
