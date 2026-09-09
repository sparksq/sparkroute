import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { afterEach, expect, it, vi } from "vitest";
import { RuntimeWorkspace } from "./RuntimeWorkspace";
import type { AdminBootstrap, LifecycleBindingStatus } from "./types";

afterEach(() => { cleanup(); vi.unstubAllGlobals(); });

const bootstrap = { schema_version: 1, edition: "standalone", config_revision: "revision", features: { lifecycle_status: true, sparkrun_controls: true } } as AdminBootstrap;
const binding: LifecycleBindingStatus = { controller: "sparkrun", deployment: "recipe", virtual_model: "ds4f", binding_revision: "revision", state: "offline",
  updated_at: "2026-09-09T03:00:00Z", active_leases: 0, queued_waiters: 0, queued_body_bytes: 0, max_queued_waiters: 100, max_queued_body_bytes: 1024 };

function mockRuntime(initial: LifecycleBindingStatus, control?: (body: Record<string, string>) => Promise<void>) {
  let current = initial;
  const fetch = vi.fn(async (input: string, init?: RequestInit) => {
    if (input === "/v1/sparkrun/workload") {
      const body = JSON.parse(String(init?.body));
      await control?.(body);
      current = body.action === "start" ? { ...current, state: "ready", phase: "ready", job_id: "job", owned: true }
        : body.action === "stop" ? { ...current, state: "offline", phase: "offline", lifecycle_actions: [] } : current;
      return Response.json({ state: current.state });
    }
    return Response.json({ observed_at: binding.updated_at, controllers: [], bindings: [current] });
  });
  vi.stubGlobal("fetch", fetch);
  return fetch;
}

it("starts a never-launched deployment, then stops it without requiring ColdSnap", async () => {
  let complete!: () => void;
  const pending = new Promise<void>(resolve => { complete = resolve; });
  const fetch = mockRuntime(binding, async body => { if (body.action === "start") await pending; });
  render(<RuntimeWorkspace bootstrap={bootstrap} token="token" />);
  fireEvent.click(await screen.findByRole("button", { name: "Start" }));
  expect(screen.getByText("Starting workload…")).toBeInTheDocument();
  expect(screen.getByRole("button", { name: "Start" })).toBeDisabled();
  const request = fetch.mock.calls.find(([path]) => path === "/v1/sparkrun/workload");
  expect(JSON.parse(String(request?.[1]?.body))).toEqual({ deployment: "recipe", job_id: "", action: "start" });
  complete();
  fireEvent.click(await screen.findByRole("button", { name: "Stop" }));
  expect(await screen.findByRole("button", { name: "Start" })).toBeEnabled();
  const calls = fetch.mock.calls.filter(([path]) => path === "/v1/sparkrun/workload");
  expect(JSON.parse(String(calls[1]?.[1]?.body))).toEqual({ deployment: "recipe", job_id: "job", action: "stop" });
  expect(screen.queryByRole("button", { name: "Sleep" })).not.toBeInTheDocument();
});

it("adds Stop beside ColdSnap controls and disables mutations during active requests", async () => {
  mockRuntime({ ...binding, state: "ready", phase: "ready", job_id: "job", owned: true, active_leases: 1,
    plugins_in_use: ["coldsnap"], lifecycle_actions: ["status", "sleep", "wake"] });
  render(<RuntimeWorkspace bootstrap={bootstrap} token="" />);
  expect(await screen.findByRole("button", { name: "Stop" })).toBeDisabled();
  expect(screen.getByRole("button", { name: "Sleep" })).toBeDisabled();
  expect(screen.getByRole("button", { name: "Wake" })).toBeDisabled();
  expect(screen.getByRole("button", { name: "Check status" })).toBeEnabled();
});

it("keeps Stop unavailable for adopted workloads and all controls unavailable to read-only users", async () => {
  mockRuntime({ ...binding, state: "ready", phase: "ready", job_id: "borrowed", owned: false });
  const view = render(<RuntimeWorkspace bootstrap={bootstrap} token="" />);
  await screen.findByText("Adopted · manual stop");
  expect(screen.queryByRole("button", { name: "Stop" })).not.toBeInTheDocument();
  view.unmount();
  mockRuntime(binding);
  render(<RuntimeWorkspace bootstrap={{ ...bootstrap, features: { ...bootstrap.features, sparkrun_controls: false } }} token="" />);
  await screen.findByText("ds4f");
  expect(screen.queryByRole("button", { name: "Start" })).not.toBeInTheDocument();
});

it("shows start failures and permits a retry", async () => {
  mockRuntime(binding, async () => { throw new Error("Cluster unavailable"); });
  render(<RuntimeWorkspace bootstrap={bootstrap} token="" />);
  fireEvent.click(await screen.findByRole("button", { name: "Start" }));
  expect(await screen.findByRole("alert")).toHaveTextContent("Cluster unavailable");
  await waitFor(() => expect(screen.getByRole("button", { name: "Start" })).toBeEnabled());
});
