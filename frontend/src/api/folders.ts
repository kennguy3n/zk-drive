import { listFolders, type Folder } from "./client";

// Max folder listings to keep in flight at once while walking a level. A wide
// level in a large workspace (hundreds of sibling folders) would otherwise
// fire one /folders request per folder simultaneously; the pool caps that
// burst so the API isn't hammered while the walk still proceeds in parallel.
export const FOLDER_FETCH_CONCURRENCY = 8;

function aborted(signal?: AbortSignal): boolean {
  return signal?.aborted ?? false;
}

function abortError(): DOMException {
  return new DOMException("Aborted", "AbortError");
}

// mapWithConcurrency runs `task` over `items` with at most `limit` calls in
// flight at once, preserving input order in the results. A shared cursor hands
// each worker the next index, so faster tasks pick up more work instead of the
// pool stalling on the slowest item in a fixed chunk.
export async function mapWithConcurrency<T, R>(
  items: readonly T[],
  limit: number,
  task: (item: T, index: number) => Promise<R>,
): Promise<R[]> {
  const results = new Array<R>(items.length);
  let next = 0;
  const worker = async (): Promise<void> => {
    for (let i = next++; i < items.length; i = next++) {
      results[i] = await task(items[i], i);
    }
  };
  const size = Math.max(1, Math.min(limit, items.length));
  await Promise.all(Array.from({ length: size }, worker));
  return results;
}

// collectAllFolders walks the workspace folder tree breadth-first into a flat
// list. The API lists a single level at a time (listFolders(parentID)) and
// exposes no recursive "all folders" endpoint, so the move/copy picker has to
// assemble the destination list itself. The folder tree is acyclic, so the
// loop terminates once a level has no children.
//
// `signal` aborts the walk if the caller goes away (e.g. the user navigates
// off the page mid-collection on a large workspace): it is threaded into every
// listFolders request so in-flight HTTP calls are cancelled, and re-checked
// between levels so a pending walk stops promptly. Each level is fetched with
// bounded concurrency so a wide level can't burst one request per folder.
export async function collectAllFolders(signal?: AbortSignal): Promise<Folder[]> {
  if (aborted(signal)) throw abortError();
  const all: Folder[] = [];
  let level = await listFolders(null, signal);
  while (level.length > 0) {
    all.push(...level);
    if (aborted(signal)) throw abortError();
    const children = await mapWithConcurrency(level, FOLDER_FETCH_CONCURRENCY, (f) =>
      listFolders(f.id, signal),
    );
    level = children.flat();
  }
  return all;
}
