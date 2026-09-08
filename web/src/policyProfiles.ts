import type { JSONObject } from "./extensions";
import type { ConfigurationDocument } from "./types";

export type PolicyKind = "pii" | "guardrails";
export const policyKinds = ["pii", "guardrails"] as const;
export const policyInfo = {
  pii: { field: "pii_profiles", reference: "pii_profile", label: "PII privacy", page: "PII Privacy", prefix: "pii-policy" },
  guardrails: { field: "guardrail_profiles", reference: "guardrail_profile", label: "Guardrail", page: "Guardrails", prefix: "guardrail-policy" },
} as const;
export const isObject = (value: unknown): value is JSONObject => Boolean(value) && typeof value === "object" && !Array.isArray(value);
export const object = (value: unknown): JSONObject => isObject(value) ? value : {};
export const rows = (value: unknown): JSONObject[] => Array.isArray(value) ? value.filter(isObject) : [];
export const profiles = (doc: ConfigurationDocument, kind: PolicyKind) => object(doc[policyInfo[kind].field]);
export const assignment = (doc: ConfigurationDocument, name: string) => object(Object.hasOwn(object(doc.model_policies), name) ? object(doc.model_policies)[name] : undefined);
export function validProfileName(name: string) { return Boolean(name) && new TextEncoder().encode(name).length <= 256 && !/[\s\p{Cc}]/u.test(name); }
export function uniqueProfileName(doc: ConfigurationDocument, kind: PolicyKind) {
  const base = policyInfo[kind].prefix, current = profiles(doc, kind);
  let name: string = base;
  for (let n = 2; Object.hasOwn(current, name); n++) name = `${base}-${n}`;
  return name;
}
export function setAssignment(doc: ConfigurationDocument, name: string, kind: PolicyKind, value: string | undefined) {
  const next = { ...assignment(doc, name) }, field = policyInfo[kind].reference;
  if (value === undefined) delete next[field]; else next[field] = value;
  const assignments = { ...object(doc.model_policies) };
  if (Object.keys(next).length) assignments[name] = next; else delete assignments[name];
  const result = { ...doc };
  if (Object.keys(assignments).length) result.model_policies = assignments; else delete result.model_policies;
  return result;
}
export function profileUses(doc: ConfigurationDocument, kind: PolicyKind, name: string) {
  return Object.entries(object(doc.model_policies)).filter(([, value]) => object(value)[policyInfo[kind].reference] === name).map(([model]) => model).sort();
}
export function renameProfile(doc: ConfigurationDocument, kind: PolicyKind, from: string, to: string) {
  if (!validProfileName(to) || from === to || Object.hasOwn(profiles(doc, kind), to)) return doc;
  const values = { ...profiles(doc, kind), [to]: profiles(doc, kind)[from] };
  delete values[from];
  let result = { ...doc, [policyInfo[kind].field]: values };
  for (const model of profileUses(doc, kind, from)) result = setAssignment(result, model, kind, to);
  return result;
}
export function inlinePolicy(model: JSONObject, kind: PolicyKind) {
  const value = kind === "pii" ? object(model.privacy).pii : model.guardrails;
  return isObject(value) && (kind === "pii" || Object.keys(value).length) ? value : undefined;
}
export function copyInlineProfile(doc: ConfigurationDocument, model: JSONObject, kind: PolicyKind) {
  const value = inlinePolicy(model, kind);
  if (!value) return doc;
  const name = uniqueProfileName(doc, kind);
  return setAssignment({ ...doc, [policyInfo[kind].field]: { ...profiles(doc, kind), [name]: structuredClone(value) } }, String(model.name), kind, name);
}
export function policyModels(doc: ConfigurationDocument, generated?: ConfigurationDocument) {
  const all = [...rows(doc.virtual_models).map(model => ({ model, generated: false })), ...rows(generated?.virtual_models).map(model => ({ model, generated: true }))];
  return all.filter(({ model }) => {
    const name = String(model.name), separator = name.lastIndexOf(":");
    return !model.request_overrides || separator < 0 || !all.some(({ model: parent }) => parent.name === name.slice(0, separator));
  });
}
