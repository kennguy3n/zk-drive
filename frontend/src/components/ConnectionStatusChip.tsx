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
import { useTranslation } from "react-i18next";
import type { ConnectionStatus } from "../collab/provider";

const COLORS: Record<ConnectionStatus, { bg: string; fg: string; dot: string }> = {
  connecting: { bg: "#fef3c7", fg: "#92400e", dot: "#d97706" },
  connected: { bg: "#dcfce7", fg: "#166534", dot: "#16a34a" },
  reconnecting: { bg: "#fef3c7", fg: "#92400e", dot: "#d97706" },
  disconnected: { bg: "#fee2e2", fg: "#991b1b", dot: "#dc2626" },
};

const LABEL_KEYS: Record<ConnectionStatus, string> = {
  connecting: "collab.statusConnecting",
  connected: "collab.statusConnected",
  reconnecting: "collab.statusReconnecting",
  disconnected: "collab.statusDisconnected",
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
  const { t } = useTranslation();
  if (readOnly) {
    return (
      <span
        title={t("collab.readOnlyTooltip")}
        style={chipStyle("#e5e7eb", "#374151")}
      >
        <span style={{ ...dotStyle, background: "#6b7280" }} />
        {t("collab.readOnly")}
      </span>
    );
  }
  const c = COLORS[status];
  const label = t(LABEL_KEYS[status]);
  return (
    <span
      title={t("collab.statusTooltip", { status: label.toLowerCase() })}
      style={chipStyle(c.bg, c.fg)}
    >
      <span style={{ ...dotStyle, background: c.dot }} />
      {label}
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
