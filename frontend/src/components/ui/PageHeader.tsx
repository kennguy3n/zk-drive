import { type ReactNode } from "react";
import { cn } from "../../lib/cn";

export interface PageHeaderProps {
  title: ReactNode;
  /** Sub-heading rendered under the title. */
  description?: ReactNode;
  /** Breadcrumb / back-link row rendered above the title. */
  eyebrow?: ReactNode;
  /** Right-aligned primary actions. */
  actions?: ReactNode;
  className?: string;
}

// PageHeader is the shared page-title block: an optional breadcrumb eyebrow,
// an <h1>, an optional description and a right-aligned actions slot. Pages
// use it for a consistent heading rhythm instead of bespoke title markup.
export function PageHeader({
  title,
  description,
  eyebrow,
  actions,
  className,
}: PageHeaderProps) {
  return (
    <div
      className={cn(
        "mb-6 flex flex-col gap-3 sm:flex-row sm:items-end sm:justify-between",
        className,
      )}
    >
      <div className="min-w-0">
        {eyebrow && <div className="mb-1 text-sm text-muted">{eyebrow}</div>}
        <h1 className="truncate text-2xl font-semibold tracking-tight text-fg">
          {title}
        </h1>
        {description && (
          <p className="mt-1 max-w-2xl text-sm text-muted">{description}</p>
        )}
      </div>
      {actions && (
        <div className="flex shrink-0 flex-wrap items-center gap-2">{actions}</div>
      )}
    </div>
  );
}
