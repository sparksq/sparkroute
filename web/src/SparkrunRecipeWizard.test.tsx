// SPDX-FileCopyrightText: 2026 Scitrera LLC
// SPDX-FileCopyrightText: 2026 Fox Engine Ltd.
// SPDX-License-Identifier: AGPL-3.0-only

import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { afterEach, expect, it, vi } from "vitest";
import { SparkrunRecipeWizard } from "./SparkrunRecipeWizard";

const empty = { providers: [], deployments: [], virtual_models: [] };
const recipe = { reference: "catalog:first", name: "@registry/coder", model: "test/model", runtime: "vllm", description: "A coding recipe", min_nodes: 1, source_path: "/recipes/one/coder.yaml", registry: "registry" };
const detail = { ...recipe, recipe_revision: "revision", native_protocols: ["openai"], capabilities: [], required_plugins: [], trusted: true, defaults: {}, issues: [] };
const json = (body: unknown, status = 200) => new Response(JSON.stringify(body), { status, headers: { "Content-Type": "application/json" } });
afterEach(() => { cleanup(); vi.unstubAllGlobals(); });

function setup(options: { failDraft?: boolean; issue?: boolean; emptyCatalog?: boolean; recipeDefaults?: boolean } = {}) {
 const calls: { url: string; body: Record<string, any> }[] = [];
 vi.stubGlobal("fetch", vi.fn(async (url: string, request: RequestInit) => {
  const body = JSON.parse(String(request.body)); calls.push({ url, body });
  expect(new Headers(request.headers).get("Authorization")).toBe("Bearer test-token");
  if (url.endsWith("recipe-draft")) return options.failDraft ? json({ error: { message: "The active revision changed" } }, 409) : json({ document: { ...empty, virtual_models: [{ name: body.recipe.name }] }, reused: false });
  switch (body.operation) {
   case "catalog_clusters": return json({ clusters: [{ name: "lab", host_count: 2, default: true }] });
   case "catalog_registries": return json({ registries: [{ name: "registry", enabled: true, cached: true }, { name: "disabled", enabled: false, cached: true }] });
   case "catalog_search": return json({ recipes: options.emptyCatalog ? [] : [recipe, { ...recipe, reference: "catalog:second", source_path: "/recipes/two/coder.yaml" }], total: options.emptyCatalog ? 0 : 2, next_offset: null, unavailable_registries: options.emptyCatalog ? ["registry"] : [] });
   case "catalog_resolve": return json({ ...detail, ...(options.recipeDefaults ? {capabilities:["vision"], sparkroute:{request_profiles:{low:{responses:{reasoning:{effort:"low"}}}}}} : {}), reference: body.arguments.reference, issues: options.issue ? [{ severity: "error", code: "recipe_trust_required", message: "Recipe hooks require explicit trust." }] : [] });
   case "catalog_import": return json(detail);
   default: throw new Error("Unexpected operation " + body.operation);
  }
 }));
 const onChange = vi.fn();
 render(<SparkrunRecipeWizard token="test-token" document={empty} revision="active" onChange={onChange} onClose={() => {}} />);
 return { calls, onChange };
}

it("preserves duplicate file identity and prepares coding on the explicit default cluster", async () => {
 const { calls, onChange } = setup();
 const choices = await screen.findAllByRole("button", { name: "Choose @registry/coder" });
 fireEvent.click(choices[1]!);
 await screen.findByLabelText("Public model name");
 expect(calls.find((call) => call.body.operation === "catalog_resolve")?.body.arguments.reference).toBe("catalog:second");
 fireEvent.change(screen.getByLabelText("Public model name"), { target: { value: "coding" } });
 fireEvent.change(screen.getByLabelText("Aliases (comma separated)"), { target: { value: "code, assistant" } });
 fireEvent.click(screen.getByLabelText("Manage the model when idle"));
 fireEvent.change(screen.getByLabelText("When persistently unhealthy"), {target: {value: "restart"}});
 fireEvent.change(screen.getByLabelText("Minimum unhealthy duration"), {target: {value: "5m"}});
 fireEvent.click(screen.getByRole("button", { name: "Add to draft" }));
 await waitFor(() => expect(onChange).toHaveBeenCalledOnce());
 const prepared = calls.find((call) => call.url.endsWith("recipe-draft"))!.body;
 expect(prepared.recipe).toMatchObject({ name: "coding", aliases: ["code", "assistant"], cluster: "lab", idle_ttl: "30m", activation_timeout: "30m", recovery: {action: "restart", unhealthy_for: "5m"} });
 expect(calls.some((call) => call.body.operation === "ensure_ready")).toBe(false);
});

it("keeps the user's form after a stale configuration error", async () => {
 setup({ failDraft: true });
 fireEvent.click((await screen.findAllByRole("button", { name: "Choose @registry/coder" }))[0]!);
 fireEvent.change(await screen.findByLabelText("Public model name"), { target: { value: "coding" } });
 fireEvent.click(screen.getByRole("button", { name: "Add to draft" }));
 expect(await screen.findByRole("alert")).toHaveTextContent("active revision changed");
 expect(screen.getByLabelText("Public model name")).toHaveValue("coding");
});

it("requires a new preview after changing overrides and blocks untrusted hooks", async () => {
 setup({ issue: true });
 fireEvent.click((await screen.findAllByRole("button", { name: "Choose @registry/coder" }))[0]!);
 fireEvent.change(await screen.findByLabelText("Public model name"), { target: { value: "coding" } });
 expect(screen.getByText("Recipe hooks require explicit trust.")).toBeVisible();
 expect(screen.getByRole("button", { name: "Add to draft" })).toBeDisabled();
 fireEvent.change(screen.getByLabelText("Recipe overrides (JSON string values)"), { target: { value: '{"tensor_parallel":"2"}' } });
 expect(screen.getByText("Refresh the recipe preview to validate these overrides.")).toBeVisible();
});

it("supports controller-local paths and upload from an empty registry cache", async () => {
 const { calls } = setup({ emptyCatalog: true });
 expect(await screen.findByText(/Not cached:/)).toBeVisible();
 expect(screen.queryByRole("option", { name: "disabled" })).not.toBeInTheDocument();
 fireEvent.change(screen.getByLabelText("Recipe source"), { target: { value: "local" } });
 fireEvent.change(screen.getByLabelText("Absolute recipe path"), { target: { value: "/recipes/a path/coder.yaml" } });
 fireEvent.click(screen.getByRole("button", { name: "Preview local file" }));
 await screen.findByLabelText("Public model name");
 expect(calls.find((call) => call.body.operation === "catalog_resolve")?.body.arguments.reference).toBe("/recipes/a path/coder.yaml");
 fireEvent.click(screen.getByRole("button", { name: /Choose another recipe/ }));
 fireEvent.change(screen.getByLabelText("Recipe source"), { target: { value: "upload" } });
 fireEvent.change(screen.getByLabelText("Recipe YAML"), { target: { value: "model: test/model" } });
 fireEvent.click(screen.getByRole("button", { name: "Preview upload" }));
 await screen.findByLabelText("Public model name");
 expect(calls.find((call) => call.body.operation === "catalog_import")?.body.arguments.content).toBe("model: test/model");
});

it("prefills the HF model and preserves a custom public name when refreshing a recipe", async () => {
 setup();
 fireEvent.click((await screen.findAllByRole("button", {name: "Choose @registry/coder"}))[0]!);
 expect(await screen.findByLabelText("Public model name")).toHaveValue("test/model");
 fireEvent.change(screen.getByLabelText("Public model name"), {target: {value:"my-coding"}});
 fireEvent.click(screen.getByRole("button", {name:"Refresh recipe preview"}));
 await waitFor(() => expect(screen.queryByText("Resolving recipe…")).not.toBeInTheDocument());
 expect(screen.getByLabelText("Public model name")).toHaveValue("my-coding");
});

it("prefills defaults.served_model_name before the HF model", async () => {
 const fetch = vi.fn(async (_url: string, request: RequestInit) => {
  const body = JSON.parse(String(request.body));
  if (body.operation === "catalog_clusters") return json({clusters:[{name:"lab",host_count:1,default:true}]});
  if (body.operation === "catalog_registries") return json({registries:[]});
  if (body.operation === "catalog_search") return json({recipes:[recipe], total:1, next_offset:null, unavailable_registries:[]});
  return json({...detail, hf_model:"hf/weights", defaults:{served_model_name:"served-coder"}});
 });
 vi.stubGlobal("fetch", fetch);
 render(<SparkrunRecipeWizard token="test-token" document={empty} revision="active" onChange={() => {}} onClose={() => {}} />);
 fireEvent.click(await screen.findByRole("button", {name:"Choose @registry/coder"}));
 expect(await screen.findByLabelText("Public model name")).toHaveValue("served-coder");
});

it("explains recipe capabilities and profiles before preparing the draft", async () => {
 const {onChange} = setup({recipeDefaults:true});
 fireEvent.click((await screen.findAllByRole("button", {name:"Choose @registry/coder"}))[0]!);
 expect(await screen.findByText("Model capabilities: Vision.")).toBeVisible();
 expect(screen.getByText(/Recipe request profiles: low/)).toHaveTextContent("Virtual Models / Aliases");
 fireEvent.click(screen.getByRole("button", {name:"Add to draft"}));
 await waitFor(() => expect(onChange).toHaveBeenCalledOnce());
});
