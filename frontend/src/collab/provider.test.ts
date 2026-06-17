// Provider unit tests — exercise the binary protocol codec without
// spinning up a real WebSocket. The framing is shared between the
// Go server (internal/collab/protocol.go) and this TS provider; if
// either side drifts, the bundle parser would silently corrupt
// snapshots and the test catches that.

import { describe, it, expect } from "vitest";
import * as Y from "yjs";
import {
  CollabProvider,
  PermanentCloseCodes,
  splitLengthPrefixed,
} from "./provider";

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
      url: "ws://localhost/api/documents/x/ws",
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
      url: "ws://127.0.0.1:1/api/documents/x/ws",
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

describe("CollabProvider close-code policy", () => {
  it("does NOT reconnect on permanent close codes (1000, 1008, 4001, 4003)", () => {
    // Pin the exported list so any future drift fails the test
    // rather than silently changing the policy.
    expect(PermanentCloseCodes).toEqual([1000, 1008, 4001, 4003]);

    const doc = new Y.Doc();
    for (const code of PermanentCloseCodes) {
      const events: string[] = [];
      let errored = false;
      const provider = new CollabProvider({
        url: "ws://127.0.0.1:1/api/documents/x/ws",
        token: "test",
        doc,
        onStatus: (s) => events.push(s),
        onError: () => {
          errored = true;
        },
      });
      // Force the provider out of the initial "disconnected"
      // state so a setStatus("disconnected") inside handleClose
      // actually emits a transition we can observe.
      (
        provider as unknown as { setStatus(s: string): void }
      ).setStatus("connected");
      // Drive handleClose directly with the permanent code.
      const handleClose = (
        provider as unknown as { handleClose(e: CloseEvent): void }
      ).handleClose.bind(provider);
      handleClose(new CloseEvent("close", { code, reason: "test" }));
      // No reconnect transition should ever appear; final status
      // must end in "disconnected"; an error must be surfaced.
      expect(events.includes("reconnecting"), `code=${code}`).toBe(false);
      expect(events[events.length - 1], `code=${code}`).toBe("disconnected");
      expect(errored, `code=${code} should onError`).toBe(true);
      provider.destroy();
    }
  });

  it("flushes local state on reconnect so offline edits are not lost", () => {
    // Wire up a fake WebSocket so we can observe what the provider
    // sends after a reconnect. The provider only calls .send() when
    // readyState === OPEN; we mock the minimum surface.
    const sent: Uint8Array[] = [];
    class FakeSocket {
      readyState = 1; // OPEN
      send(data: ArrayBuffer | ArrayBufferView) {
        if (data instanceof ArrayBuffer) {
          sent.push(new Uint8Array(data));
        } else if (ArrayBuffer.isView(data)) {
          sent.push(new Uint8Array(data.buffer, data.byteOffset, data.byteLength));
        }
      }
      close() {}
    }

    const doc = new Y.Doc();
    // Pre-populate with content so encodeStateAsUpdate has > 2 bytes.
    doc.getText("body").insert(0, "offline edit");

    const provider = new CollabProvider({
      url: "ws://127.0.0.1:1/api/documents/x/ws",
      token: "test",
      doc,
    });

    // Force needsResync=true (as a real close() would) and install
    // our fake WS so handleOpen's sendFrame path can observe it.
    (provider as unknown as { needsResync: boolean }).needsResync = true;
    (provider as unknown as { ws: unknown }).ws = new FakeSocket();
    (provider as unknown as { handleOpen(): void }).handleOpen();

    // We expect at least one SyncUpdate frame: byte0=0x00 (SYNC),
    // byte1=0x02 (SYNC_UPDATE), followed by the Y.Doc update.
    const syncUpdates = sent.filter((b) => b.length >= 2 && b[0] === 0x00 && b[1] === 0x02);
    expect(syncUpdates.length).toBeGreaterThan(0);
    // The flushed payload must round-trip back to the same content.
    const recovered = new Y.Doc();
    Y.applyUpdate(recovered, syncUpdates[0].subarray(2));
    expect(recovered.getText("body").toString()).toBe("offline edit");

    // The flag must reset so a second open does not duplicate.
    expect((provider as unknown as { needsResync: boolean }).needsResync).toBe(false);

    provider.destroy();
  });

  it("does NOT flush on the very first connect (avoids duplicate of server-sent baseline)", () => {
    const sent: Uint8Array[] = [];
    class FakeSocket {
      readyState = 1;
      send(data: ArrayBuffer | ArrayBufferView) {
        if (data instanceof ArrayBuffer) {
          sent.push(new Uint8Array(data));
        } else if (ArrayBuffer.isView(data)) {
          sent.push(new Uint8Array(data.buffer, data.byteOffset, data.byteLength));
        }
      }
      close() {}
    }

    const doc = new Y.Doc();
    doc.getText("body").insert(0, "first connect");
    const provider = new CollabProvider({
      url: "ws://127.0.0.1:1/api/documents/x/ws",
      token: "test",
      doc,
    });
    // needsResync defaults to false; first handleOpen should NOT emit
    // a sync update even though the doc has content.
    (provider as unknown as { ws: unknown }).ws = new FakeSocket();
    (provider as unknown as { handleOpen(): void }).handleOpen();

    const syncUpdates = sent.filter((b) => b.length >= 2 && b[0] === 0x00 && b[1] === 0x02);
    expect(syncUpdates).toEqual([]);
    provider.destroy();
  });

  it("DOES reconnect on transient close codes (1006 abnormal closure)", () => {
    const events: string[] = [];
    const doc = new Y.Doc();
    const provider = new CollabProvider({
      url: "ws://127.0.0.1:1/api/documents/x/ws",
      token: "test",
      doc,
      // Long enough that destroy() in the next line fires before
      // the reconnect attempt; we only care about the status edge.
      initialReconnectMs: 30_000,
      onStatus: (s) => events.push(s),
    });
    const handleClose = (
      provider as unknown as { handleClose(e: CloseEvent): void }
    ).handleClose.bind(provider);
    handleClose(new CloseEvent("close", { code: 1006, reason: "abnormal" }));
    expect(events).toContain("reconnecting");
    provider.destroy();
  });
});

describe("CollabProvider token refresh", () => {
  // Stub the global WebSocket so connect() doesn't open a real socket
  // and we can capture the subprotocol pair (where the bearer token
  // travels) the provider hands the constructor.
  function withStubbedWS(fn: (captured: { protocols: string[][] }) => void) {
    const captured = { protocols: [] as string[][] };
    const original = globalThis.WebSocket;
    class StubSocket {
      static readonly OPEN = 1;
      static readonly CONNECTING = 0;
      readyState = 0;
      onopen: (() => void) | null = null;
      onmessage: (() => void) | null = null;
      onerror: (() => void) | null = null;
      onclose: (() => void) | null = null;
      binaryType = "";
      constructor(_url: string, protocols?: string | string[]) {
        captured.protocols.push(
          Array.isArray(protocols) ? protocols : protocols ? [protocols] : [],
        );
      }
      send() {}
      close() {}
    }
    (globalThis as unknown as { WebSocket: unknown }).WebSocket = StubSocket;
    try {
      fn(captured);
    } finally {
      (globalThis as unknown as { WebSocket: unknown }).WebSocket = original;
    }
  }

  it("consults tokenProvider on connect and sends the refreshed token", () => {
    withStubbedWS((captured) => {
      let current = "fresh-token";
      const doc = new Y.Doc();
      const provider = new CollabProvider({
        url: "ws://127.0.0.1:1/api/documents/x/ws",
        token: "stale-token",
        tokenProvider: () => current,
        doc,
      });
      provider.connect();
      expect(captured.protocols[0]).toEqual(["bearer", "fresh-token"]);

      // A later reconnect picks up the rotated token.
      current = "rotated-token";
      (provider as unknown as { ws: unknown }).ws = null;
      provider.connect();
      expect(captured.protocols[1]).toEqual(["bearer", "rotated-token"]);
      provider.destroy();
    });
  });

  it("falls back to the static token when tokenProvider returns null", () => {
    withStubbedWS((captured) => {
      const doc = new Y.Doc();
      const provider = new CollabProvider({
        url: "ws://127.0.0.1:1/api/documents/x/ws",
        token: "static-token",
        tokenProvider: () => null,
        doc,
      });
      provider.connect();
      expect(captured.protocols[0]).toEqual(["bearer", "static-token"]);
      provider.destroy();
    });
  });
});

describe("CollabProvider in-band auth refresh", () => {
  // Capture frames the provider pushes on the live socket. refreshAuth
  // only sends when readyState === OPEN, mirroring sendFrame.
  class FakeSocket {
    readyState = 1; // OPEN
    sent: Uint8Array[] = [];
    send(data: ArrayBuffer | ArrayBufferView) {
      if (data instanceof ArrayBuffer) {
        this.sent.push(new Uint8Array(data));
      } else if (ArrayBuffer.isView(data)) {
        this.sent.push(new Uint8Array(data.buffer, data.byteOffset, data.byteLength));
      }
    }
    close() {}
  }

  it("sends a MessageAuth frame carrying the freshest token", () => {
    let current = "rotated-token";
    const doc = new Y.Doc();
    const provider = new CollabProvider({
      url: "ws://127.0.0.1:1/api/documents/x/ws",
      token: "stale-token",
      tokenProvider: () => current,
      doc,
    });
    const sock = new FakeSocket();
    (provider as unknown as { ws: unknown }).ws = sock;

    provider.refreshAuth();

    // Exactly one frame: byte0 = 0x02 (MESSAGE_AUTH), payload = token.
    expect(sock.sent.length).toBe(1);
    const frame = sock.sent[0];
    expect(frame[0]).toBe(0x02);
    expect(new TextDecoder().decode(frame.subarray(1))).toBe("rotated-token");

    // A second refresh after another rotation carries the new token.
    current = "rotated-again";
    provider.refreshAuth();
    expect(sock.sent.length).toBe(2);
    expect(new TextDecoder().decode(sock.sent[1].subarray(1))).toBe("rotated-again");

    provider.destroy();
  });

  it("is a no-op when the socket is not OPEN", () => {
    const doc = new Y.Doc();
    const provider = new CollabProvider({
      url: "ws://127.0.0.1:1/api/documents/x/ws",
      token: "tok",
      tokenProvider: () => "tok",
      doc,
    });
    const sock = new FakeSocket();
    sock.readyState = 0; // CONNECTING
    (provider as unknown as { ws: unknown }).ws = sock;

    provider.refreshAuth();
    expect(sock.sent).toEqual([]);
    provider.destroy();
  });
});
