import { useRef, type KeyboardEvent, type ReactNode } from "react";
import { cn } from "../../lib/cn";

export interface TabItem<T extends string = string> {
  value: T;
  label: ReactNode;
  icon?: ReactNode;
  disabled?: boolean;
}

export interface TabsProps<T extends string = string> {
  tabs: TabItem<T>[];
  value: T;
  onChange: (value: T) => void;
  /** Underline tabs (page sections) or a segmented pill control. */
  variant?: "underline" | "pill";
  /** Accessible name for the tablist. */
  "aria-label"?: string;
  className?: string;
}

// Tabs is the shared, accessible tab strip used for the admin sub-sections,
// share dialog (link / invite), etc. It implements the WAI-ARIA tabs
// keyboard pattern (Arrow keys + Home/End with roving tabindex) and is
// fully tokenised, replacing the hand-rolled inline-styled tab buttons.
export function Tabs<T extends string = string>({
  tabs,
  value,
  onChange,
  variant = "underline",
  className,
  ...rest
}: TabsProps<T>) {
  const refs = useRef<(HTMLButtonElement | null)[]>([]);

  const focusTab = (index: number) => {
    const tab = tabs[index];
    if (!tab || tab.disabled) return;
    refs.current[index]?.focus();
    onChange(tab.value);
  };

  const move = (from: number, dir: 1 | -1) => {
    const n = tabs.length;
    for (let step = 1; step <= n; step++) {
      const next = (from + dir * step + n) % n;
      if (!tabs[next].disabled) {
        focusTab(next);
        return;
      }
    }
  };

  const onKeyDown = (e: KeyboardEvent<HTMLButtonElement>, index: number) => {
    switch (e.key) {
      case "ArrowRight":
      case "ArrowDown":
        e.preventDefault();
        move(index, 1);
        break;
      case "ArrowLeft":
      case "ArrowUp":
        e.preventDefault();
        move(index, -1);
        break;
      case "Home":
        e.preventDefault();
        focusTab(tabs.findIndex((t) => !t.disabled));
        break;
      case "End": {
        e.preventDefault();
        for (let i = tabs.length - 1; i >= 0; i--) {
          if (!tabs[i].disabled) {
            focusTab(i);
            break;
          }
        }
        break;
      }
    }
  };

  return (
    <div
      role="tablist"
      aria-label={rest["aria-label"]}
      className={cn(
        variant === "underline"
          ? "flex items-center gap-1 border-b border-border"
          : "inline-flex items-center gap-1 rounded-full border border-border bg-surface-2 p-1",
        className,
      )}
    >
      {tabs.map((tab, i) => {
        const active = tab.value === value;
        return (
          <button
            key={tab.value}
            ref={(el) => (refs.current[i] = el)}
            role="tab"
            type="button"
            aria-selected={active}
            tabIndex={active ? 0 : -1}
            disabled={tab.disabled}
            onClick={() => !tab.disabled && onChange(tab.value)}
            onKeyDown={(e) => onKeyDown(e, i)}
            className={cn(
              "inline-flex items-center gap-2 text-sm font-medium transition-colors",
              "focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring",
              "disabled:opacity-50 disabled:pointer-events-none",
              variant === "underline"
                ? cn(
                    "-mb-px border-b-2 px-3 py-2.5",
                    active
                      ? "border-brand text-brand"
                      : "border-transparent text-muted hover:text-fg",
                  )
                : cn(
                    "rounded-full px-3.5 py-1.5",
                    active
                      ? "bg-surface text-fg shadow-sm"
                      : "text-muted hover:text-fg",
                  ),
            )}
          >
            {tab.icon}
            {tab.label}
          </button>
        );
      })}
    </div>
  );
}
