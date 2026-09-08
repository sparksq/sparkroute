import type { ConfigurationDocument } from "./types";

export function deploymentTitle(deployment: Record<string, unknown>): string {
  if (typeof deployment.title === "string" && deployment.title.trim()) return deployment.title;
  const name = typeof deployment.name === "string" ? deployment.name : "";
  if (!name.startsWith("sparkrun:")) return name;
  const source = deployment.endpoint_source as Record<string, unknown> | undefined;
  const clusters = Array.isArray(source?.cluster_candidates) ? source.cluster_candidates.filter((value) => typeof value === "string") : [];
  return `sparkrun:${clusters.join(",") || (source?.type === "activatable" ? "unassigned" : "discovered")}:${deployment.model || name}`;
}

export function deploymentChoices(...documents: (ConfigurationDocument | undefined)[]): Record<string, string> {
  const deployments = documents.flatMap((document) => Array.isArray(document?.deployments) ? document.deployments : [])
    .filter((deployment) => deployment && typeof deployment === "object" && typeof deployment.name === "string");
  const titles = deployments.map(deploymentTitle);
  const counts = new Map<string, number>();
  titles.forEach((title) => counts.set(title, (counts.get(title) ?? 0) + 1));
  return Object.fromEntries(deployments.map((deployment, index) => [deployment.name,
    counts.get(titles[index]!)! > 1 ? `${titles[index]} (${deployment.name})` : titles[index],
  ]));
}

// Generated titles already include cluster names from discovery/runtime status.
// Match the whole model suffix because model names themselves may contain colons.
export function sparkrunDeploymentClusters(deployment: Record<string, unknown>): string[] {
  const title = typeof deployment.title === "string" ? deployment.title : "";
  const model = typeof deployment.model === "string" ? deployment.model : "";
  if (model && title.startsWith("sparkrun:") && title.endsWith(`:${model}`)) {
    const reported = title.slice("sparkrun:".length, -(model.length + 1));
    if (reported && !["unassigned", "discovered"].includes(reported)) return reported.split(",").map((name) => name.trim()).filter(Boolean);
  }
  const source = deployment.endpoint_source as Record<string, unknown> | undefined;
  return Array.isArray(source?.cluster_candidates) ? source.cluster_candidates.filter((value): value is string => typeof value === "string" && Boolean(value.trim())) : [];
}
