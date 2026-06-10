// auth401 — classification of every backend error code that can
// come back as HTTP 401, split into:
//
//   - SESSION_DEAD_401_CODES: the JWT itself is bad (missing, dead,
//     forged, wrong-purpose, revoked, etc.). The interceptor MUST
//     clear localStorage and route the user to /login because the
//     credential they have is no longer usable.
//
//   - NON_SESSION_401_CODES: the JWT is fine (or absent in a flow
//     where that's expected, e.g. share-link viewing); the caller
//     just needs to supply something different — a share-link
//     password, the workspace header, the right MFA code, the
//     right reverify password, etc. The interceptor MUST leave
//     auth state alone and let the page's catch block handle it.
//
// The union of the two sets is the EXHAUSTIVE list of 401-emitting
// backend codes. If a new backend code starts returning 401, it
// MUST be added to exactly one set here, and the regression test
// in auth401.test.ts will fail the build until that decision is
// made explicitly. The regression test also asserts disjointness
// (no code can be classified both ways) and completeness against
// the canonical backend code list in i18n/errors.test.ts.
//
// This module is the single source of truth for the
// frontend-backend 401-semantics contract. Earlier the soft-401
// set lived inline in client.ts with only a comment for guidance;
// PR #83 review flagged that the comment is not enforced. The
// dedicated module + test fixes that.

// SESSION_DEAD_401_CODES — the JWT is dead. Clearing localStorage
// and redirecting to /login is the correct interceptor behaviour.
export const SESSION_DEAD_401_CODES = new Set<string>([
  // No Authorization header on a route that requires one. The
  // request is genuinely unauthenticated; sending the user to
  // /login is the only sensible recovery.
  "AUTH_MISSING_TOKEN",
  // JWT failed signature / expiry / claims-shape validation. The
  // token is unusable; the user must re-auth.
  "AUTH_INVALID_TOKEN",
  // Token was explicitly revoked (logout-everywhere, password
  // reset, admin force-logout). Cannot be reused.
  "AUTH_REVOKED_TOKEN",
  // Purpose-scoped token (e.g. an mfa_token) was presented to a
  // session-only endpoint. The token cannot be promoted; re-auth.
  "AUTH_BAD_PURPOSE",
  // Token is missing the issued-at claim, so revocation-cutoff
  // checks can't run. Defence-in-depth: treat as untrusted.
  "AUTH_MISSING_IAT",
  // The revocation store was unreachable when the token tried to
  // authenticate. Fail closed: the user re-auths so we never
  // accept a token whose revocation status we couldn't verify.
  "AUTH_REVOCATION_CHECK_FAILED",
  // Session device fingerprint (User-Agent + IP network) no longer
  // matches the one captured at login — the token was replayed from
  // a different browser or network (6.2 anomaly detection). The
  // session must be re-established, so clear it and route to /login;
  // the en.json copy already tells the user to sign in again.
  "AUTH_SESSION_ANOMALY",
]);

// NON_SESSION_401_CODES — the JWT is fine (or absent intentionally
// in a public-share flow). The interceptor leaves auth state alone
// and the page handles the error.
export const NON_SESSION_401_CODES = new Set<string>([
  // Share-link password challenge. The user is browsing a public
  // share and needs to enter the link's password. Redirecting to
  // /login would be actively wrong: they have no account here.
  "SHARE_PASSWORD_REQUIRED",
  // Request reached the API authenticated but without a workspace
  // header. The session is still valid; the page just needs to
  // retry with a workspace selected. Clearing the token here
  // would force a re-login for a recoverable routing error.
  "MISSING_WORKSPACE_CONTEXT",
  // Invalid MFA code during challenge or enrollment. The
  // mfa_token session is mid-flight (still valid for a few
  // minutes); the user just typed the wrong 6 digits. Redirecting
  // to /login would discard the in-progress challenge and force
  // them to enter their password again, instead of seeing
  // "Invalid code" and getting to retry.
  "AUTH_MFA_INVALID",
  // Login-flow wrong email or password. The caller is at /login
  // (a logged-out visitor in the normal case; a still-
  // authenticated user who accidentally navigated back to /login
  // in the edge case PR #83 review flagged). Either way, the
  // session-clear-and-redirect interceptor is wrong here — the
  // page's catch block already renders "Email or password is
  // incorrect" via translateApiError.
  "AUTH_INVALID_CREDENTIALS",
  // Mid-session password reverify failure (disable-2FA flow,
  // future change-email / change-password flows). The user's
  // session JWT is still valid — that's how they reached the
  // handler in the first place. They just typed the wrong
  // password on the step-up form. The page renders "Incorrect
  // password. Please try again." and lets the user retry.
  "AUTH_PASSWORD_REVERIFY_FAILED",
]);

// AUTH_MFA_REQUIRED and MFA_ENROLL_REQUIRED are declared on the
// backend (api/middleware/error_codes.go) but currently return
// HTTP 200 with mfa_required:true in the body, not 401 — so they
// don't appear in either set above. If a future handler starts
// emitting either as 401, add it to NON_SESSION_401_CODES at the
// same time (the mfa_token challenge state should not be wiped on
// a still-pending enrollment).
//
// The regression test in auth401.test.ts explicitly excludes
// these two codes from the completeness check so that omission
// stays an intentional carve-out rather than a silent gap.
export const NOT_YET_EMITTED_AS_401 = new Set<string>([
  "AUTH_MFA_REQUIRED",
  "MFA_ENROLL_REQUIRED",
]);

// is401SoftFailure returns true if the interceptor should leave
// auth state alone for this code.
export function is401SoftFailure(code: string | null | undefined): boolean {
  return code != null && NON_SESSION_401_CODES.has(code);
}
