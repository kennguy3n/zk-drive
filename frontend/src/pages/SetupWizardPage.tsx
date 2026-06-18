import { useCallback, useEffect, useState, type ReactNode } from "react";
import { useNavigate } from "react-router-dom";
import { useTranslation } from "react-i18next";
import {
  Boxes,
  Building2,
  Check,
  Database,
  ShieldCheck,
  UserCog,
  UserPlus,
} from "lucide-react";
import {
  completeSetup,
  fetchSetupStatus,
  inviteUser,
  signup,
  testSetupStorage,
  type SetupStatus,
} from "../api/client";
import { translateApiError } from "../api/errors";
import { Badge, Button, Field, Input, RadioCard, useToast } from "../components/ui";

// SetupWizardPage is the first-boot guided setup. It walks a
// brand-new operator through the minimum needed to run ZK Drive:
//
//   1. Admin account   — collect the first administrator's credentials.
//   2. Storage         — verify the S3/Fabric connection works.
//   3. Optional services — show which integrations the deployment has.
//   4. Workspace       — name it; this is the commit point that calls
//                        signup() to create the admin + workspace and
//                        authenticate the browser.
//   5. Invite          — optionally add the first teammate.
//
// signup() couples admin + workspace creation server-side, so the
// wizard collects the admin fields in step 1 and fires signup in step 4
// (workspace creation), which is also the moment the browser gains a
// session — required for the admin-only invite in step 5 and the
// admin-only complete call at the end.
//
// The page is reachable unauthenticated (a fresh box has no admin yet).
// It redirects away to /drive if setup is already complete so it can't
// be re-run against a provisioned install.

type StepId = "admin" | "storage" | "services" | "workspace" | "invite";
const STEP_ORDER: StepId[] = ["admin", "storage", "services", "workspace", "invite"];
// Workspace is the commit point: signup() fires here, creating the admin
// account + workspace. Once that has happened the earlier steps describe
// data that is already persisted server-side, so navigation back to them
// is locked to stop the displayed values drifting from what was created.
const WORKSPACE_INDEX = STEP_ORDER.indexOf("workspace");
const STEP_ICONS: Record<StepId, ReactNode> = {
  admin: <UserCog className="h-4 w-4" aria-hidden="true" />,
  storage: <Database className="h-4 w-4" aria-hidden="true" />,
  services: <Boxes className="h-4 w-4" aria-hidden="true" />,
  workspace: <Building2 className="h-4 w-4" aria-hidden="true" />,
  invite: <UserPlus className="h-4 w-4" aria-hidden="true" />,
};

interface AdminForm {
  email: string;
  name: string;
  password: string;
}

interface AdminErrors {
  email?: string;
  name?: string;
  password?: string;
}

interface StorageForm {
  endpoint: string;
  bucket: string;
  access_key: string;
  secret_key: string;
  region: string;
}

const EMAIL_RE = /^[^@\s]+@[^@\s]+\.[^@\s]+$/;
const MIN_PASSWORD = 12;

export default function SetupWizardPage() {
  const { t } = useTranslation();
  const navigate = useNavigate();

  const [status, setStatus] = useState<SetupStatus | null>(null);
  const [stepIndex, setStepIndex] = useState(0);
  const [admin, setAdmin] = useState<AdminForm>({ email: "", name: "", password: "" });
  const [adminErrors, setAdminErrors] = useState<AdminErrors>({});
  const [workspaceName, setWorkspaceName] = useState("");
  const [workspaceError, setWorkspaceError] = useState<string | null>(null);
  const [signedUp, setSignedUp] = useState(false);
  const [committing, setCommitting] = useState(false);
  const [finishing, setFinishing] = useState(false);
  const [done, setDone] = useState(false);
  const [error, setError] = useState<string | null>(null);

  // On mount, read setup status. If the install is already set up, this
  // wizard must not run again — bounce to the drive.
  useEffect(() => {
    let cancelled = false;
    (async () => {
      try {
        const s = await fetchSetupStatus();
        if (cancelled) return;
        if (s.setup_completed || !s.needs_setup) {
          navigate("/drive", { replace: true });
          return;
        }
        setStatus(s);
      } catch (e) {
        if (!cancelled) setError(translateApiError(e, t));
      }
    })();
    return () => {
      cancelled = true;
    };
  }, [navigate, t]);

  const step = STEP_ORDER[stepIndex];
  const goNext = useCallback(() => {
    setError(null);
    setStepIndex((i) => Math.min(i + 1, STEP_ORDER.length - 1));
  }, []);
  const goBack = useCallback(() => {
    setError(null);
    setStepIndex((i) => Math.max(i - 1, signedUp ? WORKSPACE_INDEX : 0));
  }, [signedUp]);
  // The stepper lets the operator jump back to any already-visited step to
  // review or correct an entry. Forward jumps stay gated behind each step's
  // validation, and once signup has committed (signedUp) the pre-commit
  // steps are locked so navigation can't surface stale, no-longer-editable
  // account/workspace data.
  const goToStep = useCallback(
    (i: number) => {
      if (i > stepIndex) return;
      if (signedUp && i < WORKSPACE_INDEX) return;
      setError(null);
      setStepIndex(i);
    },
    [stepIndex, signedUp],
  );

  const validateAdmin = useCallback((): boolean => {
    const errs: AdminErrors = {};
    if (admin.email.trim() === "") {
      errs.email = t("setup.admin.errorEmailRequired");
    } else if (!EMAIL_RE.test(admin.email.trim())) {
      errs.email = t("setup.admin.errorEmailInvalid");
    }
    if (admin.name.trim() === "") {
      errs.name = t("setup.admin.errorNameRequired");
    }
    if (admin.password.length < MIN_PASSWORD) {
      errs.password = t("setup.admin.errorPasswordShort");
    }
    setAdminErrors(errs);
    return Object.keys(errs).length === 0;
  }, [admin, t]);

  const handleAdminNext = useCallback(() => {
    if (validateAdmin()) goNext();
  }, [validateAdmin, goNext]);

  // updateAdmin writes a field and clears that field's error, so a
  // corrected value drops its stale message right away while the other
  // fields keep theirs until the next validation pass.
  const updateAdmin = useCallback((field: keyof AdminForm, value: string) => {
    setAdmin((prev) => ({ ...prev, [field]: value }));
    setAdminErrors((prev) => (prev[field] ? { ...prev, [field]: undefined } : prev));
  }, []);

  // commitWorkspace fires signup() — the admin + workspace creation
  // commit point — then advances. Two guards keep it firing exactly once:
  // signedUp stops a re-submit after a forward-then-back navigation, and
  // committing stops a concurrent double-click landing two signup() calls
  // before signedUp has been set on the first response.
  const commitWorkspace = useCallback(async () => {
    setError(null);
    if (workspaceName.trim() === "") {
      setWorkspaceError(t("setup.workspace.errorRequired"));
      return;
    }
    setWorkspaceError(null);
    if (signedUp) {
      goNext();
      return;
    }
    if (committing) {
      return;
    }
    setCommitting(true);
    try {
      await signup({
        workspace_name: workspaceName.trim(),
        email: admin.email.trim(),
        name: admin.name.trim(),
        password: admin.password,
      });
      setSignedUp(true);
      goNext();
    } catch (e) {
      setError(translateApiError(e, t));
    } finally {
      setCommitting(false);
    }
  }, [admin, workspaceName, signedUp, committing, goNext, t]);

  const finish = useCallback(async () => {
    setFinishing(true);
    setError(null);
    try {
      // Only mark complete if we actually created the account/workspace
      // (have a session). If the operator somehow reached finish without
      // signing up, completeSetup would 401 — guard by requiring signedUp.
      if (signedUp) {
        await completeSetup();
      }
      setDone(true);
      window.setTimeout(() => navigate("/drive", { replace: true }), 1500);
    } catch (e) {
      setError(translateApiError(e, t));
    } finally {
      setFinishing(false);
    }
  }, [signedUp, navigate, t]);

  if (done) {
    return (
      <Shell>
        <div className="flex flex-col items-center gap-4 py-6 text-center">
          <span className="flex h-14 w-14 items-center justify-center rounded-2xl bg-brand-gradient text-white shadow-glow">
            <Check className="h-7 w-7" aria-hidden="true" />
          </span>
          <div>
            <h1 className="text-2xl font-bold tracking-tight text-fg">{t("setup.done")}</h1>
            <p className="mt-2 text-sm text-muted">{t("setup.doneBody")}</p>
          </div>
          <Button variant="gradient" onClick={() => navigate("/drive", { replace: true })}>
            {t("setup.goToDrive")}
          </Button>
        </div>
      </Shell>
    );
  }

  return (
    <Shell>
      <header className="mb-6 flex items-start gap-3">
        <span className="flex h-11 w-11 shrink-0 items-center justify-center rounded-2xl bg-brand-gradient text-white shadow-glow">
          <ShieldCheck className="h-6 w-6" aria-hidden="true" />
        </span>
        <div>
          <h1 className="text-2xl font-bold tracking-tight text-fg">{t("setup.title")}</h1>
          <p className="mt-1 text-sm text-muted">{t("setup.subtitle")}</p>
        </div>
      </header>

      <Stepper
        current={stepIndex}
        lockedBefore={signedUp ? WORKSPACE_INDEX : 0}
        onStepClick={goToStep}
      />

      <p className="mb-4 text-xs font-medium uppercase tracking-wide text-muted">
        {t("setup.stepLabel", { current: stepIndex + 1, total: STEP_ORDER.length })}
      </p>

      {error && (
        <div
          role="alert"
          className="mb-4 rounded-lg border border-danger/30 bg-danger/10 px-3 py-2 text-sm text-danger"
        >
          {error}
        </div>
      )}

      <div className="min-h-[260px]">
        {step === "admin" && (
          <AdminStep admin={admin} onChange={updateAdmin} errors={adminErrors} />
        )}
        {step === "storage" && <StorageStep status={status} />}
        {step === "services" && <ServicesStep status={status} />}
        {step === "workspace" && (
          <WorkspaceStep
            workspaceName={workspaceName}
            setWorkspaceName={setWorkspaceName}
            signedUp={signedUp}
            error={workspaceError}
          />
        )}
        {step === "invite" && <InviteStep />}
      </div>

      <footer className="mt-8 flex items-center justify-between gap-3 border-t border-border pt-5">
        <Button
          variant="ghost"
          onClick={goBack}
          disabled={stepIndex <= (signedUp ? WORKSPACE_INDEX : 0) || finishing}
        >
          {t("setup.back")}
        </Button>
        <div className="flex items-center gap-2">
          {step === "admin" && (
            <Button variant="gradient" onClick={handleAdminNext}>
              {t("setup.next")}
            </Button>
          )}
          {(step === "storage" || step === "services") && (
            <Button variant="gradient" onClick={goNext}>
              {t("setup.next")}
            </Button>
          )}
          {step === "workspace" && (
            <Button
              variant="gradient"
              onClick={commitWorkspace}
              loading={committing}
              disabled={committing}
            >
              {committing ? t("setup.creating") : t("setup.next")}
            </Button>
          )}
          {step === "invite" && (
            <Button variant="gradient" onClick={finish} loading={finishing} disabled={finishing}>
              {finishing ? t("setup.finishing") : t("setup.finish")}
            </Button>
          )}
        </div>
      </footer>
    </Shell>
  );
}

// Shell is the branded full-bleed first-run surface: a centred rounded
// card over the lavender app background with a soft brand glow flourish.
function Shell({ children }: { children: ReactNode }) {
  return (
    <div className="relative flex min-h-screen items-center justify-center overflow-hidden bg-bg px-4 py-12">
      <div
        aria-hidden="true"
        className="pointer-events-none absolute -top-32 left-1/2 h-72 w-72 -translate-x-1/2 rounded-full bg-brand/20 blur-3xl"
      />
      <div className="relative w-full max-w-2xl rounded-card border border-border bg-surface p-8 shadow-card">
        {children}
      </div>
    </div>
  );
}

function Stepper({
  current,
  lockedBefore,
  onStepClick,
}: {
  current: number;
  lockedBefore: number;
  onStepClick: (i: number) => void;
}) {
  const { t } = useTranslation();
  return (
    <nav aria-label={t("setup.progressLabel")} className="mb-2">
      <ol className="flex flex-wrap gap-x-5 gap-y-2">
        {STEP_ORDER.map((id, i) => {
          const state = i < current ? "done" : i === current ? "active" : "todo";
          const clickable = i <= current && i >= lockedBefore;
          return (
            <li key={id}>
              <button
                type="button"
                onClick={() => onStepClick(i)}
                disabled={!clickable}
                aria-current={state === "active" ? "step" : undefined}
                className={
                  "group flex items-center gap-2 rounded-full text-sm transition-colors " +
                  "focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring focus-visible:ring-offset-2 focus-visible:ring-offset-surface " +
                  (clickable ? "cursor-pointer" : "cursor-default")
                }
              >
                <span
                  className={
                    "flex h-7 w-7 shrink-0 items-center justify-center rounded-full text-xs font-bold transition-colors " +
                    (state === "active"
                      ? "bg-brand text-brand-fg"
                      : state === "done"
                        ? "bg-success text-white"
                        : "bg-surface-2 text-muted")
                  }
                >
                  {state === "done" ? <Check className="h-4 w-4" aria-hidden="true" /> : i + 1}
                </span>
                <span
                  className={
                    "hidden font-medium sm:inline " +
                    (state === "todo" ? "text-muted" : "text-fg")
                  }
                >
                  {t(`setup.step.${id}`)}
                </span>
              </button>
            </li>
          );
        })}
      </ol>
    </nav>
  );
}

function StepHeading({ icon, title, body }: { icon: ReactNode; title: string; body: string }) {
  return (
    <div className="mb-5">
      <h2 className="flex items-center gap-2 text-lg font-semibold text-fg">
        <span className="flex h-7 w-7 items-center justify-center rounded-lg bg-brand/10 text-brand">
          {icon}
        </span>
        {title}
      </h2>
      <p className="mt-1.5 text-sm text-muted">{body}</p>
    </div>
  );
}

function AdminStep({
  admin,
  onChange,
  errors,
}: {
  admin: AdminForm;
  onChange: (field: keyof AdminForm, value: string) => void;
  errors: AdminErrors;
}) {
  const { t } = useTranslation();
  const pwLen = admin.password.length;
  const pwValid = pwLen >= MIN_PASSWORD;
  // Live password feedback: warn as soon as the operator starts typing
  // a too-short password, and confirm in success colour once it's long
  // enough — no need to wait for the Continue click.
  const pwError = pwLen > 0 && !pwValid ? t("setup.admin.errorPasswordShort") : errors.password;
  const pwHint: ReactNode = pwValid ? (
    <span className="text-success">{t("setup.admin.passwordOk")}</span>
  ) : (
    t("setup.admin.passwordHint")
  );

  return (
    <section>
      <StepHeading
        icon={STEP_ICONS.admin}
        title={t("setup.admin.title")}
        body={t("setup.admin.body")}
      />
      <div className="flex flex-col gap-4">
        <Field label={t("setup.admin.email")} error={errors.email} required>
          {(props) => (
            <Input
              {...props}
              type="email"
              autoComplete="email"
              value={admin.email}
              onChange={(e) => onChange("email", e.target.value)}
            />
          )}
        </Field>
        <Field label={t("setup.admin.name")} error={errors.name} required>
          {(props) => (
            <Input
              {...props}
              autoComplete="name"
              value={admin.name}
              onChange={(e) => onChange("name", e.target.value)}
            />
          )}
        </Field>
        <Field label={t("setup.admin.password")} hint={pwHint} error={pwError} required>
          {(props) => (
            <Input
              {...props}
              type="password"
              autoComplete="new-password"
              value={admin.password}
              onChange={(e) => onChange("password", e.target.value)}
            />
          )}
        </Field>
      </div>
    </section>
  );
}

function StorageStep({ status }: { status: SetupStatus | null }) {
  const { t } = useTranslation();
  const toast = useToast();
  const configured = status?.steps?.storage.configured ?? false;
  const [form, setForm] = useState<StorageForm>({
    endpoint: "",
    bucket: "",
    access_key: "",
    secret_key: "",
    region: "",
  });
  const [testing, setTesting] = useState(false);
  const [result, setResult] = useState<{ ok: boolean; error?: string } | null>(null);

  const canTest =
    form.endpoint.trim() !== "" &&
    form.bucket.trim() !== "" &&
    form.access_key.trim() !== "" &&
    form.secret_key !== "";

  const runTest = useCallback(async () => {
    setTesting(true);
    setResult(null);
    try {
      const r = await testSetupStorage({
        endpoint: form.endpoint.trim(),
        bucket: form.bucket.trim(),
        access_key: form.access_key.trim(),
        secret_key: form.secret_key,
        region: form.region.trim() || undefined,
      });
      setResult(r);
      if (r.ok) toast.success(t("setup.storage.testOk"));
    } catch {
      setResult({ ok: false, error: "request failed" });
    } finally {
      setTesting(false);
    }
  }, [form, toast, t]);

  return (
    <section>
      <StepHeading
        icon={STEP_ICONS.storage}
        title={t("setup.storage.title")}
        body={t("setup.storage.body")}
      />
      <div className="mb-4">
        <Badge tone={configured ? "success" : "neutral"} dot>
          {configured ? t("setup.storage.configured") : t("setup.storage.notConfigured")}
        </Badge>
      </div>
      <div className="flex flex-col gap-4">
        <Field label={t("setup.storage.endpoint")}>
          {(props) => (
            <Input
              {...props}
              placeholder="https://s3.example.com"
              value={form.endpoint}
              onChange={(e) => setForm({ ...form, endpoint: e.target.value })}
            />
          )}
        </Field>
        <Field label={t("setup.storage.bucket")}>
          {(props) => (
            <Input
              {...props}
              value={form.bucket}
              onChange={(e) => setForm({ ...form, bucket: e.target.value })}
            />
          )}
        </Field>
        <Field label={t("setup.storage.accessKey")}>
          {(props) => (
            <Input
              {...props}
              value={form.access_key}
              onChange={(e) => setForm({ ...form, access_key: e.target.value })}
            />
          )}
        </Field>
        <Field label={t("setup.storage.secretKey")}>
          {(props) => (
            <Input
              {...props}
              type="password"
              value={form.secret_key}
              onChange={(e) => setForm({ ...form, secret_key: e.target.value })}
            />
          )}
        </Field>
        <Field label={t("setup.storage.region")} hint={t("setup.storage.regionHint")}>
          {(props) => (
            <Input
              {...props}
              value={form.region}
              onChange={(e) => setForm({ ...form, region: e.target.value })}
            />
          )}
        </Field>
      </div>
      <p className="mt-4 text-sm text-muted">{t("setup.storage.testHint")}</p>
      <div className="mt-3 flex items-center gap-3">
        <Button variant="secondary" onClick={runTest} loading={testing} disabled={!canTest || testing}>
          {testing ? t("setup.storage.testing") : t("setup.storage.test")}
        </Button>
        {result?.ok === true && (
          <span className="text-sm font-medium text-success">{t("setup.storage.testOk")}</span>
        )}
        {result?.ok === false && (
          <span className="text-sm font-medium text-danger">
            {t("setup.storage.testFailed", { error: result.error ?? "" })}
          </span>
        )}
      </div>
    </section>
  );
}

function ServicesStep({ status }: { status: SetupStatus | null }) {
  const { t } = useTranslation();
  const svc = status?.steps?.optional_services;
  const rows: { key: keyof NonNullable<typeof svc>; label: string }[] = [
    { key: "email", label: t("setup.services.email") },
    { key: "virus_scanning", label: t("setup.services.virus_scanning") },
    { key: "ai", label: t("setup.services.ai") },
    { key: "collaborative_editing", label: t("setup.services.collaborative_editing") },
  ];
  return (
    <section>
      <StepHeading
        icon={STEP_ICONS.services}
        title={t("setup.services.title")}
        body={t("setup.services.body")}
      />
      <ul className="divide-y divide-border rounded-card border border-border">
        {rows.map((row) => {
          const on = svc ? Boolean(svc[row.key]) : false;
          return (
            <li key={row.key} className="flex items-center justify-between gap-3 px-4 py-3">
              <span className="text-sm text-fg">{row.label}</span>
              <Badge tone={on ? "success" : "neutral"} dot>
                {on ? t("setup.services.enabled") : t("setup.services.disabled")}
              </Badge>
            </li>
          );
        })}
      </ul>
    </section>
  );
}

function WorkspaceStep({
  workspaceName,
  setWorkspaceName,
  signedUp,
  error,
}: {
  workspaceName: string;
  setWorkspaceName: (s: string) => void;
  signedUp: boolean;
  error: string | null;
}) {
  const { t } = useTranslation();
  return (
    <section>
      <StepHeading
        icon={STEP_ICONS.workspace}
        title={t("setup.workspace.title")}
        body={t("setup.workspace.body")}
      />
      <Field
        label={t("setup.workspace.name")}
        error={error ?? undefined}
        hint={signedUp ? t("setup.workspace.createdNote") : undefined}
        required
      >
        {(props) => (
          <Input
            {...props}
            value={workspaceName}
            disabled={signedUp}
            onChange={(e) => setWorkspaceName(e.target.value)}
          />
        )}
      </Field>
    </section>
  );
}

function InviteStep() {
  const { t } = useTranslation();
  const toast = useToast();
  const [form, setForm] = useState({ email: "", name: "", password: "", role: "member" });
  const [sending, setSending] = useState(false);
  const [error, setError] = useState<string | null>(null);

  const canSend = form.email.trim() !== "" && form.name.trim() !== "" && form.password !== "";

  const send = useCallback(async () => {
    setSending(true);
    setError(null);
    try {
      await inviteUser({
        email: form.email.trim(),
        name: form.name.trim(),
        password: form.password,
        role: form.role,
      });
      toast.success(t("setup.invite.sent", { email: form.email.trim() }));
      setForm({ email: "", name: "", password: "", role: "member" });
    } catch (e) {
      setError(translateApiError(e, t));
    } finally {
      setSending(false);
    }
  }, [form, toast, t]);

  return (
    <section>
      <StepHeading
        icon={STEP_ICONS.invite}
        title={t("setup.invite.title")}
        body={t("setup.invite.body")}
      />
      {error && (
        <div
          role="alert"
          className="mb-4 rounded-lg border border-danger/30 bg-danger/10 px-3 py-2 text-sm text-danger"
        >
          {error}
        </div>
      )}
      <div className="flex flex-col gap-4">
        <Field label={t("setup.invite.email")}>
          {(props) => (
            <Input
              {...props}
              type="email"
              value={form.email}
              onChange={(e) => setForm({ ...form, email: e.target.value })}
            />
          )}
        </Field>
        <Field label={t("setup.invite.name")}>
          {(props) => (
            <Input
              {...props}
              value={form.name}
              onChange={(e) => setForm({ ...form, name: e.target.value })}
            />
          )}
        </Field>
        <Field label={t("setup.invite.password")}>
          {(props) => (
            <Input
              {...props}
              type="password"
              autoComplete="new-password"
              value={form.password}
              onChange={(e) => setForm({ ...form, password: e.target.value })}
            />
          )}
        </Field>
        <fieldset>
          <legend className="mb-1.5 text-sm font-medium text-fg">{t("setup.invite.role")}</legend>
          <div
            role="radiogroup"
            aria-label={t("setup.invite.role")}
            className="grid gap-2 sm:grid-cols-2"
          >
            <RadioCard
              selected={form.role === "member"}
              onSelect={() => setForm({ ...form, role: "member" })}
              title={t("setup.invite.roleMember")}
              description={t("setup.invite.roleMemberDesc")}
            />
            <RadioCard
              selected={form.role === "admin"}
              onSelect={() => setForm({ ...form, role: "admin" })}
              title={t("setup.invite.roleAdmin")}
              description={t("setup.invite.roleAdminDesc")}
            />
          </div>
        </fieldset>
        <div>
          <Button variant="secondary" onClick={send} loading={sending} disabled={!canSend || sending}>
            {sending ? t("setup.invite.sending") : t("setup.invite.send")}
          </Button>
        </div>
      </div>
    </section>
  );
}
