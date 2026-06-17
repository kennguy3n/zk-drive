import { type ReactNode } from "react";
import { cn } from "../../lib/cn";

export interface AppShellProps {
  /** Brand / logo block, far left of the top bar. */
  brand?: ReactNode;
  /** Primary navigation, after the brand. */
  nav?: ReactNode;
  /** Right-aligned actions (search, theme toggle, account menu…). */
  actions?: ReactNode;
  /** Constrain the main content width. Defaults to a roomy app width. */
  maxWidth?: "md" | "lg" | "xl" | "full";
  children: ReactNode;
  className?: string;
}

const widths: Record<NonNullable<AppShellProps["maxWidth"]>, string> = {
  md: "max-w-3xl",
  lg: "max-w-5xl",
  xl: "max-w-7xl",
  full: "max-w-none",
};

// AppShell is the shared application chrome: a sticky, tokenised top bar
// (brand + nav + actions) over a centered main content column. Authenticated
// surfaces (drive, admin, billing, documents) render inside it so the global
// navigation, theming and max-width are consistent instead of each page
// hand-rolling its own header bar.
export function AppShell({
  brand,
  nav,
  actions,
  maxWidth = "xl",
  children,
  className,
}: AppShellProps) {
  return (
    <div className="flex min-h-screen flex-col bg-bg text-fg">
      <header className="sticky top-0 z-40 border-b border-border bg-surface/80 backdrop-blur supports-[backdrop-filter]:bg-surface/70">
        <div
          className={cn(
            "mx-auto flex h-14 w-full items-center gap-3 px-4",
            widths[maxWidth],
          )}
        >
          {brand && <div className="flex shrink-0 items-center">{brand}</div>}
          {nav && (
            <nav className="flex min-w-0 flex-1 items-center gap-1">{nav}</nav>
          )}
          {actions && (
            <div className={cn("flex items-center gap-2", !nav && "ml-auto")}>
              {actions}
            </div>
          )}
        </div>
      </header>
      <main className={cn("mx-auto w-full flex-1 px-4 py-6", widths[maxWidth], className)}>
        {children}
      </main>
    </div>
  );
}
