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
//   4. The editor chrome — encryption-mode badge + collab-mode pill
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
import { Trans, useTranslation } from "react-i18next";
import type { TFunction } from "i18next";
import { ArrowLeft, Lock, Pencil } from "lucide-react";
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
import { translateApiError } from "../api/errors";
import EncryptionBadge from "../components/EncryptionBadge";
import ConnectionStatusChip from "../components/ConnectionStatusChip";
import PresenceChips from "../components/PresenceChips";
import CollabModeSelector from "../components/CollabModeSelector";
import {
  AppShell,
  Badge,
  Button,
  EmptyState,
  Modal,
  PageHeader,
  Skeleton,
  usePrompt,
  useToast,
} from "../components/ui";

type BadgeTone = "neutral" | "brand" | "success" | "danger" | "warning";

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
function displayNameForUser(t: TFunction, userID: string | null): string {
  if (!userID) return t("collab.anonymous");
  return t("docs.userLabel", { id: userID.slice(0, 6) });
}

export default function DocumentEditorPage() {
  const { id } = useParams<{ id: string }>();
  const nav = useNavigate();
  const { t } = useTranslation();
  const prompt = usePrompt();
  const toast = useToast();

  const [doc, setDoc] = useState<Document | null>(null);
  const [loadError, setLoadError] = useState<string | null>(null);
  const [status, setStatus] = useState<ConnectionStatus>("disconnected");
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
      })
      .catch((e) => {
        if (cancelled) return;
        setLoadError(translateApiError(e, t));
      });
    return () => {
      cancelled = true;
    };
  }, [id, t]);

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
      // Surface via toast rather than loadError: the page's loadError
      // EmptyState is gated on `!doc`, but reaching this branch requires
      // collabMode !== "disabled" which in turn requires a loaded doc, so a
      // loadError set here would never paint. The toast keeps the auth
      // failure visible while the editor stays mounted, and the connection
      // chip remains "disconnected" as the persistent signal that live
      // collaboration is not active.
      toast.error(t("errors.AUTH_MISSING_TOKEN"));
      // Mirror the normal teardown order (awareness before Y.Doc) so the
      // awareness listener is torn down explicitly rather than relying on
      // the Y.Doc.destroy() event cascade — robust against future
      // y-protocols changes and idempotent.
      nextAwareness?.destroy();
      nextYDoc.destroy();
      return;
    }
    setYDoc(nextYDoc);
    setAwareness(nextAwareness);
    const provider = new CollabProvider({
      url: documentCollabURL(id),
      token,
      // Re-read the JWT on every reconnect so a long editing session
      // survives token rotation without dropping the live document.
      tokenProvider: () => currentToken(),
      doc: nextYDoc,
      awareness: nextAwareness ?? undefined,
      presenceAllowed,
      user: {
        name: displayNameForUser(t, currentUserID()),
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
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [id, collabMode, presenceAllowed]);

  // Build the TipTap extension list based on the resolved
  // capability + collab mode. StarterKit is ALWAYS included
  // (even before doc / yDoc load) because useEditor below
  // runs on every render — including the first one, where
  // hasDoc=false and yDoc=null — and ProseMirror refuses to
  // compile a schema that lacks a top-level "doc" node type
  // (RangeError: Schema is missing its top node type 'doc').
  // The skeleton early-return below means the empty
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

  // Rename flows through the shared usePrompt dialog: a single
  // promise resolution commits exactly once, so there's no
  // double-submit window (the previous inline <input> wired both
  // Enter and blur and needed a dedup guard).
  const onRename = useCallback(async () => {
    if (!doc || !id) return;
    const next = await prompt({
      title: t("docs.renameTitle"),
      label: t("collab.documentName"),
      defaultValue: doc.name,
      confirmLabel: t("common.rename"),
      required: true,
    });
    if (next === null) return;
    const trimmed = next.trim();
    if (!trimmed || trimmed === doc.name) return;
    try {
      const updated = await renameDocument(id, trimmed);
      setDoc(updated);
      toast.success(t("docs.renamed", { name: trimmed }));
    } catch (err) {
      toast.error(translateApiError(err, t));
    }
  }, [doc, id, prompt, t, toast]);

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
        setModeSwitchError(translateApiError(err, t));
      } finally {
        setModeSwitching(false);
      }
    },
    [doc, id, t],
  );

  if (!id) {
    return (
      <AppShell maxWidth="lg">
        <EmptyState
          title={t("docs.missingDocumentId")}
          action={
            <Button onClick={() => nav("/drive")}>{t("admin.backToDrive")}</Button>
          }
        />
      </AppShell>
    );
  }
  if (loadError && !doc) {
    return (
      <AppShell maxWidth="lg">
        <EmptyState
          title={loadError}
          action={
            <Button onClick={() => nav("/drive")}>{t("admin.backToDrive")}</Button>
          }
        />
      </AppShell>
    );
  }
  if (!doc) {
    return (
      <AppShell maxWidth="lg">
        <EditorSkeleton />
      </AppShell>
    );
  }

  return (
    <AppShell
      maxWidth="lg"
      nav={
        <Link
          to={`/drive/folder/${doc.folder_id}`}
          aria-label={t("docs.backToFolderAria")}
          className="inline-flex items-center gap-1.5 rounded-full px-3 py-1.5 text-sm font-medium text-muted transition-colors hover:bg-surface-2 hover:text-fg focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring"
        >
          <ArrowLeft className="h-4 w-4" aria-hidden="true" />
          {t("docs.backToFolder")}
        </Link>
      }
      actions={
        <>
          <ConnectionStatusChip status={status} readOnly={!writable} />
          <PresenceChips
            awareness={awareness}
            localClientID={yDoc?.clientID ?? null}
          />
        </>
      }
    >
      <PageHeader
        eyebrow={
          <span className="inline-flex flex-wrap items-center gap-2">
            <EncryptionBadge mode={doc.encryption_mode} size="row" />
            <CollabModeBadge mode={doc.collab_mode} />
          </span>
        }
        title={
          <button
            type="button"
            onClick={onRename}
            title={t("docs.clickToRename")}
            className="group -ml-1 inline-flex max-w-full items-center gap-2 rounded-lg px-1 text-left transition-colors hover:bg-surface-2 focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring"
          >
            <span className="truncate">{doc.name}</span>
            <Pencil
              className="h-4 w-4 shrink-0 text-muted opacity-0 transition-opacity group-hover:opacity-100"
              aria-hidden="true"
            />
          </button>
        }
        actions={
          <Button
            variant="secondary"
            onClick={() => {
              setModeSwitchError(null);
              setModeSwitchOpen(true);
            }}
          >
            {t("docs.changeMode")}
          </Button>
        }
      />

      {collabMode === "disabled" && (
        <div
          role="status"
          className="mb-4 flex items-start gap-2 rounded-card border border-warning/30 bg-warning/10 px-4 py-3 text-sm text-warning"
        >
          <Lock className="mt-0.5 h-4 w-4 shrink-0" aria-hidden="true" />
          <span>
            <Trans
              i18nKey="docs.disabledBanner"
              components={{
                code: (
                  <code className="rounded bg-warning/20 px-1 py-0.5 font-mono text-xs" />
                ),
              }}
            />
          </span>
        </div>
      )}

      {editor ? (
        <EditorContent
          editor={editor}
          className="rounded-card border border-border bg-surface px-6 py-5 text-fg shadow-card [&_.ProseMirror]:min-h-[55vh] [&_.ProseMirror]:leading-relaxed [&_.ProseMirror]:outline-none"
        />
      ) : (
        <div
          className="rounded-card border border-border bg-surface px-6 py-5 shadow-card"
          role="status"
          aria-label={t("docs.initializing")}
        >
          <div className="flex flex-col gap-3">
            <Skeleton className="h-6 w-1/2" />
            <Skeleton className="h-4 w-full" />
            <Skeleton className="h-4 w-5/6" />
            <Skeleton className="h-4 w-2/3" />
          </div>
        </div>
      )}

      <Modal
        open={modeSwitchOpen}
        onOpenChange={(next) => {
          if (modeSwitching) return;
          // Clear any prior switch error when the modal closes so the next
          // open starts clean (the dialog is controlled, not remounted).
          if (!next) setModeSwitchError(null);
          setModeSwitchOpen(next);
        }}
        title={t("docs.changeExperience")}
        size="lg"
        description={t("docs.modeFolderHint", {
          mode:
            doc.encryption_mode === "strict_zk"
              ? t("encryption.zeroKnowledge")
              : t("encryption.confidential"),
        })}
        footer={
          <Button
            variant="secondary"
            onClick={() => {
              setModeSwitchError(null);
              setModeSwitchOpen(false);
            }}
            disabled={modeSwitching}
          >
            {t("common.close")}
          </Button>
        }
      >
        <div className="flex flex-col gap-3">
          <CollabModeSelector
            value={doc.collab_mode}
            onChange={onChangeMode}
            allowedModes={doc.allowed_collab_modes}
            encryptionMode={doc.encryption_mode}
            disabled={modeSwitching}
          />
          {modeSwitchError && (
            <p className="text-sm text-danger" role="alert">
              {modeSwitchError}
            </p>
          )}
        </div>
      </Modal>
    </AppShell>
  );
}

// EditorSkeleton mirrors the loaded layout (header block + document
// surface) so the page doesn't jump when the document metadata and
// editor finish loading.
function EditorSkeleton() {
  const { t } = useTranslation();
  return (
    <div role="status" aria-label={t("docs.initializing")}>
      <div className="mb-6 flex flex-col gap-3">
        <Skeleton className="h-5 w-40" />
        <Skeleton className="h-8 w-64" />
      </div>
      <div className="rounded-card border border-border bg-surface px-6 py-5 shadow-card">
        <div className="flex flex-col gap-3">
          <Skeleton className="h-6 w-1/2" />
          <Skeleton className="h-4 w-full" />
          <Skeleton className="h-4 w-full" />
          <Skeleton className="h-4 w-5/6" />
          <Skeleton className="h-4 w-2/3" />
        </div>
      </div>
    </div>
  );
}

// CollabModeBadge renders the active collab mode as a small Badge
// next to the encryption badge. The two badges together describe
// the (privacy boundary, editor experience) tuple that governs
// every collab feature on the page.
function CollabModeBadge({ mode }: { mode: CollabMode }) {
  const { t } = useTranslation();
  const labels: Record<CollabMode, string> = {
    markdown: t("collab.markdown"),
    rich: t("collab.rich"),
    rich_presence: t("collab.richPresence"),
    disabled: t("collab.disabled"),
  };
  const tones: Record<CollabMode, BadgeTone> = {
    markdown: "neutral",
    rich: "brand",
    rich_presence: "brand",
    disabled: "warning",
  };
  return (
    <span
      title={t("docs.editorModeTooltip", { mode: labels[mode].toLowerCase() })}
      className="inline-flex"
    >
      <Badge tone={tones[mode]} dot={mode === "rich_presence"}>
        {labels[mode]}
      </Badge>
    </span>
  );
}
