import { describe, it, expect } from "vitest";
import { Feature } from "./featureKeys";
// Vite ?raw import: pulls the Go source in as a string at test time (typed by
// vite/client). Keeps this drift guard dependency-free (no @types/node / fs).
import flagsSrc from "../../../internal/feature/flags.go?raw";

// Drift guard for the hand-maintained feature-key contract: the strings in
// internal/feature/flags.go (Go) and frontend/src/features/featureKeys.ts (TS)
// must stay identical, or feature gating silently breaks for any key present
// on only one side. Rather than rely on a CI-only check, this runs in the
// normal `npm run test` suite and reads the Go source directly so adding a
// key to one side without the other fails fast, locally.
describe("feature key parity (Go <-> TS)", () => {
  it("Feature values exactly match internal/feature/flags.go", () => {
    // Extract the string literal from each `Feature<Name> = "<value>"` const.
    // Capture the whole literal (any non-quote run) rather than a restricted
    // [a-z_]+ class: a key that doesn't fit the expected charset (e.g. a digit
    // like "feature_v2", or an accidental uppercase) must surface as a parity
    // mismatch, not be silently skipped by the regex — silent omission would
    // blind the very drift this guard exists to catch.
    const goKeys = [...flagsSrc.matchAll(/Feature\w+\s*=\s*"([^"]+)"/g)].map(
      (m) => m[1],
    );

    expect(goKeys.length).toBeGreaterThan(0);
    expect([...new Set(goKeys)].sort()).toEqual(
      [...new Set(Object.values(Feature))].sort(),
    );
  });
});
