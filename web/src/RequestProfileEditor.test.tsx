// SPDX-FileCopyrightText: 2026 Scitrera LLC
// SPDX-FileCopyrightText: 2026 Fox Engine Ltd.
// SPDX-License-Identifier: AGPL-3.0-only

import { cleanup, fireEvent, render, screen, within } from "@testing-library/react";
import { useState } from "react";
import { afterEach, expect, it } from "vitest";
import { VirtualModelEditor } from "./VirtualModelEditor";
import type { ConfigurationDocument } from "./types";

afterEach(cleanup);
const base = { name: "coding", aliases: ["code"], pools: [{ priority: 0, targets: [{ deployment: "one-job", weight: 1 }] }] };
const low = { ...base, name: "coding:low", aliases: ["quick-code"], request_overrides: { chat_completions: { reasoning_effort: "low" }, responses: { reasoning: { effort: "low" } } } };
function Editor({ generated = false, profiles = false, disabled = false, baseOverrides = false } = {}) {
  const [document, setDocument] = useState<ConfigurationDocument>({ deployments: [{ name: "one-job" }], virtual_models: [...(generated ? [] : [baseOverrides ? { ...base, request_overrides: { chat_completions: { temperature: 0.3 } } } : base]), ...(profiles ? [low] : [])] });
  return <><VirtualModelEditor document={document} disabled={disabled} onChange={setDocument} readOnlyDocument={generated ? { deployments: [], virtual_models: [base] } : undefined} /><output aria-label="Draft">{JSON.stringify(document)}</output></>;
}
const draft = () => JSON.parse(screen.getByLabelText("Draft").textContent!);
const click = (name: string) => fireEvent.click(screen.getByRole("button", { name }));
const parameters = (json: string) => fireEvent.change(screen.getByLabelText("Request parameters (JSON)"), { target: { value: json } });

it("adds a named profile into the table after general fields and before routing pools", () => {
  render(<Editor />);
  const profiles = screen.getByRole("region", { name: "Request profiles" });
  expect(screen.getByLabelText(/^Aliases/).compareDocumentPosition(profiles) & Node.DOCUMENT_POSITION_FOLLOWING).toBeTruthy();
  expect(profiles.compareDocumentPosition(screen.getByRole("heading", { name: "Routing pools" })) & Node.DOCUMENT_POSITION_FOLLOWING).toBeTruthy();
  click("Add profile");
  fireEvent.change(screen.getByLabelText("Profile selector"), { target: { value: "xhigh" } });
  parameters('{"reasoning_effort":"xhigh"}'); click("Add profile to draft");
  expect(draft().virtual_models[1]).toEqual({ ...base, name: "coding:xhigh", aliases: [], request_overrides: { chat_completions: { reasoning_effort: "xhigh" } } });
  expect(screen.getByLabelText("Canonical name")).toHaveValue("coding");
  expect(within(screen.getByRole("complementary", { name: "Virtual models" })).queryByText("coding:xhigh")).not.toBeInTheDocument();
  const row = screen.getByRole("button", { name: "Edit profile coding:xhigh" }).closest("tr")!;
  expect(within(row).getByRole("rowheader")).toHaveTextContent("xhigh");
  expect(row.querySelector("code")?.textContent).toBe('{"chat_completions":{"reasoning_effort":"xhigh"}}');
  expect(screen.queryByLabelText("Request parameters (JSON)")).not.toBeInTheDocument();
});

it("expands the selected row, preserves other API parameters and aliases, and supports cancel and delete", () => {
  render(<Editor profiles />);
  click("Edit profile coding:low");
  const row = screen.getByRole("button", { name: "Edit profile coding:low" }).closest("tr")!;
  expect(row.nextElementSibling).toContainElement(screen.getByLabelText("Request parameters (JSON)"));
  parameters('{"reasoning_effort":"high"}'); click("Cancel");
  expect(draft().virtual_models[1]).toEqual(low);
  click("Edit profile coding:low");
  fireEvent.change(screen.getByLabelText("Request API"), { target: { value: "responses" } });
  parameters('{"reasoning":{"effort":"medium"}}'); click("Apply parameters to draft");
  expect(draft().virtual_models[1]).toEqual({ ...low, request_overrides: { ...low.request_overrides, responses: { reasoning: { effort: "medium" } } } });
  click("Delete profile coding:low");
  expect(draft().virtual_models).toEqual([base]);
  expect(screen.queryByRole("button", { name: "Delete profile coding:low" })).not.toBeInTheDocument();
});

it("rejects a conflicting selector and invalid JSON without losing the editor draft", () => {
  render(<Editor profiles />); click("Add profile"); click("Add profile to draft");
  expect(screen.getByRole("alert")).toHaveTextContent("unique profile selector");
  fireEvent.change(screen.getByLabelText("Profile selector"), { target: { value: "high" } });
  parameters("[]"); click("Add profile to draft");
  expect(screen.getByRole("alert")).toHaveTextContent("JSON object");
  expect(screen.getByLabelText("Profile selector")).toHaveValue("high");
  expect(draft().virtual_models).toEqual([base, low]);
});

it("can create an operator profile from a generated model without making the generated fields editable", () => {
  render(<Editor generated />);
  expect(screen.getByLabelText("Canonical name")).toBeDisabled();
  click("Add profile");
  fireEvent.change(screen.getByLabelText("Request API"), { target: { value: "responses" } });
  parameters('{"reasoning":{"effort":"high"}}'); click("Add profile to draft");
  expect(draft().virtual_models).toEqual([{ ...base, name: "coding:low", aliases: [], request_overrides: { responses: { reasoning: { effort: "high" } } } }]);
});

it("disables every profile mutation for readers", () => {
  render(<Editor profiles disabled />);
  for (const name of ["Add profile", "Edit profile coding:low", "Delete profile coding:low"]) expect(screen.getByRole("button", { name })).toBeDisabled();
});

it("keeps a base model when deleting its default parameter override", () => {
  render(<Editor baseOverrides />);
  expect(screen.getByRole("rowheader", { name: "(default)" })).toBeInTheDocument();
  click("Delete profile coding");
  expect(draft().virtual_models).toEqual([base]);
});

it("preserves routing edits made while profile parameters are expanded", () => {
  render(<Editor baseOverrides />);
  click("Edit profile coding");
  fireEvent.change(screen.getByLabelText("Relative weight"), { target: { value: "42" } });
  parameters('{"reasoning_effort":"medium"}'); click("Apply parameters to draft");
  expect(draft().virtual_models[0].pools[0].targets[0].weight).toBe(42);
});

it("shows only the generated parent when operator variants precede it in the document", () => {
  render(<Editor generated profiles />);
  const list = screen.getByRole("complementary", { name: "Virtual models" });
  expect(within(list).queryByText("coding:low")).not.toBeInTheDocument();
  expect(within(list).getByText("coding")).toBeInTheDocument();
  expect(screen.getByLabelText("Canonical name")).toHaveValue("coding");
  expect(screen.getByLabelText("Canonical name")).toBeDisabled();
  click("Edit profile coding:low");
  parameters('{"reasoning_effort":"medium"}'); click("Apply parameters to draft");
  expect(draft().virtual_models[0].request_overrides.chat_completions).toEqual({ reasoning_effort: "medium" });
});

it("keeps standalone profiles accessible when their parent is absent", () => {
  render(<VirtualModelEditor document={{ deployments: [{ name: "one-job" }], virtual_models: [low] }} disabled={false} onChange={() => {}} />);
  expect(within(screen.getByRole("complementary", { name: "Virtual models" })).getByText("coding:low")).toBeInTheDocument();
  expect(screen.getByLabelText("Canonical name")).toHaveValue("coding:low");
});
