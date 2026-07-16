import { useCallback, useState, type RefObject } from "react";
import { useTranslation } from "react-i18next";
import type { Editor } from "@tiptap/react";
import {
  Bold,
  Italic,
  Underline as UnderlineIcon,
  Strikethrough,
  Heading1,
  Heading2,
  Heading3,
  List,
  ListOrdered,
  CheckSquare,
  Quote,
  Code,
  Link as LinkIcon,
  Image as ImageIcon,
  Table as TableIcon,
  Undo2,
  Redo2,
  type LucideIcon,
} from "lucide-react";
import { cn } from "../../lib/cn";
import LinkInputPopover from "./LinkInputPopover";
import MarkdownToggle from "./MarkdownToggle";

// Max image size for base64 embedding in the Yjs document. Larger
// images bloat the CRDT state, sync payload, and Postgres row — 2 MB
// is a reasonable ceiling for inline content.
const MAX_IMAGE_BYTES = 2 * 1024 * 1024;

// Allowed image MIME types for upload.
const ALLOWED_IMAGE_TYPES = ["image/png", "image/jpeg", "image/gif", "image/webp", "image/svg+xml"];

// Reads a File as a data URL, enforcing size and type constraints.
// Returns null if the file is rejected.
function readImageFile(file: File): Promise<string | null> {
  if (!ALLOWED_IMAGE_TYPES.includes(file.type)) {
    return Promise.resolve(null);
  }
  if (file.size > MAX_IMAGE_BYTES) {
    return Promise.resolve(null);
  }
  return new Promise((resolve) => {
    const reader = new FileReader();
    reader.onload = () => resolve(reader.result as string);
    reader.onerror = () => resolve(null);
    reader.readAsDataURL(file);
  });
}

interface ToolbarButton {
  icon: LucideIcon;
  labelKey: string;
  isActive?: (editor: Editor) => boolean;
  onClick: (editor: Editor) => void;
  richOnly?: boolean;
}

interface ToolbarGroup {
  buttons: ToolbarButton[];
}

const GROUPS: ToolbarGroup[] = [
  {
    buttons: [
      {
        icon: Undo2,
        labelKey: "editor.toolbarUndo",
        onClick: (e) => e.chain().focus().undo().run(),
      },
      {
        icon: Redo2,
        labelKey: "editor.toolbarRedo",
        onClick: (e) => e.chain().focus().redo().run(),
      },
    ],
  },
  {
    buttons: [
      {
        icon: Heading1,
        labelKey: "editor.toolbarH1",
        isActive: (e) => e.isActive("heading", { level: 1 }),
        onClick: (e) => e.chain().focus().toggleHeading({ level: 1 }).run(),
      },
      {
        icon: Heading2,
        labelKey: "editor.toolbarH2",
        isActive: (e) => e.isActive("heading", { level: 2 }),
        onClick: (e) => e.chain().focus().toggleHeading({ level: 2 }).run(),
      },
      {
        icon: Heading3,
        labelKey: "editor.toolbarH3",
        isActive: (e) => e.isActive("heading", { level: 3 }),
        onClick: (e) => e.chain().focus().toggleHeading({ level: 3 }).run(),
      },
    ],
  },
  {
    buttons: [
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
    ],
  },
  {
    buttons: [
      {
        icon: List,
        labelKey: "editor.toolbarBulletList",
        isActive: (e) => e.isActive("bulletList"),
        onClick: (e) => e.chain().focus().toggleBulletList().run(),
      },
      {
        icon: ListOrdered,
        labelKey: "editor.toolbarNumberedList",
        isActive: (e) => e.isActive("orderedList"),
        onClick: (e) => e.chain().focus().toggleOrderedList().run(),
      },
      {
        icon: CheckSquare,
        labelKey: "editor.toolbarTodoList",
        isActive: (e) => e.isActive("taskList"),
        onClick: (e) => e.chain().focus().toggleTaskList().run(),
        richOnly: true,
      },
      {
        icon: Quote,
        labelKey: "editor.toolbarQuote",
        isActive: (e) => e.isActive("blockquote"),
        onClick: (e) => e.chain().focus().toggleBlockquote().run(),
      },
    ],
  },
  {
    buttons: [
      {
        icon: LinkIcon,
        labelKey: "editor.toolbarLink",
        isActive: (e) => e.isActive("link"),
        // Link is handled specially via LinkInputPopover.
        onClick: () => {},
      },
      {
        icon: ImageIcon,
        labelKey: "editor.toolbarImage",
        onClick: (e) => {
          const input = document.createElement("input");
          input.type = "file";
          input.accept = "image/*";
          input.onchange = async () => {
            const file = input.files?.[0];
            if (!file) return;
            const dataUrl = await readImageFile(file);
            if (dataUrl) {
              e.chain().focus().setImage({ src: dataUrl }).run();
            }
          };
          input.click();
        },
        richOnly: true,
      },
      {
        icon: TableIcon,
        labelKey: "editor.toolbarTable",
        onClick: (e) =>
          e
            .chain()
            .focus()
            .insertTable({ rows: 3, cols: 3, withHeaderRow: true })
            .run(),
        richOnly: true,
      },
    ],
  },
];

export interface EditorToolbarProps {
  editor: Editor | null;
  richExtensionsAllowed: boolean;
  isCollaborative?: boolean;
  writable?: boolean;
  editorCardRef?: RefObject<HTMLDivElement | null>;
}

export default function EditorToolbar({
  editor,
  richExtensionsAllowed,
  isCollaborative = false,
  writable = true,
  editorCardRef,
}: EditorToolbarProps) {
  const { t } = useTranslation();
  const [linkInput, setLinkInput] = useState<{ open: boolean; anchorRect: DOMRect | null }>({ open: false, anchorRect: null });

  const handleMouseDown = useCallback(
    (e: React.MouseEvent, btn: ToolbarButton) => {
      e.preventDefault();
      if (!editor) return;
      // Link button opens the inline popover instead of window.prompt.
      if (btn.labelKey === "editor.toolbarLink") {
        if (editor.isActive("link")) {
          editor.chain().focus().unsetLink().run();
        } else {
          const btnEl = e.currentTarget as HTMLElement;
          setLinkInput({
            open: true,
            anchorRect: btnEl.getBoundingClientRect(),
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
    <div
      className="sticky top-0 z-10 flex flex-wrap items-center gap-1 border-b border-border bg-surface/95 px-2 py-1.5 backdrop-blur"
      role="toolbar"
      aria-label={t("editor.toolbarLabel")}
    >
      {GROUPS.map((group, gi) => (
        <div key={gi} className="flex items-center gap-0.5">
          {gi > 0 && <div className="mx-1 h-5 w-px bg-border" />}
          {group.buttons.map((btn) => {
            if (btn.richOnly && !richExtensionsAllowed) return null;
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
                  "inline-flex h-8 w-8 items-center justify-center rounded-md transition-colors",
                  "hover:bg-surface-2 focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring",
                  active
                    ? "bg-brand/10 text-brand"
                    : "text-muted hover:text-fg",
                )}
              >
                <Icon className="h-4 w-4" aria-hidden="true" />
              </button>
            );
          })}
        </div>
      ))}
      {linkInput.open && editor && (
        <LinkInputPopover
          editor={editor}
          anchorRect={linkInput.anchorRect}
          onDone={() => setLinkInput({ open: false, anchorRect: null })}
        />
      )}
      <div className="ml-auto flex items-center gap-0.5">
        {writable && (
          <>
            <div className="mx-1 h-5 w-px bg-border" />
            <MarkdownToggle
              editor={editor}
              visible={true}
              isCollaborative={isCollaborative}
              containerRef={editorCardRef}
            />
          </>
        )}
      </div>
    </div>
  );
}
