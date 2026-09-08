import { cleanup, fireEvent, render, screen } from "@testing-library/react";
import { afterEach, expect, it, vi } from "vitest";
import { ManagedConfigurationWorkspace } from "./ManagedConfigurationWorkspace";
import type { AdminBootstrap, GatewayStatus } from "./types";

afterEach(() => { cleanup(); vi.unstubAllGlobals(); });

it("uses runtime cluster titles in generated forms and pools without changing the operator draft or stored generated JSON", async () => {
  const empty = { providers: [], deployments: [], virtual_models: [] };
  const generated = {
    providers: [{ name: "sparkrun", type: "openai_compatible" }],
    deployments: [{ name: "sparkrun:5813420d8cac", title: "sparkrun:unassigned:deepseek", provider: "sparkrun", model: "deepseek" }],
    virtual_models: [],
  };
  const bootstrap = { schema_version: 1, edition: "standalone", config_revision: "revision", features: { config_managed_sets: true, config_write: true, config_read: true } } as AdminBootstrap;
  const fetch = vi.fn(async (input: string) => {
    const response = input === "/v1/config/managed-sets" ? { active_revision: "revision", managed_sets: [{ owner: "operator" }, { owner: "sparkrun" }] }
      : input.endsWith("/operator/validate") ? { valid: true, candidate_revision: "revision" }
      : input.endsWith("/operator") ? { owner: "operator", document: empty }
      : input.endsWith("/sparkrun") ? { owner: "sparkrun", document: generated }
      : { document: generated, revision: "revision" };
    return new Response(JSON.stringify(response), { headers: { "Content-Type": "application/json" } });
  });
  vi.stubGlobal("fetch", fetch);
  const targets = (cluster: string) => [{ deployment: "sparkrun:5813420d8cac", title: `sparkrun:${cluster}:deepseek` }] as GatewayStatus["targets"];
  const view = render(<ManagedConfigurationWorkspace bootstrap={bootstrap} token="" section="deployments" runtimeTargets={targets("spark-a")} />);
  expect(await screen.findByDisplayValue("sparkrun:spark-a:deepseek")).toBeDisabled();
  expect(screen.getByLabelText("Deployment ID")).toHaveValue("sparkrun:5813420d8cac");
  fireEvent.click(screen.getByRole("button", { name: "Validate" }));
  expect(await screen.findByRole("button", { name: "Save" })).toBeEnabled();
  view.rerender(<ManagedConfigurationWorkspace bootstrap={bootstrap} token="" section="models" runtimeTargets={targets("spark-b")} />);
  expect(screen.getByRole("button", { name: "Save" })).toBeEnabled();
  fireEvent.click(screen.getByTitle("Add virtual model"));
  expect(screen.getByRole("option", { name: "sparkrun:spark-b:deepseek" })).toHaveValue("sparkrun:5813420d8cac");
  fireEvent.click(screen.getByRole("button", { name: "JSON" }));
  const draft = JSON.parse((screen.getByLabelText("operator configuration JSON") as HTMLTextAreaElement).value);
  expect(draft.deployments).toEqual([]);
  expect(draft.virtual_models[0].pools[0].targets[0].deployment).toBe("sparkrun:5813420d8cac");
  expect((screen.getByLabelText("sparkrun configuration JSON") as HTMLTextAreaElement).value).toContain("sparkrun:unassigned:deepseek");
});
