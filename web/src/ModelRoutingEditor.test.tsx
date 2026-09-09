import { useState } from "react";
import { cleanup, fireEvent, render, screen, within } from "@testing-library/react";
import { afterEach, expect, it } from "vitest";
import { ModelRoutingEditor } from "./ModelRoutingEditor";
import { VirtualModelEditor } from "./VirtualModelEditor";
import { reconcileRoutingMetadata } from "./modelRoutingReferences";
import type { ConfigurationDocument } from "./types";

afterEach(cleanup);
const base = {
  providers: [{ name: "local", type: "openai", base_url: "http://localhost:8000/v1" }],
  deployments: [{ name: "target", provider: "local", model: "upstream" }],
  virtual_models: ["remaining", "new-model"].map((name) => ({ name, pools: [{ targets: [{ deployment: "target", weight: 100 }] }] })),
  model_routing: {
    version: 2, revision: 1, default_virtual_model: "auto",
    models: { remaining: { enabled: true, weight: 50 }, "new-model": { enabled: true, weight: 100 } },
    virtual_models: { auto: { strategy: "stage_router", models: ["remaining", "new-model"], aliases: ["smart"],
      stage_router: { capable_model: "remaining", efficient_model: "new-model", picker: "efficient_first" },
      kwargs: { low: { provider_priority: ["local"] } } } },
    keyword_rules: [{ name: "rule", keywords: ["code"], virtual_model: "auto" }],
  },
  observability: { saved_traces: { enabled: false } },
};
function Editor({ initial = base, showModels = false, disabled = false }: { initial?: ConfigurationDocument; showModels?: boolean; disabled?: boolean }) {
  const [document, setDocument] = useState<ConfigurationDocument>(initial);
  return <>{showModels ? <VirtualModelEditor document={document} disabled={disabled} onChange={setDocument} /> : null}
    <ModelRoutingEditor document={document} disabled={disabled} onChange={setDocument} />
    <output aria-label="Draft">{JSON.stringify(document)}</output></>;
}
const draft = () => JSON.parse(screen.getByLabelText("Draft").textContent!);

it("removes the whole routing policy even when auto has invalid references, then can re-enable it", () => {
  render(<Editor initial={{ ...base, virtual_models: [base.virtual_models[0]] }} />);
  fireEvent.click(screen.getByRole("button", { name: "Remove model routing" }));
  expect(draft()).toHaveProperty("model_routing");
  expect(screen.getByText(/Direct virtual models, their aliases, and deployments remain available/)).toBeVisible();
  fireEvent.click(screen.getByRole("button", { name: "Confirm remove model routing" }));
  expect(draft()).not.toHaveProperty("model_routing");
  expect(draft().virtual_models).toEqual([base.virtual_models[0]]);
  expect(draft().deployments).toEqual(base.deployments);
  expect(draft().observability).toEqual(base.observability);
  fireEvent.click(screen.getByRole("button", { name: "Enable model routing" }));
  expect(draft().model_routing.virtual_models.auto).toMatchObject({ strategy: "balanced", models: ["remaining"] });
});

it("shows deleted stage-role references and repairs the policy with one-model Balanced routing", () => {
  render(<Editor showModels />);
  fireEvent.click(within(screen.getByRole("complementary", { name: "Virtual models" })).getByRole("button", { name: /new-model/ }));
  expect(screen.getByText(/Used by model routing:/)).toBeVisible();
  fireEvent.click(screen.getByRole("button", { name: "Remove model" }));
  fireEvent.click(screen.getByRole("button", { name: "Confirm remove" }));
  expect(screen.getByRole("region", { name: "Missing routing models" })).toHaveTextContent("new-model");
  expect(screen.getByRole("option", { name: "new-model (missing model)" })).toBeVisible();
  expect(screen.getByLabelText("Efficient model")).toHaveValue("new-model");
  fireEvent.click(screen.getByRole("button", { name: "Use Balanced routing" }));
  expect(draft().model_routing.models).toEqual({ remaining: { enabled: true, weight: 50 } });
  expect(draft().model_routing.virtual_models.auto).toEqual({ strategy: "balanced", models: ["remaining"], aliases: ["smart"], kwargs: { low: { provider_priority: ["local"] } } });
  expect(draft().model_routing.keyword_rules).toEqual(base.model_routing.keyword_rules);
  expect(screen.queryByRole("region", { name: "Missing routing models" })).toBeNull();
});

it("shows missing candidates and removes their unused preferences when unchecked", () => {
  const initial = structuredClone(base);
  initial.virtual_models = [initial.virtual_models[0]!];
  initial.model_routing.virtual_models.auto.strategy = "balanced";
  render(<Editor initial={initial} />);
  fireEvent.click(screen.getByLabelText("new-model (missing model)", { selector: "input" }));
  expect(draft().model_routing.virtual_models.auto.models).toEqual(["remaining"]);
  // A malformed JSON draft can retain stage fields even with another strategy:
  // those references stay visible until the strategy is explicitly corrected.
  expect(screen.getByRole("region", { name: "Missing routing models" })).toHaveTextContent("efficient model");
  fireEvent.change(screen.getByLabelText("Routing strategy"), { target: { value: "balanced" } });
  expect(draft().model_routing.models).not.toHaveProperty("new-model");
});

it("cleans orphaned preferences but retains references in presets and keyword rules", () => {
  const initial = { ...base, virtual_models: [base.virtual_models[0]], model_routing: {
    ...base.model_routing, virtual_models: { auto: { strategy: "balanced", models: ["remaining"] } },
  } };
  render(<Editor initial={initial} />);
  expect(screen.getByRole("region", { name: "Missing routing models" })).toHaveTextContent("Unused routing preferences");
  fireEvent.click(screen.getByRole("button", { name: "Remove unused preferences" }));
  expect(draft().model_routing.models).not.toHaveProperty("new-model");
  const policy = { ...initial.model_routing,
    virtual_models: { auto: { strategy: "balanced", models: ["remaining"], kwargs: { private: { models: ["new-model"] } } } },
    keyword_rules: [{ name: "inline", strategy: "balanced", keywords: ["keyword"], models: ["rule-model"] }],
    models: { ...initial.model_routing.models, "rule-model": { enabled: true } },
  };
  expect(reconcileRoutingMetadata(policy, ["remaining"]).models).toHaveProperty("new-model");
  expect(reconcileRoutingMetadata(policy, ["remaining"]).models).toHaveProperty("rule-model");
});

it("keeps required auto naming and policy removal read-only when editing is not allowed", () => {
  render(<Editor disabled />);
  expect(screen.getByRole("button", { name: "Remove model routing" })).toBeDisabled();
  expect(screen.getByLabelText("Selector name")).toHaveAttribute("readonly");
});

it("repairs duplicate stage roles from a JSON draft without inventing a second model", () => {
  const initial = structuredClone(base);
  initial.virtual_models = [initial.virtual_models[0]!];
  initial.model_routing.virtual_models.auto.models = ["remaining", "remaining"];
  initial.model_routing.virtual_models.auto.stage_router.efficient_model = "remaining";
  render(<Editor initial={initial} />);
  fireEvent.click(screen.getByRole("button", { name: "Use Balanced routing" }));
  expect(draft().model_routing.virtual_models.auto.models).toEqual(["remaining"]);
  expect(draft().model_routing.virtual_models.auto).not.toHaveProperty("stage_router");
  expect(draft().model_routing.models).not.toHaveProperty("new-model");
});
