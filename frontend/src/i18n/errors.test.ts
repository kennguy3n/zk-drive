// errors.test — guarantees the English locale's `errors`
// namespace stays in sync with the backend's ErrorCode constants
// in api/middleware/error_codes.go. If a backend code lacks a
// matching JSON key the user would see the raw `errors.FOO_BAR`
// string (or fall through to the English fallback message). This
// suite catches that drift before it ships.

import { describe, it, expect } from "vitest";
import en from "./locales/en.json";

// The authoritative list of error codes the backend emits. Keep
// this in lock-step with api/middleware/error_codes.go's `const`
// block. Adding a code on the backend without adding a key here
// will fail TestErrorsLocaleCoversBackendCodes below.
const backendCodes = [
  // Authentication failures (401).
  "AUTH_MISSING_TOKEN",
  "AUTH_INVALID_TOKEN",
  "AUTH_REVOKED_TOKEN",
  "AUTH_BAD_PURPOSE",
  "AUTH_MISSING_IAT",
  "AUTH_REVOCATION_CHECK_FAILED",
  "AUTH_INVALID_CREDENTIALS",
  "AUTH_PASSWORD_REVERIFY_FAILED",
  "AUTH_MFA_REQUIRED",
  "AUTH_MFA_INVALID",
  "MFA_ENROLL_REQUIRED",
  // Authorization failures (403).
  "FORBIDDEN",
  "ADMIN_ACCESS_REQUIRED",
  "READ_ONLY_ROLE",
  "WRONG_TENANT",
  // Workspace-routing failure (401 distinct from session auth).
  "MISSING_WORKSPACE_CONTEXT",
  // Rate limiting (429).
  "RATE_LIMIT_EXCEEDED",
  // Validation (400 / 422).
  "VALIDATION_FAILED",
  "BAD_REQUEST",
  "MALFORMED_JSON",
  "MISSING_REQUIRED_FIELD",
  "UNSUPPORTED_OPERATION",
  "COLLAB_MODE_NOT_ALLOWED",
  "UNSUPPORTED_SEARCH_LANGUAGE",
  // Resource state (404 / 409 / 410).
  "NOT_FOUND",
  "CONFLICT",
  "GONE",
  "FOLDER_LOCKED",
  "WORKSPACE_QUOTA_EXCEEDED",
  "FILE_TOO_LARGE",
  "FILE_VIRUS_DETECTED",
  "FABRIC_NOT_PROVISIONED",
  // Share-link auth (401 distinct from session auth).
  "SHARE_PASSWORD_REQUIRED",
  // Share-link download cap (429 distinct from rate-limit throttle).
  "SHARE_LINK_EXHAUSTED",
  // Billing / payments (402 / 412 distinct from internal).
  "BILLING_NOT_CONFIGURED",
  // Stripe-backed billing not wired up at deployment level (501,
  // distinct from UNSUPPORTED_OPERATION so BillingPage shows
  // Stripe-specific remediation instead of generic copy).
  "STRIPE_NOT_CONFIGURED",
  // Service-level failures (5xx).
  "INTERNAL_ERROR",
  "UPSTREAM_FAILED",
  "MAINTENANCE",
  "STORAGE_FAILURE",
] as const;

describe("en.json errors namespace", () => {
  const errors = (en as { errors: Record<string, string> }).errors;

  it("has a translation for every backend ErrorCode", () => {
    const missing = backendCodes.filter((code) => !errors[code]);
    expect(missing).toEqual([]);
  });

  it("has no orphan keys not declared on the backend", () => {
    const known = new Set<string>(backendCodes);
    const orphans = Object.keys(errors).filter((k) => !known.has(k));
    expect(orphans).toEqual([]);
  });

  it("uses non-empty strings for every key", () => {
    for (const [k, v] of Object.entries(errors)) {
      expect(typeof v).toBe("string");
      expect(v.trim().length).toBeGreaterThan(0);
      // Sanity: no code should accidentally hold the literal key
      // name (which would happen if a translator copy-pasted the
      // backend constant instead of writing copy).
      expect(v).not.toBe(k);
    }
  });
});
