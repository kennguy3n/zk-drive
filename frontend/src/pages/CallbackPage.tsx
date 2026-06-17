import { useEffect, useRef, useState } from "react";
import { useNavigate } from "react-router-dom";
import { useTranslation } from "react-i18next";
import { Loader2 } from "lucide-react";
import { loadAppConfig } from "../hooks/useAppConfig";
import { handleCallback } from "../api/oidc";
import { AuthLayout } from "../components/AuthForm";
import { Button } from "../components/ui";

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
      <AuthLayout title={t("auth.ssoSignInFailed")} subtitle={t("auth.ssoSignInFailedBody")}>
        <div
          role="alert"
          className="mb-4 rounded-lg border border-danger/30 bg-danger/10 px-3 py-2 text-sm text-danger"
        >
          {error}
        </div>
        <Button className="w-full" onClick={() => nav("/login", { replace: true })}>
          {t("auth.backToLogin")}
        </Button>
      </AuthLayout>
    );
  }

  return (
    <AuthLayout title={t("auth.completingSignIn")} subtitle={t("auth.completingSignInBody")}>
      <div className="flex flex-col items-center gap-4 py-4" role="status" aria-live="polite">
        <Loader2 className="h-8 w-8 animate-spin text-brand" aria-hidden="true" />
        <span className="sr-only">{t("auth.completingSignIn")}</span>
      </div>
    </AuthLayout>
  );
}
