import { useCallback, useEffect, useState } from "react";
import type { Editor } from "@tiptap/react";
import { useTranslation } from "react-i18next";
import { cn } from "../../lib/cn";

interface HeadingEntry {
  id: string;
  level: number;
  text: string;
  pos: number;
}

function extractHeadings(editor: Editor): HeadingEntry[] {
  const headings: HeadingEntry[] = [];
  editor.state.doc.descendants((node, pos) => {
    if (node.type.name === "heading") {
      headings.push({
        id: `${pos}`,
        level: node.attrs.level as number,
        text: node.textContent,
        pos,
      });
    }
    return true;
  });
  return headings;
}

export interface DocumentOutlineProps {
  editor: Editor | null;
}

export default function DocumentOutline({ editor }: DocumentOutlineProps) {
  const { t } = useTranslation();
  const [headings, setHeadings] = useState<HeadingEntry[]>([]);

  useEffect(() => {
    if (!editor) return;
    let rafId: number | null = null;
    const update = () => {
      if (rafId !== null) cancelAnimationFrame(rafId);
      rafId = requestAnimationFrame(() => {
        rafId = null;
        setHeadings(extractHeadings(editor));
      });
    };
    update();
    editor.on("update", update);
    return () => {
      editor.off("update", update);
      if (rafId !== null) cancelAnimationFrame(rafId);
    };
  }, [editor]);

  const scrollToHeading = useCallback(
    (pos: number) => {
      if (!editor) return;
      editor.commands.focus();
      editor.commands.setTextSelection(pos);
      // Use ProseMirror's built-in scrollIntoView which works
      // regardless of whether the editor is in a scrollable container
      // or the window itself.
      const tr = editor.state.tr.scrollIntoView();
      editor.view.dispatch(tr);
    },
    [editor],
  );

  if (headings.length === 0) return null;

  return (
    <nav
      className="hidden lg:block w-56 shrink-0"
      aria-label={t("editor.outlineLabel")}
    >
      <div className="sticky top-20 rounded-card border border-border bg-surface p-3 shadow-card">
        <h3 className="mb-2 text-xs font-semibold uppercase tracking-wide text-muted">
          {t("editor.outlineTitle")}
        </h3>
        <ul className="space-y-0.5">
          {headings.map((h) => (
            <li key={h.id}>
              <button
                type="button"
                onClick={() => scrollToHeading(h.pos)}
                className={cn(
                  "block w-full truncate rounded px-2 py-1 text-left text-sm transition-colors",
                  "hover:bg-surface-2 focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring",
                  h.level === 1
                    ? "font-medium text-fg"
                    : h.level === 2
                      ? "pl-4 text-fg/80"
                      : "pl-6 text-muted",
                )}
                title={h.text}
              >
                {h.text || t("editor.outlineEmpty")}
              </button>
            </li>
          ))}
        </ul>
      </div>
    </nav>
  );
}
