import { useCallback, useEffect, useMemo, useState } from "react";
import { useNavigate } from "react-router-dom";
import { useTranslation } from "react-i18next";
import {
  completeSetup,
  fetchSetupStatus,
  inviteUser,
  signup,
  testSetupStorage,
  type SetupStatus,
} from "../api/client";
import { translateApiError } from "../api/errors";

// SetupWizardPage is the first-boot guided setup (WS8 8.2). It walks a
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

interface AdminForm {
  email: string;
  name: string;
  password: string;
}

interface StorageForm {
  endpoint: string;
  bucket: string;
  access_key: string;
  secret_key: string;
  region: string;
}

export default function SetupWizardPage() {
  const { t } = useTranslation();
  const navigate = useNavigate();

  const [status, setStatus] = useState<SetupStatus | null>(null);
  const [stepIndex, setStepIndex] = useState(0);
  const [admin, setAdmin] = useState<AdminForm>({ email: "", name: "", password: "" });
  const [workspaceName, setWorkspaceName] = useState("");
  const [signedUp, setSignedUp] = useState(false);
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
    setStepIndex((i) => Math.max(i - 1, 0));
  }, []);

  // commitWorkspace fires signup() — the admin + workspace creation
  // commit point — then advances. Idempotency guard: if the user steps
  // back to and forward through step 4 we don't sign up twice.
  const commitWorkspace = useCallback(async () => {
    setError(null);
    if (signedUp) {
      goNext();
      return;
    }
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
    }
  }, [admin, workspaceName, signedUp, goNext, t]);

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

  const adminValid = useMemo(
    () => admin.email.trim() !== "" && admin.name.trim() !== "" && admin.password.length >= 12,
    [admin],
  );

  if (done) {
    return (
      <Shell>
        <h1>{t("setup.done")}</h1>
        <p style={{ color: "#6b7280" }}>{t("setup.doneBody")}</p>
        <button onClick={() => navigate("/drive", { replace: true })}>{t("setup.goToDrive")}</button>
      </Shell>
    );
  }

  return (
    <Shell>
      <header style={{ marginBottom: 8 }}>
        <h1 style={{ margin: 0 }}>{t("setup.title")}</h1>
        <p style={{ color: "#6b7280", marginTop: 4 }}>{t("setup.subtitle")}</p>
      </header>

      <Stepper current={stepIndex} />

      <p style={{ color: "#9ca3af", fontSize: 13 }}>
        {t("setup.stepLabel", { current: stepIndex + 1, total: STEP_ORDER.length })}
      </p>

      {error && <p style={{ color: "#b91c1c" }}>{error}</p>}

      <div style={{ minHeight: 220 }}>
        {step === "admin" && <AdminStep admin={admin} setAdmin={setAdmin} />}
        {step === "storage" && <StorageStep status={status} />}
        {step === "services" && <ServicesStep status={status} />}
        {step === "workspace" && (
          <WorkspaceStep workspaceName={workspaceName} setWorkspaceName={setWorkspaceName} signedUp={signedUp} />
        )}
        {step === "invite" && <InviteStep />}
      </div>

      <footer style={{ display: "flex", justifyContent: "space-between", marginTop: 24 }}>
        <button onClick={goBack} disabled={stepIndex === 0 || finishing}>
          {t("setup.back")}
        </button>
        <div style={{ display: "flex", gap: 8 }}>
          {step === "admin" && (
            <button onClick={goNext} disabled={!adminValid}>
              {t("setup.next")}
            </button>
          )}
          {(step === "storage" || step === "services") && <button onClick={goNext}>{t("setup.next")}</button>}
          {step === "workspace" && (
            <button onClick={commitWorkspace} disabled={workspaceName.trim() === ""}>
              {t("setup.next")}
            </button>
          )}
          {step === "invite" && (
            <button onClick={finish} disabled={finishing}>
              {finishing ? t("setup.finishing") : t("setup.finish")}
            </button>
          )}
        </div>
      </footer>
    </Shell>
  );
}

function Shell({ children }: { children: React.ReactNode }) {
  return (
    <div style={{ minHeight: "100vh", display: "flex", alignItems: "center", justifyContent: "center", background: "#f8fafc" }}>
      <div
        style={{
          width: "100%",
          maxWidth: 640,
          background: "#fff",
          border: "1px solid #e5e7eb",
          borderRadius: 12,
          padding: 32,
          margin: 24,
          boxShadow: "0 1px 3px rgba(0,0,0,0.08)",
        }}
      >
        {children}
      </div>
    </div>
  );
}

function Stepper({ current }: { current: number }) {
  const { t } = useTranslation();
  return (
    <ol style={{ display: "flex", gap: 8, listStyle: "none", padding: 0, margin: "16px 0", flexWrap: "wrap" }}>
      {STEP_ORDER.map((id, i) => {
        const state = i < current ? "done" : i === current ? "active" : "todo";
        const bg = state === "active" ? "#2563eb" : state === "done" ? "#22c55e" : "#e5e7eb";
        const fg = state === "todo" ? "#6b7280" : "#fff";
        return (
          <li key={id} style={{ display: "flex", alignItems: "center", gap: 6 }}>
            <span
              style={{
                width: 22,
                height: 22,
                borderRadius: "50%",
                background: bg,
                color: fg,
                display: "inline-flex",
                alignItems: "center",
                justifyContent: "center",
                fontSize: 12,
                fontWeight: 700,
              }}
            >
              {i + 1}
            </span>
            <span style={{ fontSize: 13, color: state === "active" ? "#111827" : "#6b7280" }}>
              {t(`setup.step.${id}`)}
            </span>
          </li>
        );
      })}
    </ol>
  );
}

function Field({ label, hint, children }: { label: string; hint?: string; children: React.ReactNode }) {
  return (
    <label style={{ display: "block", marginBottom: 12 }}>
      <span style={{ display: "block", fontSize: 13, fontWeight: 600, marginBottom: 4 }}>{label}</span>
      {children}
      {hint && <span style={{ display: "block", color: "#9ca3af", fontSize: 12, marginTop: 2 }}>{hint}</span>}
    </label>
  );
}

const inputStyle: React.CSSProperties = {
  width: "100%",
  padding: "8px 10px",
  border: "1px solid #d1d5db",
  borderRadius: 6,
  fontSize: 14,
  boxSizing: "border-box",
};

function AdminStep({ admin, setAdmin }: { admin: AdminForm; setAdmin: (a: AdminForm) => void }) {
  const { t } = useTranslation();
  return (
    <section>
      <h2>{t("setup.admin.title")}</h2>
      <p style={{ color: "#6b7280" }}>{t("setup.admin.body")}</p>
      <Field label={t("setup.admin.email")}>
        <input
          style={inputStyle}
          type="email"
          autoComplete="email"
          value={admin.email}
          onChange={(e) => setAdmin({ ...admin, email: e.target.value })}
        />
      </Field>
      <Field label={t("setup.admin.name")}>
        <input style={inputStyle} value={admin.name} onChange={(e) => setAdmin({ ...admin, name: e.target.value })} />
      </Field>
      <Field label={t("setup.admin.password")} hint={t("setup.admin.passwordHint")}>
        <input
          style={inputStyle}
          type="password"
          autoComplete="new-password"
          value={admin.password}
          onChange={(e) => setAdmin({ ...admin, password: e.target.value })}
        />
      </Field>
    </section>
  );
}

function StatusBadge({ ok, okText, badText }: { ok: boolean; okText: string; badText: string }) {
  return (
    <span
      style={{
        display: "inline-block",
        padding: "2px 10px",
        borderRadius: 999,
        background: ok ? "#dcfce7" : "#f1f5f9",
        color: ok ? "#166534" : "#475569",
        fontSize: 13,
        fontWeight: 600,
      }}
    >
      {ok ? okText : badText}
    </span>
  );
}

function StorageStep({ status }: { status: SetupStatus | null }) {
  const { t } = useTranslation();
  const configured = status?.steps?.storage.configured ?? false;
  const [form, setForm] = useState<StorageForm>({ endpoint: "", bucket: "", access_key: "", secret_key: "", region: "" });
  const [testing, setTesting] = useState(false);
  const [result, setResult] = useState<{ ok: boolean; error?: string } | null>(null);

  const canTest =
    form.endpoint.trim() !== "" && form.bucket.trim() !== "" && form.access_key.trim() !== "" && form.secret_key !== "";

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
    } catch {
      setResult({ ok: false, error: "request failed" });
    } finally {
      setTesting(false);
    }
  }, [form]);

  return (
    <section>
      <h2>{t("setup.storage.title")}</h2>
      <p style={{ color: "#6b7280" }}>{t("setup.storage.body")}</p>
      <p style={{ marginBottom: 16 }}>
        <StatusBadge ok={configured} okText={t("setup.storage.configured")} badText={t("setup.storage.notConfigured")} />
      </p>
      <Field label={t("setup.storage.endpoint")}>
        <input
          style={inputStyle}
          placeholder="https://s3.example.com"
          value={form.endpoint}
          onChange={(e) => setForm({ ...form, endpoint: e.target.value })}
        />
      </Field>
      <Field label={t("setup.storage.bucket")}>
        <input style={inputStyle} value={form.bucket} onChange={(e) => setForm({ ...form, bucket: e.target.value })} />
      </Field>
      <Field label={t("setup.storage.accessKey")}>
        <input
          style={inputStyle}
          value={form.access_key}
          onChange={(e) => setForm({ ...form, access_key: e.target.value })}
        />
      </Field>
      <Field label={t("setup.storage.secretKey")}>
        <input
          style={inputStyle}
          type="password"
          value={form.secret_key}
          onChange={(e) => setForm({ ...form, secret_key: e.target.value })}
        />
      </Field>
      <Field label={t("setup.storage.region")}>
        <input style={inputStyle} value={form.region} onChange={(e) => setForm({ ...form, region: e.target.value })} />
      </Field>
      <p style={{ color: "#9ca3af", fontSize: 12, marginTop: 0, marginBottom: 12 }}>{t("setup.storage.testHint")}</p>
      <button onClick={runTest} disabled={!canTest || testing}>
        {testing ? t("setup.storage.testing") : t("setup.storage.test")}
      </button>
      {result?.ok === true && <p style={{ color: "#166534", marginTop: 8 }}>{t("setup.storage.testOk")}</p>}
      {result?.ok === false && (
        <p style={{ color: "#b91c1c", marginTop: 8 }}>{t("setup.storage.testFailed", { error: result.error ?? "" })}</p>
      )}
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
      <h2>{t("setup.services.title")}</h2>
      <p style={{ color: "#6b7280" }}>{t("setup.services.body")}</p>
      <ul style={{ listStyle: "none", padding: 0, margin: 0 }}>
        {rows.map((row) => {
          const on = svc ? Boolean(svc[row.key]) : false;
          return (
            <li
              key={row.key}
              style={{
                display: "flex",
                justifyContent: "space-between",
                alignItems: "center",
                padding: "10px 0",
                borderBottom: "1px solid #f1f5f9",
              }}
            >
              <span>{row.label}</span>
              <StatusBadge ok={on} okText={t("setup.services.enabled")} badText={t("setup.services.disabled")} />
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
}: {
  workspaceName: string;
  setWorkspaceName: (s: string) => void;
  signedUp: boolean;
}) {
  const { t } = useTranslation();
  return (
    <section>
      <h2>{t("setup.workspace.title")}</h2>
      <p style={{ color: "#6b7280" }}>{t("setup.workspace.body")}</p>
      <Field label={t("setup.workspace.name")}>
        <input
          style={inputStyle}
          value={workspaceName}
          disabled={signedUp}
          onChange={(e) => setWorkspaceName(e.target.value)}
        />
      </Field>
    </section>
  );
}

function InviteStep() {
  const { t } = useTranslation();
  const [form, setForm] = useState({ email: "", name: "", password: "", role: "member" });
  const [sending, setSending] = useState(false);
  const [sentTo, setSentTo] = useState<string | null>(null);
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
      setSentTo(form.email.trim());
      setForm({ email: "", name: "", password: "", role: "member" });
    } catch (e) {
      setError(translateApiError(e, t));
    } finally {
      setSending(false);
    }
  }, [form, t]);

  return (
    <section>
      <h2>{t("setup.invite.title")}</h2>
      <p style={{ color: "#6b7280" }}>{t("setup.invite.body")}</p>
      {sentTo && <p style={{ color: "#166534" }}>{t("setup.invite.sent", { email: sentTo })}</p>}
      {error && <p style={{ color: "#b91c1c" }}>{error}</p>}
      <Field label={t("setup.invite.email")}>
        <input
          style={inputStyle}
          type="email"
          value={form.email}
          onChange={(e) => setForm({ ...form, email: e.target.value })}
        />
      </Field>
      <Field label={t("setup.invite.name")}>
        <input style={inputStyle} value={form.name} onChange={(e) => setForm({ ...form, name: e.target.value })} />
      </Field>
      <Field label={t("setup.invite.password")}>
        <input
          style={inputStyle}
          type="password"
          value={form.password}
          onChange={(e) => setForm({ ...form, password: e.target.value })}
        />
      </Field>
      <Field label={t("setup.invite.role")}>
        <select style={inputStyle} value={form.role} onChange={(e) => setForm({ ...form, role: e.target.value })}>
          <option value="member">{t("setup.invite.roleMember")}</option>
          <option value="admin">{t("setup.invite.roleAdmin")}</option>
        </select>
      </Field>
      <button onClick={send} disabled={!canSend || sending}>
        {sending ? t("setup.invite.sending") : t("setup.invite.send")}
      </button>
    </section>
  );
}
