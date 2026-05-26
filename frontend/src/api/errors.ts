// errors maps backend error responses to locale-translated
// strings. The backend (api/middleware/error_codes.go) returns a
// JSON shape `{"code": "AUTH_MISSING_TOKEN", "message": "..."}`
// on every error path that goes through middleware/handler
// `respondError`. The `code` field is stable across releases and
// locale-independent; the `message` field is a developer-readable
// English fallback that we surface to the user ONLY when the
// code is unknown to the frontend (deploy skew, new error code
// not yet translated).
//
// This module exposes:
//   - extractErrorCode(err): pulls the code out of an axios error,
//     a fetch Response, or a thrown Error. Returns `null` if no
//     code is present (network failure, off-API error, throwing
//     a plain string).
//   - translateApiError(err, t): high-level helper that takes an
//     axios error + the i18next `t` function and returns the
//     translated user-facing message. Falls back to the server's
//     `message` field, then to the generic "Something went wrong"
//     translation if nothing else is available.
//
// Why a dedicated module: the alternative is sprinkling
// `extractErr` helpers across every page (LoginPage, SignupPage,
// MfaChallengePage all have a duplicate one today). Centralising
// the mapping keeps the error contract in one place and lets the
// admin / billing pages share the same translation logic without
// each handler re-implementing the axios shape inspection.

import type { TFunction } from "i18next";

interface ApiErrorPayload {
  code?: string;
  message?: string;
}

interface AxiosLikeError {
  response?: {
    status?: number;
    data?: ApiErrorPayload | string;
  };
  message?: string;
}

// extractErrorCode pulls a backend error code out of whatever
// shape the API client surfaced. The two production shapes are:
//   1. JSON `{code, message}` body — current handlers.
//   2. Legacy plain-text body — old handlers we haven't migrated
//      yet. Returns null for those so the caller falls back to
//      the raw text.
export function extractErrorCode(err: unknown): string | null {
  if (!err || typeof err !== "object") {
    return null;
  }
  const ax = err as AxiosLikeError;
  const data = ax.response?.data;
  if (data && typeof data === "object" && typeof data.code === "string" && data.code) {
    return data.code;
  }
  return null;
}

// extractErrorMessage pulls a developer-readable English message
// out of the error. Order of preference: JSON `message` field,
// legacy plain-text body, `.message` of the thrown Error.
export function extractErrorMessage(err: unknown): string | null {
  if (!err || typeof err !== "object") {
    if (typeof err === "string") return err;
    return null;
  }
  const ax = err as AxiosLikeError;
  const data = ax.response?.data;
  if (data) {
    if (typeof data === "string" && data.trim()) {
      return data;
    }
    if (typeof data === "object" && typeof data.message === "string" && data.message) {
      return data.message;
    }
  }
  if (typeof ax.message === "string" && ax.message) {
    return ax.message;
  }
  return null;
}

// TranslateApiErrorOptions tunes the fallback chain on a per-call
// basis. The default (no options) preserves the original "always
// returns a non-empty string" contract: missing code AND missing
// message resolve to t("common.error") ("Something went wrong").
//
// Callers that want a more context-specific fallback for the
// no-code-no-message case (e.g. a network error during checkout)
// can pass `fallback` — that string replaces the generic
// t("common.error") slot. Use case: see BillingPage.handleCheckout
// where "Could not start checkout" is more actionable than the
// generic copy.
export interface TranslateApiErrorOptions {
  // fallback is shown when the error carries neither a translatable
  // code nor a server-supplied message — i.e. the network failed
  // before the request reached the API. Defaults to t("common.error").
  fallback?: string;
}

// translateApiError is the one-call helper for UI handlers:
//
//   } catch (err) {
//     setError(translateApiError(err, t));
//   }
//
// Resolution order:
//   1. If the error carries a known code, return t(`errors.${code}`).
//   2. Otherwise return the server-supplied message verbatim.
//   3. Otherwise return options.fallback if provided, else
//      the generic "Something went wrong" copy.
//
// Devin Review BUG_0001 on commit bb909a1: the prior shape always
// returned a non-empty string (last fallback `t("common.error")`),
// so callers writing `translateApiError(e, t) || t("billing.X")`
// at BillingPage.tsx:59/72 saw the right-hand side become dead
// code. The fallback parameter replaces the generic slot directly,
// keeping a single source of truth for the "no code, no message"
// case while letting the call site provide page-specific copy.
export function translateApiError(
  err: unknown,
  t: TFunction,
  options: TranslateApiErrorOptions = {},
): string {
  const code = extractErrorCode(err);
  if (code) {
    // i18next will fall back to the English file if the code is
    // missing from the active locale; the `defaultValue` covers
    // the rarer case where the code is new and not yet in the
    // English file either (deploy skew).
    const key = `errors.${code}`;
    const translated = t(key, { defaultValue: "" });
    if (translated) {
      return translated;
    }
  }
  const msg = extractErrorMessage(err);
  if (msg) {
    return msg;
  }
  if (options.fallback) {
    return options.fallback;
  }
  return t("common.error");
}
