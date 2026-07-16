import { useCallback, useEffect, useRef, useState } from "react";
import type { Editor } from "@tiptap/react";
import type { Node as PMNode } from "@tiptap/pm/model";
import { useTranslation } from "react-i18next";
import {
  Copy,
  Trash2,
  ChevronRight,
  Type,
  Heading1,
  Heading2,
  Heading3,
  List,
  ListOrdered,
  Quote,
  Code,
  type LucideIcon,
} from "lucide-react";
import { cn } from "../../lib/cn";
import { findBlockStart } from "./blockUtils";

interface TurnIntoOption {
  labelKey: string;
  icon: LucideIcon;
  command: (editor: Editor) => void;
  isActive: (editor: Editor) => boolean;
}

const TURN_INTO_OPTIONS: TurnIntoOption[] = [
  {
    labelKey: "editor.turnIntoText",
    icon: Type,
    command: (e) => e.chain().focus().setParagraph().run(),
    isActive: (e) => e.isActive("paragraph"),
  },
  {
    labelKey: "editor.toolbarH1",
    icon: Heading1,
    command: (e) => e.chain().focus().setHeading({ level: 1 }).run(),
    isActive: (e) => e.isActive("heading", { level: 1 }),
  },
  {
    labelKey: "editor.toolbarH2",
    icon: Heading2,
    command: (e) => e.chain().focus().setHeading({ level: 2 }).run(),
    isActive: (e) => e.isActive("heading", { level: 2 }),
  },
  {
    labelKey: "editor.toolbarH3",
    icon: Heading3,
    command: (e) => e.chain().focus().setHeading({ level: 3 }).run(),
    isActive: (e) => e.isActive("heading", { level: 3 }),
  },
  {
    labelKey: "editor.toolbarBulletList",
    icon: List,
    command: (e) => e.chain().focus().toggleBulletList().run(),
    isActive: (e) => e.isActive("bulletList"),
  },
  {
    labelKey: "editor.toolbarNumberedList",
    icon: ListOrdered,
    command: (e) => e.chain().focus().toggleOrderedList().run(),
    isActive: (e) => e.isActive("orderedList"),
  },
  {
    labelKey: "editor.toolbarQuote",
    icon: Quote,
    command: (e) => e.chain().focus().toggleBlockquote().run(),
    isActive: (e) => e.isActive("blockquote"),
  },
  {
    labelKey: "editor.turnIntoCodeBlock",
    icon: Code,
    command: (e) => e.chain().focus().toggleCodeBlock().run(),
    isActive: (e) => e.isActive("codeBlock"),
  },
];

export interface BlockMenuProps {
  editor: Editor | null;
}

export default function BlockMenu({ editor }: BlockMenuProps) {
  const { t } = useTranslation();
  const [visible, setVisible] = useState(false);
  const [menuOpen, setMenuOpen] = useState(false);
  const [turnIntoOpen, setTurnIntoOpen] = useState(false);
  const [position, setPosition] = useState({ top: 0, left: 0 });
  const [blockPos, setBlockPos] = useState<number | null>(null);
  // Store a signature (type + text content + node size) of the block
  // at hover time. In collaborative mode, the block at blockPos could
  // change between hover and click — we verify the signature matches
  // before acting to avoid operating on the wrong block.
  const blockSignatureRef = useRef<string | null>(null);
  const containerRef = useRef<HTMLDivElement>(null);
  const menuOpenRef = useRef(false);
  menuOpenRef.current = menuOpen;
  // rAF throttle for mousemove — avoids excessive posAtCoords calls.
  const rafRef = useRef<number | null>(null);
  const lastMoveRef = useRef<MouseEvent | null>(null);
  // Track the mouseleave timeout so we can cancel it on unmount.
  const leaveTimeoutRef = useRef<number | null>(null);
  // Track whether the submenu should flip left (when near right edge).
  const [flipSubmenu, setFlipSubmenu] = useState(false);


  useEffect(() => {
    if (!editor) return;

    const processMouseMove = () => {
      rafRef.current = null;
      const e = lastMoveRef.current;
      lastMoveRef.current = null;
      if (!e) return;

      const view = editor.view;
      const rect = view.dom.getBoundingClientRect();
      const x = e.clientX - rect.left;

      // Only show in the left gutter (within 40px of the editor's left edge).
      if (x > 40 || x < 0) {
        if (!menuOpenRef.current) {
          setVisible(false);
          setMenuOpen(false);
        }
        return;
      }

      const pos = view.posAtCoords({ left: e.clientX, top: e.clientY });
      if (!pos) return;

      const blockStart = findBlockStart(pos.pos, view.state.doc);
      if (blockStart === null) return;

      // Check it's a top-level block.
      const $block = view.state.doc.resolve(blockStart);
      if ($block.depth !== 1) return;

      const node = view.state.doc.nodeAt(blockStart);
      if (!node) return;

      // Skip if it's inside a list item or table cell.
      if (node.type.name === "listItem" || node.type.name === "tableCell" || node.type.name === "tableHeader") {
        return;
      }

      const coords = view.coordsAtPos(blockStart);
      setPosition({
        top: coords.top - rect.top,
        left: 0,
      });
      setBlockPos(blockStart);
      blockSignatureRef.current = `${node.type.name}:${node.nodeSize}:${node.textContent}`;
      setVisible(true);
    };

    const handleMouseMove = (e: MouseEvent) => {
      lastMoveRef.current = e;
      if (rafRef.current === null) {
        rafRef.current = requestAnimationFrame(processMouseMove);
      }
    };

    const handleMouseLeave = () => {
      leaveTimeoutRef.current = window.setTimeout(() => {
        if (!menuOpenRef.current) {
          setVisible(false);
          setMenuOpen(false);
          setTurnIntoOpen(false);
        }
      }, 200);
    };

    const editorDom = editor.view.dom;
    editorDom.addEventListener("mousemove", handleMouseMove);
    editorDom.addEventListener("mouseleave", handleMouseLeave);

    return () => {
      editorDom.removeEventListener("mousemove", handleMouseMove);
      editorDom.removeEventListener("mouseleave", handleMouseLeave);
      if (rafRef.current !== null) {
        cancelAnimationFrame(rafRef.current);
        rafRef.current = null;
      }
      if (leaveTimeoutRef.current !== null) {
        clearTimeout(leaveTimeoutRef.current);
        leaveTimeoutRef.current = null;
      }
    };
  }, [editor, findBlockStart]);

  // verifyBlock checks that the node at blockPos still matches the
  // block the user originally hovered. Returns the node if valid, null
  // if the block has changed (e.g. due to concurrent edits in collab).
  const verifyBlock = useCallback((): PMNode | null => {
    if (!editor || blockPos === null) return null;
    const node = editor.state.doc.nodeAt(blockPos);
    if (!node) return null;
    const sig = `${node.type.name}:${node.nodeSize}:${node.textContent}`;
    if (blockSignatureRef.current !== sig) return null;
    return node;
  }, [editor, blockPos]);

  const handleDuplicate = useCallback(() => {
    if (!editor || blockPos === null) return;
    const node = verifyBlock();
    if (!node) return;
    editor
      .chain()
      .focus()
      .insertContentAt(blockPos + node.nodeSize, node.toJSON())
      .run();
    setMenuOpen(false);
  }, [editor, blockPos, verifyBlock]);

  const handleDelete = useCallback(() => {
    if (!editor || blockPos === null) return;
    const node = verifyBlock();
    if (!node) return;
    editor.chain().focus().deleteRange({ from: blockPos, to: blockPos + node.nodeSize }).run();
    setMenuOpen(false);
    setVisible(false);
  }, [editor, blockPos, verifyBlock]);

  const handleTurnInto = useCallback(
    (opt: TurnIntoOption) => {
      const node = verifyBlock();
      if (!node || !editor || blockPos === null) return;
      // Set selection to the entire block before running the command,
      // so "turn into" affects the block the user clicked on, not
      // wherever the cursor happened to be.
      editor
        .chain()
        .focus()
        .setTextSelection({ from: blockPos, to: blockPos + node.nodeSize })
        .run();
      opt.command(editor);
      setMenuOpen(false);
      setTurnIntoOpen(false);
    },
    [editor, blockPos, verifyBlock],
  );

  // When the turn-into submenu opens, check if it would overflow the
  // right edge of the viewport and flip it to the left side if so.
  useEffect(() => {
    if (!turnIntoOpen || !containerRef.current) return;
    const rect = containerRef.current.getBoundingClientRect();
    setFlipSubmenu(rect.right + 176 > window.innerWidth);
  }, [turnIntoOpen]);

  if (!editor || !visible) return null;

  return (
    <div
      ref={containerRef}
      className="absolute z-20"
      style={{ top: position.top, left: position.left }}
      onMouseEnter={() => setVisible(true)}
    >
      <button
        type="button"
        onClick={(e) => {
          e.preventDefault();
          e.stopPropagation();
          setMenuOpen((v) => !v);
          setTurnIntoOpen(false);
        }}
        className={cn(
          "inline-flex h-6 w-4 cursor-grab items-center justify-center rounded text-muted transition-colors",
          "hover:bg-surface-2 hover:text-fg",
          menuOpen && "bg-surface-2 text-fg",
        )}
        aria-label={t("editor.blockMenu")}
        title={t("editor.blockMenu")}
      >
        <svg viewBox="0 0 16 16" className="h-3.5 w-3.5" fill="currentColor" aria-hidden="true">
          <circle cx="4" cy="4" r="1.5" />
          <circle cx="4" cy="8" r="1.5" />
          <circle cx="4" cy="12" r="1.5" />
          <circle cx="12" cy="4" r="1.5" />
          <circle cx="12" cy="8" r="1.5" />
          <circle cx="12" cy="12" r="1.5" />
        </svg>
      </button>

      {menuOpen && (
        <div
          className="absolute left-5 top-0 z-30 w-44 rounded-xl border border-border bg-surface py-1 shadow-lg"
          role="menu"
          onMouseLeave={() => {
            setMenuOpen(false);
            setTurnIntoOpen(false);
          }}
        >
          <button
            type="button"
            role="menuitem"
            onClick={handleDuplicate}
            className="flex w-full items-center gap-3 px-3 py-2 text-left text-sm text-fg transition-colors hover:bg-surface-2"
          >
            <Copy className="h-4 w-4 shrink-0 text-muted" aria-hidden="true" />
            {t("editor.blockDuplicate")}
          </button>

          <div className="relative">
            <button
              type="button"
              role="menuitem"
              onClick={() => setTurnIntoOpen((v) => !v)}
              className="flex w-full items-center gap-3 px-3 py-2 text-left text-sm text-fg transition-colors hover:bg-surface-2"
            >
              <Type className="h-4 w-4 shrink-0 text-muted" aria-hidden="true" />
              {t("editor.blockTurnInto")}
              <ChevronRight className="ml-auto h-3.5 w-3.5 text-muted" aria-hidden="true" />
            </button>

            {turnIntoOpen && (
              <div
                className={cn(
                  "absolute top-0 z-40 w-44 rounded-xl border border-border bg-surface py-1 shadow-lg",
                  flipSubmenu ? "right-full mr-1" : "left-full ml-1",
                )}
                role="menu"
              >
                {TURN_INTO_OPTIONS.map((opt) => {
                  const Icon = opt.icon;
                  const active = opt.isActive(editor);
                  return (
                    <button
                      key={opt.labelKey}
                      type="button"
                      role="menuitem"
                      onClick={() => handleTurnInto(opt)}
                      className={cn(
                        "flex w-full items-center gap-3 px-3 py-2 text-left text-sm transition-colors hover:bg-surface-2",
                        active ? "text-brand" : "text-fg",
                      )}
                    >
                      <Icon className="h-4 w-4 shrink-0 text-muted" aria-hidden="true" />
                      {t(opt.labelKey)}
                    </button>
                  );
                })}
              </div>
            )}
          </div>

          <div className="my-1 border-t border-border" />

          <button
            type="button"
            role="menuitem"
            onClick={handleDelete}
            className="flex w-full items-center gap-3 px-3 py-2 text-left text-sm text-danger transition-colors hover:bg-danger/10"
          >
            <Trash2 className="h-4 w-4 shrink-0" aria-hidden="true" />
            {t("editor.blockDelete")}
          </button>
        </div>
      )}
    </div>
  );
}
