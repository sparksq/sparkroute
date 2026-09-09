import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { afterEach, beforeEach, expect, it, vi } from "vitest";
import { ConfigurationPresetSelector } from "./ConfigurationPresetSelector";
import type { ConfigurationPresets } from "./types";

beforeEach(() => {
  Object.defineProperty(HTMLDialogElement.prototype, "showModal", { configurable: true, value: function(this: HTMLDialogElement) { this.setAttribute("open", ""); } });
  Object.defineProperty(HTMLDialogElement.prototype, "close", { configurable: true, value: function(this: HTMLDialogElement) { this.removeAttribute("open"); } });
});
afterEach(() => { cleanup(); vi.unstubAllGlobals(); Reflect.deleteProperty(HTMLDialogElement.prototype, "showModal"); Reflect.deleteProperty(HTMLDialogElement.prototype, "close"); });

function setup(dirty = false, canWrite = true, fail = false) {
  let catalog: ConfigurationPresets = { active_preset: "work", active_revision: "revision", presets_revision: 3,
    presets: [{ id: "default", name: "Default", revision: "empty", updated_at: "now" }, { id: "work", name: "Work", revision: "config", updated_at: "now" }] };
  const fetch = vi.fn(async (path: string, init?: RequestInit) => {
    if (path === "/v1/config/presets") return Response.json(catalog);
    if (fail) return Response.json({ error: { message: "Recipe changed; review before loading" } }, { status: 422 });
    const body = JSON.parse(String(init?.body));
    if (path.endsWith("/activate")) catalog = { ...catalog, active_preset: body.id };
    if (path.endsWith("/save")) catalog = { ...catalog, active_preset: "new", presets: [...catalog.presets, { id: "new", name: body.name, revision: "config", updated_at: "now" }] };
    if (path.endsWith("/rename")) catalog = { ...catalog, presets: catalog.presets.map(preset => preset.id === body.id ? { ...preset, name: body.name } : preset) };
    if (path.endsWith("/delete")) catalog = { ...catalog, presets: catalog.presets.filter(preset => preset.id !== body.id) };
    catalog = { ...catalog, presets_revision: catalog.presets_revision + 1 };
    return Response.json({ ok: true });
  });
  vi.stubGlobal("fetch", fetch);
  const onApplied = vi.fn();
  const props = { token: "token", canWrite, dirty, locked: false, refreshKey: "1", onApplied, onBusyChange: vi.fn() };
  const view = render(<ConfigurationPresetSelector {...props} />);
  return { ...view, fetch, onApplied, props };
}

it("resumes the selected preset, saves a named copy and loads Default", async () => {
  const { fetch, onApplied } = setup();
  const select = await screen.findByRole("combobox", { name: "Configuration preset" });
  await waitFor(() => expect(select).toHaveValue("work"));
  fireEvent.change(select, { target: { value: "__save" } });
  fireEvent.change(screen.getByRole("textbox", { name: "Preset name" }), { target: { value: "Evening" } });
  fireEvent.click(screen.getByRole("button", { name: "Save preset" }));
  await waitFor(() => expect(select).toHaveValue("new"));
  const save = fetch.mock.calls.find(([path]) => path.endsWith("/save"));
  expect(JSON.parse(String(save?.[1]?.body))).toEqual({ name: "Evening", expected_active_revision: "revision", expected_presets_revision: 3 });
  fireEvent.change(select, { target: { value: "default" } });
  await waitFor(() => expect(select).toHaveValue("default"));
  expect(onApplied).toHaveBeenCalledTimes(2);
  expect(screen.getByText("Loaded Default.")).toBeInTheDocument();
});

it("protects dirty configuration until the user confirms a switch", async () => {
  const { fetch, onApplied } = setup(true);
  const select = screen.getByRole("combobox", { name: "Configuration preset" });
  await waitFor(() => expect(select).toHaveValue("work"));
  expect(screen.getByRole("option", { name: "Save as preset…" })).toBeDisabled();
  fireEvent.change(select, { target: { value: "default" } });
  expect(screen.getByRole("dialog")).toHaveTextContent("unsaved operator configuration draft");
  expect(fetch.mock.calls.filter(([path]) => path.endsWith("/activate"))).toHaveLength(0);
  fireEvent.click(screen.getByRole("button", { name: "Cancel" }));
  expect(select).toHaveValue("work");
  fireEvent.change(select, { target: { value: "default" } });
  fireEvent.click(screen.getByRole("button", { name: "Discard draft and switch" }));
  await waitFor(() => expect(onApplied).toHaveBeenCalledOnce());
  await waitFor(() => expect(select).toHaveValue("default"));
});

it("renames a preset and requires confirmation to delete an inactive preset", async () => {
  const { fetch } = setup();
  const select = screen.getByRole("combobox", { name: "Configuration preset" });
  await waitFor(() => expect(select).toHaveValue("work"));
  fireEvent.change(select, { target: { value: "__manage" } });
  expect(screen.getByRole("button", { name: "Delete preset" })).toBeDisabled();
  fireEvent.change(screen.getByRole("textbox", { name: "Preset name" }), { target: { value: "Office" } });
  fireEvent.click(screen.getByRole("button", { name: "Rename preset" }));
  await screen.findByRole("option", { name: "Office" });
  fireEvent.change(select, { target: { value: "default" } });
  await waitFor(() => expect(select).toHaveValue("default"));
  fireEvent.change(select, { target: { value: "__manage" } });
  fireEvent.click(screen.getByRole("button", { name: "Delete preset" }));
  expect(fetch.mock.calls.filter(([path]) => path.endsWith("/delete"))).toHaveLength(0);
  fireEvent.click(screen.getByRole("button", { name: "Confirm deletion of Office" }));
  await waitFor(() => expect(screen.queryByRole("option", { name: "Office" })).not.toBeInTheDocument());
  expect(screen.getByRole("option", { name: "Default" })).toBeInTheDocument();
});

it("retains the current preset and draft when activation fails", async () => {
  const { onApplied } = setup(true, true, true);
  const select = screen.getByRole("combobox", { name: "Configuration preset" });
  await waitFor(() => expect(select).toHaveValue("work"));
  fireEvent.change(select, { target: { value: "default" } });
  fireEvent.click(screen.getByRole("button", { name: "Discard draft and switch" }));
  expect(await screen.findByRole("alert")).toHaveTextContent("Recipe changed");
  expect(onApplied).not.toHaveBeenCalled();
  expect(select).toHaveValue("work");
});

it("shows the selected preset without permitting read-only users to change it", async () => {
  setup(false, false);
  const select = screen.getByRole("combobox", { name: "Configuration preset" });
  await waitFor(() => expect(select).toHaveValue("work"));
  expect(select).toBeDisabled();
  expect(screen.queryByRole("option", { name: "Save as preset…" })).not.toBeInTheDocument();
});
