import { describe, it, expect, vi, afterEach, beforeEach } from "vitest";
import { render, screen, fireEvent, cleanup } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";
import SearchBar from "./SearchBar";
import type { SearchResponse, SearchHit } from "../api/client";

// searchFiles is replaced per-test with deferreds so we can control
// exactly when each (debounced) request resolves relative to the
// others — the only way to exercise the out-of-order response race.
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

function resp(name: string): SearchResponse {
  const hit: SearchHit = {
    type: "file",
    id: name,
    name,
    path: `/${name}`,
    workspace_id: "ws",
    folder_id: null,
    updated_at: "2024-01-01T00:00:00Z",
  };
  return { query: name, limit: 20, offset: 0, hits: [hit] };
}

function renderBar() {
  return render(
    <MemoryRouter>
      <SearchBar />
    </MemoryRouter>,
  );
}

describe("SearchBar", () => {
  beforeEach(() => {
    vi.useFakeTimers();
  });
  afterEach(() => {
    vi.runOnlyPendingTimers();
    vi.useRealTimers();
    vi.clearAllMocks();
    cleanup();
  });

  it("ignores a stale response that resolves after a newer query fired", async () => {
    const stale = deferred<SearchResponse>();
    const fresh = deferred<SearchResponse>();
    vi.mocked(searchFiles)
      .mockReturnValueOnce(stale.promise)
      .mockReturnValueOnce(fresh.promise);

    renderBar();
    const input = screen.getByPlaceholderText("Search files and folders…");

    // First query fires and goes in-flight.
    fireEvent.change(input, { target: { value: "rep" } });
    await vi.advanceTimersByTimeAsync(250);
    // Second query supersedes it and also goes in-flight.
    fireEvent.change(input, { target: { value: "report" } });
    await vi.advanceTimersByTimeAsync(250);
    expect(searchFiles).toHaveBeenCalledTimes(2);

    // The newer request resolves first and populates the dropdown.
    fresh.resolve(resp("fresh-result.txt"));
    await vi.runAllTimersAsync();
    expect(screen.queryByText("fresh-result.txt")).not.toBeNull();

    // The older request resolves last — it must NOT clobber the
    // current results with stale data for a superseded query.
    stale.resolve(resp("stale-result.txt"));
    await vi.runAllTimersAsync();
    expect(screen.queryByText("stale-result.txt")).toBeNull();
    expect(screen.queryByText("fresh-result.txt")).not.toBeNull();
  });

  it("discards a response that resolves after the query is cleared", async () => {
    const d = deferred<SearchResponse>();
    vi.mocked(searchFiles).mockReturnValue(d.promise);

    renderBar();
    const input = screen.getByPlaceholderText("Search files and folders…");

    fireEvent.change(input, { target: { value: "report" } });
    await vi.advanceTimersByTimeAsync(250);
    expect(searchFiles).toHaveBeenCalledTimes(1);

    // Clear the box before the in-flight request resolves.
    fireEvent.change(input, { target: { value: "" } });
    d.resolve(resp("stale-result.txt"));
    await vi.runAllTimersAsync();

    expect(screen.queryByText("stale-result.txt")).toBeNull();
  });
});
