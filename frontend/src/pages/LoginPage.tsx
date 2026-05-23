import { useState } from "react";
import { Link, useNavigate } from "react-router-dom";
import AuthForm from "../components/AuthForm";
import { login } from "../api/client";

export default function LoginPage() {
  const nav = useNavigate();
  const [error, setError] = useState<string | null>(null);

  return (
    <AuthForm
      title="Sign in to zk-drive"
      submitLabel="Sign in"
      fields={[
        { name: "email", label: "Email", type: "email", autoComplete: "email" },
        { name: "password", label: "Password", type: "password", autoComplete: "current-password" },
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
          setError(extractErr(err));
        }
      }}
      error={error}
      footer={
        <span>
          No account? <Link to="/signup">Create one</Link>
        </span>
      }
    />
  );
}

function extractErr(e: unknown): string {
  const maybe = e as { response?: { data?: string }; message?: string };
  return maybe.response?.data || maybe.message || "Something went wrong";
}
