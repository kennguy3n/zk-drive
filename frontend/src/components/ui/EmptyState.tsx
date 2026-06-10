import { type ReactNode } from "react";
import { cn } from "../../lib/cn";

export interface EmptyStateProps {
  icon?: ReactNode;
  title: string;
  description?: string;
  /** Primary call-to-action row. */
  action?: ReactNode;
  className?: string;
}

// EmptyState is the shared "nothing here yet" panel used for empty file
// lists, no search results, no notifications, no shared items, etc. It is
// announced politely to assistive tech so a screen-reader user knows a
// search returned nothing rather than hearing silence.
export function EmptyState({
  icon,
  title,
  description,
  action,
  className,
}: EmptyStateProps) {
  return (
    <div
      role="status"
      aria-live="polite"
      className={cn(
        "flex flex-col items-center justify-center gap-3 px-6 py-14 text-center",
        className,
      )}
    >
      {icon && (
        <div
          className="flex h-14 w-14 items-center justify-center rounded-full bg-surface-2 text-muted"
          aria-hidden="true"
        >
          {icon}
        </div>
      )}
      <h3 className="text-base font-semibold text-fg">{title}</h3>
      {description && (
        <p className="max-w-sm text-sm text-muted">{description}</p>
      )}
      {action && <div className="mt-1">{action}</div>}
    </div>
  );
}
