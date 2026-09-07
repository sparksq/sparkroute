import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { useState } from "react";
import { afterEach, describe, expect, it, vi } from "vitest";
import { SubscriptionSignIn } from "./SubscriptionSignIn";
import { ProviderDeploymentEditor } from "./ProviderDeploymentEditor";

const json = (body: unknown, status = 200) => new Response(JSON.stringify(body), { status, headers: { "Content-Type": "application/json" } });
afterEach(() => { cleanup(); vi.unstubAllGlobals(); });

describe("Codex subscription configuration", () => {
  it("guides device login, observes completion, and signs out without exporting credentials", async () => {
    let state = "signed_out";
    let polls = 0;
    const fetch = vi.fn(async (url: string, options?: RequestInit) => {
      expect(new Headers(options?.headers).get("Authorization")).toBe("Bearer admin-test");
      if (url.endsWith("/login")) { state = "pending"; expect(options?.body).toBe("{}"); }
      else if (url.endsWith("/logout")) state = "signed_out";
      else if (state === "pending" && ++polls > 1) state = "connected";
      return json({ profile: "personal", state, ...(state === "pending" ? { user_code: "ABCD-EFGH", verification_url: "https://auth.openai.com/codex/device", login_expires_at: new Date(Date.now() + 600000).toISOString() } : {}), ...(state === "connected" ? { email: "operator@example.test", plan: "pro" } : {}) });
    });
    vi.stubGlobal("fetch", fetch);
    render(<SubscriptionSignIn profile="personal" token="admin-test" enabled />);
    await waitFor(() => expect(screen.getByRole("button", { name: "Sign in with ChatGPT" })).toBeEnabled());
    fireEvent.click(screen.getByRole("button", { name: "Sign in with ChatGPT" }));
    expect(await screen.findByText("ABCD-EFGH")).toBeVisible();
    expect(screen.getByRole("link", { name: "Open OpenAI sign-in" })).toHaveAttribute("href", "https://auth.openai.com/codex/device");
    await waitFor(() => expect(screen.getByText("Connected as operator@example.test")).toBeVisible(), { timeout: 3500 });
    expect(screen.queryByText("ABCD-EFGH")).not.toBeInTheDocument();
    fireEvent.click(screen.getByRole("button", { name: "Sign out of subscription" }));
    await screen.findByText("Not connected");
    expect(fetch.mock.calls.some(([url]) => url.endsWith("/logout"))).toBe(true);
  });

  it("cancels pending login and provides actionable retry after expiry", async () => {
    let state = "expired";
    vi.stubGlobal("fetch", vi.fn(async (url: string) => {
      if (url.endsWith("/login")) state = "pending";
      if (url.endsWith("/cancel")) state = "signed_out";
      return json({ profile: "codex", state, message: state === "expired" ? "The sign-in code expired. Start again." : undefined });
    }));
    render(<SubscriptionSignIn profile="codex" token="test" enabled />);
    fireEvent.click(await screen.findByRole("button", { name: "Try sign-in again" }));
    fireEvent.click(await screen.findByRole("button", { name: "Cancel sign-in" }));
    await screen.findByText("Not connected");
  });

  it("does not call sign-in APIs without permission or for invalid profiles", () => {
    const fetch = vi.fn();
    vi.stubGlobal("fetch", fetch);
    const view = render(<SubscriptionSignIn profile="codex" token="" enabled={false} />);
    expect(screen.getByText(/configuration write access/)).toBeVisible();
    expect(screen.queryByRole("button", { name: "Sign in with ChatGPT" })).not.toBeInTheDocument();
    view.rerender(<SubscriptionSignIn profile="../profile" token="test" enabled />);
    expect(screen.getByText(/Choose a profile name/)).toBeVisible();
    expect(fetch).not.toHaveBeenCalled();
  });

  it("creates a subscription provider and Responses deployment in the editable document", async () => {
    vi.stubGlobal("fetch", vi.fn(async () => json({ profile: "codex", state: "signed_out" })));
    function Editor() {
      const [document, setDocument] = useState({ providers: [{ name: "personal", type: "openai", base_url: "https://api.openai.com/v1", auth: { type: "bearer", credential: "env://OPENAI_KEY" } }], deployments: [], virtual_models: [] } as Record<string, unknown>);
      return <><ProviderDeploymentEditor document={document} disabled={false} onChange={setDocument} subscriptionAuth={{ token: "test", enabled: true }} /><output aria-label="Saved draft">{JSON.stringify(document)}</output></>;
    }
    render(<Editor />);
    fireEvent.change(screen.getByLabelText("Authentication type"), { target: { value: "codex_subscription" } });
    await waitFor(() => expect(screen.getByRole("button", { name: "Sign in with ChatGPT" })).toBeEnabled());
    expect(screen.getByLabelText("Subscription profile")).toHaveValue("codex");
    fireEvent.click(screen.getByRole("button", { name: "Add deployment" }));
    const draft = JSON.parse(screen.getByLabelText("Saved draft").textContent ?? "{}");
    expect(draft.providers).toEqual([{ name: "personal", type: "openai_subscription", subscription_profile: "codex" }]);
    expect(draft.deployments[0].capabilities).toEqual(["responses"]);
    expect(JSON.stringify(draft)).not.toContain("token");
  });
});
