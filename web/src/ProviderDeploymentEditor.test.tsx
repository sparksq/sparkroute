import { cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { useState } from "react";
import { afterEach, describe, expect, it, vi } from "vitest";
import { ProviderDeploymentEditor } from "./ProviderDeploymentEditor";
import type { ConfigurationDocument } from "./types";

afterEach(() => { cleanup(); vi.unstubAllGlobals(); });
const provider = { name: "cloud", type: "openai", base_url: "https://api.openai.com/v1" };
function Editor({ initial = { providers: [provider], deployments: [], virtual_models: [] }, disabled = false, simplified = false }: { initial?: ConfigurationDocument; disabled?: boolean; simplified?: boolean }) {
  const [document, setDocument] = useState(initial);
  return <><ProviderDeploymentEditor simplifiedCapabilities={simplified} document={document} disabled={disabled} onChange={setDocument} subscriptionAuth={{ token: "fixture", enabled: true }} /><output aria-label="Draft">{JSON.stringify(document)}</output></>;
}
function draft() { return JSON.parse(screen.getByLabelText("Draft").textContent!); }
function type(value: string) { fireEvent.change(screen.getByLabelText("Provider type"), { target: { value } }); }
function auth(value: string) { fireEvent.change(screen.getByLabelText("Authentication type"), { target: { value } }); }

describe("Native provider configuration", () => {
  it("offers native API types and scopes the Bedrock fields to its regional endpoint", () => {
    render(<Editor />);
    for (const name of ["OpenAI (Chat)", "OpenAI (Responses)", "Anthropic", "Amazon Bedrock"]) {
      expect(screen.getByRole("option", { name })).toBeInTheDocument();
    }
    expect(screen.queryByLabelText("AWS region")).not.toBeInTheDocument();
    type("bedrock");
    expect(screen.getByLabelText("AWS region")).toHaveValue("us-east-1");
    expect(screen.queryByLabelText(/^Base URL/)).not.toBeInTheDocument();
    expect(screen.getByLabelText("Authentication type")).toHaveValue("aws_sigv4");
    expect(screen.getByLabelText(/^Credential reference/)).toHaveValue("workload://aws");
    type("anthropic");
    expect(screen.queryByLabelText("AWS region")).not.toBeInTheDocument();
    expect(screen.getByLabelText(/^Base URL/)).toHaveValue("https://api.anthropic.com/v1");
    expect(screen.getByLabelText("Authentication header")).toHaveValue("x-api-key");
    expect(draft().providers[0]).not.toHaveProperty("region");
  });

  it("sets Responses on existing and new deployments while preserving additional capabilities", () => {
    render(<Editor initial={{ providers: [provider], deployments: [{ name: "target", provider: "cloud", model: "model", capabilities: ["tools"] }], virtual_models: [] }} />);
    type("openai_responses");
    expect(draft().deployments[0].capabilities).toEqual(["tools", "responses"]);
    fireEvent.click(screen.getByRole("button", { name: "Add deployment" }));
    expect(draft().deployments[1].capabilities).toEqual(["responses"]);
    const responses = screen.getByRole("checkbox", { name: "Responses" });
    expect(responses).toBeChecked();
    expect(responses).toBeDisabled();
  });

  it("adds deployments to the selected provider and infers Anthropic protocol", () => {
    render(<Editor initial={{ providers: [provider, { name: "claude", type: "anthropic", base_url: "https://api.anthropic.com/v1" }], deployments: [], virtual_models: [] }} />);
    fireEvent.click(screen.getByRole("button", { name: /claude.*Anthropic/ }));
    fireEvent.click(screen.getByRole("button", { name: "Add deployment" }));
    expect(draft().deployments[0].provider).toBe("claude");
    expect(screen.getByRole("checkbox", { name: "Anthropic" })).toBeChecked();
    expect(screen.getByRole("checkbox", { name: "Openai" })).not.toBeChecked();
  });

  it("retains entity selections across provider and deployment pages and adds to the chosen provider", () => {
    function Sections() {
      const [document, setDocument] = useState<ConfigurationDocument>({ providers: [provider, { name: "second", type: "anthropic", base_url: "https://api.anthropic.com/v1" }], deployments: [], virtual_models: [] });
      const [section, setSection] = useState<"providers" | "deployments">("providers");
      return <>
        <button onClick={() => setSection("providers")}>Providers page</button>
        <button onClick={() => setSection("deployments")}>Deployments page</button>
        <ProviderDeploymentEditor document={document} disabled={false} onChange={setDocument} section={section} />
      </>;
    }
    render(<Sections />);
    fireEvent.click(screen.getByRole("button", { name: /second.*Anthropic/ }));
    fireEvent.click(screen.getByRole("button", { name: "Deployments page" }));
    fireEvent.click(screen.getByRole("button", { name: "Add deployment" }));
    expect(screen.getByLabelText("Provider")).toHaveValue("second");
    fireEvent.click(screen.getByRole("button", { name: "Providers page" }));
    expect(screen.getByLabelText("Provider name")).toHaveValue("second");
    fireEvent.click(screen.getByRole("button", { name: "Deployments page" }));
    expect(screen.getByLabelText("Provider")).toHaveValue("second");
  });

  it("shows only optional vision and file-input declarations while preserving advanced deployment settings", () => {
    const deployment = { name: "target", provider: "cloud", model: "model", capabilities: ["tools", "files", "responses", "x-existing"], native_protocols: ["openai", "anthropic"], capability_policy: { unknown: "try" } };
    render(<Editor simplified initial={{ providers: [provider], deployments: [deployment], virtual_models: [] }} />);
    fireEvent.click(screen.getByRole("button", { name: /target.*cloud/ }));
    expect(screen.queryByText("Native upstream protocols")).not.toBeInTheDocument();
    expect(screen.queryByLabelText("Extension capabilities")).not.toBeInTheDocument();
    expect(within(screen.getByText("Deployment capabilities").closest("details")!).getAllByRole("checkbox").map((input) => input.closest("label")?.textContent)).toEqual(["Vision", "Files (file inputs)"]);
    fireEvent.click(screen.getByRole("checkbox", { name: "Vision" }));
    fireEvent.click(screen.getByRole("checkbox", { name: "Files (file inputs)" }));
    expect(draft().deployments[0]).toEqual({ ...deployment, capabilities: [...deployment.capabilities, "vision", "file_input"] });
    fireEvent.click(screen.getByRole("checkbox", { name: "Vision" }));
    fireEvent.click(screen.getByRole("checkbox", { name: "Files (file inputs)" }));
    expect(draft().deployments[0]).toEqual(deployment);
  });

  it("switches Codex auth on and off without losing the regular URL, credential reference or profile draft", async () => {
    vi.stubGlobal("fetch", vi.fn(async () => new Response(JSON.stringify({ profile: "codex", state: "signed_out" }))));
    render(<Editor initial={{ providers: [{ ...provider, base_url: "https://custom.example/v1", auth: { type: "bearer", credential: "env://MY_KEY" } }], deployments: [{ name: "target", provider: "cloud", model: "model" }], virtual_models: [] }} />);
    expect(screen.queryByRole("button", { name: "Use Codex subscription" })).not.toBeInTheDocument();
    auth("codex_subscription");
    expect(screen.getByLabelText("Provider type")).toHaveValue("openai_responses");
    expect(screen.getByLabelText("Authentication type")).toHaveValue("codex_subscription");
    await waitFor(() => expect(screen.getByRole("button", { name: "Sign in with ChatGPT" })).toBeEnabled());
    fireEvent.change(screen.getByLabelText("Subscription profile"), { target: { value: "personal" } });
    expect(draft().providers[0]).toEqual({ name: "cloud", type: "openai_subscription", subscription_profile: "personal" });
    expect(draft().deployments[0].capabilities).toEqual(["responses"]);
    fireEvent.click(screen.getByRole("button", { name: "Add deployment" }));
    fireEvent.click(screen.getByRole("button", { name: /cloud.*Codex Subscription/ }));
    auth("bearer");
    expect(screen.queryByRole("region", { name: "Codex subscription sign-in" })).not.toBeInTheDocument();
    expect(screen.getByLabelText(/^Base URL/)).toHaveValue("https://custom.example/v1");
    expect(screen.getByLabelText(/^Credential reference/)).toHaveValue("env://MY_KEY");
    expect(draft().providers[0]).not.toHaveProperty("subscription_profile");
    auth("codex_subscription");
    expect(screen.getByLabelText("Subscription profile")).toHaveValue("personal");
    expect(vi.mocked(fetch).mock.calls.some(([url]) => String(url).endsWith("/logout"))).toBe(false);
  });

  it("switches an already saved subscription back to ordinary API authentication", () => {
    vi.stubGlobal("fetch", vi.fn(async () => new Response(JSON.stringify({ profile: "saved", state: "signed_out" }))));
    render(<Editor initial={{ providers: [{ name: "saved", type: "openai_subscription", subscription_profile: "saved" }], deployments: [], virtual_models: [] }} />);
    auth("bearer");
    expect(draft().providers[0]).toEqual({ name: "saved", type: "openai_responses", base_url: "https://api.openai.com/v1", auth: { type: "bearer" } });
    expect(screen.queryByLabelText("Subscription profile")).not.toBeInTheDocument();
  });

  it("does not offer Codex conversion that would erase existing lifecycle or credential configuration", () => {
    render(<Editor initial={{ providers: [provider], deployments: [{ name: "target", provider: "cloud", model: "model", credential: "env://DEPLOYMENT_KEY", endpoint_source: { type: "discovered", controller: "sparkrun" } }], virtual_models: [] }} />);
    expect(screen.getByRole("option", { name: "Codex Subscription" })).toBeDisabled();
    expect(screen.getByText(/Use a separate provider for these deployments/)).toBeVisible();
    expect(draft().deployments[0].credential).toBe("env://DEPLOYMENT_KEY");
  });

  it("read-only subscription configuration does not call credential APIs", () => {
    const fetch = vi.fn();
    vi.stubGlobal("fetch", fetch);
    render(<Editor disabled initial={{ providers: [{ name: "saved", type: "openai_subscription", subscription_profile: "saved" }], deployments: [], virtual_models: [] }} />);
    expect(screen.getByLabelText("Authentication type")).toBeDisabled();
    expect(fetch).not.toHaveBeenCalled();
  });
});
