import { useEffect, useState, type FormEvent } from "react";
import { useLocation, useNavigate } from "react-router-dom";
import { useTranslation } from "react-i18next";
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

  // Recovery codes screen: shown exactly once after a successful
  // finalize. The user MUST download / copy / print before
  // dismissing — server-side they exist only as bcrypt hashes from
  // this point forward.
  if (recovery) {
    return (
      <div className="auth-page">
        <h1>{t("auth.mfaEnrolledTitle")}</h1>
        <p className="recovery-warning">
          <strong>{t("auth.mfaRecoveryWarning")}</strong>
        </p>
        <pre className="recovery-codes">{recovery.join("\n")}</pre>
        <button
          type="button"
          onClick={() => {
            const blob = new Blob([recovery.join("\n") + "\n"], {
              type: "text/plain",
            });
            const url = URL.createObjectURL(blob);
            const a = document.createElement("a");
            a.href = url;
            a.download = "zk-drive-recovery-codes.txt";
            a.click();
            URL.revokeObjectURL(url);
          }}
        >
          {t("auth.mfaDownloadCodes")}
        </button>
        <button
          type="button"
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
          style={{ marginLeft: "1rem" }}
        >
          {enrollToken ? t("auth.signInAgain") : t("common.done")}
        </button>
      </div>
    );
  }

  if (status?.enabled && !enrollToken) {
    return (
      <div className="auth-page">
        <h1>{t("auth.mfaAlreadyEnrolled")}</h1>
        {status.activated_at && (
          <p>
            {t("auth.mfaEnrolledAt", {
              date: new Date(status.activated_at).toLocaleString(),
            })}
          </p>
        )}
        <p>
          {t("auth.mfaRecoveryCodesRemaining", {
            count: status.recovery_codes_remaining,
          })}
        </p>
        {status.recovery_codes_remaining <= 2 && (
          <p className="warning">{t("auth.mfaRecoveryCodesLowWarning")}</p>
        )}
        <p>{t("auth.mfaReEnrollInstruction")}</p>
        <button type="button" onClick={() => nav("/drive")}>
          {t("common.back")}
        </button>
      </div>
    );
  }

  return (
    <div className="auth-page">
      <h1>{t("auth.mfaEnrollTitle")}</h1>
      {enrollToken && <p>{t("auth.mfaForcedEnrollmentExplanation")}</p>}
      {!challenge && <p>{t("common.loading")}</p>}
      {challenge && (
        <>
          <p>{t("auth.mfaEnrollPrompt")}</p>
          <img
            src={`data:image/png;base64,${challenge.qr_code_png}`}
            alt={t("auth.mfaQrAlt")}
            width={200}
            height={200}
          />
          <p>
            <small>{t("auth.mfaSecretLabel")}</small>
            <br />
            <code>{challenge.secret}</code>
          </p>
          <form onSubmit={onFinalize}>
            <label htmlFor="totp-code">{t("auth.mfaCodeInput")}</label>
            <input
              id="totp-code"
              type="text"
              inputMode="numeric"
              autoComplete="one-time-code"
              autoFocus
              value={code}
              onChange={(e) => setCode(e.target.value)}
              placeholder="123456"
            />
            <button type="submit" disabled={busy || code.trim() === ""}>
              {busy ? t("common.verifying") : t("auth.mfaEnable")}
            </button>
          </form>
        </>
      )}
      {error && <p className="auth-error">{error}</p>}
    </div>
  );
}

interface EnrollState {
  enrollToken?: string;
  expiresAt?: string;
}
