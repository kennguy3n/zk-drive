import { useState } from "react";
import { Link, useNavigate } from "react-router-dom";
import { useTranslation } from "react-i18next";
import AuthForm from "../components/AuthForm";
import { signup } from "../api/client";
import { translateApiError } from "../api/errors";

export default function SignupPage() {
  const nav = useNavigate();
  const { t } = useTranslation();
  const [error, setError] = useState<string | null>(null);

  return (
    <AuthForm
      title={t("auth.signupPageTitle")}
      submitLabel={t("auth.signup")}
      fields={[
        { name: "workspace_name", label: t("auth.workspaceName") },
        { name: "name", label: t("auth.name"), autoComplete: "name" },
        { name: "email", label: t("auth.email"), type: "email", autoComplete: "email" },
        {
          name: "password",
          label: t("auth.password"),
          type: "password",
          autoComplete: "new-password",
        },
      ]}
      onSubmit={async (v) => {
        try {
          setError(null);
          await signup({
            workspace_name: v.workspace_name,
            email: v.email,
            name: v.name,
            password: v.password,
          });
          nav("/drive", { replace: true });
        } catch (err) {
          setError(translateApiError(err, t));
        }
      }}
      error={error}
      footer={
        <span>
          {t("auth.haveAccount")} <Link to="/login">{t("auth.login")}</Link>
        </span>
      }
    />
  );
}
