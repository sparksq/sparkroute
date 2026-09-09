// SPDX-FileCopyrightText: 2026 Scitrera LLC
// SPDX-FileCopyrightText: 2026 Fox Engine Ltd.
// SPDX-License-Identifier: AGPL-3.0-only

import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { afterEach, expect, it, vi } from "vitest";
import { ManagedConfigurationWorkspace } from "./ManagedConfigurationWorkspace";
import type { AdminBootstrap } from "./types";

afterEach(() => { cleanup(); vi.unstubAllGlobals(); });

it("reloads the shared provider as a single read-only entry after saving an empty-install recipe draft", async () => {
  const empty = { providers: [], deployments: [], virtual_models: [] };
  const provider = { name: "sparkrun", type: "sparkrun" };
  let saved = false;
  const bootstrap = { schema_version: 1, edition: "standalone", config_revision: "before", features: { config_managed_sets: true, config_write: true, config_read: true } } as AdminBootstrap;
  vi.stubGlobal("fetch", vi.fn(async (input: string, init?: RequestInit) => {
    if (input.endsWith("/operator") && init?.method === "PUT") {
      saved = true;
      return Response.json({ changed: true, current: { revision: "after" }, managed_set: { owner: "operator", revision: "operator-after" } });
    }
    const operator = saved ? empty : { ...empty, providers: [provider] };
    const generated = saved ? { ...empty, providers: [provider] } : empty;
    const revision = saved ? "after" : "before";
    return Response.json(input === "/v1/config/managed-sets" ? { active_revision: revision, managed_sets: [{ owner: "operator" }, { owner: "sparkrun" }] }
      : input.endsWith("/operator/validate") ? { valid: true, candidate_revision: "after" }
      : input.endsWith("/operator") ? { owner: "operator", document: operator }
      : input.endsWith("/sparkrun") ? { owner: "sparkrun", document: generated }
      : { document: { ...empty, providers: [provider] }, revision });
  }));
  render(<ManagedConfigurationWorkspace bootstrap={bootstrap} token="" section="providers" />);
  expect(await screen.findByLabelText("Provider name")).toHaveValue("sparkrun");
  fireEvent.click(screen.getByRole("button", { name: "Validate" }));
  fireEvent.click(await screen.findByRole("button", { name: "Save" }));
  await waitFor(() => expect(screen.getByLabelText("Provider name")).toBeDisabled());
  expect(screen.getAllByRole("button", { name: /sparkrun.*Read only/ })).toHaveLength(1);
  fireEvent.click(screen.getByRole("button", { name: "JSON" }));
  expect(JSON.parse((screen.getByLabelText("operator configuration JSON") as HTMLTextAreaElement).value).providers).toEqual([]);
  expect(JSON.parse((screen.getByLabelText("sparkrun configuration JSON") as HTMLTextAreaElement).value).providers).toEqual([provider]);
});
