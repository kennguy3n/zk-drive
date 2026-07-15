import { useEffect, useRef, useState } from "react";
import { useTranslation } from "react-i18next";
import {
  Sparkles,
  Wand2,
  FileText,
  Maximize2,
  Minimize2,
  Languages,
  Lightbulb,
  Loader2,
  type LucideIcon,
} from "lucide-react";
import { cn } from "../../lib/cn";
import type { Editor } from "@tiptap/react";
import { streamEditorSkill, type SkillID } from "../../api/editorSkills";

interface SkillMenuItem {
  id: SkillID;
  labelKey: string;
  icon: LucideIcon;
}

const SKILL_ITEMS: SkillMenuItem[] = [
  { id: "improve_writing", labelKey: "editor.aiImproveWriting", icon: Wand2 },
  { id: "summarize", labelKey: "editor.aiSummarize", icon: FileText },
  { id: "expand", labelKey: "editor.aiExpand", icon: Maximize2 },
  { id: "simplify", labelKey: "editor.aiSimplify", icon: Minimize2 },
  { id: "translate", labelKey: "editor.aiTranslate", icon: Languages },
  { id: "generate_ideas", labelKey: "editor.aiGenerateIdeas", icon: Lightbulb },
];

export interface AISelectionMenuProps {
  editor: Editor | null;
  documentId: string;
  isStreaming: boolean;
  onGhostBlockStart: () => void;
  onGhostBlockToken: (token: string) => void;
  onGhostBlockDone: () => void;
  onGhostBlockError: (error: string) => void;
}

export default function AISelectionMenu({
  editor,
  documentId,
  isStreaming,
  onGhostBlockStart,
  onGhostBlockToken,
  onGhostBlockDone,
  onGhostBlockError,
}: AISelectionMenuProps) {
  const { t } = useTranslation();
  const [visible, setVisible] = useState(false);
  const [menuOpen, setMenuOpen] = useState(false);
  const [position, setPosition] = useState({ top: 0, left: 0 });
  const abortRef = useRef<AbortController | null>(null);
  const menuOpenRef = useRef(false);
  menuOpenRef.current = menuOpen;

  // Track selection changes to show/hide the AI button.
  useEffect(() => {
    if (!editor) return;
    const updateSelection = () => {
      const { state, view } = editor;
      const { selection } = state;
      if (selection.empty) {
        setVisible(false);
        setMenuOpen(false);
        return;
      }
      // Check if selection has non-empty text.
      const text = state.doc.textBetween(selection.from, selection.to, "\n");
      if (!text.trim()) {
        setVisible(false);
        setMenuOpen(false);
        return;
      }
      // Calculate position from the selection coords.
      const coords = view.coordsAtPos(selection.from);
      const editorRect = view.dom.getBoundingClientRect();
      setPosition({
        top: coords.top - editorRect.top - 40,
        left: coords.left - editorRect.left,
      });
      setVisible(true);
    };
    const handleBlur = () => {
      setTimeout(() => {
        if (!menuOpenRef.current) setVisible(false);
      }, 200);
    };

    editor.on("selectionUpdate", updateSelection);
    editor.on("blur", handleBlur);
    return () => {
      editor.off("selectionUpdate", updateSelection);
      editor.off("blur", handleBlur);
      abortRef.current?.abort();
    };
  }, [editor]);

  const handleSkill = (skillId: SkillID) => {
    if (!editor) return;
    const { state } = editor;
    const { selection } = state;
    const selectedText = state.doc.textBetween(selection.from, selection.to, "\n");

    // Get surrounding context (the paragraph containing the selection).
    const $from = state.doc.resolve(selection.from);
    const contextText = $from.parent.textContent;

    // Abort any previous stream.
    abortRef.current?.abort();

    onGhostBlockStart();
    setMenuOpen(false);

    abortRef.current = streamEditorSkill(
      documentId,
      {
        skill_id: skillId,
        selection: selectedText,
        context: contextText,
      },
      {
        onToken: (token) => onGhostBlockToken(token),
        onDone: () => onGhostBlockDone(),
        onError: (error) => onGhostBlockError(error),
      },
    );
  };

  if (!visible || !editor) return null;

  return (
    <div
      className="absolute z-20"
      style={{ top: position.top, left: position.left }}
    >
      <button
        type="button"
        disabled={isStreaming}
        onClick={() => setMenuOpen((v) => !v)}
        className={cn(
          "inline-flex h-8 items-center gap-1.5 rounded-full bg-brand px-3 text-xs font-medium text-brand-fg shadow-glow transition-opacity",
          isStreaming ? "opacity-60" : "hover:opacity-95",
        )}
        title={t("editor.aiMenu")}
      >
        {isStreaming ? (
          <Loader2 className="h-3.5 w-3.5 animate-spin" aria-hidden="true" />
        ) : (
          <Sparkles className="h-3.5 w-3.5" aria-hidden="true" />
        )}
        AI
      </button>
      {menuOpen && !isStreaming && (
        <div
          className="absolute left-0 top-10 z-30 w-56 rounded-xl border border-border bg-surface shadow-lg"
          role="menu"
        >
          {SKILL_ITEMS.map((item) => {
            const Icon = item.icon;
            return (
              <button
                key={item.id}
                type="button"
                role="menuitem"
                onClick={() => handleSkill(item.id)}
                className={cn(
                  "flex w-full items-center gap-3 px-3 py-2 text-left text-sm transition-colors",
                  "first:rounded-t-xl last:rounded-b-xl",
                  "hover:bg-surface-2",
                )}
              >
                <Icon className="h-4 w-4 shrink-0 text-muted" aria-hidden="true" />
                <span className="text-fg">{t(item.labelKey)}</span>
              </button>
            );
          })}
        </div>
      )}
    </div>
  );
}
