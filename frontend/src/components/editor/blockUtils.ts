import type { Node as PMNode, ResolvedPos } from "@tiptap/pm/model";

// findBlockStart resolves a document position to the start of its
// containing top-level block (depth 1). Returns null if the position
// is at the document root (depth 0).
//
// Shared by DragHandle and BlockMenu — extracted to avoid duplication.
export function findBlockStart(pos: number, doc: PMNode): number | null {
  const $pos: ResolvedPos = doc.resolve(pos);
  if ($pos.depth === 0) return null;
  let depth = $pos.depth;
  let $cur: ResolvedPos = $pos;
  while (depth > 1) {
    $cur = doc.resolve($cur.before(depth));
    depth = $cur.depth;
  }
  return $cur.before(depth);
}

// isTopLevelBlock returns true if the given position is the start of
// a direct child of the document root (depth 1).
export function isTopLevelBlock(pos: number, doc: PMNode): boolean {
  const $pos = doc.resolve(pos);
  return $pos.depth === 1;
}
