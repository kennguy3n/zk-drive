// Unit tests for the move/copy folder-collection layer. These lock in three
// properties that keep the picker usable on large workspaces:
//
//  - breadth-first completeness: every folder in the tree ends up in the flat
//    list regardless of depth;
//  - cancellation: an aborted signal stops the walk promptly, propagates an
//    AbortError, and is threaded into every listFolders request;
//  - bounded concurrency: a wide level never fires more than the configured
//    number of requests at once (so we don't burst hundreds of calls).
import { describe, expect, it, vi, beforeEach } from "vitest";

import type { Folder } from "./client";

const { listFolders } = vi.hoisted(() => ({ listFolders: vi.fn() }));
vi.mock("./client", () => ({ listFolders }));

import { collectAllFolders, mapWithConcurrency, FOLDER_FETCH_CONCURRENCY } from "./folders";

function folder(id: string): Folder {
  // The collection layer only reads `id`; the rest of Folder is irrelevant
  // here, so a minimal cast keeps the fixtures readable.
  return { id, name: id } as unknown as Folder;
}

// Builds a listFolders mock backed by an adjacency map (parentID -> children).
// `null` is the root key.
function treeMock(children: Record<string, Folder[]>) {
  return (parentID: string | null) =>
    Promise.resolve(children[parentID ?? "__root__"] ?? []);
}

function deferred<T>() {
  let resolve!: (v: T) => void;
  const promise = new Promise<T>((r) => {
    resolve = r;
  });
  return { promise, resolve };
}

beforeEach(() => {
  listFolders.mockReset();
});

describe("collectAllFolders", () => {
  it("collects the whole tree breadth-first", async () => {
    listFolders.mockImplementation(
      treeMock({
        __root__: [folder("a"), folder("b")],
        a: [folder("a1"), folder("a2")],
        b: [],
        a1: [],
        a2: [folder("a2x")],
        a2x: [],
      }),
    );

    const all = await collectAllFolders();
    expect(all.map((f) => f.id).sort()).toEqual(["a", "a1", "a2", "a2x", "b"]);
  });

  it("returns an empty list when the workspace has no folders", async () => {
    listFolders.mockImplementation(treeMock({}));
    expect(await collectAllFolders()).toEqual([]);
    expect(listFolders).toHaveBeenCalledTimes(1);
  });

  it("threads the abort signal into every request", async () => {
    listFolders.mockImplementation(
      treeMock({ __root__: [folder("a"), folder("b")], a: [], b: [] }),
    );
    const controller = new AbortController();
    await collectAllFolders(controller.signal);
    expect(listFolders).toHaveBeenCalled();
    for (const call of listFolders.mock.calls) {
      expect(call[1]).toBe(controller.signal);
    }
  });

  it("throws without making any request when already aborted", async () => {
    listFolders.mockImplementation(treeMock({ __root__: [folder("a")] }));
    const controller = new AbortController();
    controller.abort();
    await expect(collectAllFolders(controller.signal)).rejects.toMatchObject({
      name: "AbortError",
    });
    expect(listFolders).not.toHaveBeenCalled();
  });

  it("stops descending once the signal aborts mid-walk", async () => {
    const controller = new AbortController();
    // Root resolves normally; abort fires before the child level is walked.
    listFolders.mockImplementation((parentID: string | null) => {
      if (parentID === null) {
        controller.abort();
        return Promise.resolve([folder("a"), folder("b")]);
      }
      return Promise.resolve([]);
    });

    await expect(collectAllFolders(controller.signal)).rejects.toMatchObject({
      name: "AbortError",
    });
    // Only the root listing ran; the child level was never fetched.
    expect(listFolders).toHaveBeenCalledTimes(1);
  });

  it("never exceeds the concurrency limit on a wide level", async () => {
    const wide = Array.from({ length: 25 }, (_, i) => folder(`f${i}`));
    let inFlight = 0;
    let maxInFlight = 0;
    const gates: Array<() => void> = [];

    listFolders.mockImplementation((parentID: string | null) => {
      if (parentID === null) return Promise.resolve(wide);
      inFlight++;
      maxInFlight = Math.max(maxInFlight, inFlight);
      const d = deferred<Folder[]>();
      gates.push(() => {
        inFlight--;
        d.resolve([]);
      });
      return d.promise;
    });

    const walk = collectAllFolders();
    // Let the pool saturate, then release the child requests in waves.
    while (gates.length < FOLDER_FETCH_CONCURRENCY) await Promise.resolve();
    expect(maxInFlight).toBeLessThanOrEqual(FOLDER_FETCH_CONCURRENCY);
    while (gates.length > 0) {
      gates.shift()!();
      await Promise.resolve();
    }
    await walk;
    expect(maxInFlight).toBeLessThanOrEqual(FOLDER_FETCH_CONCURRENCY);
  });
});

describe("mapWithConcurrency", () => {
  it("preserves input order in the results", async () => {
    const out = await mapWithConcurrency([1, 2, 3, 4], 2, async (n) => n * 10);
    expect(out).toEqual([10, 20, 30, 40]);
  });

  it("passes the index to the task", async () => {
    const out = await mapWithConcurrency(["a", "b", "c"], 5, async (v, i) => `${v}${i}`);
    expect(out).toEqual(["a0", "b1", "c2"]);
  });

  it("handles an empty input without invoking the task", async () => {
    const task = vi.fn();
    expect(await mapWithConcurrency([], 4, task)).toEqual([]);
    expect(task).not.toHaveBeenCalled();
  });

  it("caps the number of concurrent tasks", async () => {
    let inFlight = 0;
    let maxInFlight = 0;
    const items = Array.from({ length: 12 }, (_, i) => i);
    await mapWithConcurrency(items, 3, async (n) => {
      inFlight++;
      maxInFlight = Math.max(maxInFlight, inFlight);
      await Promise.resolve();
      inFlight--;
      return n;
    });
    expect(maxInFlight).toBeLessThanOrEqual(3);
  });
});
