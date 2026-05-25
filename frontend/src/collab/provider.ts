// CollabProvider — TipTap/Yjs-compatible WebSocket binding for the
// zk-drive collab WS surface (api/drive/collab.go + internal/collab).
//
// The official y-websocket provider speaks a slightly different wire
// envelope (lib0 var-uint length prefixes, queryAwareness handshake)
// than what our server emits. Rather than translate two protocols at
// the edge, we implement a small provider in-tree that talks the
// exact protocol documented in internal/collab/protocol.go:
//
//   byte[0]   = MessageType  (0x00 sync, 0x01 awareness, 0x02 auth)
//   byte[1]   = SyncSubType  (when MessageType == sync; 0x00/0x01/0x02)
//   byte[2..] = payload      (binary; sync = Yjs update bytes, awareness
//                             = encoded awareness update)
//
// SyncStepUpdates (the server's cold-open frame) carries a snapshot
// BUNDLE: a series of 4-byte-big-endian-length-prefixed segments,
// where each segment is a complete Yjs update. The provider splits
// the bundle and applies each segment via Y.applyUpdate; the order
// is irrelevant because Y.applyUpdate is idempotent and CRDT-merging.
//
// AUTHENTICATION:
//   Browser WebSocket API forbids custom headers, so we carry the
//   bearer JWT via Sec-WebSocket-Protocol. The server-side
//   AuthMiddleware (api/middleware/auth.go) accepts the JWT via
//   the "bearer" marker pattern: ["bearer", "<jwt>"]. The Upgrader
//   echoes back "bearer" so the handshake completes.
//
// PRESENCE GATING:
//   Awareness frames are only emitted when capability.presence_allowed
//   is true. Strict-ZK folders drop the awareness path entirely
//   client-side AND server-side, so this is defense-in-depth.

import * as Y from "yjs";
import {
  Awareness,
  applyAwarenessUpdate,
  encodeAwarenessUpdate,
  removeAwarenessStates,
} from "y-protocols/awareness";

// Wire constants — kept in lockstep with internal/collab/protocol.go.
// Any drift here breaks the binary contract end-to-end.
const MESSAGE_SYNC = 0x00;
const MESSAGE_AWARENESS = 0x01;

const SYNC_STEP_UPDATES = 0x01;
const SYNC_UPDATE = 0x02;

// ConnectionStatus is the externally-observable state machine of the
// provider. The UI binds to the `status` event and renders a chip in
// the editor header.
export type ConnectionStatus =
  | "connecting"
  | "connected"
  | "disconnected"
  | "reconnecting";

export interface CollabProviderOptions {
  url: string;
  token: string;
  doc: Y.Doc;
  // awareness is optional — when omitted the provider only does
  // sync. Passing one (typically created alongside the Y.Doc) wires
  // up presence broadcast in both directions.
  awareness?: Awareness;
  // presenceAllowed gates outbound awareness frames. Even when an
  // Awareness instance is provided we suppress sending if the
  // folder's capability disallows presence — matches the server
  // policy and prevents leaking cursor positions in strict_zk.
  presenceAllowed?: boolean;
  // Optional per-user metadata broadcast in the awareness state.
  // Used by CollaborationCursor extension on TipTap.
  user?: { name: string; color: string };
  // Reconnect backoff (ms). The provider doubles the delay on each
  // failure up to maxReconnectMs and resets on a successful open.
  initialReconnectMs?: number;
  maxReconnectMs?: number;
  // onStatus is called on every state-machine transition.
  onStatus?: (status: ConnectionStatus) => void;
  // onError is called on socket errors and protocol decode errors.
  // Provided as a separate channel from onStatus so the UI can
  // surface a one-shot toast without clobbering the live status
  // chip.
  onError?: (err: Error) => void;
}

// CollabProvider owns a WebSocket connection to the collab WS
// endpoint and pumps frames in both directions between the socket
// and a Y.Doc (+ optional Awareness). It exposes a small lifecycle
// (connect / destroy) and a status callback for the UI.
export class CollabProvider {
  private readonly url: string;
  private readonly token: string;
  private readonly doc: Y.Doc;
  private readonly awareness: Awareness | undefined;
  private readonly presenceAllowed: boolean;
  private readonly onStatus: (s: ConnectionStatus) => void;
  private readonly onError: (e: Error) => void;
  private readonly initialReconnectMs: number;
  private readonly maxReconnectMs: number;

  private ws: WebSocket | null = null;
  private reconnectMs: number;
  private reconnectTimer: number | null = null;
  private destroyed = false;
  // _status is updated through setStatus() so the onStatus callback
  // is always invoked on a real transition; surface as private
  // because callers should consume the value through the onStatus
  // channel rather than poll.
  private currentStatus: ConnectionStatus = "disconnected";
  // needsResync flips true after every WS close. On the next
  // successful open we flush our full local Y.Doc state to the
  // server as a SyncUpdate, recovering any edits the user typed
  // while the socket was down (handleDocUpdate's send is a no-op
  // when the socket is not OPEN). We start at false so the
  // first-ever open does NOT echo the empty initial doc back to the
  // server. Yjs CRDT merge means a duplicate update is harmless on
  // the server side; the only cost is at most one extra delta row
  // per reconnect, which compaction folds away.
  private needsResync = false;

  // Bound handlers so we can attach AND detach the same function
  // references on the Y.Doc / Awareness emitters.
  private readonly handleDocUpdate: (update: Uint8Array, origin: unknown) => void;
  private readonly handleAwarenessUpdate: (
    changes: { added: number[]; updated: number[]; removed: number[] },
    origin: unknown,
  ) => void;

  constructor(opts: CollabProviderOptions) {
    this.url = opts.url;
    this.token = opts.token;
    this.doc = opts.doc;
    this.awareness = opts.awareness;
    // Fail-closed: presence broadcasts ONLY when the caller has
    // explicitly affirmed the capability allows it. Strict-ZK folders
    // would leak cursor positions if we defaulted to true and a
    // future caller forgot to pass the flag; the matching server
    // policy (api/drive/collab.go drops awareness frames on
    // capability.PresenceAllowed=false) is the second line of
    // defense, but the client should not require the server to
    // catch its mistakes.
    this.presenceAllowed = opts.presenceAllowed ?? false;
    this.onStatus = opts.onStatus ?? (() => undefined);
    this.onError = opts.onError ?? (() => undefined);
    this.initialReconnectMs = opts.initialReconnectMs ?? 500;
    this.maxReconnectMs = opts.maxReconnectMs ?? 30_000;
    this.reconnectMs = this.initialReconnectMs;

    // Pre-bind so detach uses the same fn reference. Origin tracking
    // (`origin === this`) is the standard Yjs trick to avoid
    // re-broadcasting updates we just received over the wire.
    this.handleDocUpdate = (update: Uint8Array, origin: unknown) => {
      if (origin === this) return;
      this.sendFrame(buildSyncUpdate(update));
    };
    this.handleAwarenessUpdate = (
      changes: { added: number[]; updated: number[]; removed: number[] },
      origin: unknown,
    ) => {
      if (origin === this) return;
      if (!this.presenceAllowed || !this.awareness) return;
      const changedClients = changes.added
        .concat(changes.updated)
        .concat(changes.removed);
      if (changedClients.length === 0) return;
      const payload = encodeAwarenessUpdate(this.awareness, changedClients);
      this.sendFrame(buildAwarenessFrame(payload));
    };

    if (opts.user && this.awareness) {
      this.awareness.setLocalStateField("user", opts.user);
    }
    this.doc.on("update", this.handleDocUpdate);
    if (this.awareness) {
      this.awareness.on("update", this.handleAwarenessUpdate);
    }
  }

  // connect starts (or restarts) the WebSocket. Safe to call
  // multiple times — an existing open socket is left alone.
  connect(): void {
    if (this.destroyed) return;
    if (this.ws && (this.ws.readyState === WebSocket.OPEN ||
                    this.ws.readyState === WebSocket.CONNECTING)) {
      return;
    }
    this.setStatus("connecting");
    // ["bearer", token] is the well-known subprotocol pair that
    // tells our AuthMiddleware to read the JWT off the next entry.
    // The server echoes "bearer" as the chosen subprotocol; the
    // token itself is never echoed back.
    const ws = new WebSocket(this.url, ["bearer", this.token]);
    ws.binaryType = "arraybuffer";
    ws.onopen = () => this.handleOpen();
    ws.onmessage = (e) => this.handleMessage(e);
    ws.onerror = () => this.handleError(new Error("websocket error"));
    ws.onclose = (e) => this.handleClose(e);
    this.ws = ws;
  }

  // destroy detaches all listeners and closes the socket. Idempotent.
  // After destroy() the provider cannot be revived; create a new
  // instance.
  destroy(): void {
    if (this.destroyed) return;
    this.destroyed = true;
    if (this.reconnectTimer != null) {
      window.clearTimeout(this.reconnectTimer);
      this.reconnectTimer = null;
    }
    this.doc.off("update", this.handleDocUpdate);
    if (this.awareness) {
      this.awareness.off("update", this.handleAwarenessUpdate);
      // Clean up our awareness slot so peers see us disappear.
      removeAwarenessStates(this.awareness, [this.doc.clientID], this);
    }
    if (this.ws) {
      // close() is safe regardless of readyState; suppress the
      // post-close error handler by clearing it first so a transit
      // close doesn't trip onError after destroy.
      this.ws.onopen = null;
      this.ws.onmessage = null;
      this.ws.onerror = null;
      this.ws.onclose = null;
      try {
        this.ws.close(1000, "client destroy");
      } catch {
        // close() can throw if the socket is in an unexpected
        // state; the goal is just to release the FD, so swallow.
      }
      this.ws = null;
    }
    this.setStatus("disconnected");
  }

  // --- internal ----------------------------------------------------

  private handleOpen(): void {
    this.reconnectMs = this.initialReconnectMs;
    this.setStatus("connected");
    // After a reconnect, push our full local Y.Doc state so any
    // edits typed while the socket was down reach the server. See
    // the needsResync field comment for why this is a one-shot
    // per-reconnect operation (not on first connect) and why a
    // single full-state send is safe under CRDT merge semantics.
    if (this.needsResync) {
      this.needsResync = false;
      const localState = Y.encodeStateAsUpdate(this.doc);
      // Yjs encodes an empty doc as a 2-byte structural sentinel;
      // skip if we have nothing meaningful to push.
      if (localState.length > 2) {
        this.sendFrame(buildSyncUpdate(localState));
      }
    }
    // On reconnect, immediately advertise our local awareness state
    // so peers re-discover us without waiting for the next change.
    if (this.presenceAllowed && this.awareness) {
      const payload = encodeAwarenessUpdate(this.awareness, [this.doc.clientID]);
      this.sendFrame(buildAwarenessFrame(payload));
    }
  }

  private handleMessage(e: MessageEvent): void {
    if (!(e.data instanceof ArrayBuffer)) {
      this.onError(new Error("non-binary frame received"));
      return;
    }
    const frame = new Uint8Array(e.data);
    if (frame.length < 1) return;
    const msgType = frame[0];
    try {
      if (msgType === MESSAGE_SYNC) {
        if (frame.length < 2) {
          this.onError(new Error("truncated sync frame"));
          return;
        }
        const subType = frame[1];
        const payload = frame.subarray(2);
        if (subType === SYNC_STEP_UPDATES) {
          // Snapshot bundle: split on 4-byte length prefixes and
          // apply each segment. See AssembleSnapshotBundle in
          // internal/collab/protocol.go for the producer side.
          for (const seg of splitLengthPrefixed(payload)) {
            if (seg.length === 0) continue;
            Y.applyUpdate(this.doc, seg, this);
          }
        } else if (subType === SYNC_UPDATE) {
          if (payload.length > 0) Y.applyUpdate(this.doc, payload, this);
        }
      } else if (msgType === MESSAGE_AWARENESS) {
        if (!this.awareness) return;
        const payload = frame.subarray(1);
        if (payload.length === 0) return;
        applyAwarenessUpdate(this.awareness, payload, this);
      }
    } catch (err) {
      this.onError(err instanceof Error ? err : new Error(String(err)));
    }
  }

  private handleError(err: Error): void {
    this.onError(err);
  }

  private handleClose(e: CloseEvent): void {
    this.ws = null;
    // Flag a resync for the next successful open so we can recover
    // any local edits typed during the outage.
    this.needsResync = true;
    if (this.destroyed) return;
    // Stop the reconnect loop when the server signals a permanent
    // failure. Reconnecting an expired JWT or a policy-rejected
    // session would just burn the network — the user has to take
    // explicit action (refresh, re-auth, fix permissions) before a
    // new connection has any chance of succeeding.
    //
    //   1008 — RFC 6455 "Policy violation" (used by gorilla on
    //          authorization / capability failures).
    //   4001 — application-defined "auth failed / token rejected".
    //   4003 — application-defined "document inaccessible /
    //          forbidden / disabled".
    //
    // A graceful 1000 ("normal closure") is also non-retriable from
    // the client's POV: the server explicitly told us to stop.
    if (isPermanentCloseCode(e.code)) {
      this.setStatus("disconnected");
      this.onError(
        new Error(`websocket closed permanently (code=${e.code}${e.reason ? `, reason=${e.reason}` : ""})`),
      );
      return;
    }
    this.setStatus("reconnecting");
    // Exponential backoff with cap, plus ±20% uniform jitter. The cap
    // alone is insufficient against thundering herd: if a backend
    // restart drops 1,000 connections at the same instant, every
    // client without jitter wakes at the SAME multiplied delay (500,
    // 1000, 2000 …) and re-stampedes the server in lockstep. Adding
    // jitter to the per-attempt delay smears the wake-up window so
    // reconnect traffic arrives spread across a 40% band of the
    // nominal delay. The jitter is symmetric (multiplier ∈ [0.8,
    // 1.2]) so the long-run mean stays close to the configured
    // backoff schedule; we don't decorrelate further (full jitter)
    // because that hurts the p99 reconnect time without much
    // additional herd-smoothing benefit at our connection counts.
    const jittered = this.reconnectMs * (0.8 + Math.random() * 0.4);
    this.reconnectTimer = window.setTimeout(() => {
      this.reconnectTimer = null;
      this.reconnectMs = Math.min(this.reconnectMs * 2, this.maxReconnectMs);
      this.connect();
    }, jittered);
  }

  private sendFrame(frame: Uint8Array): void {
    if (!this.ws || this.ws.readyState !== WebSocket.OPEN) return;
    // The browser will copy the buffer; reusing the same Uint8Array
    // after the call is safe.
    this.ws.send(frame);
  }

  private setStatus(s: ConnectionStatus): void {
    if (s === this.currentStatus) return;
    this.currentStatus = s;
    this.onStatus(s);
  }
}

// --- close-code policy --------------------------------------------

// PermanentCloseCodes enumerates the WebSocket close codes that
// indicate the server will keep rejecting future connections from
// this client until something external changes (token refresh,
// permission grant, etc.). Reconnecting on these codes is wasted
// effort and can amplify auth-failure traffic.
//
// 1000 ("normal closure") is included because the server only sends
// 1000 from explicit shutdown paths (Hub.Shutdown) — a transient
// network blip surfaces as 1006 ("abnormal closure") instead, which
// is correctly retriable.
//
// Exported for tests; the runtime check uses isPermanentCloseCode.
export const PermanentCloseCodes: readonly number[] = [1000, 1008, 4001, 4003];

function isPermanentCloseCode(code: number): boolean {
  return PermanentCloseCodes.includes(code);
}

// --- frame builders ------------------------------------------------

function buildSyncUpdate(update: Uint8Array): Uint8Array {
  const out = new Uint8Array(2 + update.length);
  out[0] = MESSAGE_SYNC;
  out[1] = SYNC_UPDATE;
  out.set(update, 2);
  return out;
}

function buildAwarenessFrame(payload: Uint8Array): Uint8Array {
  const out = new Uint8Array(1 + payload.length);
  out[0] = MESSAGE_AWARENESS;
  out.set(payload, 1);
  return out;
}

// splitLengthPrefixed yields each segment of a 4-byte-big-endian
// length-prefixed bundle. Mirrors LengthPrefix +
// AssembleSnapshotBundle in internal/collab/protocol.go. Exported
// for unit tests; callers shouldn't need it.
export function* splitLengthPrefixed(buf: Uint8Array): Generator<Uint8Array> {
  let offset = 0;
  const view = new DataView(buf.buffer, buf.byteOffset, buf.byteLength);
  while (offset + 4 <= buf.length) {
    const len = view.getUint32(offset, false); // big-endian
    offset += 4;
    if (offset + len > buf.length) {
      // Truncated segment; abandon the rest of the bundle. The
      // caller will surface this via onError if the bundle was
      // malformed end-to-end — we just stop yielding.
      return;
    }
    yield buf.subarray(offset, offset + len);
    offset += len;
  }
}
