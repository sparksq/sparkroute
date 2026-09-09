import { cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { afterEach, expect, it, vi } from "vitest";
import { AdminApp } from "./App";
import { OverviewWorkspace } from "./OverviewWorkspace";
import type { AdminBootstrap, GatewayStatus, LifecycleBindingStatus, LifecycleSnapshot, RuntimeEndpoint } from "./types";

afterEach(() => { cleanup(); vi.unstubAllGlobals(); window.history.replaceState({}, "", "/admin/"); });
const bootstrap: AdminBootstrap = { schema_version: 1, edition: "standalone", config_revision: "revision", features: { status: true, lifecycle_status: true, endpoint_inventory: true, runtime_events: true, sparkrun_controls: true } };
const target = { deployment: "local-a", title: "Flash on g610", provider: "sparkrun", model: "deepseek/flash", model_names: ["ds4f"], endpoint_source: "activatable" as const, cold_start: "wait" as const, circuit_state: "closed", admission_available: true, active_requests: 2, max_concurrency: 8, consecutive_failures: 0, failures: 0, samples: 1, ejection_count: 0, half_open_probe_active: false, recent_failures: [] };
const status = { config_revision: "revision", providers: 2, deployments: 2, virtual_models: 2, targets: [target, { ...target, deployment: "hosted", title: "Hosted", endpoint_source: "static", provider: "hosted", active_requests: 0 }] } as unknown as GatewayStatus;
const binding: LifecycleBindingStatus = { deployment: "local-a", virtual_model: "deepseek/flash", controller: "sparkrun", binding_revision: "a", state: "ready", phase: "ready", job_id: "job-a", owned: true, cluster_candidates: ["g610"], updated_at: "2026-09-08T12:00:00Z", active_leases: 2, queued_waiters: 3, queued_body_bytes: 128, max_queued_waiters: 100, max_queued_body_bytes: 1024 };
const endpoint: RuntimeEndpoint = { endpoint_id: "endpoint-a", target: "local-a", served_models: ["deepseek/flash"], controller: "sparkrun", cluster_id: "g610", state: "ready", active_requests: 2, max_concurrency: 8, expired: false };

function mockAPI(options: { bindings?: LifecycleBindingStatus[]; endpoints?: RuntimeEndpoint[]; status?: GatewayStatus; lifecycleFailure?: () => boolean; endpointFailure?: () => boolean; control?: (body: Record<string, string>) => Promise<void> } = {}) {
  const fetch = vi.fn(async (input: string, init?: RequestInit) => {
    const url = new URL(String(input), "http://localhost");
    if (url.pathname === "/v1/ui/bootstrap") return Response.json(bootstrap);
    if (url.pathname === "/v1/status") return Response.json(options.status ?? status);
    if (url.pathname === "/v1/runtime/status") {
      if (options.lifecycleFailure?.()) return Response.json({ error: { message: "Controller unavailable" } }, { status: 503 });
      return Response.json({ observed_at: binding.updated_at, controllers: [], bindings: options.bindings ?? [binding] } satisfies LifecycleSnapshot);
    }
    if (url.pathname === "/v1/runtime/endpoints") {
      if (options.endpointFailure?.()) return Response.json({ error: { message: "Registry unavailable" } }, { status: 503 });
      return Response.json({ endpoints: options.endpoints ?? [endpoint] });
    }
    if (url.pathname === "/v1/runtime/events") return Response.json({ records: [] });
    if (url.pathname === "/v1/sparkrun/workload") { await options.control?.(JSON.parse(String(init?.body))); return Response.json({}); }
    return Response.json({ error: { message: "Unexpected request" } }, { status: 404 });
  });
  vi.stubGlobal("fetch", fetch);
  return fetch;
}

function deploymentRow(name: string) { return within(screen.getByRole("button", { name }).closest("tr")!); }

it("joins overlapping sources by deployment, preserves inactive bindings, and counts traffic once", async () => {
  const fetch = mockAPI({ bindings: [binding, { ...binding, deployment: "local-b", binding_revision: "b", state: "offline", phase: "offline", job_id: undefined, active_leases: 0, queued_waiters: 0 }] });
  render(<OverviewWorkspace bootstrap={bootstrap} token="" initialStatus={status} />);
  await screen.findByRole("button", { name: "local-b" });
  expect(screen.getAllByRole("row")).toHaveLength(4); // Header and three deployments.
  expect(deploymentRow("Flash on g610").getByText("Running")).toBeInTheDocument();
  expect(deploymentRow("local-b").getByRole("button", { name: "Start" })).toBeEnabled();
  expect(screen.getByRole("button", { name: "Requests in flight 2" })).toBeInTheDocument();
  expect(screen.getByRole("button", { name: "Queued requests 3" })).toBeInTheDocument();
  expect(screen.getByRole("button", { name: "Inactive activatable 1" })).toBeInTheDocument();
  expect(fetch.mock.calls.some(([url]) => url.startsWith("/v1/runtime/events"))).toBe(false);
  fireEvent.click(screen.getByRole("button", { name: "Inactive activatable 1" }));
  expect(screen.queryByRole("button", { name: "Flash on g610" })).not.toBeInTheDocument();
  expect(deploymentRow("local-b").getByText("Stopped")).toBeInTheDocument();
  fireEvent.click(screen.getByRole("button", { name: "Clear filters" }));
  fireEvent.change(screen.getByLabelText("Filter deployments"), { target: { value: "g610" } });
  expect(screen.queryByRole("button", { name: "Hosted" })).not.toBeInTheDocument();
  expect(screen.getByRole("button", { name: "Flash on g610" })).toBeInTheDocument();
});

it("keeps lifecycle independent of circuit health and treats stopped on-demand workloads as normal", async () => {
  const data = { ...status, targets: [{ ...target, circuit_state: "open", admission_available: false, active_requests: 0 }, { ...target, deployment: "cold", title: "Cold", active_requests: 0, admission_available: false }] };
  mockAPI({ status: data, bindings: [{ ...binding, active_leases: 0 }, { ...binding, deployment: "cold", state: "offline", phase: "offline", active_leases: 0, queued_waiters: 0 }], endpoints: [] });
  render(<OverviewWorkspace bootstrap={bootstrap} token="" initialStatus={data} />);
  await waitFor(() => expect(deploymentRow("Cold").getByText("Stopped")).toBeInTheDocument());
  expect(deploymentRow("Cold").getByText("Starts on request")).toBeInTheDocument();
  expect(deploymentRow("Cold").getByText("Stopped")).toHaveClass("neutral");
  expect(deploymentRow("Flash on g610").getByText("Running")).toBeInTheDocument();
  expect(deploymentRow("Flash on g610").getByText("Circuit Open")).toBeInTheDocument();
  fireEvent.click(screen.getByRole("button", { name: "Needs attention 1" }));
  expect(screen.queryByRole("button", { name: "Cold" })).not.toBeInTheDocument();
});

it("offers Wake and Stop for sleeping jobs, and blocks all remote changes on adopted jobs", async () => {
  const sleeping = { ...binding, phase: "sleeping", state: "offline", active_leases: 0, lifecycle_actions: ["status", "sleep", "wake"], plugins_in_use: ["coldsnap"] };
  const data = { ...status, targets: [{ ...target, active_requests: 0 }] };
  mockAPI({ status: data, bindings: [sleeping, { ...sleeping, deployment: "adopted", owned: false }], endpoints: [] });
  render(<OverviewWorkspace bootstrap={bootstrap} token="" initialStatus={data} />);
  await screen.findByRole("button", { name: "adopted" });
  const owned = deploymentRow("Flash on g610");
  expect(owned.getByText("Sleeping")).toBeInTheDocument();
  expect(owned.getByRole("button", { name: "Wake" })).toBeEnabled();
  expect(owned.getByRole("button", { name: "Stop" })).toBeEnabled();
  expect(owned.queryByRole("button", { name: "Start" })).not.toBeInTheDocument();
  expect(owned.queryByRole("button", { name: "Sleep" })).not.toBeInTheDocument();
  const adopted = deploymentRow("adopted");
  expect(adopted.getAllByRole("button")).toHaveLength(2); // Details and Check status.
  expect(adopted.getByRole("button", { name: "Check status" })).toBeEnabled();
});

it("retains deployment health on partial failure and disables mutations until lifecycle status recovers", async () => {
  let fail = false;
  const data = { ...status, targets: [{ ...target, active_requests: 0 }] };
  mockAPI({ status: data, bindings: [{ ...binding, active_leases: 0 }], endpoints: [], lifecycleFailure: () => fail });
  const view = render(<OverviewWorkspace bootstrap={bootstrap} token="" initialStatus={data} />);
  expect(await screen.findByRole("button", { name: "Stop" })).toBeEnabled();
  fail = true;
  view.rerender(<OverviewWorkspace bootstrap={bootstrap} token="" initialStatus={data} refreshKey={1} />);
  expect(await screen.findByRole("alert")).toHaveTextContent("Lifecycle status is stale");
  expect(screen.getByRole("button", { name: "Stop" })).toBeDisabled();
  expect(deploymentRow("Flash on g610").getByText("Admitting")).toBeInTheDocument();
  fail = false;
  fireEvent.click(screen.getByRole("button", { name: "Retry lifecycle status" }));
  await waitFor(() => expect(screen.getByRole("button", { name: "Stop" })).toBeEnabled());
  expect(screen.queryByRole("alert")).not.toBeInTheDocument();
});

it("does not assume an unobserved activatable workload is externally managed or stopped", async () => {
  mockAPI({ lifecycleFailure: () => true, endpointFailure: () => true });
  render(<OverviewWorkspace bootstrap={bootstrap} token="" initialStatus={status} />);
  await screen.findByText(/Controller unavailable/);
  expect(deploymentRow("Flash on g610").getAllByText("Unknown").length).toBeGreaterThan(0);
  expect(deploymentRow("Flash on g610").queryByText("Stopped")).not.toBeInTheDocument();
  expect(deploymentRow("Flash on g610").queryByRole("button", { name: "Start" })).not.toBeInTheDocument();
  expect(deploymentRow("Hosted").getByText("Admitting")).toBeInTheDocument();
});

it("fetches every endpoint page and keeps diagnostic filters out of the main inventory", async () => {
  const fetch = vi.fn(async (input: string) => {
    const url = new URL(input, "http://localhost");
    return Response.json(url.searchParams.has("cursor") ? { endpoints: [{ ...endpoint, endpoint_id: "endpoint-b", target: "local-b" }] } : { endpoints: [endpoint], next_cursor: "second" });
  });
  vi.stubGlobal("fetch", fetch);
  render(<OverviewWorkspace bootstrap={{ ...bootstrap, features: { endpoint_inventory: true } }} token="" />);
  await screen.findByRole("button", { name: "local-b" });
  expect(fetch.mock.calls).toHaveLength(2);
  expect(screen.getByRole("button", { name: "Running workloads 2" })).toBeInTheDocument();
  fireEvent.click(screen.getByRole("tab", { name: "Diagnostics" }));
  fireEvent.change(screen.getByLabelText("Runtime target"), { target: { value: "local-b" } });
  fireEvent.click(screen.getByRole("button", { name: "Apply filters" }));
  expect(screen.queryByText("endpoint-a")).not.toBeInTheDocument();
  expect(screen.getByText("endpoint-b")).toBeInTheDocument();
  fireEvent.click(screen.getByRole("tab", { name: "Deployments" }));
  expect(screen.getByRole("button", { name: "Running workloads 2" })).toBeInTheDocument();
  expect(screen.getAllByRole("row")).toHaveLength(3);
});

it("scopes recent activity and full history to the selected deployment", async () => {
  const fetch = mockAPI();
  render(<OverviewWorkspace bootstrap={bootstrap} token="" initialStatus={status} />);
  await screen.findByRole("button", { name: "Stop" });
  fireEvent.click(screen.getByRole("button", { name: "Flash on g610" }));
  const details = screen.getByRole("region", { name: "local-a deployment details" });
  expect(within(details).getByText("Requests can start this workload again after it stops.")).toBeInTheDocument();
  await waitFor(() => expect(fetch.mock.calls.some(([url]) => url.includes("deployment=local-a") && url.includes("limit=5"))).toBe(true));
  fireEvent.click(screen.getByRole("button", { name: "View all activity" }));
  expect(await screen.findByLabelText("Transition deployment")).toHaveValue("local-a");
  expect(fetch.mock.calls.some(([url]) => url.includes("deployment=local-a") && url.includes("limit=100"))).toBe(true);
});

it("keeps a pending control local to its deployment", async () => {
  let complete!: () => void;
  const pending = new Promise<void>(resolve => { complete = resolve; });
  const data = { ...status, targets: [{ ...target, active_requests: 0 }] };
  mockAPI({ status: data, bindings: [{ ...binding, active_leases: 0 }, { ...binding, deployment: "other", state: "offline", phase: "offline", active_leases: 0 }], endpoints: [], control: async () => pending });
  render(<OverviewWorkspace bootstrap={bootstrap} token="" initialStatus={data} />);
  fireEvent.click(await screen.findByRole("button", { name: "Stop" }));
  expect(screen.getByRole("button", { name: "Stop" })).toBeDisabled();
  expect(deploymentRow("other").getByRole("button", { name: "Start" })).toBeEnabled();
  complete();
  await waitFor(() => expect(screen.getByRole("button", { name: "Stop" })).toBeEnabled());
});

it("redirects the legacy Runtime URL into Overview and refreshes without losing its view", async () => {
  window.history.replaceState({}, "", "/admin/runtime");
  const fetch = mockAPI();
  render(<AdminApp productName="SparkRoute" />);
  await screen.findByRole("button", { name: "Stop" });
  expect(window.location.pathname).toBe("/admin/");
  expect(screen.getByRole("link", { name: /Overview/ })).toHaveAttribute("aria-current", "page");
  expect(screen.queryByRole("link", { name: /Runtime/ })).not.toBeInTheDocument();
  expect(screen.queryByText("Content-free runtime operations")).not.toBeInTheDocument();
  fireEvent.click(screen.getByRole("tab", { name: "Diagnostics" }));
  fireEvent.click(screen.getByRole("button", { name: "Refresh" }));
  await waitFor(() => expect(fetch.mock.calls.filter(([url]) => url === "/v1/status")).toHaveLength(2));
  expect(screen.getByRole("tab", { name: "Diagnostics" })).toHaveAttribute("aria-selected", "true");
});
