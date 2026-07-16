import { Extension } from "@tiptap/core";
import { Plugin, PluginKey } from "@tiptap/pm/state";
import { Decoration, DecorationSet } from "@tiptap/pm/view";
import type { EditorView } from "@tiptap/pm/view";
import { DOMSerializer } from "@tiptap/pm/model";
import { findBlockStart, isTopLevelBlock } from "./blockUtils";

// BlockDragHandle is a TipTap extension that adds a drag handle to the
// left side of top-level blocks (paragraphs, headings, lists, etc.) on
// hover. The handle serves as a drag source for reordering blocks via
// native HTML5 drag-and-drop — ProseMirror's built-in drop handling
// moves the node to the new position.
//
// The handle is rendered as a ProseMirror decoration (a widget) so it
// participates in the editor's render cycle without polluting the
// document state. A single decoration is created for the block nearest
// the mouse cursor; it moves as the user hovers different blocks.
//
// Performance: mousemove is throttled via requestAnimationFrame so we
// do at most one posAtCoords + dispatch per frame. The drag image
// element is cleaned up on dragend (not setTimeout) to avoid leaks if
// the drag is cancelled.

const dragHandlePluginKey = new PluginKey("blockDragHandle");

interface DragHandleOptions {
  handleClass?: string;
  handleWidth?: number;
}

// Stored drag source info for verifying the node hasn't changed
// between dragstart and drop (important in collaborative mode).
interface DragSourceInfo {
  pos: number;
  type: string;
  nodeSize: number;
  textContent: string;
}

export const BlockDragHandle = Extension.create<DragHandleOptions>({
  name: "blockDragHandle",

  addOptions() {
    return {
      handleClass: "block-drag-handle",
      handleWidth: 24,
    };
  },

  addProseMirrorPlugins() {
    const opts = this.options;
    // rAF throttle state — ensures at most one mousemove processing per frame.
    let rafId: number | null = null;
    let lastMoveEvent: MouseEvent | null = null;
    // Drag source verification — set in dragstart, checked in drop.
    let dragSourceInfo: DragSourceInfo | null = null;
    // Reference to the drag image element for cleanup on dragend.
    let dragImageEl: HTMLElement | null = null;

    function processMouseMove(view: EditorView) {
      rafId = null;
      const event = lastMoveEvent;
      lastMoveEvent = null;
      if (!event) return;

      const pluginState = dragHandlePluginKey.getState(view.state) as {
        deco: Decoration | null;
        blockPos: number | null;
      };
      const rect = view.dom.getBoundingClientRect();
      const x = event.clientX - rect.left;

      if (x > opts.handleWidth! || x < 0) {
        if (pluginState?.deco) {
          view.dispatch(
            view.state.tr.setMeta(dragHandlePluginKey, {
              deco: null,
              blockPos: null,
            }),
          );
        }
        return;
      }

      const pos = view.posAtCoords({ left: event.clientX, top: event.clientY });
      if (!pos) return;

      const blockPos = findBlockStart(pos.pos, view.state.doc);
      if (blockPos === null) return;

      if (!isTopLevelBlock(blockPos, view.state.doc)) return;

      if (pluginState?.blockPos === blockPos) return;

      const handle = document.createElement("div");
      handle.className = opts.handleClass!;
      handle.setAttribute("draggable", "true");
      handle.setAttribute("contenteditable", "false");
      handle.setAttribute("aria-label", "Drag to reorder");
      handle.dataset.blockPos = String(blockPos);

      handle.addEventListener("dragstart", (e: DragEvent) => {
        if (!e.dataTransfer) return;
        const node = view.state.doc.nodeAt(blockPos);
        if (!node) return;
        const serializer = DOMSerializer.fromSchema(view.state.schema);
        const dom = serializer.serializeNode(node);
        const tempDiv = document.createElement("div");
        tempDiv.appendChild(dom);
        e.dataTransfer.setData("text/html", tempDiv.innerHTML);
        e.dataTransfer.setData("text/plain", node.textContent);
        e.dataTransfer.effectAllowed = "move";
        e.dataTransfer.setData("application/x-block-drag", String(blockPos));
        dragSourceInfo = {
          pos: blockPos,
          type: node.type.name,
          nodeSize: node.nodeSize,
          textContent: node.textContent,
        };
        dragImageEl = handle.cloneNode(true) as HTMLElement;
        dragImageEl.style.opacity = "0.7";
        document.body.appendChild(dragImageEl);
        e.dataTransfer.setDragImage(dragImageEl, 0, 0);
      });

      handle.addEventListener("dragend", () => {
        if (dragImageEl) {
          dragImageEl.remove();
          dragImageEl = null;
        }
        dragSourceInfo = null;
      });

      const deco = Decoration.widget(blockPos, handle, {
        side: -1,
        key: `drag-${blockPos}`,
      });

      view.dispatch(
        view.state.tr.setMeta(dragHandlePluginKey, {
          deco,
          blockPos,
        }),
      );
    }

    return [
      new Plugin({
        key: dragHandlePluginKey,
        state: {
          init() {
            return { deco: null as Decoration | null, blockPos: null as number | null };
          },
          apply(tr, prev) {
            if (tr.getMeta(dragHandlePluginKey)) {
              return tr.getMeta(dragHandlePluginKey);
            }
            if (tr.docChanged) {
              return { deco: null, blockPos: null };
            }
            return prev;
          },
        },
        props: {
          decorations(state) {
            const pluginState = dragHandlePluginKey.getState(state) as {
              deco: Decoration | null;
              blockPos: number | null;
            };
            if (!pluginState || !pluginState.deco) return DecorationSet.empty;
            return DecorationSet.create(state.doc, [pluginState.deco]);
          },
          handleDOMEvents: {
            mousemove: (view, event: MouseEvent) => {
              lastMoveEvent = event;
              if (rafId === null) {
                rafId = requestAnimationFrame(() => processMouseMove(view));
              }
              return false;
            },
            mouseleave: (view) => {
              if (rafId !== null) {
                cancelAnimationFrame(rafId);
                rafId = null;
              }
              lastMoveEvent = null;
              const pluginState = dragHandlePluginKey.getState(view.state) as {
                deco: Decoration | null;
                blockPos: number | null;
              };
              if (pluginState?.deco) {
                view.dispatch(
                  view.state.tr.setMeta(dragHandlePluginKey, {
                    deco: null,
                    blockPos: null,
                  }),
                );
              }
              return false;
            },
            drop: (view, event: DragEvent) => {
              const dragData = event.dataTransfer?.getData("application/x-block-drag");
              if (!dragData) return false;
              const sourcePos = parseInt(dragData, 10);
              if (isNaN(sourcePos)) return false;

              // Verify the source node hasn't changed since dragstart.
              if (!dragSourceInfo) return false;

              const dropPos = view.posAtCoords({
                left: event.clientX,
                top: event.clientY,
              });
              if (!dropPos) return false;

              const targetBlockPos = findBlockStart(dropPos.pos, view.state.doc);
              if (targetBlockPos === null || targetBlockPos === sourcePos) return false;

              const sourceNode = view.state.doc.nodeAt(sourcePos);
              if (!sourceNode) return false;

              // Verify the node at sourcePos is the same one we started
              // dragging — type, size, and content must match.
              const currentSig = `${sourceNode.type.name}:${sourceNode.nodeSize}:${sourceNode.textContent}`;
              const originalSig = `${dragSourceInfo.type}:${dragSourceInfo.nodeSize}:${dragSourceInfo.textContent}`;
              if (currentSig !== originalSig) return false;

              event.preventDefault();

              const tr = view.state.tr;
              let adjustedTarget = targetBlockPos;
              if (targetBlockPos > sourcePos) {
                adjustedTarget -= sourceNode.nodeSize;
              }
              tr.delete(sourcePos, sourcePos + sourceNode.nodeSize);
              tr.insert(adjustedTarget, sourceNode);
              view.dispatch(tr);
              return true;
            },
          },
        },
      }),
    ];
  },
});

export { dragHandlePluginKey };
