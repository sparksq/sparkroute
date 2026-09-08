import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { useState } from "react";
import { afterEach, expect, it, vi } from "vitest";
import { ProviderDeploymentEditor } from "./ProviderDeploymentEditor";
import type { ConfigurationDocument } from "./types";

const empty: ConfigurationDocument = { providers: [], deployments: [], virtual_models: [] };
const provider = {name: "cloud", type: "openai", base_url: "https://api.example.test/v1"};
const detail = {reference: "catalog:one", name: "@registry/coder", model: "test/model", runtime: "vllm", min_nodes: 1, source_path: "/recipes/coder.yaml", registry: "registry", recipe_revision: "recipe-revision", native_protocols: ["openai"], required_plugins: [], issues: []};
const deployment = {name: "stable-target", title: "sparkrun:lab:test/model", provider: "local-provider", model: "test/model", endpoint_source: {type: "activatable", controller: "sparkrun", recipe: detail.reference, recipe_revision: detail.recipe_revision, revision: "binding", cluster_candidates: ["lab"], activation_timeout: "15m", idle_ttl: "30m"}};
const integrated: ConfigurationDocument = {providers: [{name: "local-provider", type: "openai"}], deployments: [deployment], virtual_models: [{name: "coding", aliases: ["code"], pools: [{targets: [{deployment: "stable-target", weight: 1}]}]}]};
afterEach(() => { cleanup(); vi.unstubAllGlobals(); });

function mockCatalog() {
  const calls: any[] = [];
  vi.stubGlobal("fetch", vi.fn(async (_url: string, request: RequestInit) => {
    const body = JSON.parse(String(request.body)); calls.push(body);
    let response: unknown;
    if (body.recipe) response = {document: body.recipe.deployment ? {...body.document, deployments: [{...deployment, endpoint_source: {...deployment.endpoint_source, idle_ttl: body.recipe.idle_ttl}}]} : integrated, deployment: deployment.name, reused: false};
    else switch (body.operation) {
      case "catalog_clusters": response = {clusters: [{name: "lab", host_count: 2, default: true}]}; break;
      case "catalog_registries": response = {registries: []}; break;
      case "catalog_search": response = {recipes: [detail], total: 1, next_offset: null, unavailable_registries: []}; break;
      case "catalog_resolve": response = detail; break;
      default: throw new Error("unexpected operation: " + body.operation);
    }
    return new Response(JSON.stringify(response), {headers: {"Content-Type": "application/json"}});
  }));
  return calls;
}
function Editor({enabled = true, initial = empty, generated}: {enabled?: boolean; initial?: ConfigurationDocument; generated?: ConfigurationDocument}) {
  const [document, setDocument] = useState(initial);
  const [editing, setEditing] = useState(false);
  return <><ProviderDeploymentEditor section="deployments" document={document} readOnlyDocument={generated} disabled={false} onChange={setDocument}
    sparkrun={{enabled, token: "fixture", revision: "active", onEditingChange: setEditing, onPrepared: setDocument}} />
    <output aria-label="Draft">{JSON.stringify(document)}</output><output aria-label="Editing">{String(editing)}</output></>;
}
const draft = () => JSON.parse(screen.getByLabelText("Draft").textContent!);

it("does not offer or call the integration when it is disabled", () => {
  const fetch = vi.fn(); vi.stubGlobal("fetch", fetch);
  render(<Editor enabled={false} initial={{...empty, providers: [provider]}} />);
  fireEvent.click(screen.getByRole("button", {name: "Add deployment"}));
  expect(screen.queryByRole("option", {name: "sparkrun"})).not.toBeInTheDocument();
  expect(screen.getByLabelText("Provider")).toHaveValue("cloud");
  expect(fetch).not.toHaveBeenCalled();
});

it("stages the normal Local form without creating a partial deployment and cancels cleanly", () => {
  const fetch = vi.fn(); vi.stubGlobal("fetch", fetch);
  render(<Editor initial={{...empty, providers: [provider]}} />);
  fireEvent.click(screen.getByRole("button", {name: "Add deployment"}));
  expect(screen.getByLabelText("Deployment type")).toHaveValue("local");
  expect(screen.getByLabelText("Provider")).toHaveValue("cloud");
  expect(draft().deployments).toEqual([]);
  expect(screen.getByLabelText("Editing")).toHaveTextContent("true");
  fireEvent.click(screen.getByRole("button", {name: "Cancel"}));
  expect(draft().deployments).toEqual([]);
  expect(screen.getByLabelText("Editing")).toHaveTextContent("false");
  expect(fetch).not.toHaveBeenCalled();
});

it("creates sparkrun deployments through the type selector and edits settings without duplicating aliases", async () => {
  const calls = mockCatalog(); render(<Editor />);
  fireEvent.click(screen.getByRole("button", {name: "Add deployment"}));
  fireEvent.change(screen.getByLabelText("Deployment type"), {target: {value: "sparkrun"}});
  fireEvent.click(await screen.findByRole("button", {name: "Choose @registry/coder"}));
  fireEvent.change(await screen.findByLabelText("Public model name"), {target: {value: "coding"}});
  fireEvent.click(screen.getByRole("button", {name: "Add to draft"}));
  fireEvent.click(await screen.findByRole("button", {name: "Edit recipe settings"}));
  await waitFor(() => expect(screen.getByLabelText("Cluster")).toHaveValue("lab"));
  expect(screen.getByLabelText("Idle time (minutes)")).toHaveValue(30);
  expect(screen.queryByLabelText("Public model name")).not.toBeInTheDocument();
  fireEvent.change(screen.getByLabelText("Idle time (minutes)"), {target: {value: "45"}});
  fireEvent.click(screen.getByRole("button", {name: "Apply to draft"}));
  await screen.findByRole("button", {name: "Edit recipe settings"});
  expect(calls.filter((call) => call.recipe).at(-1).recipe).toMatchObject({deployment: "stable-target", idle_ttl: "45m"});
  expect(draft().virtual_models).toEqual(integrated.virtual_models);
  expect(draft().deployments).toHaveLength(1);
  expect(calls.some((call) => call.operation === "ensure_ready")).toBe(false);
});

it("keeps generated sparkrun deployments read-only", async () => {
  mockCatalog(); render(<Editor generated={integrated} />);
  expect(await screen.findByText("@registry/coder")).toBeVisible();
  expect(screen.getByText("sparkrun generated · Read only")).toBeVisible();
  expect(screen.queryByRole("button", {name: "Edit recipe settings"})).not.toBeInTheDocument();
  expect(screen.getByLabelText("Deployment type")).toBeDisabled();
  expect(draft()).toEqual(empty);
});
