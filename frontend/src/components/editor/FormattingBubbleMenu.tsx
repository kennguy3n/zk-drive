import { useCallback, useState } from "react";
import { BubbleMenu } from "@tiptap/react";
import type { Editor } from "@tiptap/react";
import { useTranslation } from "react-i18next";
import {
  Bold,
  Italic,
  Underline as UnderlineIcon,
  Strikethrough,
  Code,
  Link as LinkIcon,
  type LucideIcon,
} from "lucide-react";
import { cn } from "../../lib/cn";
import LinkInputPopover from "./LinkInputPopover";

// LinkInputState tracks whether the link popover is open and the
// anchor rect for positioning (the BubbleMenu container's bounding box).
interface LinkInputState {
  open: boolean;
  anchorRect: DOMRect | null;
}

interface BubbleButtonItem {
  icon: LucideIcon;
  labelKey: string;
  isActive?: (editor: Editor) => boolean;
  onClick: (editor: Editor) => void;
}

const BUTTONS: BubbleButtonItem[] = [
  {
    icon: Bold,
    labelKey: "editor.toolbarBold",
    isActive: (e) => e.isActive("bold"),
    onClick: (e) => e.chain().focus().toggleBold().run(),
  },
  {
    icon: Italic,
    labelKey: "editor.toolbarItalic",
    isActive: (e) => e.isActive("italic"),
    onClick: (e) => e.chain().focus().toggleItalic().run(),
  },
  {
    icon: UnderlineIcon,
    labelKey: "editor.toolbarUnderline",
    isActive: (e) => e.isActive("underline"),
    onClick: (e) => e.chain().focus().toggleUnderline().run(),
  },
  {
    icon: Strikethrough,
    labelKey: "editor.toolbarStrikethrough",
    isActive: (e) => e.isActive("strike"),
    onClick: (e) => e.chain().focus().toggleStrike().run(),
  },
  {
    icon: Code,
    labelKey: "editor.toolbarInlineCode",
    isActive: (e) => e.isActive("code"),
    onClick: (e) => e.chain().focus().toggleCode().run(),
  },
  {
    icon: LinkIcon,
    labelKey: "editor.toolbarLink",
    isActive: (e) => e.isActive("link"),
    // Link is handled specially via LinkInputPopover — onClick is a no-op.
    onClick: () => {},
  },
];

export interface FormattingBubbleMenuProps {
  editor: Editor | null;
}

export default function FormattingBubbleMenu({ editor }: FormattingBubbleMenuProps) {
  const { t } = useTranslation();
  const [linkInput, setLinkInput] = useState<LinkInputState>({ open: false, anchorRect: null });

  const handleMouseDown = useCallback(
    (e: React.MouseEvent, btn: BubbleButtonItem) => {
      e.preventDefault();
      if (!editor) return;
      // Link button opens the inline popover instead of window.prompt.
      if (btn.labelKey === "editor.toolbarLink") {
        if (editor.isActive("link")) {
          // If already a link, unset it.
          editor.chain().focus().unsetLink().run();
        } else {
          // Open the popover anchored to the bubble menu element.
          const bubbleEl = (e.currentTarget as HTMLElement).closest("[data-bubble-menu]") as HTMLElement | null;
          setLinkInput({
            open: true,
            anchorRect: bubbleEl?.getBoundingClientRect() ?? null,
          });
        }
        return;
      }
      btn.onClick(editor);
    },
    [editor],
  );

  if (!editor) return null;

  return (
    <>
    <BubbleMenu
      editor={editor}
      tippyOptions={{ duration: 100, placement: "top" }}
      shouldShow={({ editor, state, from, to }) => {
        // Don't show bubble menu if the link input popover is open.
        if (linkInput.open) return false;
        // Only show for non-empty text selections, not in code blocks.
        if (from === to) return false;
        if (editor.isActive("codeBlock")) return false;
        const text = state.doc.textBetween(from, to, "\n");
        return text.trim().length > 0;
      }}
    >
      <div
        className="flex items-center gap-0.5 rounded-lg border border-border bg-surface px-1 py-1 shadow-lg"
        role="toolbar"
        aria-label={t("editor.bubbleMenuLabel")}
        data-bubble-menu
      >
        {BUTTONS.map((btn) => {
          const Icon = btn.icon;
          const active = btn.isActive?.(editor) ?? false;
          return (
            <button
              key={btn.labelKey}
              type="button"
              title={t(btn.labelKey)}
              aria-label={t(btn.labelKey)}
              aria-pressed={active}
              onMouseDown={(e) => handleMouseDown(e, btn)}
              className={cn(
                "inline-flex h-7 w-7 items-center justify-center rounded transition-colors",
                "hover:bg-surface-2 focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring",
                active ? "bg-brand/10 text-brand" : "text-muted hover:text-fg",
              )}
            >
              <Icon className="h-3.5 w-3.5" aria-hidden="true" />
            </button>
          );
        })}
      </div>
    </BubbleMenu>
    {linkInput.open && editor && (
      <LinkInputPopover
        editor={editor}
        anchorRect={linkInput.anchorRect}
        onDone={() => setLinkInput({ open: false, anchorRect: null })}
      />
    )}
    </>
  );
}
