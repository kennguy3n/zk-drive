import { Extension } from "@tiptap/core";
import { type Editor, type Range } from "@tiptap/core";
import Suggestion, { type SuggestionProps, type SuggestionKeyDownProps } from "@tiptap/suggestion";
import { PluginKey } from "@tiptap/pm/state";

// Max image size for base64 embedding (2 MB). Mirrors the toolbar
// constant — kept here so the slash command path is self-contained.
const MAX_IMAGE_BYTES = 2 * 1024 * 1024;
const ALLOWED_IMAGE_TYPES = ["image/png", "image/jpeg", "image/gif", "image/webp", "image/svg+xml"];

function readImageFile(file: File): Promise<string | null> {
  if (!ALLOWED_IMAGE_TYPES.includes(file.type)) return Promise.resolve(null);
  if (file.size > MAX_IMAGE_BYTES) return Promise.resolve(null);
  return new Promise((resolve) => {
    const reader = new FileReader();
    reader.onload = () => resolve(reader.result as string);
    reader.onerror = () => resolve(null);
    reader.readAsDataURL(file);
  });
}

export interface SlashCommandItem {
  title: string;
  description: string;
  icon: string;
  category: "block" | "rich" | "ai";
  richOnly?: boolean;
  command: (editor: Editor, range: Range) => void;
}

// AISkillTrigger is the callback the editor page provides so slash
// command AI items can invoke the streaming skill service. The skill
// ID matches the backend's SkillID enum. precedingText is the text
// before the cursor for context. selection is the selected text (may
// be empty for generative skills like generate_ideas). replaceRange
// is an optional {from, to} pair — when set, the ghost block accept
// handler replaces that range instead of inserting at the cursor.
export interface AISkillTrigger {
  (
    skillId: string,
    precedingText: string,
    selection: string,
    replaceRange?: { from: number; to: number },
  ): void;
}

export interface SlashCommandOptions {
  richExtensionsAllowed: boolean;
  onAISkill?: AISkillTrigger;
  suggestion?: {
    items?: (opts: {
      query: string;
      editor: Editor;
      richExtensionsAllowed: boolean;
      onAISkill?: AISkillTrigger;
    }) => SlashCommandItem[];
    render?: () => {
      onBeforeStart?: (props: SuggestionProps<SlashCommandItem>) => void;
      onStart?: (props: SuggestionProps<SlashCommandItem>) => void;
      onBeforeUpdate?: (props: SuggestionProps<SlashCommandItem>) => void;
      onUpdate?: (props: SuggestionProps<SlashCommandItem>) => void;
      onExit?: () => void;
      onKeyDown?: (props: SuggestionKeyDownProps) => boolean;
    };
  };
}

const slashCommandPluginKey = new PluginKey("slashCommand");

// Default command items — populated by the SlashMenu React component
// via the suggestion render callback. The items list is built here so
// the Suggestion plugin has a data source; the rendering is handled
// by the React component passed through `render`.
function defaultItems(opts: {
  query: string;
  editor: Editor;
  richExtensionsAllowed: boolean;
  onAISkill?: AISkillTrigger;
}): SlashCommandItem[] {
  const { query, richExtensionsAllowed, onAISkill } = opts;
  const q = query.toLowerCase();

  const allItems: SlashCommandItem[] = [
    {
      title: "Text",
      description: "Plain text paragraph",
      icon: "Type",
      category: "block",
      command: (editor, range) =>
        editor.chain().focus().deleteRange(range).setParagraph().run(),
    },
    {
      title: "Heading 1",
      description: "Large section heading",
      icon: "Heading1",
      category: "block",
      command: (editor, range) =>
        editor.chain().focus().deleteRange(range).setHeading({ level: 1 }).run(),
    },
    {
      title: "Heading 2",
      description: "Medium section heading",
      icon: "Heading2",
      category: "block",
      command: (editor, range) =>
        editor.chain().focus().deleteRange(range).setHeading({ level: 2 }).run(),
    },
    {
      title: "Heading 3",
      description: "Small section heading",
      icon: "Heading3",
      category: "block",
      command: (editor, range) =>
        editor.chain().focus().deleteRange(range).setHeading({ level: 3 }).run(),
    },
    {
      title: "Bullet list",
      description: "Create a bulleted list",
      icon: "List",
      category: "block",
      command: (editor, range) =>
        editor.chain().focus().deleteRange(range).toggleBulletList().run(),
    },
    {
      title: "Numbered list",
      description: "Create a numbered list",
      icon: "ListOrdered",
      category: "block",
      command: (editor, range) =>
        editor.chain().focus().deleteRange(range).toggleOrderedList().run(),
    },
    {
      title: "To-do list",
      description: "Create a task list with checkboxes",
      icon: "CheckSquare",
      category: "rich",
      richOnly: true,
      command: (editor, range) =>
        editor.chain().focus().deleteRange(range).toggleTaskList().run(),
    },
    {
      title: "Quote",
      description: "Capture a quote",
      icon: "Quote",
      category: "block",
      command: (editor, range) =>
        editor.chain().focus().deleteRange(range).toggleBlockquote().run(),
    },
    {
      title: "Code block",
      description: "Capture a code snippet",
      icon: "Code",
      category: "block",
      command: (editor, range) =>
        editor.chain().focus().deleteRange(range).toggleCodeBlock().run(),
    },
    {
      title: "Divider",
      description: "Visual separator between blocks",
      icon: "Minus",
      category: "block",
      command: (editor, range) =>
        editor.chain().focus().deleteRange(range).setHorizontalRule().run(),
    },
    {
      title: "Table",
      description: "Insert a table",
      icon: "Table",
      category: "rich",
      richOnly: true,
      command: (editor, range) =>
        editor
          .chain()
          .focus()
          .deleteRange(range)
          .insertTable({ rows: 3, cols: 3, withHeaderRow: true })
          .run(),
    },
    {
      title: "Image",
      description: "Upload an image from your device",
      icon: "Image",
      category: "rich",
      richOnly: true,
      command: (editor, range) => {
        editor.chain().focus().deleteRange(range).run();
        const input = document.createElement("input");
        input.type = "file";
        input.accept = "image/*";
        input.onchange = async () => {
          const file = input.files?.[0];
          if (!file) return;
          const dataUrl = await readImageFile(file);
          if (dataUrl) {
            editor.chain().focus().setImage({ src: dataUrl }).run();
          }
        };
        input.click();
      },
    },
  ];

  // AI skill items — only shown when an onAISkill callback is wired
  // (managed_encrypted folders with rich extensions). The command
  // deletes the "/" range, gathers preceding text as context, and
  // invokes the skill via the callback.
  if (onAISkill) {
    const aiItems: SlashCommandItem[] = [
      {
        title: "Continue writing",
        description: "AI continues from where you left off",
        icon: "Sparkles",
        category: "ai",
        richOnly: true,
        command: (editor, range) => {
          const $from = editor.state.doc.resolve(range.from);
          const precedingText = $from.parent.textContent.slice(0, range.from - $from.start());
          editor.chain().focus().deleteRange(range).run();
          onAISkill("expand", precedingText, "");
        },
      },
      {
        title: "Generate ideas",
        description: "AI generates ideas based on the current topic",
        icon: "Lightbulb",
        category: "ai",
        richOnly: true,
        command: (editor, range) => {
          const $from = editor.state.doc.resolve(range.from);
          const precedingText = $from.parent.textContent.slice(0, range.from - $from.start());
          editor.chain().focus().deleteRange(range).run();
          onAISkill("generate_ideas", precedingText, "");
        },
      },
      {
        title: "Summarize",
        description: "AI summarizes the current paragraph",
        icon: "FileText",
        category: "ai",
        richOnly: true,
        command: (editor, range) => {
          const $from = editor.state.doc.resolve(range.from);
          const text = $from.parent.textContent;
          const paraRange = { from: $from.start(), to: $from.end() };
          editor.chain().focus().deleteRange(range).run();
          onAISkill("summarize", "", text, paraRange);
        },
      },
      {
        title: "Improve writing",
        description: "AI improves clarity and grammar of the current paragraph",
        icon: "Wand2",
        category: "ai",
        richOnly: true,
        command: (editor, range) => {
          const $from = editor.state.doc.resolve(range.from);
          const text = $from.parent.textContent;
          const paraRange = { from: $from.start(), to: $from.end() };
          editor.chain().focus().deleteRange(range).run();
          onAISkill("improve_writing", "", text, paraRange);
        },
      },
    ];
    allItems.push(...aiItems);
  }

  const filtered = allItems.filter((item) => {
    if (item.richOnly && !richExtensionsAllowed) return false;
    return (
      item.title.toLowerCase().includes(q) ||
      item.description.toLowerCase().includes(q)
    );
  });

  return filtered;
}

// The SlashCommand extension wraps TipTap's Suggestion plugin to
// provide a Notion-style "/" menu. The rendering is delegated to a
// React component (SlashMenuView) via the suggestion.render callback,
// which is set up in SlashMenu.tsx.
export const SlashCommand = Extension.create<SlashCommandOptions>({
  name: "slashCommand",

  addOptions() {
    return {
      richExtensionsAllowed: false,
      onAISkill: undefined as AISkillTrigger | undefined,
      suggestion: {
        items: defaultItems,
        render: () => ({
          onStart: () => {},
          onUpdate: () => {},
          onExit: () => {},
          onKeyDown: () => false,
        }),
      },
    };
  },

  addProseMirrorPlugins() {
    const opts = this.options;
    return [
      Suggestion<SlashCommandItem>({
        pluginKey: slashCommandPluginKey,
        editor: this.editor,
        char: "/",
        startOfLine: false,
        allow: ({ state, range }: { state: import("@tiptap/pm/state").EditorState; range: Range }) => {
          const $from = state.doc.resolve(range.from);
          const type = $from.parent.type;
          // Only trigger in empty or text paragraphs and headings,
          // not inside code blocks or other leaf nodes.
          return (
            type.name === "paragraph" || type.name === "heading"
          );
        },
        command: ({ editor, range, props }: { editor: Editor; range: Range; props: SlashCommandItem }) => {
          props.command(editor, range);
        },
        items: ({ query, editor }: { query: string; editor: Editor }) => {
          const itemsFn = opts.suggestion?.items ?? defaultItems;
          return itemsFn({
            query,
            editor,
            richExtensionsAllowed: opts.richExtensionsAllowed,
            onAISkill: opts.onAISkill,
          });
        },
        render: opts.suggestion?.render,
      }),
    ];
  },
});

export { slashCommandPluginKey };
