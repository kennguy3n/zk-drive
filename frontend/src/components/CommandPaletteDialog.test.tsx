import { describe, it, expect, vi, afterEach, beforeEach } from "vitest";
import { render, screen, fireEvent, cleanup } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";
import CommandPaletteDialog from "./CommandPaletteDialog";
import type { SearchResponse } from "../api/client";

// searchFiles is replaced per-test with a deferred so we can control exactly
// when the (debounced) request resolves relative to the query being cleared.
vi.mock("../api/client", async (orig) => {
  const actual = await orig<typeof import("../api/client")>();
  return { ...actual, searchFiles: vi.fn() };
});

import { searchFiles } from "../api/client";

function deferred<T>() {
  let resolve!: (v: T) => void;
  const promise = new Promise<T>((r) => {
    resolve = r;
  });
  return { promise, resolve };
}

function renderDialog() {
  return render(
    <MemoryRouter>
      <CommandPaletteDialog open onOpenChange={() => {}} />
    </MemoryRouter>,
  );
}

describe("CommandPaletteDialog", () => {
  beforeEach(() => {
    vi.useFakeTimers();
    localStorage.clear();
  });

  afterEach(() => {
    vi.runOnlyPendingTimers();
    vi.useRealTimers();
    vi.clearAllMocks();
    cleanup();
  });

  it("discards a search response that resolves after the query is cleared", async () => {
    const d = deferred<SearchResponse>();
    vi.mocked(searchFiles).mockReturnValue(d.promise);

    renderDialog();
    const input = screen.getByPlaceholderText("Search files and folders…");

    // Type a query and let the debounce fire so searchFiles is in-flight.
    fireEvent.change(input, { target: { value: "report" } });
    await vi.advanceTimersByTimeAsync(300);
    expect(searchFiles).toHaveBeenCalledTimes(1);

    // Clear the query before the in-flight request resolves (mirrors closing +
    // reopening the palette, which resets query to "").
    fireEvent.change(input, { target: { value: "" } });

    // The stale response now resolves — it must NOT repopulate the palette.
    d.resolve({
      query: "report",
      limit: 20,
      offset: 0,
      hits: [
        {
          type: "file",
          id: "f1",
          name: "stale-result.txt",
          path: "/stale-result.txt",
          workspace_id: "ws",
          folder_id: null,
          updated_at: "2024-01-01T00:00:00Z",
        },
      ],
    });
    await vi.runAllTimersAsync();

    expect(screen.queryByText("stale-result.txt")).toBeNull();
  });
});
