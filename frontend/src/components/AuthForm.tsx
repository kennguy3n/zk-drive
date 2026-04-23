import { useState, type FormEvent } from "react";

export interface AuthFormField {
  name: string;
  label: string;
  type?: string;
  autoComplete?: string;
}

export interface AuthFormProps {
  title: string;
  submitLabel: string;
  fields: AuthFormField[];
  onSubmit: (values: Record<string, string>) => Promise<void> | void;
  error?: string | null;
  footer?: React.ReactNode;
}

// AuthForm is the generic login/signup scaffolding. Pages pass in the
// fields they care about so we don't duplicate markup across LoginPage
// and SignupPage.
export default function AuthForm({
  title,
  submitLabel,
  fields,
  onSubmit,
  error,
  footer,
}: AuthFormProps) {
  const [values, setValues] = useState<Record<string, string>>(() =>
    Object.fromEntries(fields.map((f) => [f.name, ""])),
  );
  const [busy, setBusy] = useState(false);

  const handleChange = (name: string, value: string) => {
    setValues((prev) => ({ ...prev, [name]: value }));
  };

  const handleSubmit = async (e: FormEvent) => {
    e.preventDefault();
    setBusy(true);
    try {
      await onSubmit(values);
    } finally {
      setBusy(false);
    }
  };

  return (
    <form
      onSubmit={handleSubmit}
      style={{
        maxWidth: 360,
        margin: "64px auto",
        padding: 24,
        border: "1px solid #e5e7eb",
        borderRadius: 8,
        background: "white",
      }}
    >
      <h1 style={{ marginTop: 0, marginBottom: 16, fontSize: 20 }}>{title}</h1>
      {fields.map((f) => (
        <label
          key={f.name}
          style={{ display: "block", marginBottom: 12, fontSize: 13 }}
        >
          <span style={{ display: "block", marginBottom: 4 }}>{f.label}</span>
          <input
            type={f.type ?? "text"}
            autoComplete={f.autoComplete}
            value={values[f.name] ?? ""}
            onChange={(e) => handleChange(f.name, e.target.value)}
            required
            style={{
              width: "100%",
              padding: "8px 10px",
              border: "1px solid #d1d5db",
              borderRadius: 4,
              fontSize: 14,
            }}
          />
        </label>
      ))}
      {error ? (
        <div style={{ color: "#b91c1c", fontSize: 13, marginBottom: 12 }}>{error}</div>
      ) : null}
      <button
        type="submit"
        disabled={busy}
        style={{
          width: "100%",
          padding: "10px 12px",
          background: "#2563eb",
          color: "white",
          border: "none",
          borderRadius: 4,
          fontSize: 14,
          opacity: busy ? 0.6 : 1,
        }}
      >
        {busy ? "Working..." : submitLabel}
      </button>
      {footer ? <div style={{ marginTop: 16, fontSize: 13 }}>{footer}</div> : null}
    </form>
  );
}
