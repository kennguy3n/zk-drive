import { useCallback, useEffect, useLayoutEffect, useRef, useState } from "react";
import { useTranslation } from "react-i18next";
import {
  Type,
  Heading1,
  Heading2,
  Heading3,
  Heading,
  List,
  ListOrdered,
  ListChecks,
  CheckSquare,
  CheckCheck,
  Quote,
  Code,
  Minus,
  Table,
  Image as ImageIcon,
  Sparkles,
  Lightbulb,
  FileText,
  Wand2,
  HelpCircle,
  type LucideIcon,
} from "lucide-react";
import { cn } from "../../lib/cn";
import type { SlashCommandItem } from "./SlashCommand";
import type { SuggestionProps } from "@tiptap/suggestion";

const ICONS: Record<string, LucideIcon> = {
  Type,
  Heading1,
  Heading2,
  Heading3,
  Heading,
  List,
  ListOrdered,
  ListChecks,
  CheckSquare,
  CheckCheck,
  Quote,
  Code,
  Minus,
  Table,
  Image: ImageIcon,
  Sparkles,
  Lightbulb,
  FileText,
  Wand2,
  HelpCircle,
};

interface SlashMenuViewProps {
  items: SlashCommandItem[];
  command: (item: SlashCommandItem) => void;
  // The position is relative to the editor container, computed by
  // tippy.js or a manual positioning helper. We receive client coords.
  clientRect?: (() => DOMRect | null) | null;
}

export function SlashMenuView({ items, command, clientRect }: SlashMenuViewProps) {
  const { t } = useTranslation();
  const [selectedIndex, setSelectedIndex] = useState(0);
  const containerRef = useRef<HTMLDivElement>(null);

  useEffect(() => {
    setSelectedIndex(0);
  }, [items]);

  const upHandler = useCallback(() => {
    setSelectedIndex((prev) => (prev + items.length - 1) % items.length);
  }, [items.length]);

  const downHandler = useCallback(() => {
    setSelectedIndex((prev) => (prev + 1) % items.length);
  }, [items.length]);

  const enterHandler = useCallback(() => {
    const item = items[selectedIndex];
    if (item) command(item);
  }, [items, selectedIndex, command]);

  useEffect(() => {
    const onKeyDown = (e: KeyboardEvent) => {
      if (e.key === "ArrowUp") {
        e.preventDefault();
        upHandler();
      } else if (e.key === "ArrowDown") {
        e.preventDefault();
        downHandler();
      } else if (e.key === "Enter") {
        e.preventDefault();
        enterHandler();
      }
    };
    document.addEventListener("keydown", onKeyDown);
    return () => document.removeEventListener("keydown", onKeyDown);
  }, [upHandler, downHandler, enterHandler]);

  // Position the menu near the cursor, keeping it inside the viewport.
  useLayoutEffect(() => {
    const el = containerRef.current;
    if (!el || !clientRect) return;
    const rect = clientRect();
    if (!rect) return;

    // Start below the cursor by default.
    let top = rect.bottom + 6;
    let left = rect.left;

    // Force layout so we can measure the rendered menu before clamping.
    el.style.top = `${top}px`;
    el.style.left = `${left}px`;
    const menuRect = el.getBoundingClientRect();
    const viewportWidth = window.innerWidth;
    const viewportHeight = window.innerHeight;

    // If the menu overflows the bottom edge, flip it above the cursor.
    if (menuRect.bottom > viewportHeight - 8) {
      top = Math.max(8, rect.top - menuRect.height - 6);
    }
    // If it overflows the right edge, pin it to the right with padding.
    if (menuRect.right > viewportWidth - 8) {
      left = Math.max(8, viewportWidth - menuRect.width - 8);
    }
    // Keep at least a small gap from the left/top edge.
    left = Math.max(8, left);
    top = Math.max(8, top);

    el.style.top = `${top}px`;
    el.style.left = `${left}px`;
  }, [clientRect, items]);

  if (items.length === 0) {
    return (
      <div
        ref={containerRef}
        className="fixed z-50 w-64 rounded-xl border border-border bg-surface shadow-lg"
        role="listbox"
      >
        <div className="px-3 py-4 text-center text-sm text-muted">
          {t("editor.slashNoResults")}
        </div>
      </div>
    );
  }

  // Group items by category for display.
  const categories: { label: string; items: SlashCommandItem[] }[] = [
    { label: t("editor.slashBlocks"), items: items.filter((i) => i.category === "block") },
    { label: t("editor.slashRich"), items: items.filter((i) => i.category === "rich") },
    { label: t("editor.slashAI"), items: items.filter((i) => i.category === "ai") },
  ].filter((g) => g.items.length > 0);

  let runningIndex = 0;

  return (
    <div
      ref={containerRef}
      className="fixed z-50 w-64 max-h-80 overflow-y-auto rounded-xl border border-border bg-surface shadow-lg"
      role="listbox"
    >
      {categories.map((cat) => (
        <div key={cat.label}>
          <div className="px-3 pt-2 pb-1 text-xs font-medium text-muted uppercase tracking-wide">
            {cat.label}
          </div>
          {cat.items.map((item) => {
            const idx = runningIndex++;
            const Icon = ICONS[item.icon] ?? Type;
            return (
              <button
                key={item.title}
                type="button"
                role="option"
                aria-selected={idx === selectedIndex}
                onMouseEnter={() => setSelectedIndex(idx)}
                onClick={() => command(item)}
                className={cn(
                  "flex w-full items-start gap-3 px-3 py-2 text-left transition-colors",
                  idx === selectedIndex
                    ? "bg-surface-2"
                    : "hover:bg-surface-2",
                )}
              >
                <Icon className="mt-0.5 h-4 w-4 shrink-0 text-muted" aria-hidden="true" />
                <div className="flex flex-col">
                  <span className="text-sm font-medium text-fg">{item.title}</span>
                  <span className="text-xs text-muted">{item.description}</span>
                </div>
              </button>
            );
          })}
        </div>
      ))}
    </div>
  );
}

// renderSlashMenu creates the suggestion render callbacks that TipTap's
// Suggestion plugin expects. It mounts/unmounts the SlashMenuView React
// component via a single createRoot that is reused across updates and
// properly unmounted on exit to avoid fiber-tree leaks.
import { createRoot, type Root } from "react-dom/client";

export function renderSlashMenu() {
  let containerEl: HTMLDivElement | null = null;
  let root: Root | null = null;

  const render = (props: SuggestionProps<SlashCommandItem>) => {
    if (!containerEl || !root) return;
    root.render(
      <SlashMenuView
        items={props.items}
        command={props.command}
        clientRect={props.clientRect}
      />,
    );
  };

  return {
    onStart(props: SuggestionProps<SlashCommandItem>) {
      containerEl = document.createElement("div");
      document.body.appendChild(containerEl);
      root = createRoot(containerEl);
      render(props);
    },
    onUpdate(props: SuggestionProps<SlashCommandItem>) {
      render(props);
    },
    onExit() {
      if (root) {
        root.unmount();
        root = null;
      }
      if (containerEl) {
        containerEl.remove();
        containerEl = null;
      }
    },
    onKeyDown() {
      // Keyboard handling is done inside SlashMenuView via document
      // event listener. Return false so Suggestion doesn't also
      // try to handle arrow keys.
      return false;
    },
  };
}
