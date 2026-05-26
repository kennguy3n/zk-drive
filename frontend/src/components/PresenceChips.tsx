// PresenceChips renders one chip per remote collaborator currently
// in the document's awareness state. The chip carries the user's
// name and a coloured dot matching their cursor color in the editor
// (CollaborationCursor extension uses the same color). The local
// user is filtered out — they already see their own name in the
// app header.
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
  return (
    <div
      style={{
        display: "inline-flex",
        alignItems: "center",
        gap: 6,
        flexWrap: "wrap",
      }}
      aria-label={t("collab.peersPresent", { count: peers.length })}
    >
      {peers.map((p) => (
        <span
          key={p.id}
          title={p.name}
          style={{
            display: "inline-flex",
            alignItems: "center",
            gap: 6,
            padding: "2px 8px",
            borderRadius: 9999,
            background: "#f3f4f6",
            color: "#111827",
            fontSize: 12,
            fontWeight: 500,
          }}
        >
          <span
            style={{
              width: 8,
              height: 8,
              borderRadius: "50%",
              background: p.color,
              flexShrink: 0,
            }}
          />
          {p.name}
        </span>
      ))}
    </div>
  );
}
