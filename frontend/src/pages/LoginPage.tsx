import { useState } from "react";
import { Link, useNavigate } from "react-router-dom";
import { useTranslation } from "react-i18next";
import AuthForm from "../components/AuthForm";
import { login } from "../api/client";
import { translateApiError } from "../api/errors";
import { useAppConfig } from "../hooks/useAppConfig";
import { beginLogin } from "../api/oidc";

export default function LoginPage() {
  const nav = useNavigate();
  const { t } = useTranslation();
  const [error, setError] = useState<string | null>(null);
  const { config, loading } = useAppConfig();

  // While the auth mode is unknown, render nothing rather than briefly
  // flashing the password form when the deployment actually uses SSO.
  if (loading) {
    return null;
  }

  // iam-core mode: replace the password form with a single "Sign in with
  // SSO" action that kicks off the Authorization Code + PKCE redirect to
  // iam-core's Universal Login. MFA (TOTP, passkeys) is handled entirely
  // by iam-core during that flow, so there is no MFA UI here.
  if (config && config.auth_mode === "iam-core") {
    return (
      <div className="auth-form" style={{ maxWidth: 360, margin: "4rem auto", textAlign: "center" }}>
        <h1>{t("auth.loginPageTitle")}</h1>
        <p>{t("auth.ssoPrompt", "Your organization uses single sign-on.")}</p>
        {error && <p style={{ color: "var(--color-danger, #b00020)" }}>{error}</p>}
        <button
          type="button"
          onClick={async () => {
            try {
              setError(null);
              await beginLogin(config);
            } catch (e) {
              setError(e instanceof Error ? e.message : String(e));
            }
          }}
        >
          {t("auth.signInWithSso", "Sign in with SSO")}
        </button>
      </div>
    );
  }

  return (
    <AuthForm
      title={t("auth.loginPageTitle")}
      submitLabel={t("auth.login")}
      fields={[
        { name: "email", label: t("auth.email"), type: "email", autoComplete: "email" },
        {
          name: "password",
          label: t("auth.password"),
          type: "password",
          autoComplete: "current-password",
        },
      ]}
      onSubmit={async (v) => {
        try {
          setError(null);
          const resp = await login({ email: v.email, password: v.password });
          if ("mfa_required" in resp && resp.mfa_required) {
            // Hand the short-lived challenge token off via navigation
            // state instead of localStorage so it disappears when the
            // user navigates away. The MFA challenge page exchanges
            // it for a real session token via /auth/totp/verify.
            nav("/mfa-challenge", {
              replace: true,
              state: {
                mfaToken: resp.mfa_token,
                expiresAt: resp.expires_at,
                mustEnroll: resp.must_enroll === true,
              },
            });
            return;
          }
          nav("/drive", { replace: true });
        } catch (err) {
          setError(translateApiError(err, t));
        }
      }}
      error={error}
      footer={
        <span>
          {t("auth.noAccount")} <Link to="/signup">{t("auth.createOne")}</Link>
        </span>
      }
    />
  );
}
