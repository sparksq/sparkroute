import { useEffect, useState } from "react";
import { changeProviderSignIn, fetchProviderSignIn, type ProviderSignInStatus } from "./api";

export function SubscriptionSignIn({ profile, token, enabled }: { profile: string; token: string; enabled: boolean }) {
  const [status, setStatus] = useState<ProviderSignInStatus>();
  const [error, setError] = useState("");
  const [busy, setBusy] = useState(false);
  const [refresh, setRefresh] = useState(0);
  const validProfile = /^[A-Za-z0-9_-]{1,64}$/.test(profile);
  const pending = status?.state === "starting" || status?.state === "pending";

  useEffect(() => {
    if (!enabled || !validProfile) return;
    const controller = new AbortController();
    let timer: ReturnType<typeof setTimeout>;
    const load = async () => {
      try {
        const result = await fetchProviderSignIn(token, profile, controller.signal);
        if (controller.signal.aborted) return;
        setStatus(result);
        if (result.state === "pending" || result.state === "starting") timer = setTimeout(() => void load(), 2000);
      } catch (cause) {
        if (!controller.signal.aborted) setError(cause instanceof Error ? cause.message : "Could not check sign-in status.");
      }
    };
    void load();
    return () => { controller.abort(); clearTimeout(timer); };
  }, [enabled, validProfile, profile, token, refresh]);

  const change = async (action: "login" | "cancel" | "logout") => {
    setBusy(true);
    setError("");
    try { setStatus(await changeProviderSignIn(token, profile, action)); }
    catch (cause) { setError(cause instanceof Error ? cause.message : "Could not update sign-in."); }
    finally { setBusy(false); setRefresh((value) => value + 1); }
  };

  return (
    <section className="subscription-sign-in" aria-label="Codex subscription sign-in">
      <p>Use your ChatGPT subscription for Codex through the Responses API. Subscription limits and workspace permissions apply.</p>
      <p>Sign-in belongs to this credential profile. Providers using the same profile share its account and sign-out status. Save your configuration after connecting.</p>
      {!enabled ? <p>Provider sign-in requires a private provider credential store and configuration write access. Managed SQLite configuration enables the store automatically.</p>
        : !validProfile ? <p>Choose a profile name with 1–64 letters, digits, underscores, or hyphens.</p>
        : <>
          <div aria-live="polite">
            {status?.state === "connected" ? <>
              <strong>Connected{status.email ? ` as ${status.email}` : ""}</strong>
              {status.plan ? <p>Plan: {status.plan}</p> : null}
              {status.account_id ? <p>Account: {status.account_id}</p> : null}
              <p>Credentials refresh automatically during use. Sign out before switching accounts.</p>
            </> : pending ? <>
              <strong>{status?.state === "starting" ? "Requesting a sign-in code…" : "Waiting for OpenAI sign-in"}</strong>
              {status?.user_code && status.verification_url === "https://auth.openai.com/codex/device" ? <ol>
                <li>Enable device-code login in your ChatGPT security settings or ask your workspace administrator.</li>
                <li><a href={status.verification_url} target="_blank" rel="noreferrer">Open OpenAI sign-in</a> and enter <strong className="device-code">{status.user_code}</strong>.</li>
                <li>Complete sign-in, then return here. This page will update automatically.</li>
              </ol> : <p>{status?.state === "pending" ? "A sign-in is in progress in another administrator session." : "Contacting OpenAI…"}</p>}
              {status?.login_expires_at ? <p>Code expires by {new Date(status.login_expires_at).toLocaleTimeString()}.</p> : null}
            </> : <strong>{status?.state === "expired" ? "Sign-in expired" : status?.state === "failed" ? "Sign-in failed" : "Not connected"}</strong>}
            {status?.message ? <p>{status.message}</p> : null}
          </div>
          {error ? <p role="alert">{error}</p> : null}
          <div className="button-row">
            {status?.state === "connected" ? <button type="button" disabled={busy} onClick={() => void change("logout")}>Sign out of subscription</button>
              : pending ? <button type="button" disabled={busy} onClick={() => void change("cancel")}>Cancel sign-in</button>
              : <button type="button" disabled={busy || !status} onClick={() => void change("login")}>{status?.state === "failed" || status?.state === "expired" ? "Try sign-in again" : "Sign in with ChatGPT"}</button>}
            {error ? <button type="button" disabled={busy} onClick={() => { setError(""); setRefresh((value) => value + 1); }}>Check status</button> : null}
          </div>
        </>}
      <p><a href="https://learn.chatgpt.com/docs/auth#login-on-headless-devices" target="_blank" rel="noreferrer">OpenAI device-code login help</a></p>
    </section>
  );
}
