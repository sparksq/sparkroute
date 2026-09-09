import { useState } from "react";
import { cleanup, fireEvent, render, screen, within } from "@testing-library/react";
import { afterEach, expect, it } from "vitest";
import { SparkrunRemoval, ExcludedSparkrunDeployments } from "./SparkrunRemoval";
import { VirtualModelEditor } from "./VirtualModelEditor";
import { ProviderDeploymentEditor } from "./ProviderDeploymentEditor";
import { sparkrunRemovalPreview, visibleSparkrunDocument } from "./sparkrunExclusions";
import type { ConfigurationDocument } from "./types";

afterEach(cleanup);
const empty = {providers: [], deployments: [], virtual_models: []};
const model = (name: string, targets = ["stopped"]) => ({name, pools: [{priority: 0, targets: targets.map(deployment => ({deployment, weight: 1}))}]});
const generated = {
  providers: [{name: "sparkrun", type: "sparkrun"}],
  deployments: [{name: "stopped", title: "sparkrun:lab:coding", model: "upstream", provider: "sparkrun", endpoint_source: {type: "activatable", controller: "sparkrun", recipe: "/recipes/coding.yaml"}}],
  virtual_models: [{...model("coding"), aliases: ["code"]}, {...model("coding:low"), request_overrides: {chat_completions: {temperature: .1}}}],
};
function Editor({page = "models", initial = empty, disabled = false}: {page?: string; initial?: ConfigurationDocument; disabled?: boolean}) {
  const [document, setDocument] = useState<ConfigurationDocument>(initial);
  const visible = visibleSparkrunDocument(document, generated);
  return <>{page === "models" ? <VirtualModelEditor document={document} readOnlyDocument={visible} allowGeneratedRemoval disabled={disabled} onChange={setDocument} />
    : <ProviderDeploymentEditor section="deployments" document={document} readOnlyDocument={visible} allowGeneratedRemoval disabled={disabled} onChange={setDocument}
      sparkrun={{enabled: false, token: "", revision: "", onEditingChange: () => {}, onPrepared: setDocument}} />}
    <ExcludedSparkrunDeployments document={document} generated={generated} disabled={disabled} onChange={setDocument} />
    <output aria-label="Draft">{JSON.stringify(document)}</output></>;
}
const draft = () => JSON.parse(screen.getByLabelText("Draft").textContent!);

it.each(["models", "deployments"])("removes stopped generated entries through %s and can restore them", page => {
  render(<Editor page={page} />);
  fireEvent.click(screen.getByRole("button", {name: "Remove from sparkroute"}));
  expect(draft()).not.toHaveProperty("sparkrun_overrides");
  expect(screen.getByText(/Generated names and their aliases removed: coding, coding:low/)).toBeVisible();
  expect(screen.getByText(/Running workloads are left alone/)).toBeVisible();
  fireEvent.click(screen.getByRole("button", {name: "Confirm removal"}));
  expect(draft().sparkrun_overrides.excluded_deployments).toEqual(["stopped"]);
  expect(screen.queryByRole("button", {name: "Remove from sparkroute"})).toBeNull();
  expect(draft().virtual_models).toEqual([]);
  fireEvent.click(screen.getByText("Excluded sparkrun deployments (1)"));
  fireEvent.click(screen.getByRole("button", {name: "Restore sparkrun:lab:coding"}));
  expect(draft()).not.toHaveProperty("sparkrun_overrides");
  expect(screen.getByRole("button", {name: "Remove from sparkroute"})).toBeVisible();
});

it("names operator profile blockers while generated profile variants are removed together", () => {
  const operator = {...empty, virtual_models: [{...model("coding:xhigh"), request_overrides: {chat_completions: {temperature: .9}}}]};
  render(<Editor initial={operator} />);
  fireEvent.click(screen.getByRole("button", {name: "Remove from sparkroute"}));
  expect(screen.getByText("Virtual Models / Aliases: coding:xhigh uses this deployment")).toBeVisible();
  expect(screen.getByRole("button", {name: "Confirm removal"})).toBeDisabled();
  expect(draft()).toEqual(operator);
});

it("shows routing references and preserves targets that still have a deployment", () => {
  const shared = {...generated, deployments: [...generated.deployments, {name: "kept", model: "other", provider: "sparkrun"}], virtual_models: [...generated.virtual_models, model("shared", ["stopped", "kept"])]};
  const operator = {...empty, model_routing: {models: {coding: {enabled: true}}, virtual_models: {auto: {models: ["coding"], strategy: "balanced"}}}};
  const preview = sparkrunRemovalPreview(operator, shared, ["stopped"]);
  expect(preview.blockers[0]).toMatch(/Model Routing: coding/);
  const visible = visibleSparkrunDocument(preview.document, shared)!;
  expect(visible.virtual_models).toEqual([model("shared", ["kept"])]);
  expect(generated.virtual_models).toHaveLength(2);
  expect(preview.document.model_routing).toEqual(operator.model_routing);
});

it("does not permit removal or restore for a read-only user", () => {
  render(<Editor disabled />);
  expect(screen.getByRole("button", {name: "Remove from sparkroute"})).toBeDisabled();
});

it("cancels without changing the document", () => {
  render(<SparkrunRemoval document={empty} generated={generated} deployments={["stopped"]} disabled={false} onChange={() => {throw new Error("unexpected change");}} />);
  fireEvent.click(screen.getByRole("button", {name: "Remove from sparkroute"}));
  fireEvent.click(screen.getByRole("button", {name: "Cancel removal"}));
  expect(within(screen.getByRole("region")).queryByRole("button", {name: "Confirm removal"})).toBeNull();
});
