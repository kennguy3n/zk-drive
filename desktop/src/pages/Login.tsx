import { useState } from "react";

import * as shell from "../api/shell";

/**
 * OAuth2 PKCE login screen.
 *
 * Clicking a provider invokes `begin_login`, which (in the Rust host)
 * generates a PKCE challenge, opens the system browser at the
 * backend's `/api/auth/oauth/{provider}` endpoint, captures the
 * loopback redirect, exchanges the code, and stores the token in the
 * OS keychain. The promise resolves once a token is persisted.
 */
export default function Login({ onAuthenticated }: { onAuthenticated: () => void }) {
  const [busy, setBusy] = useState<shell.Provider | null>(null);
  const [error, setError] = useState<string | null>(null);

  async function login(provider: shell.Provider) {
    setBusy(provider);
    setError(null);
    try {
      const ok = await shell.beginLogin(provider);
      if (ok) onAuthenticated();
    } catch (err) {
      setError(formatError(err));
    } finally {
      setBusy(null);
    }
  }

  return (
    <div className="login">
      <div className="login-card">
        <div className="logo-lg" aria-hidden>
          ◆
        </div>
        <h1>ZK Drive</h1>
        <p className="muted">Sign in to sync your secure workspaces.</p>

        <button className="provider" disabled={busy !== null} onClick={() => login("google")}>
          {busy === "google" ? "Waiting for browser…" : "Continue with Google"}
        </button>
        <button
          className="provider"
          disabled={busy !== null}
          onClick={() => login("microsoft")}
        >
          {busy === "microsoft" ? "Waiting for browser…" : "Continue with Microsoft"}
        </button>

        {error && <p className="error">{error}</p>}
        <p className="hint muted">
          A browser window will open to complete sign-in. Return here once it confirms.
        </p>
      </div>
    </div>
  );
}

function formatError(err: unknown): string {
  if (err && typeof err === "object" && "detail" in err) {
    const detail = (err as { detail: unknown }).detail;
    return typeof detail === "string" ? detail : JSON.stringify(detail);
  }
  return String(err);
}
