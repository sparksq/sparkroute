import type { ConfigurationDocument } from "./types";
import { routingModelUses } from "./modelRoutingReferences";

type ObjectValue = Record<string, unknown>;
const object = (value: unknown): ObjectValue => value && typeof value === "object" && !Array.isArray(value) ? value as ObjectValue : {};
const entries = (value: unknown): ObjectValue[] => Array.isArray(value) ? value.map(object) : [];
const name = (value: unknown) => typeof value === "string" ? value : "";

export function excludedSparkrunDeployments(document: ConfigurationDocument): string[] {
  const value = object(document.sparkrun_overrides).excluded_deployments;
  return Array.isArray(value) ? value.filter((entry): entry is string => typeof entry === "string") : [];
}

export function setSparkrunExclusions(document: ConfigurationDocument, names: string[]): ConfigurationDocument {
  const overrides = {...object(document.sparkrun_overrides)};
  if (names.length) overrides.excluded_deployments = [...new Set(names)].sort();
  else delete overrides.excluded_deployments;
  const next = {...document};
  if (Object.keys(overrides).length) next.sparkrun_overrides = overrides;
  else delete next.sparkrun_overrides;
  return next;
}

// Mirror managed.ApplySparkrunOverrides for the shared configuration draft.
// Source fragments remain unchanged; the server validates the effective merge.
export function visibleSparkrunDocument(document: ConfigurationDocument, generated?: ConfigurationDocument): ConfigurationDocument | undefined {
  if (!generated) return generated;
  const excluded = new Set(excludedSparkrunDeployments(document));
  if (!excluded.size) return generated;
  return {...generated,
    deployments: entries(generated.deployments).filter(deployment => !excluded.has(name(deployment.name))),
    virtual_models: entries(generated.virtual_models).flatMap(model => {
      let changed = false;
      const pools = entries(model.pools).flatMap(pool => {
        const current = entries(pool.targets);
        const targets = current.filter(target => !excluded.has(name(target.deployment)));
        if (targets.length !== current.length) changed = true;
        return targets.length || !current.length ? [{...pool, targets}] : [];
      });
      return changed && !pools.length ? [] : [{...model, pools}];
    }),
  };
}

export function modelDeploymentNames(model: ObjectValue): string[] {
  return [...new Set(entries(model.pools).flatMap(pool => entries(pool.targets).map(target => name(target.deployment))).filter(Boolean))];
}

export function sparkrunRemovalPreview(document: ConfigurationDocument, generated: ConfigurationDocument, deployments: string[]) {
  const next = setSparkrunExclusions(document, [...excludedSparkrunDeployments(document), ...deployments]);
  const before = visibleSparkrunDocument(document, generated)!;
  const after = visibleSparkrunDocument(next, generated)!;
  const remaining = new Set(entries(after.virtual_models).map(model => name(model.name)));
  const removed = entries(before.virtual_models).filter(model => !remaining.has(name(model.name)));
  const affected = entries(before.virtual_models).filter(model => modelDeploymentNames(model).some(target => deployments.includes(target)));
  const blockers: string[] = [];
  for (const model of entries(document.virtual_models)) {
    if (modelDeploymentNames(model).some(target => deployments.includes(target))) blockers.push(`Virtual Models / Aliases: ${name(model.name)} uses this deployment`);
  }
  const policy = object(document.model_routing), uses = routingModelUses(policy);
  for (const model of removed) {
    const modelName = name(model.name);
    if (uses[modelName]?.length) blockers.push(`Model Routing: ${modelName} — ${uses[modelName]!.join("; ")}`);
    else if (Object.hasOwn(object(policy.models), modelName)) blockers.push(`Model Routing: remove unused preferences for ${modelName}`);
  }
  // Full Validate checks effective guardrails after resolving policy assignment
  // and inheritance; unused profile definitions must not block exclusion.
  return {document: next, removed: removed.map(model => name(model.name)), affected: affected.map(model => name(model.name)), blockers: [...new Set(blockers)]};
}
