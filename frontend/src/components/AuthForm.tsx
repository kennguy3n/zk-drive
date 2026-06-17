import { useState, type FormEvent, type ReactNode } from "react";
import { useTranslation } from "react-i18next";
import { ShieldCheck } from "lucide-react";
import { Button, Field, Input } from "./ui";

export interface AuthFormField {
  name: string;
  label: string;
  type?: string;
  autoComplete?: string;
  placeholder?: string;
  /** Field-level hint rendered under the control. */
  hint?: string;
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

export interface AuthLayoutProps {
  title: ReactNode;
  /** Sub-heading rendered under the title. */
  subtitle?: ReactNode;
  /** Logo / brand mark shown in the gradient tile (defaults to a shield). */
  icon?: ReactNode;
  /** Card body. */
  children: ReactNode;
  /** Centered helper row under the card (e.g. "Create a workspace"). */
  footer?: ReactNode;
  /** Card width preset. Auth forms are narrow; enrolment is wider. */
  width?: "sm" | "md" | "lg";
}

const widths: Record<NonNullable<AuthLayoutProps["width"]>, string> = {
  sm: "max-w-sm",
  md: "max-w-md",
  lg: "max-w-lg",
};

// AuthLayout is the shared, KChat-branded chrome for every unauthenticated
// screen (login, signup, SSO, MFA challenge/enrolment, OAuth callback). It
// centres a rounded-card surface on the lavender app background, with a
// gradient brand tile and Mona Sans headline, so the whole auth surface is
// visually consistent and re-themes (incl. dark mode) from tokens alone.
//
// NOTE (cross-workstream): this lives in AuthForm.tsx — an owned file —
// rather than components/ui so the auth pages can share it without touching
// the frozen Phase-0 primitives. It is a good candidate for the coordinator
// to promote into components/ui later.
export function AuthLayout({
  title,
  subtitle,
  icon,
  children,
  footer,
  width = "sm",
}: AuthLayoutProps) {
  return (
    <div className="flex min-h-screen items-center justify-center bg-bg px-4 py-12">
      <div className={`w-full ${widths[width]}`}>
        <div className="mb-6 flex flex-col items-center text-center">
          <span className="mb-4 flex h-14 w-14 items-center justify-center rounded-2xl bg-brand-gradient text-white shadow-glow">
            {icon ?? <ShieldCheck className="h-7 w-7" aria-hidden="true" />}
          </span>
          <h1 className="text-2xl font-bold tracking-tight text-fg">{title}</h1>
          {subtitle && (
            <p className="mt-2 max-w-xs text-sm text-muted">{subtitle}</p>
          )}
        </div>

        <div className="rounded-card border border-border bg-surface p-6 shadow-card">
          {children}
        </div>

        {footer && (
          <div className="mt-6 text-center text-sm text-muted">{footer}</div>
        )}
      </div>
    </div>
  );
}

export interface AuthFormProps {
  title: string;
  /** Optional sub-heading under the title. */
  subtitle?: string;
  /** Primary submit label. Omitted when there are no fields (SSO-only). */
  submitLabel?: string;
  /** Password / credential fields. Empty for an SSO-only screen. */
  fields?: AuthFormField[];
  onSubmit?: (values: Record<string, string>) => Promise<void> | void;
  error?: string | null;
  footer?: ReactNode;
  /** Optional social sign-in buttons (Google/Microsoft via iam-core). */
  socialProviders?: SocialProvider[];
  /** Optional "Sign in with SSO" primary CTA handler. */
  onSSO?: () => void;
}

// AuthForm is the branded login/signup surface. Pages pass the fields they
// need so markup isn't duplicated across LoginPage and SignupPage. Inputs
// are built from the shared Field/Input primitives (label association,
// focus ring, dark mode) and the primary action is a KChat pill Button.
// Social and SSO affordances are optional props — present only when a page
// wires them, keeping the iam-core integration merge clean.
export default function AuthForm({
  title,
  subtitle,
  submitLabel,
  fields = [],
  onSubmit,
  error,
  footer,
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
    if (!onSubmit) return;
    setBusy(true);
    try {
      await onSubmit(values);
    } finally {
      setBusy(false);
    }
  };

  const hasSocial = Boolean(socialProviders && socialProviders.length > 0);
  const hasForm = fields.length > 0;

  return (
    <AuthLayout title={title} subtitle={subtitle} footer={footer}>
      {onSSO && (
        <Button type="button" variant="gradient" className="w-full" onClick={onSSO}>
          {t("auth.signInWithSso")}
        </Button>
      )}

      {hasSocial && (
        <div className={onSSO ? "mt-3 flex flex-col gap-2" : "flex flex-col gap-2"}>
          {socialProviders!.map((p) => (
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

      {(onSSO || hasSocial) && hasForm && (
        <div className="my-4 flex items-center gap-3 text-xs text-muted">
          <span className="h-px flex-1 bg-border" />
          {t("auth.orContinueWith")}
          <span className="h-px flex-1 bg-border" />
        </div>
      )}

      {error && (
        <div
          role="alert"
          className="mb-4 rounded-lg border border-danger/30 bg-danger/10 px-3 py-2 text-sm text-danger"
        >
          {error}
        </div>
      )}

      {hasForm && (
        <form onSubmit={handleSubmit} noValidate className="flex flex-col gap-4">
          {fields.map((f) => (
            <Field key={f.name} label={f.label} hint={f.hint} required>
              {(props) => (
                <Input
                  {...props}
                  type={f.type ?? "text"}
                  autoComplete={f.autoComplete}
                  placeholder={f.placeholder}
                  value={values[f.name] ?? ""}
                  onChange={(e) => handleChange(f.name, e.target.value)}
                />
              )}
            </Field>
          ))}

          <Button type="submit" className="w-full" loading={busy}>
            {busy ? t("common.working") : (submitLabel ?? t("auth.login"))}
          </Button>
        </form>
      )}
    </AuthLayout>
  );
}
