// SPDX-FileCopyrightText: 2026 Scitrera LLC
// SPDX-FileCopyrightText: 2026 Fox Engine Ltd.
// SPDX-License-Identifier: AGPL-3.0-only

import { cleanup, fireEvent, render, screen } from "@testing-library/react";
import { useState } from "react";
import { afterEach, expect, it } from "vitest";
import { ProviderDeploymentEditor } from "./ProviderDeploymentEditor";
import { VirtualModelEditor } from "./VirtualModelEditor";
import { deploymentChoices } from "./deploymentTitles";
import type { ConfigurationDocument } from "./types";

afterEach(cleanup);
const generated: ConfigurationDocument = {
  providers: [{ name: "sparkrun", type: "openai_compatible" }],
  deployments: [{ name: "sparkrun:abc123", title: "sparkrun:spark-a:model", provider: "sparkrun", model: "model" }],
  virtual_models: [{ name: "generated-model", pools: [{ targets: [{ deployment: "sparkrun:abc123", weight: 1 }] }] }],
};
const local: ConfigurationDocument = {
  providers: [{ name: "local", type: "openai", base_url: "http://localhost:8000/v1" }],
  deployments: [{ name: "local-target", provider: "local", model: "local-model" }],
  virtual_models: [],
};
function draft() { return JSON.parse(screen.getByLabelText("Submitted draft").textContent!); }

it("keeps generated entities read only while editing and deleting local providers and deployments", () => {
  function Editor() {
    const [document, setDocument] = useState(local);
    return <><ProviderDeploymentEditor document={document} readOnlyDocument={generated} disabled={false} onChange={setDocument} /><output aria-label="Submitted draft">{JSON.stringify(document)}</output></>;
  }
  render(<Editor />);
  fireEvent.click(screen.getByRole("button", { name: /sparkrun.*OpenAI compatible.*Read only/ }));
  expect(screen.getByLabelText("Provider name")).toBeDisabled();
  expect(screen.getByRole("button", { name: "Remove" })).toBeDisabled();
  fireEvent.click(screen.getByRole("button", { name: /sparkrun:spark-a:model.*Read only/ }));
  expect(screen.getByLabelText("Deployment ID")).toBeDisabled();
  expect(screen.getByLabelText("Display title")).toHaveValue("sparkrun:spark-a:model");
  fireEvent.click(screen.getByRole("button", { name: /local-target.*local/ }));
  fireEvent.change(screen.getByLabelText("Deployment name"), { target: { value: "renamed-local" } });
  expect(draft().deployments.map((deployment: { name: string }) => deployment.name)).toEqual(["renamed-local"]);
  fireEvent.click(screen.getByRole("button", { name: "Remove" }));
  fireEvent.click(screen.getByRole("button", { name: "Confirm remove" }));
  expect(draft().deployments).toEqual([]);
  fireEvent.click(screen.getByRole("button", { name: /local.*OpenAI \(Chat\)/ }));
  fireEvent.click(screen.getByRole("button", { name: "Remove" }));
  fireEvent.click(screen.getByRole("button", { name: "Confirm remove" }));
  expect(draft().providers).toEqual([]);
});

it("adds and removes local virtual models without copying generated ones", () => {
  function Editor() {
    const [document, setDocument] = useState({ providers: [], deployments: [], virtual_models: [] } as ConfigurationDocument);
    return <><VirtualModelEditor document={document} readOnlyDocument={generated} disabled={false} onChange={setDocument} /><output aria-label="Submitted draft">{JSON.stringify(document)}</output></>;
  }
  render(<Editor />);
  expect(screen.getByLabelText("Canonical name")).toBeDisabled();
  expect(screen.getByRole("button", { name: "Remove model" })).toBeDisabled();
  fireEvent.click(screen.getByTitle("Add virtual model"));
  expect(screen.getByLabelText("Canonical name")).not.toBeDisabled();
  expect(screen.getByRole("option", { name: "sparkrun:spark-a:model" })).toHaveValue("sparkrun:abc123");
  expect(draft().virtual_models).toHaveLength(1);
  expect(draft().deployments).toEqual([]);
  fireEvent.click(screen.getByRole("button", { name: "Remove model" }));
  fireEvent.click(screen.getByRole("button", { name: "Confirm remove" }));
  expect(draft().virtual_models).toEqual([]);
});

it("disambiguates equal titles and supports older generated deployments", () => {
  const titles = deploymentChoices({ deployments: [
    { name: "sparkrun:one", title: "sparkrun:spark-a:model" },
    { name: "sparkrun:two", title: "sparkrun:spark-a:model" },
    { name: "sparkrun:old", model: "old-model", endpoint_source: { type: "activatable", cluster_candidates: ["spark-b"] } },
  ] });
  expect(titles["sparkrun:one"]).toBe("sparkrun:spark-a:model (sparkrun:one)");
  expect(titles["sparkrun:two"]).toBe("sparkrun:spark-a:model (sparkrun:two)");
  expect(titles["sparkrun:old"]).toBe("sparkrun:spark-b:old-model");
});
