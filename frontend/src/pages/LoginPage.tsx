import { useState } from "react";
import { Link, useNavigate } from "react-router-dom";
import { useTranslation } from "react-i18next";
import AuthForm from "../components/AuthForm";
import { login } from "../api/client";
import { translateApiError } from "../api/errors";

export default function LoginPage() {
  const nav = useNavigate();
  const { t } = useTranslation();
  const [error, setError] = useState<string | null>(null);

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
