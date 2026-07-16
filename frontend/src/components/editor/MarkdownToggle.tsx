import { useCallback, useEffect, useRef, useState } from "react";
import { useTranslation } from "react-i18next";
import { Code2, Eye, AlertTriangle } from "lucide-react";
import { cn } from "../../lib/cn";
import type { Editor } from "@tiptap/react";
import { Modal } from "../ui/Modal";

export interface MarkdownToggleProps {
  editor: Editor | null;
  visible: boolean;
  // When true, the editor is in a collaborative mode with other
  // users potentially editing. Switching back from markdown mode
  // will overwrite concurrent edits — we warn the user first.
  isCollaborative?: boolean;
}

// MarkdownToggle provides a WYSIWYG ↔ Markdown source toggle for the
// document editor. In markdown mode, the TipTap editor is hidden and a
// monospace textarea is shown instead, displaying the document content
// as raw markdown via tiptap-markdown's serializer.
//
// Round-trip is lossless: bold, italic, code blocks, tables, task
// lists, images, and links all survive the conversion because
// tiptap-markdown uses a proper ProseMirror ↔ markdown serializer/parser
// (backed by markdown-it and prosemirror-markdown).
//
// In collaborative mode, switching back to WYSIWYG calls setContent()
// which overwrites the entire Yjs document — this would clobber
// concurrent edits from other users. We show a confirmation dialog
// before proceeding.
export default function MarkdownToggle({ editor, visible, isCollaborative = false }: MarkdownToggleProps) {
  const { t } = useTranslation();
  const [isMarkdownMode, setIsMarkdownMode] = useState(false);
  const [markdownText, setMarkdownText] = useState("");
  const [showCollabWarning, setShowCollabWarning] = useState(false);
  const textareaRef = useRef<HTMLTextAreaElement>(null);

  // Reset to WYSIWYG mode when the editor changes (e.g. document switch).
  useEffect(() => {
    setIsMarkdownMode(false);
    setShowCollabWarning(false);
  }, [editor]);

  // Focus the textarea when entering markdown mode.
  useEffect(() => {
    if (isMarkdownMode && textareaRef.current) {
      textareaRef.current.focus();
    }
  }, [isMarkdownMode]);

  const getMarkdownFromEditor = useCallback((): string => {
    if (!editor) return "";
    const storage = editor.storage.markdown;
    if (storage?.getMarkdown) {
      return storage.getMarkdown();
    }
    // Fallback: plain text extraction (shouldn't happen if the
    // Markdown extension is properly configured).
    return editor.state.doc.textBetween(0, editor.state.doc.content.size, "\n\n");
  }, [editor]);

  const applyMarkdownToEditor = useCallback(() => {
    if (!editor) return;
    const storage = editor.storage.markdown;
    if (storage?.parser?.parse) {
      const html = storage.parser.parse(markdownText);
      editor.chain().focus().setContent(html).run();
    } else {
      // Fallback: insert as plain text paragraphs.
      const lines = markdownText.split("\n");
      const content = lines.map((line) => ({
        type: "paragraph",
        content: line.trim() ? [{ type: "text", text: line.trim() }] : [],
      }));
      editor.chain().focus().setContent(content).run();
    }
  }, [editor, markdownText]);

  const handleToggle = useCallback(() => {
    if (!editor) return;
    if (!isMarkdownMode) {
      // Switching to markdown mode. Make the editor non-editable so
      // the underlying TipTap instance doesn't process keystrokes or
      // selection changes while the textarea is visible.
      const md = getMarkdownFromEditor();
      setMarkdownText(md);
      editor.setEditable(false);
      setIsMarkdownMode(true);
    } else {
      // Switching back to WYSIWYG. In collaborative mode, warn the
      // user that this will overwrite concurrent edits.
      if (isCollaborative) {
        setShowCollabWarning(true);
      } else {
        applyMarkdownToEditor();
        editor.setEditable(true);
        setIsMarkdownMode(false);
      }
    }
  }, [editor, isMarkdownMode, isCollaborative, getMarkdownFromEditor, applyMarkdownToEditor]);

  const handleConfirmCollabOverwrite = useCallback(() => {
    applyMarkdownToEditor();
    if (editor) editor.setEditable(true);
    setIsMarkdownMode(false);
    setShowCollabWarning(false);
  }, [applyMarkdownToEditor, editor]);

  const handleCancelCollabWarning = useCallback(() => {
    setShowCollabWarning(false);
  }, []);

  // Restore editor editability if the component unmounts in markdown mode.
  useEffect(() => {
    return () => {
      if (editor && isMarkdownMode) {
        editor.setEditable(true);
      }
    };
  }, [editor, isMarkdownMode]);

  if (!visible || !editor) return null;

  return (
    <>
      <button
        type="button"
        onClick={handleToggle}
        title={isMarkdownMode ? t("editor.toggleWysiwyg") : t("editor.toggleMarkdown")}
        aria-label={isMarkdownMode ? t("editor.toggleWysiwyg") : t("editor.toggleMarkdown")}
        className={cn(
          "inline-flex h-8 w-8 items-center justify-center rounded-md transition-colors",
          "hover:bg-surface-2 focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring",
          isMarkdownMode ? "bg-brand/10 text-brand" : "text-muted hover:text-fg",
        )}
      >
        {isMarkdownMode ? (
          <Eye className="h-4 w-4" aria-hidden="true" />
        ) : (
          <Code2 className="h-4 w-4" aria-hidden="true" />
        )}
      </button>

      {isMarkdownMode && (
        <textarea
          ref={textareaRef}
          value={markdownText}
          onChange={(e) => setMarkdownText(e.target.value)}
          className="absolute inset-0 z-30 w-full rounded-card border border-border bg-surface p-6 font-mono text-sm leading-relaxed text-fg outline-none"
          style={{ minHeight: "55vh" }}
          spellCheck={false}
          aria-label={t("editor.markdownSource")}
          // Prevent the underlying TipTap editor from receiving
          // keyboard events while the textarea is visible.
          onKeyDown={(e) => e.stopPropagation()}
        />
      )}

      {showCollabWarning && (
        <Modal
          open={showCollabWarning}
          onOpenChange={(open) => {
            if (!open) setShowCollabWarning(false);
          }}
          title={t("editor.markdownCollabWarningTitle")}
          description={t("editor.markdownCollabWarningBody")}
          size="sm"
          footer={
            <>
              <button
                type="button"
                onClick={handleCancelCollabWarning}
                className="rounded-md px-3 py-1.5 text-sm text-muted transition-colors hover:bg-surface-2 hover:text-fg"
              >
                {t("common.cancel")}
              </button>
              <button
                type="button"
                onClick={handleConfirmCollabOverwrite}
                className="rounded-md bg-warning px-3 py-1.5 text-sm font-medium text-white transition-colors hover:bg-warning/90"
              >
                {t("editor.markdownCollabConfirm")}
              </button>
            </>
          }
        >
          <div className="flex items-start gap-3">
            <AlertTriangle className="mt-0.5 h-5 w-5 shrink-0 text-warning" aria-hidden="true" />
          </div>
        </Modal>
      )}
    </>
  );
}
