import type { AnyExtension } from "@tiptap/react";
import StarterKit from "@tiptap/starter-kit";
import Collaboration from "@tiptap/extension-collaboration";
import CollaborationCursor from "@tiptap/extension-collaboration-cursor";
import Link from "@tiptap/extension-link";
import Image from "@tiptap/extension-image";
import Table from "@tiptap/extension-table";
import TableRow from "@tiptap/extension-table-row";
import TableCell from "@tiptap/extension-table-cell";
import TableHeader from "@tiptap/extension-table-header";
import Underline from "@tiptap/extension-underline";
import TaskList from "@tiptap/extension-task-list";
import TaskItem from "@tiptap/extension-task-item";
import Typography from "@tiptap/extension-typography";
import Placeholder from "@tiptap/extension-placeholder";
import CodeBlockLowlight from "@tiptap/extension-code-block-lowlight";
import { createLowlight, common } from "lowlight";
import { Markdown } from "tiptap-markdown";
import type { Doc as YDoc } from "yjs";
import type { Awareness } from "y-protocols/awareness";
import type { CollabMode } from "../../api/client";
import { SlashCommand, type AISkillTrigger } from "./SlashCommand";
import { renderSlashMenu } from "./SlashMenu";
import { BlockDragHandle } from "./DragHandle";

const lowlightInstance = createLowlight(common);

export interface BuildExtensionsOptions {
  yDoc: YDoc | null;
  awareness: Awareness | null;
  collabMode: CollabMode;
  presenceAllowed: boolean;
  richExtensionsAllowed: boolean;
  placeholderText?: string;
  onAISkill?: AISkillTrigger;
}

export function buildExtensions(opts: BuildExtensionsOptions): AnyExtension[] {
  const {
    yDoc,
    awareness,
    collabMode,
    presenceAllowed,
    richExtensionsAllowed,
    placeholderText,
    onAISkill,
  } = opts;

  const base: AnyExtension[] = [
    StarterKit.configure({
      history: false,
      // Replace the default code block with the lowlight-enhanced
      // version that provides syntax highlighting.
      codeBlock: false,
    }),
  ];

  // Typography adds smart quotes, em-dashes, ellipses, etc.
  base.push(Typography);

  // Code block with syntax highlighting via lowlight (highlight.js).
  // Replaces StarterKit's plain code block.
  base.push(
    CodeBlockLowlight.configure({
      lowlight: lowlightInstance,
    }),
  );

  // Link is always available — it's a text-level mark, not a rich
  // content block, so it works in markdown mode too.
  base.push(
    Link.configure({
      openOnClick: false,
      autolink: true,
      linkOnPaste: true,
    }),
  );

  // Underline is a basic text mark, available in all modes.
  base.push(Underline);

  // Markdown serialization/parsing support via tiptap-markdown.
  // Enables WYSIWYG ↔ Markdown round-tripping: bold, italic, code
  // blocks, tables, task lists, images, links all survive the
  // conversion. html:false blocks raw HTML in markdown source to
  // prevent XSS — only schema-validated nodes/marks are allowed.
  if (collabMode !== "disabled") {
    base.push(
      Markdown.configure({
        html: false,
        linkify: true,
        breaks: false,
        transformPastedText: false,
        transformCopiedText: false,
      }),
    );
  }

  // Placeholder — shown on empty documents and empty lines.
  if (placeholderText) {
    base.push(
      Placeholder.configure({
        placeholder: placeholderText,
        includeChildren: true,
      }),
    );
  }

  // Rich content extensions — only in rich / rich_presence modes
  // where the folder's encryption mode allows server-side snapshots.
  if (richExtensionsAllowed && (collabMode === "rich" || collabMode === "rich_presence")) {
    base.push(Image);
    base.push(
      Table.configure({ resizable: true }),
      TableRow,
      TableCell,
      TableHeader,
    );
    base.push(
      TaskList,
      TaskItem.configure({ nested: true }),
    );
  }

  // Slash command menu — available in all writable modes.
  if (collabMode !== "disabled") {
    base.push(
      SlashCommand.configure({
        richExtensionsAllowed,
        onAISkill,
        suggestion: {
          render: renderSlashMenu,
        },
      }),
    );
    // Block drag handle — shows a drag icon on hover for reordering
    // top-level blocks via native HTML5 drag-and-drop.
    base.push(BlockDragHandle);
  }

  // Collaboration — Yjs CRDT sync. Always paired with a non-null yDoc.
  if (yDoc && collabMode !== "disabled") {
    base.push(Collaboration.configure({ document: yDoc }));
  }

  // Presence cursors — only in rich_presence mode with awareness.
  if (presenceAllowed && awareness) {
    base.push(
      CollaborationCursor.configure({
        provider: { awareness },
      }),
    );
  }

  return base;
}
