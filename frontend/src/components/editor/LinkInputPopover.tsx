import { useCallback, useEffect, useRef, useState } from "react";
import { useTranslation } from "react-i18next";
import { Link2, Check, X } from "lucide-react";
import { cn } from "../../lib/cn";
import type { Editor } from "@tiptap/react";

function isSafeHref(url: string): boolean {
  try {
    const parsed = new URL(url);
    return ["http:", "https:", "mailto:"].includes(parsed.protocol);
  } catch {
    if (url.startsWith("/") || url.startsWith("#")) return true;
    return false;
  }
}

export interface LinkInputPopoverProps {
  editor: Editor;
  // Anchor position for the popover. If null, the popover is centered.
  anchorRect: DOMRect | null;
  onDone: () => void;
}

// LinkInputPopover is an inline URL input that replaces window.prompt()
// for link insertion. It provides:
// - Inline text field with placeholder
// - URL validation (http/https/mailto/relative paths only)
// - Enter to confirm, Escape to cancel
// - Click outside to dismiss
// - Pre-fills with existing href if the selection has a link
export default function LinkInputPopover({ editor, anchorRect, onDone }: LinkInputPopoverProps) {
  const { t } = useTranslation();
  const inputRef = useRef<HTMLInputElement>(null);
  const containerRef = useRef<HTMLDivElement>(null);
  const [url, setUrl] = useState("");
  const [error, setError] = useState<string | null>(null);

  // Pre-fill with existing href if the selection has a link.
  useEffect(() => {
    if (editor.isActive("link")) {
      const attrs = editor.getAttributes("link");
      if (attrs?.href) setUrl(attrs.href as string);
    }
  }, [editor]);

  // Focus the input on mount.
  useEffect(() => {
    inputRef.current?.focus();
    inputRef.current?.select();
  }, []);

  // Click outside to dismiss.
  useEffect(() => {
    const handleClickOutside = (e: MouseEvent) => {
      if (containerRef.current && !containerRef.current.contains(e.target as Node)) {
        onDone();
      }
    };
    document.addEventListener("mousedown", handleClickOutside);
    return () => document.removeEventListener("mousedown", handleClickOutside);
  }, [onDone]);

  const handleConfirm = useCallback(() => {
    const trimmed = url.trim();
    if (!trimmed) {
      editor.chain().focus().unsetLink().run();
      onDone();
      return;
    }
    if (!isSafeHref(trimmed)) {
      setError(t("editor.linkInvalidUrl"));
      return;
    }
    editor.chain().focus().setLink({ href: trimmed }).run();
    onDone();
  }, [editor, url, onDone, t]);

  const handleKeyDown = useCallback(
    (e: React.KeyboardEvent) => {
      if (e.key === "Enter") {
        e.preventDefault();
        handleConfirm();
      } else if (e.key === "Escape") {
        e.preventDefault();
        onDone();
      }
    },
    [handleConfirm, onDone],
  );

  // Position the popover below the anchor, centered horizontally.
  const style: React.CSSProperties = anchorRect
    ? {
        position: "fixed",
        top: anchorRect.bottom + 6,
        left: Math.max(8, Math.min(anchorRect.left, window.innerWidth - 320)),
        zIndex: 50,
      }
    : {
        position: "fixed",
        top: "50%",
        left: "50%",
        transform: "translate(-50%, -50%)",
        zIndex: 50,
      };

  return (
    <div
      ref={containerRef}
      style={style}
      className="flex items-center gap-2 rounded-lg border border-border bg-surface px-2 py-1.5 shadow-lg"
      role="dialog"
      aria-label={t("editor.toolbarLink")}
    >
      <Link2 className="h-4 w-4 shrink-0 text-muted" aria-hidden="true" />
      <input
        ref={inputRef}
        type="text"
        value={url}
        onChange={(e) => {
          setUrl(e.target.value);
          setError(null);
        }}
        onKeyDown={handleKeyDown}
        placeholder={t("editor.linkPlaceholder")}
        className={cn(
          "w-48 bg-transparent text-sm text-fg outline-none",
          "placeholder:text-muted",
        )}
        spellCheck={false}
        aria-label={t("editor.linkPlaceholder")}
        aria-invalid={!!error}
      />
      {error && (
        <span className="text-xs text-danger" role="alert">
          {error}
        </span>
      )}
      <button
        type="button"
        onClick={handleConfirm}
        className="inline-flex h-6 w-6 items-center justify-center rounded text-muted transition-colors hover:bg-surface-2 hover:text-fg"
        aria-label={t("common.save")}
        title={t("common.save")}
      >
        <Check className="h-3.5 w-3.5" aria-hidden="true" />
      </button>
      <button
        type="button"
        onClick={onDone}
        className="inline-flex h-6 w-6 items-center justify-center rounded text-muted transition-colors hover:bg-surface-2 hover:text-fg"
        aria-label={t("common.cancel")}
        title={t("common.cancel")}
      >
        <X className="h-3.5 w-3.5" aria-hidden="true" />
      </button>
    </div>
  );
}
