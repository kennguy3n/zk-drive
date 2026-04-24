import { useState } from "react";
import { Link, useNavigate } from "react-router-dom";
import AuthForm from "../components/AuthForm";
import { signup } from "../api/client";

export default function SignupPage() {
  const nav = useNavigate();
  const [error, setError] = useState<string | null>(null);

  return (
    <AuthForm
      title="Create a zk-drive workspace"
      submitLabel="Create workspace"
      fields={[
        { name: "workspace_name", label: "Workspace name" },
        { name: "name", label: "Your name", autoComplete: "name" },
        { name: "email", label: "Email", type: "email", autoComplete: "email" },
        { name: "password", label: "Password", type: "password", autoComplete: "new-password" },
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
          setError(extractErr(err));
        }
      }}
      error={error}
      footer={
        <span>
          Already have an account? <Link to="/login">Sign in</Link>
        </span>
      }
    />
  );
}

function extractErr(e: unknown): string {
  const maybe = e as { response?: { data?: string }; message?: string };
  return maybe.response?.data || maybe.message || "Something went wrong";
}
