import { type ReactNode } from "react";
import { cn } from "../../lib/cn";

type BadgeTone = "neutral" | "brand" | "success" | "danger" | "warning";

export interface BadgeProps {
  tone?: BadgeTone;
  /** Renders a small leading dot in the tone colour. */
  dot?: boolean;
  children: ReactNode;
  className?: string;
}

const tones: Record<BadgeTone, string> = {
  neutral: "bg-surface-2 text-muted",
  brand: "bg-brand/10 text-brand",
  success: "bg-success/10 text-success",
  danger: "bg-danger/10 text-danger",
  warning: "bg-warning/10 text-warning",
};

const dotTones: Record<BadgeTone, string> = {
  neutral: "bg-muted",
  brand: "bg-brand",
  success: "bg-success",
  danger: "bg-danger",
  warning: "bg-warning",
};

// Badge is the shared status/label pill (encryption mode, plan tier,
// health state, counts…). Token-based so it re-themes with the app.
export function Badge({ tone = "neutral", dot, children, className }: BadgeProps) {
  return (
    <span
      className={cn(
        "inline-flex items-center gap-1.5 rounded-full px-2.5 py-0.5 text-xs font-medium",
        tones[tone],
        className,
      )}
    >
      {dot && (
        <span
          className={cn("h-1.5 w-1.5 rounded-full", dotTones[tone])}
          aria-hidden="true"
        />
      )}
      {children}
    </span>
  );
}
