import { useState } from "react";
import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { afterEach, expect, it, vi } from "vitest";
import { fetchStatus, probeMMProjection } from "./api";
import { MMProjectionSettingsEditor } from "./MMProjectionSettingsEditor";
import type { ConfigurationDocument, GatewayStatus, MMProjectionStatus } from "./types";

vi.mock("./api", () => ({ fetchStatus: vi.fn(), probeMMProjection: vi.fn() }));
afterEach(() => { cleanup(); vi.resetAllMocks(); });

const disabledStatus: MMProjectionStatus = { configured: false, state: "disabled", models: 0, consecutive_failures: 0, circuit_open: false };
function Editor({ readOnly = false, status = false, revision = "a" }: { readOnly?: boolean; status?: boolean; revision?: string }) {
  const [document, setDocument] = useState<ConfigurationDocument>({ providers: [], deployments: [], virtual_models: [], observability: { saved_traces: { enabled: false } } });
  return <><MMProjectionSettingsEditor document={document} disabled={readOnly} onChange={setDocument} token="test" canReadStatus={status} runtimeRevision={revision} /><output aria-label="Draft">{JSON.stringify(document)}</output></>;
}
const draft = () => JSON.parse(screen.getByLabelText("Draft").textContent!);

it("preserves connection fields when disabled and other advanced settings when returning to startup defaults", () => {
  render(<Editor />);
  expect(screen.getByLabelText("Projection connection")).toHaveValue("inherit");
  expect(screen.queryByLabelText("Bridge base URL")).toBeNull();
  fireEvent.change(screen.getByLabelText("Projection connection"), { target: { value: "enabled" } });
  fireEvent.change(screen.getByLabelText("Bridge base URL"), { target: { value: "http://localhost:8100/v1" } });
  fireEvent.change(screen.getByLabelText("Bridge token reference"), { target: { value: "env://BRIDGE_TOKEN" } });
  fireEvent.change(screen.getByLabelText("Default analyzer model"), { target: { value: "vision" } });
  fireEvent.change(screen.getByLabelText("Default projection timeout (seconds)"), { target: { value: "120" } });
  expect(draft().mm_projection).toEqual({ enabled: true, url: "http://localhost:8100/v1", token_ref: "env://BRIDGE_TOKEN", analyzer_model: "vision", timeout_ms: 120000 });
  fireEvent.change(screen.getByLabelText("Projection connection"), { target: { value: "disabled" } });
  expect(draft().mm_projection).toMatchObject({ enabled: false, analyzer_model: "vision" });
  fireEvent.change(screen.getByLabelText("Projection connection"), { target: { value: "enabled" } });
  expect(screen.getByLabelText("Default analyzer model")).toHaveValue("vision");
  fireEvent.change(screen.getByLabelText("Projection connection"), { target: { value: "inherit" } });
  expect(draft()).not.toHaveProperty("mm_projection");
  expect(draft().observability).toEqual({ saved_traces: { enabled: false } });
});

it("refreshes active status after a saved revision and probes independently of draft edits", async () => {
  vi.mocked(fetchStatus).mockResolvedValue({ mm_projection: disabledStatus } as GatewayStatus);
  const view = render(<Editor status />);
  await waitFor(() => expect(screen.getByText("disabled")).toBeVisible());
  expect(screen.getByRole("button", { name: "Test bridge" })).toBeDisabled();
  fireEvent.change(screen.getByLabelText("Projection connection"), { target: { value: "enabled" } });
  expect(screen.getByRole("button", { name: "Test bridge" })).toBeDisabled();
  const active = { ...disabledStatus, configured: true, state: "unknown", analyzer_model: "vision" };
  vi.mocked(fetchStatus).mockResolvedValue({ mm_projection: active } as GatewayStatus);
  view.rerender(<Editor status revision="b" />);
  await waitFor(() => expect(screen.getByRole("button", { name: "Test bridge" })).toBeEnabled());
  vi.mocked(probeMMProjection).mockResolvedValue({ ...active, state: "healthy", models: 2, projection_api: 1 });
  fireEvent.click(screen.getByRole("button", { name: "Test bridge" }));
  await waitFor(() => expect(screen.getByText("healthy")).toBeVisible());
  expect(probeMMProjection).toHaveBeenCalledWith("test");
  expect(screen.getByText("v1")).toBeVisible();
  expect(draft().mm_projection).toEqual({ enabled: true });
});

it("keeps connection edits read-only without exposing status to a config-only reader", () => {
  render(<Editor readOnly />);
  expect(screen.getByLabelText("Projection connection")).toBeDisabled();
  expect(screen.queryByRole("button", { name: "Test bridge" })).toBeNull();
  expect(fetchStatus).not.toHaveBeenCalled();
});

it("reports failed bridge tests without losing the draft", async () => {
  vi.mocked(fetchStatus).mockResolvedValue({ mm_projection: { ...disabledStatus, configured: true } } as GatewayStatus);
  vi.mocked(probeMMProjection).mockRejectedValue(new Error("Bridge test unavailable"));
  render(<Editor status />);
  await waitFor(() => expect(screen.getByRole("button", { name: "Test bridge" })).toBeEnabled());
  fireEvent.click(screen.getByRole("button", { name: "Test bridge" }));
  await waitFor(() => expect(screen.getByText("Bridge test unavailable")).toBeVisible());
  expect(draft()).not.toHaveProperty("mm_projection");
});
