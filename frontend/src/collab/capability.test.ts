// capability.test — exercises the client-side mirror of
// internal/document/capability.go. The two MUST agree on which
// modes are allowed for each encryption_mode; this suite pins the
// matrix so a drift between front and backend is caught locally.

import { describe, it, expect } from "vitest";
import { resolveCapability, resolveAllowedCollabModes } from "./capability";

describe("resolveCapability", () => {
  it("grants every capability for managed_encrypted", () => {
    expect(resolveCapability("managed_encrypted")).toEqual({
      serverSnapshotAllowed: true,
      richExtensionsAllowed: true,
      presenceAllowed: true,
    });
  });

  it("denies every capability for strict_zk", () => {
    expect(resolveCapability("strict_zk")).toEqual({
      serverSnapshotAllowed: false,
      richExtensionsAllowed: false,
      presenceAllowed: false,
    });
  });

  it("fails closed on unknown or missing modes", () => {
    expect(resolveCapability(undefined)).toEqual({
      serverSnapshotAllowed: false,
      richExtensionsAllowed: false,
      presenceAllowed: false,
    });
    expect(resolveCapability("future_mode_that_doesnt_exist_yet")).toEqual({
      serverSnapshotAllowed: false,
      richExtensionsAllowed: false,
      presenceAllowed: false,
    });
  });
});

describe("resolveAllowedCollabModes", () => {
  it("returns [markdown, rich, rich_presence] for managed_encrypted", () => {
    expect(resolveAllowedCollabModes("managed_encrypted")).toEqual([
      "markdown",
      "rich",
      "rich_presence",
    ]);
  });

  it("returns [markdown] only for strict_zk", () => {
    expect(resolveAllowedCollabModes("strict_zk")).toEqual(["markdown"]);
  });

  it("never returns 'disabled' (server-set tombstone)", () => {
    for (const mode of ["managed_encrypted", "strict_zk", undefined]) {
      expect(resolveAllowedCollabModes(mode)).not.toContain("disabled");
    }
  });
});
