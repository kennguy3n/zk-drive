// ConnectionStatusChip renders the collab WS connection state as a
// small pill next to the document title. The four states map 1:1 to
// CollabProvider's ConnectionStatus enum:
//
//   - connecting   — initial dial-out, or first reconnect attempt
//   - connected    — live; updates are flowing
//   - reconnecting — socket closed, waiting for the next attempt
//   - disconnected — provider destroyed (component unmounting) or
//                    auth failure; the editor surfaces "read-only"
//                    in that case so the chip is only seen briefly

import type { CSSProperties } from "react";
import type { ConnectionStatus } from "../collab/provider";

const COLORS: Record<ConnectionStatus, { bg: string; fg: string; dot: string }> = {
  connecting: { bg: "#fef3c7", fg: "#92400e", dot: "#d97706" },
  connected: { bg: "#dcfce7", fg: "#166534", dot: "#16a34a" },
  reconnecting: { bg: "#fef3c7", fg: "#92400e", dot: "#d97706" },
  disconnected: { bg: "#fee2e2", fg: "#991b1b", dot: "#dc2626" },
};

const LABELS: Record<ConnectionStatus, string> = {
  connecting: "Connecting",
  connected: "Live",
  reconnecting: "Reconnecting",
  disconnected: "Disconnected",
};

export interface ConnectionStatusChipProps {
  status: ConnectionStatus;
  // readOnly switches the rendering to a static "Read-only" pill,
  // used when CanWrite=false (document is in 'disabled' collab
  // mode OR the viewer lacks editor permission). In read-only the
  // WS provider is never connected, so the live status is not
  // meaningful and would confuse users.
  readOnly?: boolean;
}

export default function ConnectionStatusChip({
  status,
  readOnly,
}: ConnectionStatusChipProps) {
  if (readOnly) {
    return (
      <span
        title="You have view-only access to this document."
        style={chipStyle("#e5e7eb", "#374151")}
      >
        <span style={{ ...dotStyle, background: "#6b7280" }} />
        Read-only
      </span>
    );
  }
  const c = COLORS[status];
  return (
    <span
      title={`Collab connection: ${LABELS[status].toLowerCase()}`}
      style={chipStyle(c.bg, c.fg)}
    >
      <span style={{ ...dotStyle, background: c.dot }} />
      {LABELS[status]}
    </span>
  );
}

function chipStyle(bg: string, fg: string): CSSProperties {
  return {
    display: "inline-flex",
    alignItems: "center",
    gap: 6,
    padding: "2px 8px",
    borderRadius: 9999,
    background: bg,
    color: fg,
    fontSize: 12,
    fontWeight: 500,
    lineHeight: 1.5,
  };
}

const dotStyle: CSSProperties = {
  width: 6,
  height: 6,
  borderRadius: "50%",
  flexShrink: 0,
};
