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
