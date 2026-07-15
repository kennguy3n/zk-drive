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
import type { Y as YType } from "yjs";
import type { Awareness } from "y-protocols/awareness";
import type { CollabMode } from "../../api/client";
import { SlashCommand, type AISkillTrigger } from "./SlashCommand";
import { renderSlashMenu } from "./SlashMenu";

export interface BuildExtensionsOptions {
  yDoc: YType.Doc | null;
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
    }),
  ];

  // Typography adds smart quotes, em-dashes, ellipses, etc.
  base.push(Typography);

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
