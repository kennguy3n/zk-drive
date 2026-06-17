import { useEffect, useState, type FormEvent } from "react";
import { useLocation, useNavigate } from "react-router-dom";
import { useTranslation } from "react-i18next";
import { Copy, Download, KeyRound, Loader2, ShieldCheck } from "lucide-react";
import {
  totpEnrollBegin,
  totpEnrollBeginRequired,
  totpEnrollFinalize,
  totpEnrollFinalizeRequired,
  totpStatus,
  type TOTPEnrollBeginResponse,
  type TOTPStatus,
} from "../api/client";
import { translateApiError } from "../api/errors";
import { AuthLayout } from "../components/AuthForm";
import { Badge, Button, Field, Input, useToast } from "../components/ui";

// TwoFactorEnrollPage handles BOTH:
//
//   - /account/2fa  : authenticated re-enrollment from settings. The
//                     stored session token is used as the
//                     Authorization header.
//   - /mfa-enroll   : forced enrollment under a workspace MFA
//                     policy. The page receives an mfa_enroll-purpose
//                     token via react-router state and uses it
//                     instead of the (absent) session token.
//
// The route distinguishes by presence of an enrollToken in the
// router state; the API helpers route to the right backend endpoint
// accordingly.
export default function TwoFactorEnrollPage() {
  const nav = useNavigate();
  const loc = useLocation();
  const { t } = useTranslation();
  const toast = useToast();
  const enrollState = (loc.state as EnrollState | null) ?? null;
  const enrollToken = enrollState?.enrollToken ?? null;

  const [status, setStatus] = useState<TOTPStatus | null>(null);
  const [challenge, setChallenge] = useState<TOTPEnrollBeginResponse | null>(null);
  const [code, setCode] = useState("");
  const [recovery, setRecovery] = useState<string[] | null>(null);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string | null>(null);

  // For the authenticated path we fetch the current status first so a
  // user who is already enrolled sees a "disable first" prompt
  // instead of accidentally generating a fresh secret. For the
  // forced-enrollment path we know status is "not enrolled" (the
  // server only issues an enroll token when status is empty), so we
  // skip the lookup and go straight to BeginEnrollment.
  useEffect(() => {
    let cancelled = false;
    (async () => {
      try {
        if (enrollToken) {
          // Required-enrollment path: no status fetch (the user has
          // no session token; the status endpoint would 401).
          const ch = await totpEnrollBeginRequired(enrollToken);
          if (!cancelled) setChallenge(ch);
          return;
        }
        const s = await totpStatus();
        if (cancelled) return;
        setStatus(s);
        if (!s.enabled) {
          const ch = await totpEnrollBegin();
          if (!cancelled) setChallenge(ch);
        }
      } catch (e) {
        if (!cancelled) setError(translateApiError(e, t));
      }
    })();
    return () => {
      cancelled = true;
    };
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [enrollToken]);

  const onFinalize = async (e: FormEvent) => {
    e.preventDefault();
    if (busy || !challenge) return;
    setBusy(true);
    setError(null);
    try {
      const resp = enrollToken
        ? await totpEnrollFinalizeRequired(enrollToken, code.trim())
        : await totpEnrollFinalize(code.trim());
      setRecovery(resp.recovery_codes);
      // Re-fetch status only on the authenticated path. On the
      // required-enrollment path the user still doesn't have a
      // session token — status would 401.
      if (!enrollToken) {
        setStatus(await totpStatus());
      }
    } catch (err) {
      setError(translateApiError(err, t));
    } finally {
      setBusy(false);
    }
  };

  const copySecret = async () => {
    if (!challenge) return;
    try {
      await navigator.clipboard.writeText(challenge.secret);
      toast.success(t("auth.mfaSecretCopied"));
    } catch {
      setError(t("auth.mfaCopyFailed"));
    }
  };

  const copyRecovery = async () => {
    if (!recovery) return;
    try {
      await navigator.clipboard.writeText(recovery.join("\n"));
      toast.success(t("auth.mfaCodesCopied"));
    } catch {
      setError(t("auth.mfaCopyFailed"));
    }
  };

  const downloadRecovery = () => {
    if (!recovery) return;
    const blob = new Blob([recovery.join("\n") + "\n"], { type: "text/plain" });
    const url = URL.createObjectURL(blob);
    const a = document.createElement("a");
    a.href = url;
    a.download = "zk-drive-recovery-codes.txt";
    a.click();
    URL.revokeObjectURL(url);
    toast.success(t("auth.mfaCodesDownloaded"));
  };

  // Recovery codes screen: shown exactly once after a successful
  // finalize. The user MUST download / copy / print before
  // dismissing — server-side they exist only as bcrypt hashes from
  // this point forward.
  if (recovery) {
    return (
      <AuthLayout
        title={t("auth.mfaEnrolledTitle")}
        subtitle={t("auth.mfaEnrolledSubtitle")}
        icon={<ShieldCheck className="h-7 w-7" aria-hidden="true" />}
        width="md"
      >
        <div className="mb-4 rounded-lg border border-warning/30 bg-warning/10 px-3 py-2 text-sm text-warning">
          {t("auth.mfaRecoveryWarning")}
        </div>
        <ul className="mb-4 grid grid-cols-2 gap-2 rounded-lg border border-border bg-surface-2 p-4 font-mono text-sm text-fg">
          {recovery.map((c) => (
            <li key={c} className="text-center tracking-wide">
              {c}
            </li>
          ))}
        </ul>
        <div className="flex flex-col gap-2 sm:flex-row">
          <Button variant="secondary" className="flex-1" onClick={downloadRecovery}>
            <Download className="h-4 w-4" aria-hidden="true" />
            {t("auth.mfaDownloadCodes")}
          </Button>
          <Button variant="secondary" className="flex-1" onClick={copyRecovery}>
            <Copy className="h-4 w-4" aria-hidden="true" />
            {t("auth.mfaCopyCodes")}
          </Button>
        </div>
        <Button
          className="mt-3 w-full"
          onClick={() => {
            if (enrollToken) {
              // After forced enrollment the user must complete the
              // login by going BACK through /login — the mfa_enroll
              // token cannot mint a session, only the password
              // factor can.
              nav("/login", { replace: true });
            } else {
              nav("/drive", { replace: true });
            }
          }}
        >
          {enrollToken ? t("auth.signInAgain") : t("common.done")}
        </Button>
      </AuthLayout>
    );
  }

  if (status?.enabled && !enrollToken) {
    return (
      <AuthLayout
        title={t("auth.mfaAlreadyEnrolled")}
        subtitle={t("auth.mfaAlreadyEnrolledSubtitle")}
        icon={<ShieldCheck className="h-7 w-7" aria-hidden="true" />}
        width="md"
      >
        <dl className="flex flex-col gap-3 text-sm">
          {status.activated_at && (
            <div className="flex items-center justify-between gap-3">
              <dt className="text-muted">{t("auth.mfaEnrolledOnLabel")}</dt>
              <dd className="font-medium text-fg">
                {new Date(status.activated_at).toLocaleString()}
              </dd>
            </div>
          )}
          <div className="flex items-center justify-between gap-3">
            <dt className="text-muted">{t("auth.mfaRecoveryCodesLabel")}</dt>
            <dd>
              <Badge tone={status.recovery_codes_remaining <= 2 ? "warning" : "success"}>
                {t("auth.mfaRecoveryCodesRemaining", {
                  count: status.recovery_codes_remaining,
                })}
              </Badge>
            </dd>
          </div>
        </dl>
        {status.recovery_codes_remaining <= 2 && (
          <p className="mt-3 rounded-lg border border-warning/30 bg-warning/10 px-3 py-2 text-sm text-warning">
            {t("auth.mfaRecoveryCodesLowWarning")}
          </p>
        )}
        <p className="mt-4 text-sm text-muted">{t("auth.mfaReEnrollInstruction")}</p>
        <Button variant="secondary" className="mt-4 w-full" onClick={() => nav("/drive")}>
          {t("common.back")}
        </Button>
      </AuthLayout>
    );
  }

  return (
    <AuthLayout
      title={t("auth.mfaEnrollTitle")}
      subtitle={enrollToken ? t("auth.mfaForcedEnrollmentExplanation") : t("auth.mfaEnrollPrompt")}
      icon={<KeyRound className="h-7 w-7" aria-hidden="true" />}
      width="md"
    >
      {error && (
        <div
          role="alert"
          className="mb-4 rounded-lg border border-danger/30 bg-danger/10 px-3 py-2 text-sm text-danger"
        >
          {error}
        </div>
      )}

      {!challenge ? (
        <div
          className="flex flex-col items-center gap-3 py-6 text-sm text-muted"
          role="status"
          aria-live="polite"
        >
          <Loader2 className="h-6 w-6 animate-spin text-brand" aria-hidden="true" />
          {t("common.loading")}
        </div>
      ) : (
        <>
          <div className="flex flex-col items-center">
            <div className="rounded-xl border border-border bg-white p-3">
              <img
                src={`data:image/png;base64,${challenge.qr_code_png}`}
                alt={t("auth.mfaQrAlt")}
                width={180}
                height={180}
              />
            </div>
          </div>

          <div className="mt-4">
            <p className="mb-1.5 text-sm font-medium text-fg">{t("auth.mfaSecretLabel")}</p>
            <div className="flex items-center gap-2 rounded-lg border border-border bg-surface-2 p-2">
              <code className="flex-1 break-all px-1 font-mono text-sm text-fg">
                {challenge.secret}
              </code>
              <Button
                type="button"
                variant="ghost"
                size="sm"
                onClick={copySecret}
                aria-label={t("auth.mfaCopySecret")}
              >
                <Copy className="h-4 w-4" aria-hidden="true" />
                {t("common.copy")}
              </Button>
            </div>
          </div>

          <form onSubmit={onFinalize} className="mt-5 flex flex-col gap-4">
            <Field label={t("auth.mfaCodeInput")} hint={t("auth.mfaCodeInputHint")}>
              {(props) => (
                <Input
                  {...props}
                  type="text"
                  inputMode="numeric"
                  autoComplete="one-time-code"
                  autoFocus
                  value={code}
                  onChange={(e) => setCode(e.target.value)}
                  placeholder="123456"
                  className="h-12 text-center font-mono text-lg tracking-[0.3em]"
                />
              )}
            </Field>
            <Button
              type="submit"
              className="w-full"
              loading={busy}
              disabled={busy || code.trim() === ""}
            >
              {busy ? t("common.verifying") : t("auth.mfaEnable")}
            </Button>
          </form>
        </>
      )}
    </AuthLayout>
  );
}

interface EnrollState {
  enrollToken?: string;
  expiresAt?: string;
}
