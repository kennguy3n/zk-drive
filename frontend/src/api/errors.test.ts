// Unit tests for translateApiError and friends. The goal is to lock
// in the resolution-order contract (code -> server message -> caller
// fallback -> generic copy) AND specifically guard against two
// regressions we shipped in earlier PR #83 commits:
//
//  - BUG_0001 (commit bb909a1): callers wrote `translateApiError(e, t)
//    || t("billing.X")`, assuming the helper could return an empty
//    string when no code and no server message were available. Final
//    fallback was t("common.error"), so the `||` branch was dead.
//    Fixed in 7d7b4fe by introducing the `options.fallback` parameter.
//
//  - BUG_0002 (commit 929da57): i18next's returnEmptyString:false
//    rejects an explicit defaultValue:"" and returns the raw key,
//    so an unknown code surfaced "errors.SOME_NEW_CODE" verbatim to
//    the user instead of falling through to the server-supplied
//    message during deploy skew. Fixed in this commit by adding
//    the `translated !== key` guard.
//
// We isolate the test from the global i18next singleton (src/test/
// setup.ts) by creating a fresh per-test instance so we can control
// the `returnEmptyString` behaviour and the set of known keys.
import { describe, expect, it } from "vitest";
import i18next, { type TFunction } from "i18next";

import { translateApiError } from "./errors";

async function makeT(opts: {
  knownKeys?: Record<string, string>;
  returnEmptyString?: boolean;
} = {}): Promise<TFunction> {
  const instance = i18next.createInstance();
  await instance.init({
    lng: "en",
    fallbackLng: "en",
    resources: { en: { translation: opts.knownKeys ?? {} } },
    interpolation: { escapeValue: false },
    returnEmptyString: opts.returnEmptyString ?? false,
  });
  return instance.t.bind(instance) as TFunction;
}

describe("translateApiError", () => {
  it("returns the localized copy when the error carries a known code", async () => {
    const t = await makeT({
      knownKeys: {
        "errors.WORKSPACE_QUOTA_EXCEEDED": "You're out of room.",
        "common.error": "Something went wrong",
      },
    });
    const out = translateApiError(
      { response: { data: { code: "WORKSPACE_QUOTA_EXCEEDED", message: "raw go err" } } },
      t,
    );
    expect(out).toBe("You're out of room.");
  });

  it("falls back to the server-supplied message when the code is unknown to the locale", async () => {
    // Deploy skew: the SPA's en.json doesn't ship a translation for
    // SOME_BRAND_NEW_CODE yet, but the backend sends it in the
    // response. The bug we're guarding against (BUG_0002) is i18next
    // returning the raw key string "errors.SOME_BRAND_NEW_CODE"
    // instead of falling through.
    const t = await makeT({
      knownKeys: { "common.error": "Something went wrong" },
      returnEmptyString: false,
    });
    const out = translateApiError(
      { response: { data: { code: "SOME_BRAND_NEW_CODE", message: "Backend developer copy." } } },
      t,
    );
    expect(out).toBe("Backend developer copy.");
    expect(out).not.toContain("errors.SOME_BRAND_NEW_CODE");
  });

  it("falls back to the generic message when no code and no server message are present", async () => {
    const t = await makeT({
      knownKeys: { "common.error": "Something went wrong" },
    });
    const out = translateApiError(new Error("network gone"), t);
    // .message of an Error is returned by extractErrorMessage so this
    // path is the "server message" branch — verify the generic copy
    // doesn't override it.
    expect(out).toBe("network gone");
  });

  it("uses options.fallback when no code, no server message, and a fallback is supplied", async () => {
    const t = await makeT({
      knownKeys: { "common.error": "Something went wrong" },
    });
    const out = translateApiError({}, t, { fallback: "Could not start checkout" });
    expect(out).toBe("Could not start checkout");
  });

  it("uses the generic copy when no code, no server message, and no fallback", async () => {
    const t = await makeT({
      knownKeys: { "common.error": "Something went wrong" },
    });
    const out = translateApiError({}, t);
    expect(out).toBe("Something went wrong");
  });

  it("never surfaces a raw `errors.*` key even when the locale rejects the empty default value", async () => {
    // Direct regression test for BUG_0002. With returnEmptyString:
    // false (production setting), i18next returns the key string
    // when defaultValue:"" is supplied and the key is unresolved.
    // We assert the helper NEVER lets that key leak to the UI.
    const t = await makeT({
      knownKeys: { "common.error": "Something went wrong" },
      returnEmptyString: false,
    });
    const out = translateApiError(
      { response: { data: { code: "UNKNOWN_CODE_FROM_FUTURE_BACKEND" } } },
      t,
    );
    expect(out).toBe("Something went wrong");
    expect(out).not.toContain("errors.");
  });
});
