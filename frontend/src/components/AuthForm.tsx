import { useState, type FormEvent, type ReactNode } from "react";
import { useTranslation } from "react-i18next";
import { ShieldCheck } from "lucide-react";
import { Button } from "./ui/Button";
import { cn } from "../lib/cn";

export interface AuthFormField {
  name: string;
  label: string;
  type?: string;
  autoComplete?: string;
}

// SocialProvider describes an optional third-party sign-in button. The
// actual OIDC redirect is owned by the iam-core workstream; AuthForm only
// renders the button and calls back, so wiring is a one-line prop later.
export interface SocialProvider {
  id: string;
  label: string;
  icon?: ReactNode;
  onClick: () => void;
}

export interface AuthFormProps {
  title: string;
  submitLabel: string;
  fields: AuthFormField[];
  onSubmit: (values: Record<string, string>) => Promise<void> | void;
  error?: string | null;
  footer?: ReactNode;
  /** Optional sub-heading under the title. */
  subtitle?: string;
  /** Optional social sign-in buttons (Google/Microsoft via iam-core). */
  socialProviders?: SocialProvider[];
  /** Optional "Sign in with SSO" primary CTA handler. */
  onSSO?: () => void;
}

// AuthForm is the branded login/signup surface. Pages pass the fields they
// need so markup isn't duplicated across LoginPage and SignupPage. Styling
// uses semantic tokens so it adapts to dark mode automatically. Social and
// SSO affordances are optional props — present only when a page wires them,
// keeping the iam-core integration merge clean.
export default function AuthForm({
  title,
  submitLabel,
  fields,
  onSubmit,
  error,
  footer,
  subtitle,
  socialProviders,
  onSSO,
}: AuthFormProps) {
  const { t } = useTranslation();
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
    <div className="flex min-h-screen items-center justify-center bg-bg px-4 py-12">
      <div className="w-full max-w-sm">
        <div className="mb-6 flex flex-col items-center text-center">
          <span className="mb-3 flex h-12 w-12 items-center justify-center rounded-xl bg-brand text-brand-fg">
            <ShieldCheck className="h-7 w-7" aria-hidden="true" />
          </span>
          <h1 className="text-xl font-bold text-fg">{title}</h1>
          {subtitle && <p className="mt-1 text-sm text-muted">{subtitle}</p>}
        </div>

        <div className="rounded-card border border-border bg-surface p-6 shadow-card">
          {onSSO && (
            <Button type="button" className="mb-3 w-full" onClick={onSSO}>
              {t("auth.signInWithSSO", { defaultValue: "Sign in with SSO" })}
            </Button>
          )}

          {socialProviders && socialProviders.length > 0 && (
            <div className="mb-3 flex flex-col gap-2">
              {socialProviders.map((p) => (
                <Button
                  key={p.id}
                  type="button"
                  variant="secondary"
                  className="w-full"
                  onClick={p.onClick}
                >
                  {p.icon}
                  {p.label}
                </Button>
              ))}
            </div>
          )}

          {(onSSO || (socialProviders && socialProviders.length > 0)) && (
            <div className="my-4 flex items-center gap-3 text-xs text-muted">
              <span className="h-px flex-1 bg-border" />
              {t("auth.orContinueWith", { defaultValue: "or" })}
              <span className="h-px flex-1 bg-border" />
            </div>
          )}

          <form onSubmit={handleSubmit} noValidate>
            {fields.map((f) => (
              <label key={f.name} className="mb-3 block text-sm">
                <span className="mb-1.5 block font-medium text-fg">{f.label}</span>
                <input
                  type={f.type ?? "text"}
                  autoComplete={f.autoComplete}
                  value={values[f.name] ?? ""}
                  onChange={(e) => handleChange(f.name, e.target.value)}
                  required
                  className={cn(
                    "w-full rounded-lg border border-border bg-bg px-3 py-2 text-sm text-fg",
                    "placeholder:text-muted focus-visible:outline-none focus-visible:ring-2",
                    "focus-visible:ring-ring focus-visible:border-brand",
                  )}
                />
              </label>
            ))}

            {error && (
              <div
                role="alert"
                className="mb-3 rounded-lg border border-danger/30 bg-danger/10 px-3 py-2 text-sm text-danger"
              >
                {error}
              </div>
            )}

            <Button type="submit" className="w-full" loading={busy}>
              {busy ? t("common.working") : submitLabel}
            </Button>
          </form>
        </div>

        {footer && <div className="mt-5 text-center text-sm text-muted">{footer}</div>}
      </div>
    </div>
  );
}
