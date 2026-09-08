export const configurationSections = [
  { id: "providers", label: "Providers" },
  { id: "deployments", label: "Model Deployments" },
  { id: "models", label: "Virtual Models / Aliases" },
  { id: "routing", label: "Model Routing" },
  { id: "privacy", label: "PII Privacy" },
  { id: "guardrails", label: "Guardrails" },
  { id: "advanced", label: "Advanced Options" },
] as const;

export type ConfigurationSection = typeof configurationSections[number]["id"];

export function configurationSection(page: string): ConfigurationSection | undefined {
  if (page === "configuration") return "providers";
  if (page === "configuration/sparkrun") return "deployments";
  return configurationSections.find((section) => page === `configuration/${section.id}`)?.id;
}
