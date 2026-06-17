// PresenceChips renders an overlapping stack of avatars, one per
// remote collaborator currently in the document's awareness state.
// Each avatar shows the collaborator's initials over their cursor
// color (the CollaborationCursor extension uses the same color), so
// the people you see in the margin match the people in the header.
// The local user is filtered out — they already see their own name
// in the app header.
//
// Empty state: when no remote users are present the component
// renders nothing rather than an empty container so the editor
// header collapses cleanly on a single-user document.

import { useEffect, useState } from "react";
import { useTranslation } from "react-i18next";
import type { Awareness } from "y-protocols/awareness";

interface AwarenessUserState {
  user?: { name?: string; color?: string };
}

export interface PresenceChipsProps {
  // awareness is the y-protocols Awareness instance used by the
  // editor's CollaborationCursor extension. Pass null to suppress
  // rendering entirely (e.g. when presence is disabled by the
  // folder's capability matrix).
  awareness: Awareness | null;
  // localClientID is filtered out of the chip list so the user
  // doesn't see themselves as a "remote" collaborator. Comes from
  // the Y.Doc's clientID property; pass null and we'll show all.
  localClientID: number | null;
}

interface Peer {
  id: number;
  name: string;
  color: string;
}

// Cap the visible avatars so a busy document doesn't blow out the
// header; the remainder collapse into a "+N" overflow chip.
const MAX_VISIBLE = 4;

// initials derives a 1–2 letter monogram from a display name for the
// avatar. Falls back to "?" for an empty name.
function initials(name: string): string {
  const parts = name.trim().split(/\s+/).filter(Boolean);
  if (parts.length === 0) return "?";
  if (parts.length === 1) return parts[0].slice(0, 1).toUpperCase();
  return (parts[0][0] + parts[parts.length - 1][0]).toUpperCase();
}

export default function PresenceChips({
  awareness,
  localClientID,
}: PresenceChipsProps) {
  const { t } = useTranslation();
  const [peers, setPeers] = useState<Peer[]>([]);

  useEffect(() => {
    if (!awareness) {
      setPeers([]);
      return;
    }
    const computePeers = () => {
      const states = awareness.getStates() as Map<number, AwarenessUserState>;
      const next: Peer[] = [];
      states.forEach((state, clientID) => {
        if (clientID === localClientID) return;
        const u = state.user;
        if (!u) return;
        next.push({
          id: clientID,
          name: u.name ?? t("collab.anonymous"),
          color: u.color ?? "#6b7280",
        });
      });
      // Deterministic order so chips don't reshuffle on every
      // awareness tick — sort by clientID (the Yjs random per-tab
      // identifier; stable for the session).
      next.sort((a, b) => a.id - b.id);
      setPeers(next);
    };
    computePeers();
    awareness.on("update", computePeers);
    return () => {
      awareness.off("update", computePeers);
    };
    // t is stable per i18next mount, intentionally excluded to keep
    // peers from re-allocating on every locale-namespace warm-up.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [awareness, localClientID]);

  if (peers.length === 0) return null;

  const visible = peers.slice(0, MAX_VISIBLE);
  const overflow = peers.length - visible.length;

  return (
    <div
      className="inline-flex items-center"
      role="group"
      aria-label={t("collab.peersPresent", { count: peers.length })}
    >
      {visible.map((p) => (
        <span
          key={p.id}
          title={p.name}
          // The per-user cursor color is genuine per-collaborator data
          // (not a theme color), so it stays an inline background; the
          // ring uses a token so overlapping avatars separate cleanly in
          // both light and dark mode.
          style={{ backgroundColor: p.color }}
          className="-ml-1.5 inline-flex h-7 w-7 items-center justify-center rounded-full text-[11px] font-semibold uppercase text-white ring-2 ring-surface first:ml-0"
        >
          {initials(p.name)}
        </span>
      ))}
      {overflow > 0 && (
        <span
          title={t("collab.morePeople", { count: overflow })}
          aria-label={t("collab.morePeople", { count: overflow })}
          className="-ml-1.5 inline-flex h-7 min-w-7 items-center justify-center rounded-full bg-surface-2 px-1.5 text-[11px] font-semibold text-muted ring-2 ring-surface"
        >
          +{overflow}
        </span>
      )}
    </div>
  );
}
