// auth401.test — enforces the 401-classification contract.
//
// Every backend ErrorCode that can be returned with HTTP 401 must
// be explicitly classified in api/auth401.ts as either
// SESSION_DEAD_401_CODES (the JWT is bad, interceptor MUST nuke
// the session and redirect) or NON_SESSION_401_CODES (the JWT is
// fine, interceptor MUST leave the session alone). This test
// fails the build if:
//
//   1. A backend 401 code is unclassified (silent gap → potential
//      session loss or improper auth-state retention).
//   2. A code is classified in both sets (ambiguous routing).
//   3. A code in the union is not actually a backend 401 code
//      (typo / stale entry).
//
// The canonical list of 401-emitting backend codes is captured
// here directly. Keep it in lock-step with the actual
// `respondError(... StatusUnauthorized, ErrCodeXxx, ...)` call
// sites in api/. A separate test in i18n/errors.test.ts catches
// locale drift for the same codes; this test catches
// interceptor-routing drift.
//
// The architectural alternative — codegen the manifest from a
// backend-emitted JSON spec — is overkill for the current scope.
// A manually-maintained manifest is good enough because every
// new 401 path is a deliberate auth design decision that will
// already be reviewed; the test failure forces the reviewer to
// add the new code here, which is exactly the design discussion
// we want to surface.

import { describe, it, expect } from "vitest";
import {
  SESSION_DEAD_401_CODES,
  NON_SESSION_401_CODES,
  NOT_YET_EMITTED_AS_401,
  is401SoftFailure,
} from "./auth401";

// Authoritative list of every backend error code currently
// returned with HTTP 401. Sourced from grepping
// `RespondError(w, http.StatusUnauthorized, ...)` plus a small
// number of forward-declared codes (the MFA carve-out below).
const BACKEND_401_CODES = [
  // Token-validation failures from api/middleware/auth.go and
  // related auth middleware. JWT itself is dead.
  "AUTH_MISSING_TOKEN",
  "AUTH_INVALID_TOKEN",
  "AUTH_REVOKED_TOKEN",
  "AUTH_BAD_PURPOSE",
  "AUTH_MISSING_IAT",
  "AUTH_REVOCATION_CHECK_FAILED",
  // Login + step-up flows from api/auth/. JWT is fine (or absent
  // intentionally for login); the user just supplied wrong creds.
  "AUTH_INVALID_CREDENTIALS",
  "AUTH_PASSWORD_REVERIFY_FAILED",
  "AUTH_MFA_INVALID",
  // Share-link + workspace-routing 401s. JWT (if any) is fine.
  "SHARE_PASSWORD_REQUIRED",
  "MISSING_WORKSPACE_CONTEXT",
] as const;

describe("401 classification contract", () => {
  it("classifies every backend 401 code into exactly one bucket", () => {
    const unclassified = BACKEND_401_CODES.filter(
      (code) =>
        !SESSION_DEAD_401_CODES.has(code) &&
        !NON_SESSION_401_CODES.has(code),
    );
    expect(unclassified).toEqual([]);
  });

  it("does not classify a code into both buckets", () => {
    const overlap = BACKEND_401_CODES.filter(
      (code) =>
        SESSION_DEAD_401_CODES.has(code) &&
        NON_SESSION_401_CODES.has(code),
    );
    expect(overlap).toEqual([]);
  });

  it("classification sets contain only known backend 401 codes", () => {
    const known = new Set<string>(BACKEND_401_CODES);
    const stale = [
      ...SESSION_DEAD_401_CODES,
      ...NON_SESSION_401_CODES,
    ].filter((code) => !known.has(code));
    expect(stale).toEqual([]);
  });

  it("does not classify codes that are not currently emitted as 401", () => {
    // MFA codes are currently returned as HTTP 200 with
    // mfa_required:true. They should NOT be in either 401 set
    // until a handler starts emitting them as actual 401s.
    const leaked = [...NOT_YET_EMITTED_AS_401].filter(
      (code) =>
        SESSION_DEAD_401_CODES.has(code) ||
        NON_SESSION_401_CODES.has(code),
    );
    expect(leaked).toEqual([]);
  });
});

describe("is401SoftFailure", () => {
  it("returns true for every soft-401 code", () => {
    for (const code of NON_SESSION_401_CODES) {
      expect(is401SoftFailure(code)).toBe(true);
    }
  });

  it("returns false for every session-dead code", () => {
    for (const code of SESSION_DEAD_401_CODES) {
      expect(is401SoftFailure(code)).toBe(false);
    }
  });

  it("returns false for null, undefined, and unknown codes", () => {
    expect(is401SoftFailure(null)).toBe(false);
    expect(is401SoftFailure(undefined)).toBe(false);
    expect(is401SoftFailure("")).toBe(false);
    expect(is401SoftFailure("DEFINITELY_NOT_A_REAL_CODE")).toBe(false);
  });
});
