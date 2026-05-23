import { useState, useEffect } from "react";
import { useNavigate, useLocation } from "react-router-dom";
import { totpVerifyWithChallenge } from "../api/client";

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
    <div className="auth-page">
      <h1>Two-factor verification</h1>
      <p>
        Enter the 6-digit code from your authenticator app, or one of your
        recovery codes if you no longer have access to the device.
      </p>
      <form
        onSubmit={async (e) => {
          e.preventDefault();
          if (busy) return;
          setBusy(true);
          setError(null);
          try {
            await totpVerifyWithChallenge(state.mfaToken, code.trim());
            nav("/drive", { replace: true });
          } catch (err) {
            setError(extractErr(err));
          } finally {
            setBusy(false);
          }
        }}
      >
        <label htmlFor="mfa-code">Code</label>
        <input
          id="mfa-code"
          type="text"
          inputMode="text"
          autoComplete="one-time-code"
          autoFocus
          value={code}
          onChange={(e) => setCode(e.target.value)}
          placeholder="123456 or xb-4q-9z-pm-tk"
        />
        <button type="submit" disabled={busy || code.trim() === ""}>
          {busy ? "Verifying…" : "Verify"}
        </button>
      </form>
      {error && <p className="auth-error">{error}</p>}
      <p className="auth-footer">
        <button
          type="button"
          className="link-button"
          onClick={() => nav("/login", { replace: true })}
        >
          Cancel and sign in again
        </button>
      </p>
    </div>
  );
}

interface MfaChallengeState {
  mfaToken: string;
  expiresAt: string;
  mustEnroll?: boolean;
}

function extractErr(e: unknown): string {
  const maybe = e as { response?: { data?: string }; message?: string };
  return maybe.response?.data || maybe.message || "Invalid code";
}
