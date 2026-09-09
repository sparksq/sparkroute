type ObjectValue = Record<string, unknown>;
const object = (value: unknown): ObjectValue => value && typeof value === "object" && !Array.isArray(value) ? value as ObjectValue : {};
const strings = (value: unknown): string[] => Array.isArray(value) ? value.filter((item): item is string => typeof item === "string") : [];

// Selection references are separate from optional routing preferences. Keeping
// this inventory independent of discovery makes vanished models repairable.
export function routingModelUses(value: unknown): Record<string, string[]> {
  const policy = object(value);
  const uses: Record<string, string[]> = Object.create(null);
  function add(name: unknown, location: string) {
    if (typeof name !== "string" || !name) return;
    const locations = uses[name] ?? [];
    if (!locations.includes(location)) locations.push(location);
    uses[name] = locations;
  }
  for (const [name, raw] of Object.entries(object(policy.virtual_models))) {
    const selector = object(raw), stage = object(selector.stage_router);
    strings(selector.models).forEach((model) => add(model, `${name} · candidates`));
    add(stage.capable_model, `${name} · capable model`);
    add(stage.efficient_model, `${name} · efficient model`);
    for (const [preset, kwargs] of Object.entries(object(selector.kwargs))) {
      strings(object(kwargs).models).forEach((model) => add(model, `${name}:${preset} · preset`));
    }
  }
  if (Array.isArray(policy.keyword_rules)) policy.keyword_rules.forEach((raw, index) => {
    const rule = object(raw);
    strings(rule.models).forEach((model) => add(model, `Keyword rule ${String(rule.name || index + 1)}`));
  });
  return uses;
}

export function missingRoutingModels(value: unknown, canonicalModels: string[]) {
  const policy = object(value), uses = routingModelUses(policy);
  const known = new Set(canonicalModels);
  return [...new Set([...Object.keys(object(policy.models)), ...Object.keys(uses)])]
    .filter((name) => !known.has(name)).sort()
    .map((name) => ({ name, locations: uses[name] ?? [] }));
}

export function reconcileRoutingMetadata(policy: ObjectValue, canonicalModels: string[]): ObjectValue {
  const models: ObjectValue = Object.assign(Object.create(null), object(policy.models));
  const uses = routingModelUses(policy);
  const known = new Set(canonicalModels);
  for (const name of Object.keys(models)) {
    if (!known.has(name) && !Object.hasOwn(uses, name)) delete models[name];
  }
  for (const name of Object.keys(uses)) {
    if (known.has(name) && !Object.hasOwn(models, name)) models[name] = { enabled: true, weight: 100 };
  }
  return { ...policy, models };
}
