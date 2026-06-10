import { useEffect, useRef, useState } from "react";
import { useNavigate } from "react-router-dom";
import { useTranslation } from "react-i18next";
import { loadAppConfig } from "../hooks/useAppConfig";
import { handleCallback } from "../api/oidc";

// CallbackPage handles the iam-core OAuth2 redirect at /auth/callback.
// It exchanges the authorization code for tokens (PKCE), resolves the
// zk-drive identity, then navigates to the originally requested path.
// On failure it surfaces the error and offers a route back to /login.
export default function CallbackPage() {
  const nav = useNavigate();
  const { t } = useTranslation();
  const [error, setError] = useState<string | null>(null);
  // StrictMode double-invokes effects in dev; the authorization code is
  // single-use, so guard against running the exchange twice.
  const ran = useRef(false);

  useEffect(() => {
    if (ran.current) {
      return;
    }
    ran.current = true;
    void (async () => {
      try {
        const cfg = await loadAppConfig();
        const params = new URLSearchParams(window.location.search);
        const { returnTo } = await handleCallback(cfg, params);
        nav(returnTo, { replace: true });
      } catch (e) {
        setError(e instanceof Error ? e.message : String(e));
      }
    })();
  }, [nav]);

  if (error) {
    return (
      <div style={{ padding: "2rem", textAlign: "center" }}>
        <h1>{t("auth.ssoSignInFailed", "Sign-in failed")}</h1>
        <p style={{ color: "var(--color-danger, #b00020)" }}>{error}</p>
        <button type="button" onClick={() => nav("/login", { replace: true })}>
          {t("auth.backToLogin", "Back to sign in")}
        </button>
      </div>
    );
  }

  return (
    <div style={{ padding: "2rem", textAlign: "center" }}>
      <p>{t("auth.completingSignIn", "Completing sign-in…")}</p>
    </div>
  );
}
