// ConnectionStatusChip renders the collab WS connection state as a
// small KChat Badge next to the document title. The four states map
// 1:1 to CollabProvider's ConnectionStatus enum:
//
//   - connecting   — initial dial-out, or first reconnect attempt
//   - connected    — live; updates are flowing
//   - reconnecting — socket closed, waiting for the next attempt
//   - disconnected — provider destroyed (component unmounting) or
//                    auth failure; the editor surfaces "read-only"
//                    in that case so the chip is only seen briefly
//
// Colour comes entirely from the Badge tone tokens (success / warning
// / danger / neutral) so the chip re-themes and dark-modes for free.

import { useTranslation } from "react-i18next";
import { Badge } from "./ui";
import { cn } from "../lib/cn";
import type { ConnectionStatus } from "../collab/provider";

type Tone = "success" | "warning" | "danger" | "neutral";

// Each status maps to a semantic tone + whether its dot should pulse.
// Transient states (connecting / reconnecting) pulse to signal work in
// progress; settled states (connected / disconnected) hold steady.
const STATUS: Record<
  ConnectionStatus,
  { tone: Tone; labelKey: string; pulse: boolean }
> = {
  connecting: { tone: "warning", labelKey: "collab.statusConnecting", pulse: true },
  connected: { tone: "success", labelKey: "collab.statusConnected", pulse: false },
  reconnecting: { tone: "warning", labelKey: "collab.statusReconnecting", pulse: true },
  disconnected: { tone: "danger", labelKey: "collab.statusDisconnected", pulse: false },
};

const dotTone: Record<Tone, string> = {
  success: "bg-success",
  warning: "bg-warning",
  danger: "bg-danger",
  neutral: "bg-muted",
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

// StatusDot is the leading indicator inside the badge. It carries the
// tone colour and an optional pulse; aria-hidden because the badge
// text already conveys the state to assistive tech.
function StatusDot({ tone, pulse }: { tone: Tone; pulse?: boolean }) {
  return (
    <span
      aria-hidden="true"
      className={cn("h-1.5 w-1.5 rounded-full", dotTone[tone], pulse && "animate-pulse")}
    />
  );
}

export default function ConnectionStatusChip({
  status,
  readOnly,
}: ConnectionStatusChipProps) {
  const { t } = useTranslation();

  if (readOnly) {
    return (
      // Badge doesn't forward arbitrary DOM props, so the tooltip lives
      // on a wrapping span (same pattern used for the live-status case).
      <span title={t("collab.readOnlyTooltip")} className="inline-flex">
        <Badge tone="neutral">
          <StatusDot tone="neutral" />
          {t("collab.readOnly")}
        </Badge>
      </span>
    );
  }

  const { tone, labelKey, pulse } = STATUS[status];
  const label = t(labelKey);
  return (
    <span
      title={t("collab.statusTooltip", { status: label.toLowerCase() })}
      className="inline-flex"
    >
      <Badge tone={tone}>
        <StatusDot tone={tone} pulse={pulse} />
        {label}
      </Badge>
    </span>
  );
}
