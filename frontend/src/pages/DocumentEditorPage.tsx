// DocumentEditorPage — TipTap + Yjs collaborative editor.
//
// The page assembles four moving parts:
//
//   1. The Y.Doc + (optional) Awareness — the CRDT state surface.
//   2. The CollabProvider — a WebSocket connection to
//      `/api/documents/{id}/ws` that pumps binary Yjs updates
//      between the Y.Doc and the server (see src/collab/provider.ts).
//   3. TipTap with capability-gated extensions — StarterKit always,
//      plus tables / image / link in rich modes, plus
//      CollaborationCursor in rich+presence modes.
//   4. The editor header — encryption-mode badge + collab-mode pill
//      + connection-status chip + presence chips.
//
// CAPABILITY MATRIX (matches internal/document/capability.go):
//
//   encryption_mode = managed_encrypted
//     collab_mode = markdown      → StarterKit only, no presence.
//     collab_mode = rich          → StarterKit + Table + Image + Link.
//     collab_mode = rich_presence → above + CollaborationCursor.
//
//   encryption_mode = strict_zk
//     collab_mode = markdown      → StarterKit only, no presence
//                                   (server drops awareness frames).
//
//   collab_mode = disabled        → editor is rendered read-only;
//                                   no WS connection is opened.

import { useCallback, useEffect, useMemo, useRef, useState } from "react";
import { Link, useNavigate, useParams } from "react-router-dom";
import * as Y from "yjs";
import { Awareness } from "y-protocols/awareness";
import { useEditor, EditorContent, type AnyExtension } from "@tiptap/react";
import StarterKit from "@tiptap/starter-kit";
import Collaboration from "@tiptap/extension-collaboration";
import CollaborationCursor from "@tiptap/extension-collaboration-cursor";
import {
  currentToken,
  currentUserID,
  documentCollabURL,
  getDocument,
  renameDocument,
  setDocumentCollabMode,
  type CollabMode,
  type Document,
} from "../api/client";
import { CollabProvider, type ConnectionStatus } from "../collab/provider";
import EncryptionBadge from "../components/EncryptionBadge";
import ConnectionStatusChip from "../components/ConnectionStatusChip";
import PresenceChips from "../components/PresenceChips";
import CollabModeSelector from "../components/CollabModeSelector";

// userPresenceColor deterministically picks a hue from a user id so
// the same user gets the same cursor color across reconnects and
// across tabs. Twelve evenly-spaced HSL slots avoid clashes with the
// app palette while staying readable on light backgrounds.
function userPresenceColor(userID: string): string {
  let hash = 0;
  for (let i = 0; i < userID.length; i++) {
    hash = (hash * 31 + userID.charCodeAt(i)) | 0;
  }
  const hue = ((hash % 12) + 12) % 12;
  return `hsl(${hue * 30}, 65%, 50%)`;
}

// displayNameForUser produces a short label from the user id. The
// API currently does not return display names on AuthResponse, so
// we use a deterministic prefix; once a profile endpoint exists the
// chip will switch to real names without a wire-shape change
// (PresenceChips reads `awareness.user.name`).
function displayNameForUser(userID: string | null): string {
  if (!userID) return "Anonymous";
  return `User ${userID.slice(0, 6)}`;
}

export default function DocumentEditorPage() {
  const { id } = useParams<{ id: string }>();
  const nav = useNavigate();

  const [doc, setDoc] = useState<Document | null>(null);
  const [loadError, setLoadError] = useState<string | null>(null);
  const [status, setStatus] = useState<ConnectionStatus>("disconnected");
  const [renameValue, setRenameValue] = useState<string>("");
  const [renaming, setRenaming] = useState(false);
  const [modeSwitchOpen, setModeSwitchOpen] = useState(false);
  const [modeSwitching, setModeSwitching] = useState(false);
  const [modeSwitchError, setModeSwitchError] = useState<string | null>(null);

  // Y.Doc + Awareness live in state so the TipTap extension list
  // recomputes when they're (re)created. Holding them only in refs
  // would mean the useMemo below wouldn't see the new instances on
  // its next pass and the editor would mount with an empty
  // extension list. Provider stays in a ref because nothing
  // downstream needs to render off it.
  const [yDoc, setYDoc] = useState<Y.Doc | null>(null);
  const [awareness, setAwareness] = useState<Awareness | null>(null);
  const providerRef = useRef<CollabProvider | null>(null);

  // Fetch the document metadata on mount / id change.
  useEffect(() => {
    if (!id) return;
    let cancelled = false;
    setLoadError(null);
    setDoc(null);
    getDocument(id)
      .then((d) => {
        if (cancelled) return;
        setDoc(d);
        setRenameValue(d.name);
      })
      .catch((e) => {
        if (cancelled) return;
        setLoadError(String((e as Error)?.message ?? e));
      });
    return () => {
      cancelled = true;
    };
  }, [id]);

  // Build (or rebuild) the Y.Doc + provider whenever the document's
  // collab_mode changes. We avoid recreating on every doc-fetch by
  // depending on (id, collab_mode, presenceAllowed) — three primitive
  // values derived from doc, NOT the `doc` object reference itself.
  // Including `doc` in the dependency array would tear the
  // WebSocket down on every rename (rename triggers setDoc which
  // produces a new object reference), destroying the live CRDT
  // state and forcing a resync. presenceAllowed is itself derived
  // from collabMode + doc.capability.presence_allowed; when the
  // capability flips (only possible across a collab_mode change),
  // the dependency captures that transition.
  const collabMode: CollabMode = doc?.collab_mode ?? "disabled";
  const presenceAllowed =
    !!doc && doc.capability.presence_allowed && collabMode === "rich_presence";
  const writable = !!doc && collabMode !== "disabled";

  useEffect(() => {
    if (!id) return;
    if (collabMode === "disabled") {
      // No WS connection in tombstone mode (also the pre-load
      // state where doc has not yet fetched, since collabMode
      // defaults to "disabled" then). The editor below is
      // rendered with setEditable(false). Awareness instance is
      // not created so PresenceChips stays empty.
      setYDoc(null);
      setAwareness(null);
      return;
    }
    const nextYDoc = new Y.Doc();
    const nextAwareness = presenceAllowed ? new Awareness(nextYDoc) : null;
    const token = currentToken();
    if (!token) {
      setLoadError("not authenticated");
      nextYDoc.destroy();
      return;
    }
    setYDoc(nextYDoc);
    setAwareness(nextAwareness);
    const provider = new CollabProvider({
      url: documentCollabURL(id),
      token,
      doc: nextYDoc,
      awareness: nextAwareness ?? undefined,
      presenceAllowed,
      user: {
        name: displayNameForUser(currentUserID()),
        color: userPresenceColor(currentUserID() ?? id),
      },
      onStatus: setStatus,
      onError: (e) => {
        // Errors flow through the status chip (reconnecting state)
        // and a console line so devs can debug protocol issues
        // without surfacing a noisy toast for every transient
        // disconnect.
        // eslint-disable-next-line no-console
        console.warn("collab provider error", e);
      },
    });
    providerRef.current = provider;
    provider.connect();
    return () => {
      // Teardown order matches the y-protocols canonical lifecycle:
      //   1. provider.destroy() — detaches WS, removes our awareness
      //      slot via removeAwarenessStates so peers see us disappear.
      //   2. awareness.destroy() — unregisters the internal listener
      //      on the Y.Doc's destroy event. Calling this BEFORE the
      //      Y.Doc is destroyed is the documented order; Y.Doc.destroy
      //      would also trigger the same cleanup via its event emit,
      //      but an explicit call is robust against future y-protocols
      //      changes (and idempotent — destroy() is safe to call twice).
      //   3. yDoc.destroy() — releases the CRDT state surface.
      provider.destroy();
      nextAwareness?.destroy();
      nextYDoc.destroy();
      providerRef.current = null;
      setYDoc(null);
      setAwareness(null);
      setStatus("disconnected");
    };
  }, [id, collabMode, presenceAllowed]);

  // Build the TipTap extension list based on the resolved
  // capability + collab mode. StarterKit is ALWAYS included
  // (even before doc / yDoc load) because useEditor below
  // runs on every render — including the first one, where
  // hasDoc=false and yDoc=null — and ProseMirror refuses to
  // compile a schema that lacks a top-level "doc" node type
  // (RangeError: Schema is missing its top node type 'doc').
  // The "Loading…" early-return below means the empty
  // StarterKit editor never paints, so the cost is one
  // editor instantiation that's torn down + recreated when
  // yDoc arrives. Collaboration / CollaborationCursor stay
  // conditional because they require a non-null Y.Doc and
  // Awareness respectively.
  const extensions = useMemo(() => {
    // StarterKit's bundled history extension conflicts with
    // Collaboration's CRDT undo stack — disabling it here is the
    // documented TipTap pattern for collab editors.
    const base: AnyExtension[] = [StarterKit.configure({ history: false })];
    if (yDoc && collabMode !== "disabled") {
      base.push(Collaboration.configure({ document: yDoc }));
    }
    if (presenceAllowed && awareness) {
      base.push(
        CollaborationCursor.configure({
          provider: { awareness },
        }),
      );
    }
    return base;
    // Deliberately depend on the primitive yDoc / collabMode /
    // presenceAllowed instead of the `doc` object reference.
    // Renames mutate `doc` (new object reference) but leave
    // collabMode / presenceAllowed unchanged, so the editor
    // doesn't re-instantiate and the cursor / selection state
    // is preserved across renames.
  }, [yDoc, awareness, collabMode, presenceAllowed]);

  // TipTap editor instance. Keyed by document id so a route
  // change tears down the previous editor entirely; this also
  // avoids the "useEditor doesn't re-mount on dep change" trap
  // where extensions update silently but the rendered doc
  // doesn't reset.
  const editor = useEditor(
    {
      extensions,
      editable: writable,
      // The initial content is intentionally empty — Yjs will
      // hydrate the editor from the snapshot bundle on connect.
      content: "",
    },
    // Re-instantiate the editor when the extension shape changes.
    [extensions, writable],
  );

  // Keep the editor's editable flag in sync with permissions
  // (e.g. an admin downgrades the collab_mode to "disabled" mid-
  // session — the editor should flip to read-only without a
  // page reload).
  useEffect(() => {
    if (editor) editor.setEditable(writable);
  }, [editor, writable]);

  // renameSubmitted dedupes the commit path of the rename input. The
  // <input> wires both onSubmit (Enter key) AND onBlur to the same
  // handler so a click-outside also commits; pressing Enter then fires
  // submit, which sets renaming=false in finally, which unmounts the
  // input, which fires the blur synchronously during DOM detach. The
  // previous "renameInFlight reset in finally" guard was insufficient:
  // by the time React commits the unmount the ref has already flipped
  // back to false, so the blur-triggered second invocation sailed past
  // the in-flight check and sent a duplicate PATCH (Devin Review
  // BUG_pr-review-job-d387c.._0001).
  //
  // The correct shape is to gate on "has this rename attempt already
  // dispatched a commit" rather than "is the network call still in
  // flight". renameSubmitted is set to true on the first invocation
  // and stays true until the user re-opens the rename input (the
  // useEffect below clears it on the renaming=true edge), so the
  // synchronous unmount-blur cycle is a no-op.
  const renameSubmitted = useRef(false);
  useEffect(() => {
    if (renaming) renameSubmitted.current = false;
  }, [renaming]);
  const onRenameSubmit = useCallback(
    async (e: React.FormEvent) => {
      e.preventDefault();
      if (renameSubmitted.current) return;
      if (!doc || !id) return;
      const next = renameValue.trim();
      if (!next || next === doc.name) {
        renameSubmitted.current = true;
        setRenaming(false);
        return;
      }
      renameSubmitted.current = true;
      try {
        const updated = await renameDocument(id, next);
        setDoc(updated);
      } catch (err) {
        setLoadError(String((err as Error)?.message ?? err));
      } finally {
        setRenaming(false);
      }
    },
    [doc, id, renameValue],
  );

  const onChangeMode = useCallback(
    async (next: CollabMode) => {
      if (!doc || !id) return;
      if (next === doc.collab_mode) {
        setModeSwitchOpen(false);
        return;
      }
      setModeSwitching(true);
      setModeSwitchError(null);
      try {
        const updated = await setDocumentCollabMode(id, next);
        setDoc(updated);
        setModeSwitchOpen(false);
      } catch (err) {
        setModeSwitchError(String((err as Error)?.message ?? err));
      } finally {
        setModeSwitching(false);
      }
    },
    [doc, id],
  );

  if (!id) {
    return <div style={pageStyle}>Missing document id.</div>;
  }
  if (loadError) {
    return (
      <div style={pageStyle}>
        <p style={{ color: "#991b1b" }}>{loadError}</p>
        <Link to="/drive">Back to drive</Link>
      </div>
    );
  }
  if (!doc) {
    return <div style={pageStyle}>Loading…</div>;
  }

  return (
    <div style={pageStyle}>
      <header
        style={{
          display: "flex",
          alignItems: "center",
          justifyContent: "space-between",
          padding: "16px 24px",
          borderBottom: "1px solid #e5e7eb",
          gap: 12,
          flexWrap: "wrap",
        }}
      >
        <div style={{ display: "flex", alignItems: "center", gap: 12, flex: 1, minWidth: 0 }}>
          <button
            onClick={() => nav(`/drive/folder/${doc.folder_id}`)}
            style={backBtn}
            aria-label="Back to folder"
          >
            ←
          </button>
          {renaming ? (
            <form onSubmit={onRenameSubmit} style={{ flex: 1 }}>
              <input
                autoFocus
                value={renameValue}
                onChange={(e) => setRenameValue(e.target.value)}
                onBlur={onRenameSubmit}
                style={{
                  fontSize: 20,
                  fontWeight: 600,
                  border: "1px solid #d1d5db",
                  borderRadius: 4,
                  padding: "4px 8px",
                  width: "100%",
                  maxWidth: 500,
                }}
              />
            </form>
          ) : (
            <h1
              style={{ fontSize: 20, fontWeight: 600, margin: 0, cursor: "text" }}
              onClick={() => setRenaming(true)}
              title="Click to rename"
            >
              {doc.name}
            </h1>
          )}
          <EncryptionBadge mode={doc.encryption_mode} size="row" />
          <CollabModeBadge mode={doc.collab_mode} />
          <ConnectionStatusChip status={status} readOnly={!writable} />
        </div>
        <div style={{ display: "flex", alignItems: "center", gap: 12 }}>
          <PresenceChips
            awareness={awareness}
            localClientID={yDoc?.clientID ?? null}
          />
          <button onClick={() => setModeSwitchOpen(true)} style={modeBtn}>
            Change mode
          </button>
        </div>
      </header>

      <div
        style={{
          padding: 24,
          maxWidth: 880,
          margin: "0 auto",
          width: "100%",
          boxSizing: "border-box",
        }}
      >
        {collabMode === "disabled" && (
          <div
            style={{
              padding: 12,
              background: "#fee2e2",
              border: "1px solid #fecaca",
              color: "#991b1b",
              borderRadius: 4,
              marginBottom: 16,
            }}
          >
            This document's collab mode is <code>disabled</code> — editing is locked.
          </div>
        )}
        {editor ? (
          <EditorContent
            editor={editor}
            style={{
              minHeight: 400,
              padding: 16,
              border: "1px solid #e5e7eb",
              borderRadius: 4,
              outline: "none",
              fontSize: 15,
              lineHeight: 1.6,
            }}
          />
        ) : (
          <div style={{ color: "#6b7280" }}>Initializing editor…</div>
        )}
      </div>

      {modeSwitchOpen && (
        <div
          role="dialog"
          aria-modal="true"
          style={modalBackdrop}
          onClick={() => !modeSwitching && setModeSwitchOpen(false)}
        >
          <div
            style={modalCard}
            onClick={(e) => e.stopPropagation()}
          >
            <h2 style={{ margin: 0, fontSize: 18 }}>Change editor experience</h2>
            <p style={{ color: "#4b5563", fontSize: 13, marginTop: 4 }}>
              The folder is{" "}
              <strong>
                {doc.encryption_mode === "strict_zk" ? "zero-knowledge" : "confidential"}
              </strong>
              . Only modes allowed by the folder's privacy boundary are selectable.
            </p>
            <CollabModeSelector
              value={doc.collab_mode}
              onChange={onChangeMode}
              allowedModes={doc.allowed_collab_modes}
              encryptionMode={doc.encryption_mode}
              disabled={modeSwitching}
            />
            {modeSwitchError && (
              <p style={{ color: "#991b1b", fontSize: 13 }}>{modeSwitchError}</p>
            )}
            <div style={{ display: "flex", justifyContent: "flex-end", gap: 8 }}>
              <button
                onClick={() => setModeSwitchOpen(false)}
                disabled={modeSwitching}
                style={modeBtn}
              >
                Close
              </button>
            </div>
          </div>
        </div>
      )}
    </div>
  );
}

// CollabModeBadge renders the active collab mode as a small pill
// next to the encryption badge. The two badges together describe
// the (privacy boundary, editor experience) tuple that governs
// every collab feature on the page.
function CollabModeBadge({ mode }: { mode: CollabMode }) {
  const labels: Record<CollabMode, string> = {
    markdown: "Markdown",
    rich: "Rich",
    rich_presence: "Rich + presence",
    disabled: "Disabled",
  };
  const colors: Record<CollabMode, { bg: string; fg: string }> = {
    markdown: { bg: "#eff6ff", fg: "#1d4ed8" },
    rich: { bg: "#ecfdf5", fg: "#065f46" },
    rich_presence: { bg: "#f5f3ff", fg: "#5b21b6" },
    disabled: { bg: "#f3f4f6", fg: "#374151" },
  };
  const c = colors[mode];
  return (
    <span
      title={`Editor mode: ${labels[mode].toLowerCase()}`}
      style={{
        display: "inline-flex",
        alignItems: "center",
        padding: "2px 8px",
        borderRadius: 9999,
        background: c.bg,
        color: c.fg,
        fontSize: 12,
        fontWeight: 500,
      }}
    >
      {labels[mode]}
    </span>
  );
}

const pageStyle: React.CSSProperties = {
  minHeight: "100vh",
  background: "#f9fafb",
};

const backBtn: React.CSSProperties = {
  background: "white",
  border: "1px solid #d1d5db",
  borderRadius: 4,
  padding: "4px 10px",
  fontSize: 16,
  cursor: "pointer",
};

const modeBtn: React.CSSProperties = {
  padding: "6px 12px",
  background: "white",
  border: "1px solid #d1d5db",
  borderRadius: 4,
  fontSize: 13,
  cursor: "pointer",
};

const modalBackdrop: React.CSSProperties = {
  position: "fixed",
  inset: 0,
  background: "rgba(0,0,0,0.4)",
  display: "flex",
  alignItems: "center",
  justifyContent: "center",
  zIndex: 100,
};

const modalCard: React.CSSProperties = {
  background: "white",
  borderRadius: 8,
  padding: 24,
  width: "min(480px, 90vw)",
  display: "flex",
  flexDirection: "column",
  gap: 12,
};
