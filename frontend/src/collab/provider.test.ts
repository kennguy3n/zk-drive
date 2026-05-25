// Provider unit tests — exercise the binary protocol codec without
// spinning up a real WebSocket. The framing is shared between the
// Go server (internal/collab/protocol.go) and this TS provider; if
// either side drifts, the bundle parser would silently corrupt
// snapshots and the test catches that.

import { describe, it, expect } from "vitest";
import * as Y from "yjs";
import { CollabProvider, splitLengthPrefixed } from "./provider";

// Build a Yjs update that meaningfully exercises Y.applyUpdate so
// the snapshot bundle ingestion test below verifies decode + apply.
function makeUpdate(text: string): Uint8Array {
  const doc = new Y.Doc();
  doc.getText("body").insert(0, text);
  return Y.encodeStateAsUpdate(doc);
}

// Encode a snapshot bundle the way the Go server does in
// internal/collab/protocol.go AssembleSnapshotBundle: each segment
// is prefixed with a 4-byte big-endian length. We hand-roll the
// encoder here so the test is a true round-trip against the
// documented wire shape, not against another TS helper.
function buildBundle(segments: Uint8Array[]): Uint8Array {
  let total = 0;
  for (const s of segments) total += 4 + s.length;
  const out = new Uint8Array(total);
  const view = new DataView(out.buffer);
  let off = 0;
  for (const s of segments) {
    view.setUint32(off, s.length, false);
    off += 4;
    out.set(s, off);
    off += s.length;
  }
  return out;
}

describe("splitLengthPrefixed", () => {
  it("splits a bundle of three updates back into the originals", () => {
    const a = makeUpdate("alpha");
    const b = makeUpdate("bravo");
    const c = makeUpdate("charlie");
    const bundle = buildBundle([a, b, c]);

    const segs = Array.from(splitLengthPrefixed(bundle));
    expect(segs.length).toBe(3);
    expect(segs[0]).toEqual(a);
    expect(segs[1]).toEqual(b);
    expect(segs[2]).toEqual(c);
  });

  it("yields nothing for an empty buffer", () => {
    expect(Array.from(splitLengthPrefixed(new Uint8Array(0)))).toEqual([]);
  });

  it("stops cleanly on a truncated final segment", () => {
    // Build a valid first segment, then a length prefix declaring 100
    // bytes followed by only 4 bytes of payload — a truncated bundle.
    const valid = makeUpdate("hi");
    const validFramed = buildBundle([valid]);
    const truncated = new Uint8Array(validFramed.length + 4 + 4);
    truncated.set(validFramed, 0);
    const view = new DataView(truncated.buffer);
    view.setUint32(validFramed.length, 100, false);
    // Only 4 bytes of "payload" — well short of 100 — follow.

    const segs = Array.from(splitLengthPrefixed(truncated));
    expect(segs.length).toBe(1);
    expect(segs[0]).toEqual(valid);
  });
});

describe("CollabProvider snapshot ingestion", () => {
  it("applies a server-shaped SyncStepUpdates bundle to the Y.Doc", () => {
    // Build the canonical y_state ("alpha") + two tail deltas
    // ("bravo", "charlie") that the Go AssembleSnapshotBundle would
    // emit, and feed it through the provider's frame handler.
    const yState = makeUpdate("alpha");
    const tail1 = makeUpdate("bravo");
    const tail2 = makeUpdate("charlie");
    const bundle = buildBundle([yState, tail1, tail2]);

    // MessageSync (0x00) + SyncStepUpdates (0x01) + bundle.
    const frame = new Uint8Array(2 + bundle.length);
    frame[0] = 0x00;
    frame[1] = 0x01;
    frame.set(bundle, 2);

    const doc = new Y.Doc();
    const provider = new CollabProvider({
      url: "ws://localhost/api/v1/documents/x/ws",
      token: "test-token",
      doc,
    });
    // Drive the message handler directly — we don't need a live
    // socket to verify the codec. The provider's internal state
    // (ws / status) is irrelevant for this assertion.
    const handler = (provider as unknown as {
      handleMessage(e: MessageEvent): void;
    }).handleMessage.bind(provider);
    handler(new MessageEvent("message", { data: frame.buffer }));

    // After applying the three concatenated updates Yjs's CRDT
    // merge yields all three segments visible in the doc. Order
    // tolerance is the whole point of the design.
    const merged = doc.getText("body").toString();
    expect(merged).toContain("alpha");
    expect(merged).toContain("bravo");
    expect(merged).toContain("charlie");

    provider.destroy();
  });
});

describe("CollabProvider lifecycle", () => {
  it("emits a connecting status when connect() is called and disconnected on destroy()", () => {
    const events: string[] = [];
    const doc = new Y.Doc();
    const provider = new CollabProvider({
      url: "ws://127.0.0.1:1/api/v1/documents/x/ws",
      token: "test",
      doc,
      onStatus: (s) => events.push(s),
    });
    provider.connect();
    // The browser WebSocket constructor synchronously transitions
    // to CONNECTING, so we must see at least "connecting" before
    // we tear it down. We then call destroy() which forces
    // "disconnected" through setStatus().
    provider.destroy();
    expect(events).toContain("connecting");
    expect(events[events.length - 1]).toBe("disconnected");
  });
});
