import { useState, useEffect } from "react";
import { useNavigate, useLocation } from "react-router-dom";
import { useTranslation } from "react-i18next";
import { KeyRound } from "lucide-react";
import { totpVerifyWithChallenge } from "../api/client";
import { translateApiError } from "../api/errors";
import { AuthLayout } from "../components/AuthForm";
import { Button, Field, Input } from "../components/ui";

// MfaChallengePage is the second-factor step after a successful
// password login. The /auth/login handler returned a short-lived
// (5 min) mfa_token marked with purpose=mfa_challenge; this page
// exchanges it for a real session token via POST /auth/totp/verify.
//
// Accepts EITHER a 6-digit TOTP value OR a recovery code (the same
// xb-4q-9z-pm-tk format displayed at enrollment). The server tries
// TOTP first and falls back to the recovery-code path on a TOTP
// mismatch, so the user just types whichever they have to hand.
//
// If the navigation state has must_enroll=true, the workspace
// requires MFA but this user has no credential yet — we redirect
// to the enrollment page with the mfa_enroll-purpose token instead.
export default function MfaChallengePage() {
  const nav = useNavigate();
  const loc = useLocation();
  const { t } = useTranslation();
  const state = (loc.state as MfaChallengeState | null) ?? null;
  const [code, setCode] = useState("");
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string | null>(null);

  useEffect(() => {
    if (!state?.mfaToken) {
      // Direct visit without state: send the user back to /login.
      // Hitting this page with no token means either a stale tab
      // after a successful login or a manual URL paste; either way
      // there's nothing useful we can do.
      nav("/login", { replace: true });
      return;
    }
    if (state.mustEnroll) {
      nav("/mfa-enroll", {
        replace: true,
        state: { enrollToken: state.mfaToken, expiresAt: state.expiresAt },
      });
    }
  }, [state, nav]);

  if (!state?.mfaToken) {
    return null;
  }

  return (
    <AuthLayout
      title={t("auth.mfaTitle")}
      subtitle={t("auth.mfaPrompt")}
      icon={<KeyRound className="h-7 w-7" aria-hidden="true" />}
      footer={
        <button
          type="button"
          className="font-medium text-brand hover:underline"
          onClick={() => nav("/login", { replace: true })}
        >
          {t("auth.mfaCancelAndSignIn")}
        </button>
      }
    >
      <form
        className="flex flex-col gap-4"
        onSubmit={async (e) => {
          e.preventDefault();
          if (busy) return;
          setBusy(true);
          setError(null);
          try {
            await totpVerifyWithChallenge(state.mfaToken, code.trim());
            nav("/drive", { replace: true });
          } catch (err) {
            setError(translateApiError(err, t));
          } finally {
            setBusy(false);
          }
        }}
      >
        <Field label={t("auth.mfaCodeLabel")} error={error ?? undefined}>
          {(props) => (
            <Input
              {...props}
              type="text"
              inputMode="text"
              autoComplete="one-time-code"
              autoFocus
              value={code}
              onChange={(e) => setCode(e.target.value)}
              placeholder={t("auth.mfaCodePlaceholder")}
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
          {busy ? t("common.verifying") : t("auth.mfaVerify")}
        </Button>
      </form>
    </AuthLayout>
  );
}

interface MfaChallengeState {
  mfaToken: string;
  expiresAt: string;
  mustEnroll?: boolean;
}
